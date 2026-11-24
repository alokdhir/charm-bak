import Darwin
import Foundation

/// The menu bar's connection to charm-bak-daemon (docs/protocol.md). The
/// menu needs little: status, sources, pause/resume, and events to know
/// when to refresh. Each request uses its own short connection; events
/// come on one long-lived subscription.
public enum DaemonClient {
    public enum Failure: Error, CustomStringConvertible {
        /// Nothing accepts connections on the socket.
        case notRunning
        /// It accepted the connection but didn't answer in time: busy or
        /// stuck, not absent. Never a reason to start another daemon.
        case noReply
        case protocolError(code: String, message: String)
        case bad(String)

        public var description: String {
            switch self {
            case .notRunning: "the daemon isn't running"
            case .noReply: "the daemon didn't answer"
            case .protocolError(_, let m): m
            case .bad(let m): m
            }
        }
    }

    static func connect(_ path: String) throws -> Int32 {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw Failure.notRunning }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8CString)
        guard bytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else { close(fd); throw Failure.bad("socket path too long") }
        withUnsafeMutableBytes(of: &addr.sun_path) { buf in
            for (i, b) in bytes.enumerated() { buf[i] = UInt8(bitPattern: b) }
        }
        let ok = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                Darwin.connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard ok == 0 else { close(fd); throw Failure.notRunning }
        var on: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &on, socklen_t(MemoryLayout<Int32>.size))
        var tv = timeval(tv_sec: 10, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
        return fd
    }

    static func writeLine(_ fd: Int32, _ data: Data) throws {
        var out = data
        out.append(UInt8(ascii: "\n"))
        try out.withUnsafeBytes { raw in
            var off = 0
            while off < raw.count {
                let n = Darwin.write(fd, raw.baseAddress! + off, raw.count - off)
                // Connected, so something is there; it just went away or
                // stalled mid-request.
                if n <= 0 { throw Failure.noReply }
                off += n
            }
        }
    }

    /// Reads lines from fd, calling `line` for each, until it returns
    /// false or the connection ends.
    static func readLines(_ fd: Int32, _ line: (Data) -> Bool) {
        var buffer = Data()
        var chunk = [UInt8](repeating: 0, count: 64 << 10)
        while true {
            while let nl = buffer.firstIndex(of: UInt8(ascii: "\n")) {
                let l = Data(buffer[buffer.startIndex..<nl])
                buffer = Data(buffer[buffer.index(after: nl)...])
                if !l.isEmpty && !line(l) { return }
            }
            let n = read(fd, &chunk, chunk.count)
            if n <= 0 { return }
            buffer.append(contentsOf: chunk[0..<n])
        }
    }

    /// Whether something accepts connections on the socket. This, not a
    /// timed-out request, decides whether a daemon needs starting.
    public static func reachable(socket: String = ProtocolInfo.socketPath) -> Bool {
        guard let fd = try? connect(socket) else { return false }
        close(fd)
        return true
    }

    private struct Request<P: Encodable>: Encodable { var id: Int; var method: String; var params: P?; var host: String? = nil }
    private struct NoParams: Encodable {}
    private struct ReplyHead: Decodable {
        struct Err: Decodable { var code: String; var message: String }
        var id: Int?
        var error: Err?
    }
    private struct ResultOnly<R: Decodable>: Decodable { var result: R }

    /// hello is the first request on every connection (docs/protocol.md),
    /// so a daemon speaking another protocol version is caught here.
    private static func hello() throws -> Data {
        try Wire.encoder.encode(Request(id: 0, method: "hello",
                                        params: HelloParams(protocol: ProtocolInfo.version, client: "charm-bak-menu")))
    }

    /// One request on its own connection.
    /// `host` relays the call to that paired remote.
    public static func call<R: Decodable>(_ method: String, params: (some Encodable)? = Optional<Empty>.none, host: String? = nil,
                                          as: R.Type, socket: String = ProtocolInfo.socketPath) throws -> R {
        let fd = try connect(socket)
        defer { close(fd) }
        try writeLine(fd, hello())
        try writeLine(fd, Wire.encoder.encode(Request(id: 1, method: method, params: params, host: host)))
        var reply: Data?
        var refused: Failure?
        readLines(fd) { line in
            guard let head = try? Wire.decoder.decode(ReplyHead.self, from: line) else { return true }
            if head.id == 0 {
                if let e = head.error { refused = .protocolError(code: e.code, message: e.message); return false }
                return true
            }
            reply = line
            return false
        }
        if let refused { throw refused }
        guard let reply else { throw Failure.noReply }
        if let err = try Wire.decoder.decode(ReplyHead.self, from: reply).error {
            throw Failure.protocolError(code: err.code, message: err.message)
        }
        return try Wire.decoder.decode(ResultOnly<R>.self, from: reply).result
    }

    /// Calls `onEvent` for each event, with the remote it came from (nil
    /// for this machine's), until the daemon goes away; blocks, so run it
    /// off the main thread.
    public static func subscribe(socket: String = ProtocolInfo.socketPath, _ onEvent: (Event, String?) -> Void) throws {
        let fd = try connect(socket)
        defer { close(fd) }
        var noTimeout = timeval(tv_sec: 0, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &noTimeout, socklen_t(MemoryLayout<timeval>.size))
        try writeLine(fd, hello())
        try writeLine(fd, Wire.encoder.encode(Request<NoParams>(id: 1, method: "subscribe", params: nil)))
        var refused: Failure?
        readLines(fd) { line in
            if let head = try? Wire.decoder.decode(ReplyHead.self, from: line), head.id != nil {
                if let e = head.error { refused = .protocolError(code: e.code, message: e.message); return false }
                return true // the hello and subscribe replies
            }
            if let e = try? Event.decode(line) { onEvent(e, Event.host(line)) }
            return true
        }
        if let refused { throw refused }
    }
}
