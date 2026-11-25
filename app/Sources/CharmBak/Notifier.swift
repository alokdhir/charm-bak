import CharmBakCore
import Foundation
import UserNotifications

/// macOS notifications for failed runs. Successes stay quiet. One
/// notification per machine: failures that arrive within a few seconds of
/// each other (several sources hitting the same locked repository) become
/// one ("6 backups on pantry failed"), a repeat failure replaces it instead
/// of stacking up, and it goes away once everything in it has had a good
/// run. Clicking one opens the TUI.
final class Notifier: NSObject, UNUserNotificationCenterDelegate, @unchecked Sendable {
    static let shared = Notifier()

    private var center: UNUserNotificationCenter { .current() }

    /// What's failing, per machine ("" is this one), keyed by source id or
    /// job kind; guarded by `queue`.
    private struct Failure { var title: String; var name: String; var error: String; var job: String }
    private var failing: [String: [String: Failure]] = [:]
    private var pending: [String: DispatchWorkItem] = [:]
    private let queue = DispatchQueue(label: "charm-bak.notifier")
    /// How long to wait for more failures before posting.
    private static let gather: TimeInterval = 5

    /// Asks for permission once (macOS remembers the answer) and takes
    /// clicks. Call at launch.
    /// `open -a charm-bak --args --notify-test` posts a sample failure,
    /// to check the permission and how it looks.
    func start() {
        center.delegate = self
        center.requestAuthorization(options: [.alert, .sound]) { granted, _ in
            if granted, CommandLine.arguments.contains("--notify-test") {
                self.post(id: "failed-test", title: "Backup of example failed (test)",
                          body: "This is what a failed run looks like. Click to open the TUI.")
            }
        }
    }

    /// Called for every job_finished event.
    func runFinished(_ run: Run, host: String?) {
        // Restores were started by hand, from the TUI, which says how they went.
        if run.kind == "restore" { return }
        let machine = host ?? ""
        let key = run.source ?? run.kind
        // Removing something from past snapshots (Exclude in the TUI) is
        // long and asked for by hand: say when it's done, not only when
        // it fails.
        if run.kind == "rewrite" && run.status == "ok" {
            let (_, name) = Self.describe(run, host: host)
            let on = host.map { " on \($0)" } ?? ""
            post(id: "rewrite-" + run.job, title: "Past snapshots of \(name)\(on) cleaned up",
                 body: "It's removed from them and the space is freed.", notice: Notice(host: machine, job: run.job))
        }
        switch run.status {
        case "ok":
            queue.async {
                guard self.failing[machine]?.removeValue(forKey: key) != nil else { return }
                if self.failing[machine]?.isEmpty ?? true {
                    self.failing[machine] = nil
                    self.pending.removeValue(forKey: machine)?.cancel()
                    self.center.removeDeliveredNotifications(withIdentifiers: [Self.id(machine)])
                }
            }
        case "failed", "incomplete":
            let (failedTitle, name) = Self.describe(run, host: host)
            // Incomplete is a warning: the snapshot exists, without some files.
            let title = run.status == "incomplete" ? Self.skippedTitle(run, name: name, host: host) : failedTitle
            queue.async {
                self.failing[machine, default: [:]][key] = Failure(
                    title: title, name: name, error: run.error.isEmpty ? "No details; see the TUI's run log." : run.error, job: run.job)
                self.pending[machine]?.cancel()
                let work = DispatchWorkItem { self.postFailures(machine) }
                self.pending[machine] = work
                self.queue.asyncAfter(deadline: .now() + Self.gather, execute: work)
            }
        default: // cancelled: someone did that on purpose
            break
        }
    }

    private static func id(_ machine: String) -> String { "failed-" + machine }

    /// "Backup of home on pantry: 12 files skipped", as the TUI's ^N says.
    private static func skippedTitle(_ run: Run, name: String, host: String?) -> String {
        let n = run.skipped ?? 0
        let files = n == 0 ? "some files" : n == 1 ? "1 file" : "\(n) files"
        return "Backup of \(name)\(host.map { " on \($0)" } ?? ""): \(files) skipped"
    }

    /// On `queue`: one notification for everything failing on a machine.
    private func postFailures(_ machine: String) {
        pending[machine] = nil
        let all = (failing[machine] ?? [:]).values.sorted { $0.name < $1.name }
        guard let first = all.first else { return }
        if all.count == 1 {
            post(id: Self.id(machine), title: first.title, body: first.error, notice: Notice(host: machine, job: first.job))
            return
        }
        let on = machine.isEmpty ? "" : " on \(machine)"
        let names = all.map(\.name).joined(separator: ", ")
        let errors = Set(all.map(\.error))
        let why = errors.count == 1 ? first.error : "Different reasons; see the TUI's run log."
        // Several: a click opens the list, where they're together.
        post(id: Self.id(machine), title: "\(all.count) backups\(on) need attention", body: names + "\n" + why,
             notice: Notice(host: machine, job: ""))
    }

    /// The title for one failure ("Backup of videos on pantry failed") and
    /// the short name used when several are listed together; a remote's
    /// source is looked up through the relay.
    private static func describe(_ run: Run, host: String?) -> (title: String, name: String) {
        let on = host.map { " on \($0)" } ?? ""
        if let source = run.source {
            let name = (try? DaemonClient.call("sources", host: host, as: SourcesResult.self).sources)?
                .first { $0.id == source }?.name ?? source
            let title = switch run.kind {
            case "backup": "Backup of \(name)\(on) failed"
            case "rewrite": "Removing from past snapshots of \(name)\(on) failed"
            default: "\(run.kind.capitalized) of \(name)\(on) failed"
            }
            return (title, name)
        }
        switch run.kind {
        case "prune": return ("Repository cleanup\(on) failed", "cleanup")
        case "check": return ("Repository check\(on) found a problem", "check")
        case "state": return ("Backup of charm-bak's own settings\(on) failed", "charm-bak's settings")
        case "rotate": return ("Replacing an AWS key failed", "AWS key")
        default: return ("\(run.kind.capitalized)\(on) failed", run.kind)
        }
    }

    private func post(id: String, title: String, body: String, notice: Notice? = nil) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = .default
        if let notice {
            content.userInfo = ["host": notice.host, "job": notice.job]
        }
        center.add(UNNotificationRequest(identifier: id, content: content, trigger: nil))
    }

    // A menu bar app is always "in front" as far as macOS is concerned, so
    // without this, banners would be swallowed.
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound, .list])
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        // The daemon holds the notification until a TUI takes it: the one
        // running hears of it at once, a new one asks when it starts.
        let info = response.notification.request.content.userInfo
        if let host = info["host"] as? String, let job = info["job"] as? String {
            let notice = Notice(host: host, job: job)
            DispatchQueue.global(qos: .userInitiated).async {
                _ = try? DaemonClient.call("notice_show", params: notice, as: Empty.self)
                TUILauncher.open()
            }
        } else {
            TUILauncher.openInBackground()
        }
        completionHandler()
    }
}
