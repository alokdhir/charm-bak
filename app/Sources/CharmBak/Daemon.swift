import CharmBakCore
import Foundation

/// Keeps charm-bak-daemon running. The daemon ships inside this app's
/// bundle and runs as a child process: a child inherits the app's Full
/// Disk Access (TREK-11), so restic run by the daemon can read protected
/// folders.
///
/// - `ensureRunning()` is called at launch, every minute, and whenever the
///   menu loses its connection: if nothing answers on the socket and no
///   child is running, it starts one. That also covers a daemon this app
///   didn't start (one run by hand while developing) going away.
/// - The child gets a pipe as stdin and `-exit-with-parent`: however this
///   app ends (Quit, a crash, kill), the pipe closes and the daemon shuts
///   down, so it's never left behind.
/// - A daemon that exits within seconds of starting is restarted with a
///   growing delay, and `problem` says so for the menu.
final class DaemonSupervisor: @unchecked Sendable {
    static let shared = DaemonSupervisor()

    private let lock = NSLock()
    private var process: Process?
    private var stdinPipe: Pipe? // held open for the child's lifetime
    private var stopping = false
    private var restartPending = false
    private var startedAt = Date.distantPast
    private var quickExits = 0
    private var lastProblem: String?

    /// An exit sooner than this after starting counts as failing to start.
    private static let quickExit: TimeInterval = 10

    /// The daemon binary inside the bundle: Contents/MacOS/charm-bak-daemon.
    static var binary: URL? {
        let url = Bundle.main.bundleURL.appending(path: "Contents/MacOS/charm-bak-daemon")
        return FileManager.default.isExecutableFile(atPath: url.path) ? url : nil
    }

    /// Why the daemon isn't staying up, for the menu; nil when it's fine.
    var problem: String? {
        lock.withLock {
            if process != nil, Date().timeIntervalSince(startedAt) >= Self.quickExit { return nil }
            return lastProblem
        }
    }

    /// Starts the daemon if none is running or answering. Safe to call
    /// often and from any thread.
    func ensureRunning() {
        let busy = lock.withLock { stopping || process != nil || restartPending }
        if busy { return }
        // Something accepts connections: a daemon exists, even if it's slow
        // to answer. Starting another would only fight over the socket.
        if DaemonClient.reachable() { return }
        spawn()
    }

    private func spawn() {
        guard let binary = Self.binary else {
            lock.withLock { lastProblem = "charm-bak-daemon isn't in the app bundle" }
            return
        }
        let p = Process()
        p.executableURL = binary
        p.arguments = ["-exit-with-parent"]
        let pipe = Pipe()
        p.standardInput = pipe
        p.standardOutput = FileHandle.nullDevice
        p.standardError = Self.logHandle() ?? FileHandle.nullDevice
        p.terminationHandler = { [weak self] done in self?.exited(done) }
        lock.withLock {
            guard !stopping, process == nil else { return }
            do {
                try p.run()
                process = p
                stdinPipe = pipe
                startedAt = Date()
            } catch {
                lastProblem = "couldn't start the daemon: \(error.localizedDescription)"
            }
        }
    }

    private func exited(_ p: Process) {
        let delay: TimeInterval? = lock.withLock {
            process = nil
            stdinPipe = nil
            if stopping { return nil }
            if Date().timeIntervalSince(startedAt) < Self.quickExit {
                quickExits += 1
                let why = Self.lastLogLine() ?? "see daemon.log"
                lastProblem = "the daemon keeps exiting (status \(p.terminationStatus)): \(why)"
            } else {
                quickExits = 0
                lastProblem = nil
            }
            restartPending = true
            // 5s, then doubling up to a minute, so a daemon that can't start
            // doesn't spin or flood its log.
            return min(5 * pow(2, Double(max(quickExits - 1, 0))), 60)
        }
        guard let delay else { return }
        DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self else { return }
            self.lock.withLock { self.restartPending = false }
            self.ensureRunning()
        }
    }

    /// Stops the daemon for good: SIGTERM lets it cancel a running job
    /// cleanly; if it hasn't gone after 10 seconds it's killed. Nothing is
    /// started again afterwards.
    func stop() {
        let p = lock.withLock { () -> Process? in
            stopping = true
            return process
        }
        guard let p, p.isRunning else { return }
        p.terminate()
        let deadline = Date().addingTimeInterval(10)
        while p.isRunning && Date() < deadline { usleep(50_000) }
        if p.isRunning { kill(p.processIdentifier, SIGKILL) }
    }

    private static var logURL: URL { ProtocolInfo.supportDir.appending(path: "daemon.log") }

    /// The daemon's stderr goes to daemon.log beside its socket.
    private static func logHandle() -> FileHandle? {
        if !FileManager.default.fileExists(atPath: logURL.path) {
            FileManager.default.createFile(atPath: logURL.path, contents: nil)
        }
        let h = try? FileHandle(forWritingTo: logURL)
        h?.seekToEndOfFile()
        return h
    }

    /// The last line the daemon logged, which is why it exited.
    private static func lastLogLine() -> String? {
        guard let h = try? FileHandle(forReadingFrom: logURL) else { return nil }
        defer { try? h.close() }
        let size = (try? h.seekToEnd()) ?? 0
        try? h.seek(toOffset: size > 2048 ? size - 2048 : 0)
        let tail = String(decoding: (try? h.readToEnd()) ?? Data(), as: UTF8.self)
        return tail.split(separator: "\n").last.map { String($0.prefix(160)) }
    }
}
