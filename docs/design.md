# charm-bak design

How charm-bak is put together. Why each piece is the way it is: `docs/notes.md` for what was tested on macOS and restic, `AGENTS.md` for the decisions.

## Shape

```
                    Unix socket (NDJSON)
 charm-bak TUI ───────────────────────────▶ charm-bak-daemon (this Mac) ──restic──▶ this Mac's repository
 menu bar app ─────────────────────────────▶   child of charm-bak.app          └──restic──▶ each remote's repository (reads)
                                                    │
                                                    │ mutual TLS, port 7457
                                                    ▼
                                            charm-bak-daemon (a remote) ──restic──▶ its own repository
                                              systemd, launchd or a Windows service
```

- **`charm-bak-daemon`** (Go) owns everything on its machine: sources, schedules, retention, run history, pins and notes, the repository and its credentials, the job queue, and every restic call. The same code runs on the Mac and on every remote.
- **`charm-bak.app`** (Swift) is a thin shell: the menu bar icon, notifications and the login item. It carries the daemon inside its bundle and starts it as a child, so the daemon inherits the app's Full Disk Access.
- **The TUI** (Go, Bubble Tea) is a client of the Mac's daemon. It holds no credentials and never runs restic.
- **Remotes** are the same daemon on another machine (Linux, a Mac or Windows). Each backs up on its own schedule with its own credentials, whether the Mac is on or not.

## The daemon

- **Scheduler:** every minute it asks which sources are due. A source's next run is its interval after the last run ended, so runs missed while a machine slept or was off happen as soon as it's back.
- **Job queue:** one restic job at a time, because a repository locks. "Back up now" queues; it never interrupts.
- **A backup:** `restic backup --json --tag src:<id>`, then `forget` with the source's retention (pinned snapshots are never forgotten). Retention periods are restic's calendar ones.
- **Cleanup:** `prune` runs separately, weekly, between 01:00 and 05:00 (or at any hour once it's a day overdue). It locks the repository exclusively, so reads answer "busy" while it runs.
- **Integrity:** a weekly `check` that reads back a sample of the data.
- **Its own state:** SQLite beside the daemon. Daily, a copy of it is backed up into the repository under the tag `charm-bak:state`.
- **Credentials:** on a Mac in the Keychain, read through `security` for each restic process; elsewhere in owner-only files in the daemon's state directory. They reach restic through that process's environment only, never a command line or a log.

## Browsing

- Every restic call pays about a second of startup and index loading, so browsing is cached:
  - the first folder opened in a snapshot starts one listing of the whole snapshot in the background, and every later folder comes from it;
  - those listings are kept in a SQLite cache of their own (one row per folder, up to 512 MB, least recently used first out), so a snapshot opened once stays instant across restarts;
  - listing a source's snapshots warms the newest one.
- **Changes** between a snapshot and the one before it come from `restic diff --metadata` and are cached the same way: two snapshots never change.

## Sources

- A source has a name, one or more paths, exclude rules, a schedule and a retention. Unset fields inherit the machine's defaults. In the repository it exists only as the tag `src:<id>` on its snapshots, so editing its paths doesn't start a new retention group.
- **The picker** is a tree over the live disk: tick what's in, untick what's out. The ticks compile to paths, anchored excludes and re-includes (`internal/backup/selection.go`). restic doesn't look inside an excluded folder, so to keep one child of an excluded folder the compiler excludes the folder's contents (`dir/*`) and re-includes the child (`!dir/child`). Names containing `* ? [ \` are escaped.
- **Patterns** that aren't one path (`node_modules`, `*.tmp`) are a list beside the picker.
- **Every source also gets** `--exclude-caches`, `--exclude-if-present .nobackup` and, on macOS and Windows, `--exclude-cloud-files`. On Windows, excludes are case-insensitive (`--iexclude`), open files are read from a shadow copy (`--use-fs-snapshot`), and paths are converted at the edges: charm-bak and the protocol use restic's `/C/...` form, restic's backup arguments and the live disk `C:\...`.
- **A backup that couldn't read some files** still made a snapshot: it's a warning (health `skipped`), notified, with every file and the reason kept beside the run for `^N`'s details.
- **Snapshots without a charm-bak tag,** and those of a source removed with its snapshots kept, show under an "untagged" pseudo-source. Nothing is hidden.

## Remotes

- **Pairing:** Add Remote shows one command to paste on the new machine. It fetches an installer and the daemon from the Mac over HTTPS on port 7458, checked against the Mac's key pin, using a one-time code. The installer sets the daemon up as a service (systemd on Linux, a launchd daemon on a Mac, a Windows service with restart-on-failure, a firewall rule and restic beside it on Windows, where the command runs in an administrator PowerShell) listening on port 7457. Both sides then pin each other's key; from then on only the Mac connects, over mutual TLS.
- **Relay:** a TUI request that names a host goes from the Mac's daemon to that remote; the remote's events come back the same way, tagged with its name.
- **Reading a remote's backups:** the Mac keeps a copy of each remote's repository password and storage credentials in its Keychain, and saves the remote's repository location, sources, pins and notes whenever it answers. Snapshot lists, folder listings, previews and changes are read from the remote's repository by the Mac's own daemon, so a remote that's off or dead can still be browsed and restored from. Only changes to the remote (its sources, schedules, a backup now) need it to answer.
- **Updates:** the app carries the daemon for Linux and Windows (amd64, arm64) and the Mac-remote bundle. Each time the Mac connects to a remote whose build differs, it sends the new one over the paired connection; the remote checks the hash, replaces itself and restarts in place (on Windows by renaming the running executable aside and letting the service manager restart it). Windows remotes also run `restic self-update` weekly, since their restic is a copy no package manager updates.
- **Retry:** a remote that isn't answering is tried again every few seconds.
- **Removal:** taking a remote away uninstalls its daemon there and unpairs it, optionally deleting its snapshots first; if that deletion fails, nothing is uninstalled.

## Socket protocol

Newline-delimited JSON over a Unix socket that only its owner can open. Requests and replies are matched by `id`; events arrive on a connection that subscribed. The full contract, with an example of every method and event, is `docs/protocol.md` and `testdata/protocol/`.

## Not done

- **Mounting a snapshot as a drive:** `restic mount` needs FUSE, which on macOS means macFUSE.
- **Object Lock or other immutable storage,** and several repositories per machine.
