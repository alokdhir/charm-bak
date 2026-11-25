import CharmBakCore
import ServiceManagement
import SwiftUI

/// The menu bar app: a thin shell. It starts charm-bak-daemon (bundled
/// inside it), which owns the schedule and every restic call, and shows
/// the daemon's state. The menu is just another socket client.
@main
struct CharmBakApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @State private var model = AppModel()

    init() {
        // `--render-icon <dir>` writes the app icon's .iconset; build.sh
        // runs it so the icon comes from the same geometry as the menu bar.
        if let i = CommandLine.arguments.firstIndex(of: "--render-icon"), i + 1 < CommandLine.arguments.count {
            do {
                try StrataIcon.renderIconset(to: URL(filePath: CommandLine.arguments[i + 1]))
                exit(0)
            } catch {
                FileHandle.standardError.write(Data("render-icon: \(error)\n".utf8))
                exit(1)
            }
        }
        // `open -a charm-bak --args --probe` runs the Full Disk Access probe
        // at launch, so it can be scripted; results go to probe.log.
        if CommandLine.arguments.contains("--probe") {
            AccessProbe.run()
            if CommandLine.arguments.contains("--quit") {
                exit(0)
            }
        }
    }

    var body: some Scene {
        MenuBarExtra {
            AppMenu(model: model)
        } label: {
            Image(nsImage: StrataIcon.menuBar(model.icon))
        }
    }
}

/// Stops the daemon whenever the app terminates in an orderly way (the
/// menu's Quit, `quit app` from AppleScript, logout). A crash or kill is
/// covered by the daemon's own -exit-with-parent.
final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationWillTerminate(_ notification: Notification) {
        DaemonSupervisor.shared.stop()
    }
}

/// What the menu shows, read from the daemon: after every event that can
/// change it, and once a minute (overdue changes with time alone).
@Observable
@MainActor
final class AppModel {
    struct Summary: Sendable {
        var reachable = false
        /// Set when the daemon speaks another protocol version.
        var mismatch: String?
        var state: AppState = .idle
        var paused = false
        var configured = false
        var sources = 0
        var failing = 0
        var skipped = 0
        var overdue = 0
    }

    var summary = Summary()
    var daemonProblem: String?
    var unreachableSince: Date? = Date()
    var diskAccess = AccessProbe.hasFullDiskAccess()
    @ObservationIgnored private var ticker: Timer?
    @ObservationIgnored private var refreshing = false
    @ObservationIgnored private var refreshAgain = false

    /// How long "Starting…" is believable before the menu calls it a failure.
    private static let startGrace: TimeInterval = 20

    var paused: Bool { summary.paused }

    /// The daemon isn't usable and that's not just startup.
    var daemonDown: Bool {
        guard !summary.reachable else { return false }
        if summary.mismatch != nil || daemonProblem != nil { return true }
        return unreachableSince.map { Date().timeIntervalSince($0) > Self.startGrace } ?? false
    }

    var icon: StrataIcon.State {
        if summary.failing > 0 || !diskAccess || daemonDown { return .failed }
        return summary.paused ? .paused : .idle
    }

    init() {
        // The build runs this binary with --render-icon; that's not the app.
        if CommandLine.arguments.contains("--render-icon") { return }
        Notifier.shared.start()
        Task.detached {
            DaemonSupervisor.shared.ensureRunning()
            await self.requestRefresh()
            self.listen()
        }
        ticker = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated {
                self?.diskAccess = AccessProbe.hasFullDiskAccess()
                self?.requestRefresh()
            }
            // Also the safety net for a daemon that went away quietly.
            DispatchQueue.global().async { DaemonSupervisor.shared.ensureRunning() }
        }
    }

    /// Refreshes the summary. Only one refresh runs at a time and requests
    /// made meanwhile collapse into one more, so results always apply in
    /// order and a burst of events costs two reads, not one per event.
    func requestRefresh() {
        if refreshing {
            refreshAgain = true
            return
        }
        refreshing = true
        Task {
            repeat {
                refreshAgain = false
                let s = await Task.detached { Self.fetch() }.value
                summary = s
                daemonProblem = DaemonSupervisor.shared.problem
                if s.reachable {
                    unreachableSince = nil
                } else if unreachableSince == nil {
                    unreachableSince = Date()
                }
            } while refreshAgain
            refreshing = false
        }
    }

    /// Reads status and sources from the daemon (blocking; off the main thread).
    nonisolated static func fetch() -> Summary {
        var s = Summary()
        let status: StatusResult
        do {
            status = try DaemonClient.call("status", as: StatusResult.self)
        } catch DaemonClient.Failure.protocolError(let code, let message) where code == "unsupported_protocol" {
            s.mismatch = message
            return s
        } catch {
            return s
        }
        s.reachable = true
        s.state = status.state
        s.paused = status.paused
        s.configured = status.repository != nil
        if let sources = try? DaemonClient.call("sources", as: SourcesResult.self).sources {
            s.sources = sources.count
            s.failing = sources.filter { $0.health == "failed" }.count
            s.skipped = sources.filter { $0.health == "skipped" }.count
            s.overdue = sources.filter { $0.health == "overdue" }.count
        }
        return s
    }

    /// Follows the daemon's events; when the connection drops, makes sure
    /// a daemon is running and reconnects.
    nonisolated func listen() {
        while true {
            try? DaemonClient.subscribe { [weak self] event, host in
                // The menu doesn't show progress, and there's one of those
                // a second for the length of a backup.
                if case .progress = event { return }
                if case .jobFinished(let run) = event { Notifier.shared.runFinished(run, host: host) }
                Task { @MainActor in self?.requestRefresh() }
            }
            Task { @MainActor in self.requestRefresh() }
            DaemonSupervisor.shared.ensureRunning()
            sleep(2)
        }
    }

    /// The menu's first line: a colored dot and what it means.
    enum Light {
        case ok, busy, attention, failing, off

        var color: NSColor {
            switch self {
            case .ok: NSColor(srgbRed: 0.0, green: 0.82, blue: 0.53, alpha: 1)      // Charmtone Julep
            case .busy: NSColor(srgbRed: 0.42, green: 0.314, blue: 1, alpha: 1)    // Charmtone Charple
            case .attention: NSColor(srgbRed: 0.96, green: 0.76, blue: 0.2, alpha: 1) // Charmtone Mustard
            case .failing: NSColor(srgbRed: 1, green: 0.341, blue: 0.49, alpha: 1) // Charmtone Coral
            case .off: NSColor.systemGray
            }
        }
    }

    var status: (light: Light, line: String) {
        func n(_ count: Int, _ noun: String) -> String { "\(count) \(noun)\(count == 1 ? "" : "s")" }
        if !summary.reachable {
            if let mismatch = summary.mismatch { return (.failing, mismatch) }
            if let daemonProblem { return (.failing, "Not running: \(daemonProblem)") }
            return daemonDown ? (.failing, "The daemon isn't responding") : (.off, "Starting…")
        }
        // No Full Disk Access means backups silently skip protected folders
        // (Documents, Desktop, Mail): a failure, and it outranks everything.
        if !diskAccess { return (.failing, "No Full Disk Access") }
        if !summary.configured { return (.off, "No repository set up yet") }
        switch summary.state {
        case .running: return (.busy, "Backing up…")
        case .pruning: return (.busy, "Cleaning up the repository…")
        case .paused: return (.off, "Backups paused")
        case .idle: break
        }
        if summary.failing > 0 { return (.failing, "\(n(summary.failing, "source")) failing") }
        if summary.skipped > 0 { return (.attention, "\(n(summary.skipped, "source")) skipped files") }
        if summary.overdue > 0 { return (.attention, "\(n(summary.overdue, "source")) overdue") }
        if summary.sources == 0 { return (.off, "No sources yet") }
        return (.ok, summary.sources == 1 ? "Backups OK" : "All \(summary.sources) sources OK")
    }

    func togglePause() {
        let method = summary.paused ? "resume" : "pause"
        Task.detached {
            _ = try? DaemonClient.call(method, as: Empty.self)
            await self.requestRefresh()
        }
    }

    /// The daemon is stopped in AppDelegate.applicationWillTerminate.
    func quit() { NSApplication.shared.terminate(nil) }
}

struct AppMenu: View {
    let model: AppModel
    @State private var openAtLogin = SMAppService.mainApp.status == .enabled

    var body: some View {
        Text("charm-bak \(AppInfo.version)")
        Label {
            Text(model.status.line)
        } icon: {
            Image(nsImage: StrataIcon.dot(model.status.light.color))
        }
        if !model.diskAccess {
            Button("Full Disk Access needed…") { AccessProbe.openSettings() }
        }
        Divider()
        Button("Open TUI") { TUILauncher.openInBackground() }
            .keyboardShortcut("o")
            .disabled(TUILauncher.binary == nil)
        Button(model.paused ? "Resume backups" : "Pause backups") { model.togglePause() }
            .disabled(!model.summary.reachable)
        Toggle("Open at login", isOn: $openAtLogin)
            .onChange(of: openAtLogin) { _, on in
                do {
                    if on {
                        try SMAppService.mainApp.register()
                    } else {
                        try SMAppService.mainApp.unregister()
                    }
                } catch {
                    openAtLogin = SMAppService.mainApp.status == .enabled
                }
            }
        Divider()
        Button("Quit charm-bak") { model.quit() }
            .keyboardShortcut("q")
    }
}

enum AppInfo {
    static var version: String {
        let info = Bundle.main.infoDictionary
        let short = info?["CFBundleShortVersionString"] as? String ?? "dev"
        let build = info?["CFBundleVersion"] as? String ?? "?"
        return "\(short) (\(build))"
    }

    static var supportDir: URL { ProtocolInfo.supportDir }
}
