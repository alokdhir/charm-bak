import Darwin
import Foundation
import Testing
@testable import CharmBakCore

/// testdata/protocol, shared with the Go tests.
let fixtureDir = URL(filePath: #filePath)
    .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
    .deletingLastPathComponent().appending(path: "testdata/protocol")

func fixtures() throws -> [(String, [String: Any])] {
    let files = try FileManager.default.contentsOfDirectory(at: fixtureDir, includingPropertiesForKeys: nil)
        .filter { $0.pathExtension == "json" }
        .sorted { $0.lastPathComponent < $1.lastPathComponent }
    return try files.map { url in
        let obj = try JSONSerialization.jsonObject(with: Data(contentsOf: url)) as! [String: Any]
        return (url.lastPathComponent, obj)
    }
}

func json(_ value: Any) throws -> Data {
    try JSONSerialization.data(withJSONObject: value, options: [.fragmentsAllowed])
}

/// Drops nulls and turns RFC 3339 times into instants, so "absent" and
/// "null" compare equal (Swift omits nil optionals, Go writes null) and
/// time zones don't matter.
func normalized(_ v: Any) -> Any {
    if let d = v as? [String: Any] {
        return d.filter { !($0.value is NSNull) }.mapValues(normalized)
    }
    if let a = v as? [Any] { return a.map(normalized) }
    if let s = v as? String, s.count >= 20, s.dropFirst(4).first == "-", s.contains("T"),
       let d = Wire.parseTime(s) {
        return d.timeIntervalSince1970
    }
    return v
}

func roundTripsAs<T: Codable>(_ type: T.Type, _ value: Any, _ what: String) throws {
    let decoded = try Wire.decoder.decode(T.self, from: json(value))
    let back = try JSONSerialization.jsonObject(with: Wire.encoder.encode(decoded), options: [.fragmentsAllowed])
    let want = normalized(value) as AnyObject
    let got = normalized(back) as AnyObject
    #expect(want.isEqual(got), "\(what): round trip differs\nwant \(want)\n got \(got)")
}

/// Opens the existential so `roundTripsAs` gets a concrete type.
func roundTrips(_ type: any Decodable.Type, _ value: Any, _ what: String) throws {
    guard let codable = type as? any Codable.Type else { return }
    func open<T: Codable>(_: T.Type) throws { try roundTripsAs(T.self, value, what) }
    try open(codable)
}

@Test func everyExampleDecodesAndRoundTrips() throws {
    var methodsSeen = Set<String>()
    var eventsSeen = Set<String>()
    for (name, fx) in try fixtures() {
        if let ev = fx["event"] as? [String: Any] {
            let event = try #require(try Event.decode(json(ev)), "\(name): unknown event")
            eventsSeen.insert(event.name)
            let back = try JSONSerialization.jsonObject(with: event.encoded())
            #expect((normalized(ev) as AnyObject).isEqual(normalized(back)), "\(name): event round trip differs")
            continue
        }
        let req = try #require(fx["request"] as? [String: Any], "\(name): no request")
        let rep = try #require(fx["reply"] as? [String: Any], "\(name): no reply")
        let method = try #require(Method(rawValue: req["method"] as? String ?? ""), "\(name): unknown method")
        let (paramsType, resultType) = method.types
        if let paramsType, let params = req["params"] {
            _ = try Wire.decoder.decode(paramsType, from: json(params))
        }
        if let err = rep["error"] as? [String: Any] {
            #expect(ErrorCode(rawValue: err["code"] as? String ?? "") != nil, "\(name): unknown error code")
            continue
        }
        try roundTrips(resultType, try #require(rep["result"], "\(name): no result"), name)
        methodsSeen.insert(method.rawValue)
    }
    let all = Set(Method.allCases.map(\.rawValue))
    #expect(methodsSeen == all, "methods without an example: \(all.subtracting(methodsSeen))")
    #expect(eventsSeen.count == 7, "events seen: \(eventsSeen)")
}

@Test func parsesNanosecondTimes() throws {
    let d = try #require(Wire.parseTime("2026-11-19T12:57:13.548012172-05:00"))
    let base = try #require(Wire.parseTime("2026-11-19T12:57:13-05:00"))
    #expect(abs(d.timeIntervalSince(base) - 0.548012172) < 0.000001)
}

/// DaemonClient against a one-shot stand-in daemon on a temp socket.
@Test func clientCallsAndReportsErrors() throws {
    let path = "/tmp/cbs-\(UUID().uuidString.prefix(6)).sock"
    let listener = socket(AF_UNIX, SOCK_STREAM, 0)
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    withUnsafeMutableBytes(of: &addr.sun_path) { buf in
        for (i, b) in Array(path.utf8CString).enumerated() { buf[i] = UInt8(bitPattern: b) }
    }
    _ = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    listen(listener, 4)
    defer { close(listener); unlink(path) }

    let replies = [
        #"{"id":1,"result":{"state":"paused","paused":true,"job":null,"repository":null}}"#,
        #"{"id":1,"error":{"code":"not_configured","message":"No repository is set up yet."}}"#,
    ]
    let served = DispatchSemaphore(value: 0)
    DispatchQueue.global().async {
        for reply in replies {
            let fd = accept(listener, nil, nil)
            readRequest(fd)
            try? DaemonClient.writeLine(fd, Data(reply.utf8))
            close(fd)
        }
        served.signal()
    }

    let status = try DaemonClient.call("status", as: StatusResult.self, socket: path)
    #expect(status.state == .paused && status.paused)
    #expect(throws: DaemonClient.Failure.self) {
        _ = try DaemonClient.call("sources", as: SourcesResult.self, socket: path)
    }
    served.wait()
    #expect(throws: DaemonClient.Failure.self) {
        _ = try DaemonClient.call("status", as: StatusResult.self, socket: "/tmp/cbs-nothing-here.sock")
    }
}

/// Reads what the client sends on a connection: hello, then the request.
/// (Stops early if the client hangs up, as `reachable` does.)
func readRequest(_ fd: Int32) {
    var n = 0
    DaemonClient.readLines(fd) { _ in n += 1; return n < 2 }
}

/// A stand-in daemon: for each connection, `serve` gets the file descriptor.
func withStandIn(_ connections: Int, _ serve: @escaping @Sendable (Int32, Int) -> Void, _ body: (String) throws -> Void) rethrows {
    let path = "/tmp/cbs-\(UUID().uuidString.prefix(6)).sock"
    let listener = socket(AF_UNIX, SOCK_STREAM, 0)
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    withUnsafeMutableBytes(of: &addr.sun_path) { buf in
        for (i, b) in Array(path.utf8CString).enumerated() { buf[i] = UInt8(bitPattern: b) }
    }
    _ = withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
    listen(listener, 4)
    defer { close(listener); unlink(path) }
    DispatchQueue.global().async {
        for i in 0..<connections {
            let fd = accept(listener, nil, nil)
            if fd < 0 { return }
            serve(fd, i)
            close(fd)
        }
    }
    try body(path)
}

@Test func clientSaysHelloFirstAndReportsAMismatch() throws {
    let firstLine = Locked("")
    try withStandIn(1, { fd, _ in
        var n = 0
        DaemonClient.readLines(fd) { line in
            if n == 0 { firstLine.set(String(decoding: line, as: UTF8.self)) }
            n += 1
            return n < 2
        }
        try? DaemonClient.writeLine(fd, Data(#"{"id":0,"error":{"code":"unsupported_protocol","message":"This daemon speaks protocol 2."}}"#.utf8))
    }) { path in
        do {
            _ = try DaemonClient.call("status", as: StatusResult.self, socket: path)
            Issue.record("a mismatched daemon was accepted")
        } catch DaemonClient.Failure.protocolError(let code, let message) {
            #expect(code == "unsupported_protocol" && message.contains("protocol 2"))
        }
    }
    #expect(firstLine.get().contains(#""method":"hello""#), "first line was \(firstLine.get())")
}

@Test func silentDaemonIsNoReplyNotNotRunning() throws {
    try withStandIn(2, { fd, _ in
        readRequest(fd)
        // Accept, read, say nothing, hang up.
    }) { path in
        #expect(DaemonClient.reachable(socket: path))
        do {
            _ = try DaemonClient.call("status", as: StatusResult.self, socket: path)
            Issue.record("no reply was treated as success")
        } catch DaemonClient.Failure.noReply {
        } catch {
            Issue.record("want noReply, got \(error)")
        }
    }
    #expect(!DaemonClient.reachable(socket: "/tmp/cbs-nothing-here.sock"))
}

/// A value guarded by a lock, for state shared with the stand-in's thread.
final class Locked<T>: @unchecked Sendable {
    private var value: T
    private let lock = NSLock()
    init(_ value: T) { self.value = value }
    func get() -> T { lock.withLock { value } }
    func set(_ v: T) { lock.withLock { value = v } }
}
