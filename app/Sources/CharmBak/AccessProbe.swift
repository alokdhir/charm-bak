import AppKit

/// Checks whether the app (and a child process it starts) can read folders
/// that only Full Disk Access unlocks. These locations fail quietly without
/// the grant instead of raising a consent prompt, so probing is safe.
/// Results are appended to probe.log in the support dir.
enum AccessProbe {
    static let protected = ["Library/Mail", "Library/Messages", "Library/Safari"]

    /// Whether the app has Full Disk Access: it can read at least one of the
    /// protected folders that exist. True when none exist (nothing to test).
    static func hasFullDiskAccess() -> Bool {
        let home = FileManager.default.homeDirectoryForCurrentUser
        let present = protected.map { home.appending(path: $0).path }.filter { FileManager.default.fileExists(atPath: $0) }
        return present.isEmpty || present.contains { (try? FileManager.default.contentsOfDirectory(atPath: $0)) != nil }
    }

    /// Opens System Settings at Privacy & Security › Full Disk Access.
    static func openSettings() {
        if let url = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles") {
            NSWorkspace.shared.open(url)
        }
    }

    static func run() {
        let home = FileManager.default.homeDirectoryForCurrentUser
        var lines = ["\(ISO8601DateFormatter().string(from: Date())) build \(AppInfo.version)"]

        for rel in protected {
            let url = home.appending(path: rel)
            do {
                let n = try FileManager.default.contentsOfDirectory(atPath: url.path).count
                lines.append("  app   ~/\(rel): ok (\(n) entries)")
            } catch {
                lines.append("  app   ~/\(rel): denied (\((error as NSError).code))")
            }
        }

        // A child process: restic will run this way, so it has to inherit
        // the app's grant (macOS attributes it to the responsible app).
        let rel = protected[0]
        let ls = Process()
        ls.executableURL = URL(filePath: "/bin/ls")
        ls.arguments = [home.appending(path: rel).path]
        ls.standardOutput = FileHandle.nullDevice
        ls.standardError = FileHandle.nullDevice
        do {
            try ls.run()
            ls.waitUntilExit()
            lines.append("  child ~/\(rel): \(ls.terminationStatus == 0 ? "ok" : "denied (exit \(ls.terminationStatus))")")
        } catch {
            lines.append("  child ~/\(rel): couldn't start ls: \(error.localizedDescription)")
        }

        lines.append(resticProbe(home.appending(path: "Library/Safari")))

        let log = AppInfo.supportDir.appending(path: "probe.log")
        let text = lines.joined(separator: "\n") + "\n"
        if let handle = try? FileHandle(forWritingTo: log) {
            handle.seekToEndOfFile()
            handle.write(Data(text.utf8))
            try? handle.close()
        } else {
            try? text.write(to: log, atomically: true, encoding: .utf8)
        }
    }

    /// Runs restic as a child: a dry-run backup of a protected folder into a
    /// throwaway repo in a temp dir, deleted afterwards. restic exits 3 when
    /// it couldn't read some files, 0 when it read everything.
    static func resticProbe(_ dir: URL) -> String {
        let restic = "/opt/homebrew/bin/restic"
        guard FileManager.default.isExecutableFile(atPath: restic) else {
            return "  restic: not installed at \(restic)"
        }
        let repo = FileManager.default.temporaryDirectory.appending(path: "charm-bak-probe-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: repo) }
        var env = ProcessInfo.processInfo.environment
        env["RESTIC_REPOSITORY"] = repo.path
        env["RESTIC_PASSWORD"] = "probe-only"

        func run(_ args: [String]) -> Int32 {
            let p = Process()
            p.executableURL = URL(filePath: restic)
            p.arguments = args
            p.environment = env
            p.standardOutput = FileHandle.nullDevice
            p.standardError = FileHandle.nullDevice
            do { try p.run() } catch { return -1 }
            p.waitUntilExit()
            return p.terminationStatus
        }
        guard run(["init", "-q"]) == 0 else {
            return "  restic: couldn't init a temp repo"
        }
        let status = run(["backup", "--dry-run", "-q", dir.path])
        let rel = dir.path.replacingOccurrences(of: FileManager.default.homeDirectoryForCurrentUser.path, with: "~")
        return "  restic \(rel): \(status == 0 ? "ok" : "incomplete or failed (exit \(status))")"
    }
}
