import AppKit
import UniformTypeIdentifiers

/// Opens the TUI (bundled as Contents/MacOS/charm-bak-tui) in a terminal
/// window. The terminal is the one set with
/// `defaults write io.github.alokdhir.charm-bak terminal <bundle id>`,
/// else a known terminal that's already running, else the system default
/// for executables (Terminal.app unless changed).
enum TUILauncher {
    /// Known terminals, and the arguments that make each run a command in
    /// a new window when launched with `open -n -a <app> --args …`. An
    /// empty list means the app runs an executable opened as a document.
    private static let terminals: [(id: String, args: @Sendable (String) -> [String]?)] = [
        // kitty: opened as a document, so a running kitty makes a new
        // window instead of a new instance. ~/.config/kitty/launch-actions.conf
        // has a rule for charm-bak-tui; without it kitty asks before
        // running an executable and holds the window after it exits.
        ("net.kovidgoyal.kitty", { _ in nil }),
        ("com.mitchellh.ghostty", { ["-e", $0] }),
        ("com.googlecode.iterm2", { _ in nil }),
        ("com.apple.Terminal", { _ in nil }),
    ]

    /// The TUI binary inside the bundle.
    static var binary: URL? {
        let url = Bundle.main.bundleURL.appending(path: "Contents/MacOS/charm-bak-tui")
        return FileManager.default.isExecutableFile(atPath: url.path) ? url : nil
    }

    private static func appURL(_ id: String) -> URL? {
        NSWorkspace.shared.urlForApplication(withBundleIdentifier: id)
    }

    private static func pick() -> (app: URL, args: [String]?)? {
        guard let tui = binary?.path else { return nil }
        let installed = terminals.compactMap { t in appURL(t.id).map { (id: t.id, app: $0, args: t.args(tui)) } }
        if let want = UserDefaults.standard.string(forKey: "terminal") {
            if let known = installed.first(where: { $0.id == want }) { return (known.app, known.args) }
            // A terminal this list doesn't know: open the binary with it.
            if let app = appURL(want) { return (app, nil) }
        }
        if let running = installed.first(where: { !NSRunningApplication.runningApplications(withBundleIdentifier: $0.id).isEmpty }) {
            return (running.app, running.args)
        }
        // Nothing running: whatever macOS opens executables with.
        guard let app = NSWorkspace.shared.urlForApplication(toOpen: .unixExecutable) else { return nil }
        let known = installed.first { $0.app.standardizedFileURL == app.standardizedFileURL }
        return (app, known?.args)
    }

    /// Returns false if there's no TUI in the bundle or nothing to run it in.
    /// A TUI that's already running isn't opened again: its terminal is
    /// brought to the front instead.
    /// open(), off the main thread: finding and raising a running TUI runs
    /// osascript and tmux and waits for them, and the first time macOS asks
    /// to allow the automation, which would freeze the menu meanwhile.
    static func openInBackground() {
        DispatchQueue.global(qos: .userInitiated).async { _ = open() }
    }

    @discardableResult
    static func open() -> Bool {
        if raiseRunning() { return true }
        guard let tui = binary, let (app, args) = pick() else { return false }
        let p = Process()
        p.executableURL = URL(filePath: "/usr/bin/open")
        // Through `open`, not as our child: the terminal must not be tied
        // to this app's lifetime or its Full Disk Access.
        if let args {
            p.arguments = ["-n", "-a", app.path, "--args"] + args
        } else {
            p.arguments = ["-a", app.path, tui.path]
        }
        do { try p.run() } catch { return false }
        return true
    }

    /// Brings a running TUI to the front, in any terminal. The TUI is a
    /// process whose executable is our charm-bak-tui (also when started as
    /// charm-bak through the PATH link, which resolves to it); what's
    /// raised is found from its terminal device (tty):
    /// - inside tmux: tmux selects the TUI's pane, and the terminal showing
    ///   that tmux session is raised the same way, by the client's tty;
    /// - Terminal.app and iTerm2: AppleScript selects the tab with that tty
    ///   and brings its window forward (macOS asks once to allow it);
    /// - any other terminal: its app is activated. Exact for kitty and
    ///   Ghostty as Open TUI starts them: an instance for the TUI alone.
    static func raiseRunning() -> Bool {
        guard let tui = binary?.resolvingSymlinksInPath().path else { return false }
        for pid in allPIDs() where executablePath(pid) == tui {
            if raise(pid: pid) { return true }
        }
        return false
    }

    private static func raise(pid: pid_t) -> Bool {
        let tty = ttyPath(pid)
        if let app = owningApp(pid) {
            return raise(app: app, tty: tty)
        }
        // No app above it: a tmux server's child, most likely.
        if let tty, let client = tmuxSelect(paneTTY: tty),
           let clientPID = allPIDs().first(where: { ttyPath($0) == client }),
           let app = owningApp(clientPID) {
            return raise(app: app, tty: client)
        }
        return false
    }

    /// The first GUI app among pid's ancestors (pid included).
    private static func owningApp(_ pid: pid_t) -> NSRunningApplication? {
        var p = pid
        for _ in 0..<24 {
            if let app = NSRunningApplication(processIdentifier: p), app.bundleIdentifier != nil {
                return app
            }
            guard let parent = procInfo(p)?.ppid, parent > 1 else { return nil }
            p = parent
        }
        return nil
    }

    private static func raise(app: NSRunningApplication, tty: String?) -> Bool {
        if let tty {
            switch app.bundleIdentifier {
            case "com.apple.Terminal":
                if osascript("""
                    tell application id "com.apple.Terminal"
                        repeat with w in windows
                            repeat with t in tabs of w
                                if tty of t is "\(tty)" then
                                    set selected of t to true
                                    set index of w to 1
                                    activate
                                    return
                                end if
                            end repeat
                        end repeat
                    end tell
                    """) { return true }
            case "com.googlecode.iterm2":
                if osascript("""
                    tell application id "com.googlecode.iterm2"
                        repeat with w in windows
                            repeat with t in tabs of w
                                repeat with s in sessions of t
                                    if tty of s is "\(tty)" then
                                        select s
                                        select t
                                        select w
                                        activate
                                        return
                                    end if
                                end repeat
                            end repeat
                        end repeat
                    end tell
                    """) { return true }
            default:
                break
            }
        }
        return app.activate()
    }

    /// Runs AppleScript through osascript, so the events come from an
    /// Apple tool and this app's signature needs no automation
    /// entitlement. False if it fails (not allowed, the app quit).
    private static func osascript(_ script: String) -> Bool {
        let p = Process()
        p.executableURL = URL(filePath: "/usr/bin/osascript")
        p.arguments = ["-e", script]
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        do { try p.run() } catch { return false }
        p.waitUntilExit()
        return p.terminationStatus == 0
    }

    /// tmux: selects the window and pane on paneTTY and returns the tty of a
    /// client showing that session, nil when no tmux pane has that tty.
    private static func tmuxSelect(paneTTY: String) -> String? {
        guard let tmux = ["/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"].first(where: {
            FileManager.default.isExecutableFile(atPath: $0)
        }) else { return nil }
        let panes = run(tmux, ["list-panes", "-a", "-F", "#{pane_tty} #{session_id} #{window_id} #{pane_id}"])
        guard let line = panes.split(separator: "\n").first(where: { $0.hasPrefix(paneTTY + " ") }) else { return nil }
        let f = line.split(separator: " ").map(String.init)
        guard f.count == 4 else { return nil }
        _ = run(tmux, ["select-window", "-t", f[2]])
        _ = run(tmux, ["select-pane", "-t", f[3]])
        let clients = run(tmux, ["list-clients", "-t", f[1], "-F", "#{client_tty}"])
        return clients.split(separator: "\n").first.map(String.init)
    }

    private static func run(_ exe: String, _ args: [String]) -> String {
        let p = Process()
        p.executableURL = URL(filePath: exe)
        p.arguments = args
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        do { try p.run() } catch { return "" }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return String(decoding: data, as: UTF8.self)
    }

    private static func allPIDs() -> [pid_t] {
        let n = proc_listallpids(nil, 0)
        guard n > 0 else { return [] }
        var pids = [pid_t](repeating: 0, count: Int(n) + 32)
        let got = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
        return Array(pids.prefix(Int(max(got, 0)))).filter { $0 > 0 }
    }

    private static func executablePath(_ pid: pid_t) -> String? {
        var buf = [CChar](repeating: 0, count: 4 * Int(MAXPATHLEN))
        guard proc_pidpath(pid, &buf, UInt32(buf.count)) > 0 else { return nil }
        let path = String(decoding: buf.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }, as: UTF8.self)
        return URL(filePath: path).resolvingSymlinksInPath().path
    }

    /// A process's parent and controlling terminal device, from the kernel.
    private static func procInfo(_ pid: pid_t) -> (ppid: pid_t, tdev: dev_t)? {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0, size > 0 else { return nil }
        return (info.kp_eproc.e_ppid, info.kp_eproc.e_tdev)
    }

    /// "/dev/ttys003", or nil without a terminal.
    private static func ttyPath(_ pid: pid_t) -> String? {
        guard let dev = procInfo(pid)?.tdev, dev != -1, dev != 0 else { return nil }
        guard let name = devname(dev, S_IFCHR), let tty = String(validatingCString: name) else { return nil }
        return "/dev/" + tty
    }
}
