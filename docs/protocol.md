# charm-bak socket protocol, v1

The TUI (client) talks to the menu bar app over a Unix socket. This file is the contract; `internal/protocol` (Go) and the app's `Protocol.swift` implement it, and `testdata/protocol/*.json` holds one worked example per method and event that both sides' tests decode.

## Transport

- Socket: `~/Library/Application Support/charm-bak/app.sock` on the Mac (a root daemon elsewhere: `/run/charm-bak/daemon.sock`), mode 0600. File permissions are the authentication. Between paired daemons the same protocol runs over TLS 1.3 with pinned keys (see Remotes).
- One JSON object per line (UTF-8, `\n`-terminated), both directions. A line is at most 16 MiB; the app closes a connection that sends a longer one.
- The app serves many connections at once. A client should use one connection for requests and one for its event subscription. Requests on one connection may be answered out of order; match replies by `id`.

## Envelopes

```json
{"id": 7, "method": "ls", "params": {"snapshot": "2438fde6", "path": "/Users/alok/src"}}
{"id": 7, "result": {"entries": [], "total": 0, "next_offset": null}}
{"id": 7, "error": {"code": "not_found", "message": "no such path in snapshot 2438fde6"}}
{"event": "progress", "job_id": "j-42", "percent": 0.11}
```

- `id`: a non-negative integer chosen by the client, unique among its requests in flight (the Swift client uses 0 for its `hello`).
- `params` may be omitted when a method takes none.
- Exactly one of `result` or `error` per reply.
- Events have `event` and no `id`; they arrive only on a connection that called `subscribe`.
- Unknown fields must be ignored by both sides, so later versions can add fields without bumping the version. (The fixture tests are strict instead, to keep the examples honest.)

## Conventions

- Times: RFC 3339 with offset, e.g. `2026-11-19T12:57:13-05:00`.
- A field that is `null` may also be left out; readers treat the two the same. (Swift's encoder omits nil fields; Go's writes `null`.)
- Sizes: integer bytes. Durations: integer seconds.
- Paths: absolute, as restic stores them.
- Snapshot IDs: the full ID in replies; requests accept the full or the short ID.

## Errors

| code | meaning | `data` |
|---|---|---|
| `bad_request` | malformed line or params | |
| `unknown_method` | | |
| `unsupported_protocol` | `hello` asked for a version the app doesn't speak | `{"protocol": 1}`, the version it does speak |
| `not_found` | source, snapshot, path or job doesn't exist | |
| `busy` | the repository is locked by a job that blocks this request (prune locks out every read) | `{"job": Job}` |
| `restic_failed` | restic exited non-zero | `{"exit": 11, "stderr": "last lines"}` |
| `not_configured` | no repository set up yet | |
| `internal` | a bug in the app | |

`message` is a human-readable sentence the TUI may show as is.

## Shared types

**Source**
```json
{
  "id": "3f9a",                      // the restic tag is "src:3f9a"
  "name": "src",
  "host": "studio",                  // the machine it's on (every Source carries it)
  "paths": ["/Users/alok/src"],
  "excludes": ["node_modules", "!/Users/alok/src/keep/node_modules"],
  "schedule": {"every": 3600},       // seconds; null = manual only
  "retention": {"last": 10, "hourly": 48, "daily": 7, "weekly": 4, "monthly": 12, "yearly": 3},
  "inherited": ["retention"],        // fields taken from the defaults, not set on the source
  "paused": false,
  "health": "ok",                    // ok | overdue | failed | skipped (last backup couldn't read some files) | never
  "files": 1832, "bytes": 41234567,  // size of its newest snapshot (from the last run that made one); 0 before the first
  "last_run": Run,                   // or null
  "next_run": "2026-11-19T14:00:00-05:00"   // or null when manual or paused
}
```

**Snapshot**: restic's snapshot plus charm-bak's own fields.
```json
{
  "id": "2438fde6…", "short_id": "2438fde6",
  "time": "…", "end": "…",           // end from summary.backup_end, null if restic recorded none
  "paths": ["/Users/alok/src"], "hostname": "studio.local", "tags": ["src:3f9a"],
  "files": 1832, "bytes": 41234567, "bytes_added": 1203,   // from restic's summary; 0 if absent
  "kept_by": ["last snapshot", "daily snapshot"],          // restic forget's matches; [] = next forget removes it
  "pinned": false,                   // stored in the app (restic's tag command would change the snapshot's ID); retention never removes a pinned snapshot
  "note": ""                         // stored in the app, not in the repository
}
```

**Entry**: one item of a directory listing.
```json
{"name": "app.go", "type": "file", "size": 18400, "mode": "-rw-r--r--", "mtime": "…"}
```
`type`: `dir`, `file`, `symlink` or `other`. `size` is 0 for directories.

**Job**
```json
{"id": "j-42", "kind": "backup", "source": "3f9a", "started": "…"}
```
`kind`: `backup`, `forget`, `prune`, `check`, `restore`, `rewrite` (removing something from past snapshots), `state` (the daemon backing up its own database, daily). `source` is null for repository-wide jobs.

**Run**: a finished job, from the app's history.
```json
{"job": "j-42", "kind": "backup", "source": "3f9a", "started": "…", "ended": "…",
 "status": "ok", "files_new": 3, "files_changed": 12, "bytes_added": 52311,
 "files": 1832, "bytes": 41234567,   // everything a backup covered, changed or not; 0 for other kinds
 "snapshot": "2438fde6…", "error": "",
 "skipped": 0}                       // incomplete: how many files couldn't be read
```
`status`: `ok`, `incomplete` (restic exit 3: some files unreadable; the snapshot exists without them, `skipped` counts them and `error` names the first few with the reason), `failed`, `cancelled`.

## Methods

| method | params | result |
|---|---|---|
| `hello` | `{protocol, client}` | `{protocol, app, build, os, arch, bundle?}` |
| `status` | | `{repository, state, job, paused, full_disk_access?}` |
| `sources` | | `{sources: [Source]}` |
| `snapshots` | `{source}` (`"untagged"` for snapshots without a charm-bak tag, and ones whose source was since removed) | `{snapshots: [Snapshot]}`, newest first |
| `ls` | `{snapshot, path, offset?, limit?}` | `{entries: [Entry], total, next_offset}` |
| `dump` | `{snapshot, path, max}` | `{data, size, truncated, binary}` |
| `diff` | `{snapshot, path}` | `{entries: [{name, type, change}]}` |
| `changes` | `{snapshot, base}` | `{changes: [{path, change}], added, removed, changed, touched, bytes_added}` |
| `restore` | `{snapshot, paths, target, overwrite, from?}` | `{job: Job}` |
| `backup_now` | `{source}` | `{job: Job}` |
| `cancel` | `{job}` | `{}` |
| `source_add` | `{name, paths, excludes, schedule?, retention?, back_up_now}` | `{source: Source}` |
| `source_set` | `{id, …fields to change, inherit?}` | `{source: Source}` |
| `source_rewrite` | `{id, excludes, dry_run?}` | `{snapshots, job?}`: takes paths out of the source's existing snapshots |
| `source_remove` | `{id, keep_snapshots}` | `{}` |
| `snapshot_set` | `{id, note?, pinned?}` | `{snapshot: Snapshot}` |
| `snapshot_meta` | `{source}` | `{pinned: [ids], notes: {id: text}, added: {id: bytes}}` |
| `pause` / `resume` | | `{}` |
| `history` | `{since}` | `{runs: [Run]}`, oldest first |
| `run` | `{job}` | `{run: Run, skipped_files: [string]}`: one run's details; `skipped_files` names every file an incomplete backup couldn't read, with the reason (up to 1000; history leaves them out) |
| `subscribe` | | `{}` first, then events on this connection (a client waits for the reply before trusting the stream) |
| `repository` | | `{repository: RepositoryInfo or null, defaults: Defaults}` |
| `defaults_set` | `{schedule?, retention?, excludes?}` | `{defaults: Defaults}` |
| `hosts` | | `{hosts: [Host]}`: this daemon first (`local: true`), then paired remotes by name |
| `pair_start` | `{name}` | `{command, expires}`: the one line to run on the new remote as root |
| `host_repair` | `{name}` | `{command, expires}`: the same for a paired remote, reinstalling its daemon without pairing |
| `self_update` | `{sha256, size, offset, data, bundle?}` | `{received, restarting}`; daemon to daemon only |
| `host_set` | `{name, address}` | `{host: Host}` |
| `host_remove` | `{name, delete_backups?}` | `{uninstalled, message?}` |
| `uninstall` | `{delete_backups?}` | `{}` |
| `access_settings` | none | `{}` |
| `aws_status` | none | `{cli, manager?: {account, bucket, buckets, region}}` |
| `aws_keys` | `{host, bucket?, region?, sign_in}` | `{bucket, region, env, bucket_created?}` |
| `repository_set` | `{location, password, aws_key, init, env?}` | `repository` reply; sent daemon to daemon only |
| `repository_probe` | `{location, env?, password?}` | `{state, message?}` |
| `repository_setup` | `{host, location, env?, password?, init}` | `{repository: RepositoryInfo, password?}` |

Notes:

- **`pair_start`** makes a one-time code (10 minutes, single use) and serves, on port 7458 with the daemon's identity key, an install script (`/i/<code>`), the Linux daemon (`/d/<code>/<amd64|arm64>`) and the pairing call (`POST /p/<code>`, where the remote presents its own key in the TLS handshake). `command` is `curl -fsS --pinnedpubkey '<pin>' -k https://<lan ip>:7458/i/<code> | sudo sh`: `-k` because the certificate is self-signed, the pin is the check (curl enforces it with `-k`; tested in `TestPairing`). A new `pair_start` replaces one in progress. When the remote pairs, it appears in `hosts` and a `host_changed` follows once it's reachable.
- **`host_repair`** serves the same install script for a paired remote (`not_found` otherwise), minus the pairing step: run there, it reinstalls this app's daemon (on a Mac the whole bundle, root-owned) and rewrites its service (launchd plist or systemd unit), then restarts it, leaving its state directory alone (database, identity key, secrets, pairing). The code expires like a pairing code and can't be used to pair; it replaces a pairing in progress.
- **`host_set`** (on the controller) changes where a paired remote is reached, when its address changed: `address` is `host` or `host:port` (port 7457 if omitted). Saved, then the controller reconnects at once; the reply is the host as it stands (usually `connecting`), and `host_changed` follows. The pin doesn't change, so another machine at that address is still refused. `not_found` for a name that isn't a paired remote.
- **`host_remove`** (on the controller) removes a paired remote altogether. The controller sends it `uninstall`; then it unpairs it, stops following it and publishes `host_changed` (the host is gone from `hosts`). If the remote doesn't answer it's unpaired anyway and `message` gives the commands to run there; with `delete_backups` it isn't (only the remote can delete them: `host_unreachable`). A remote that answers with an error (one not installed by pairing: `bad_request`) stays paired. With `delete_backups` the controller also drops its own copies of the remote's secrets (Keychain `charm-bak-repo`, `charm-bak-env`, `charm-bak-aws` for that host); kept backups keep them, so they can still be opened.
- **`access_settings`** (macOS daemons, usually relayed to a Mac remote) shows charm-bak's app bundle in Finder and opens System Settings at Privacy & Security › Full Disk Access, in the session of whoever is logged in at that Mac's screen (a root daemon runs `open` there through `launchctl asuser`). macOS can't be asked for this grant; a person adds the app. `bad_request` off macOS, or when no one is logged in there. `status` reports the result as `full_disk_access` (macOS only): whether the daemon can open the system TCC database, which only Full Disk Access allows.
- **`aws_status`** and **`aws_keys`** (the controller only) make S3 keys so nobody has to make IAM users by hand. `aws_keys` with `sign_in` runs `aws login` (the AWS CLI's browser sign-in, into a temporary config folder, never `~/.aws`, deleted afterwards; up to 10 minutes) and, with those temporary credentials: makes the bucket if it isn't there (`bucket` empty: `charm-bak-<account id>`; private, SSE-S3, a lifecycle transition to Intelligent-Tiering, nothing that expires or archives; a bucket that's there is used as it is), the managed policy `charm-bak-boundary-<bucket>` (that bucket's objects and listing, the most any machine's key can do), and the key manager: IAM user `charm-bak-manager` (path `/charm-bak-manager/`, no S3 access) that can only create, change and delete users at path `/charm-bak/` and only with that boundary (`iam:PermissionsBoundary` on CreateUser, PutUserPolicy, DeleteUserPolicy and PutUserPermissionsBoundary, IAM's documented delegation pattern; a machine's user made earlier for another bucket has its boundary moved); its key goes into the Keychain (`charm-bak-aws/manager`) and its buckets into settings. Each sign-in adds its bucket to the ones the key manager serves (`buckets`; `bucket` is the latest); the boundary condition lists every one. Without `sign_in`, the key manager does it, for `bucket` (empty: the latest) if it serves it, else `bad_request`. Either way `host` gets IAM user `charm-bak-<host>` at `/charm-bak/` with the boundary and an inline policy for `<bucket>/<host>/` only, its old keys deleted and a new one made; the reply waits until the new key is accepted (IAM is eventually consistent) and carries it as `env` for the setup flow's check and `repository_setup`, like a typed one. A user of that name that charm-bak didn't make (another path) is refused. `not_configured` without the CLI or (no `sign_in`) a key manager.
- **`uninstall`** (controller to remote): the remote pauses and cancels its jobs and, if `delete_backups`, forgets its own snapshots (those tagged for its sources, and its `charm-bak:state` ones; other machines' in a shared repository are left) and prunes, all before replying; if that fails the reply is `bad_request` with the reason and nothing is uninstalled (it stays paired, and the controller keeps its copies of the secrets). Then it replies and removes what the pairing script installed: disables the service, deletes its unit, the executable and the state directory (database, secrets, identity key), and exits. The repository itself stays. `bad_request` on a daemon not installed by pairing.
- **`repository_probe`** says what's at a location before anything is saved, run by the daemon that will use it (send it with that machine's `host`: its network, drives and SSH keys). It runs restic `cat config` with the given credentials (`env`, a backend's environment variables, `internal/backends`) and password: `state` is `empty` (nothing there: it can be created), `opens` (a repository the password opens), `password` (a repository it doesn't open, or no password was given), or `failed` with restic's reason in `message`. restic retries backend errors for minutes; the probe stops at the first retry and reports its reason (a wrong key answers in a second or two). A location under `/Volumes/<drive>` whose drive isn't mounted fails with "The drive … isn't connected." instead of restic making the folder on the startup disk.
- **`repository_setup`** (on the controller) is the setup flow's save: for this machine (`host` empty or its own name) or a paired remote. `init: true` creates the repository (a password is made unless one is given, and returned once as `password` so the user can keep a copy off the machine); otherwise `password` must open the repository already there (checked first; nothing is stored when it doesn't). On macOS the secrets go into the Keychain (`charm-bak-repo/<host>`, and the credentials as `charm-bak-env/<host>`); a remote gets them with `repository_set` and the Mac keeps the same copy. Refused (`bad_request`) when the machine already has a repository.
- **`repository_set`** writes `repo-password` and `aws` (`KEYID:SECRET`) as owner-only files (0600, directory 0700) in the daemon's state directory (`/var/lib/charm-bak` for a root Linux daemon), and `env` (a backend's credentials) as `credentials`, JSON, optionally runs `init` (or, without it, checks the password opens the repository there), and makes that the repository; caches are dropped and a `state_changed` follows. restic reads the password file itself (`RESTIC_PASSWORD_FILE`); `repository` then shows `password_from`/`aws_from` as `file <path>`. A daemon that keeps secrets in the Keychain (the Mac) refuses with `bad_request`. `busy` while a job runs.
- **Remotes.** Any request may carry `"host": "<name>"` beside `method`; the daemon relays it to that paired remote over mutual TLS and returns the remote's reply as is. Reading is the exception: the controller reads every remote's repository itself. `snapshots`, `ls`, `changes` and `dump` (other than of `"live"`) naming a remote are answered by the controller running its own engine against that remote's repository, whether or not the remote answers, as long as the controller holds the repository's location (saved from the remote's `repository` reply) and its password and credentials (kept in its own secret store when the remote was set up through it); otherwise they are relayed. `sources` and `repository` are relayed and the reply saved; when the remote doesn't answer, the copy saved at last contact is returned (`host_unreachable` if there is none). `status`, `"live"` reads, `diff` and every change need the remote. `restore` with `host` restores on the remote; `restore` without `host` and with `from: "<name>"` restores from that remote's repository onto the controller (`target` must be an absolute path; `not_configured` if the controller can't open that repository). `snapshot_meta` is what a daemon's database knows about a source's snapshots that the repository doesn't (pins; all its notes and recalculated added sizes): the controller asks the remote for it when listing, saves it, and uses the saved copy when the remote is off. No host (or the daemon's own name) is local. A remote that doesn't answer gives `host_unreachable`; an unknown name gives `not_found`. Events from remotes come through the same subscription with `"host": "<name>"` beside `"event"`. `host_changed` (`{host_info: Host}`) says a remote became reachable or stopped answering. A Host is `{name, local, address, state: "ok" | "unreachable", error, last_seen}`.

- **`repository`** is for a host's header (its hero and info) and its Edit popup's defaults: `{location, password_from, aws_from, stored_bytes, logical_bytes, last_prune, last_check, next_maintenance}`. Credentials are described by where they come from ("Keychain charm-bak-repo / studio"), never sent; a password command that isn't the usual Keychain lookup isn't shown at all, since it could contain the password. `defaults` is `{schedule, retention, excludes}` (a retention count of `-1` means unlimited, restic's `unlimited`), `schedule` null for manual; `excludes` are patterns every source skips before its own (none built in).
- **`defaults_set`** changes what sources inherit; a schedule with `every` 0 means manual; `excludes: []` clears the default skips (absent leaves them). Every source gets a `source_changed` event, since the ones that inherit now show (and run on) the new values.

- **`hello`** should be the first request on a request connection; the daemon doesn't enforce it, and a connection that only subscribes may skip it (events carry no version-dependent replies). If `protocol` isn't one the app speaks, the reply is `unsupported_protocol` and the TUI tells the user which side to update. `build` is the SHA-256 (hex) of the daemon's own executable, `os`/`arch` Go's GOOS/GOARCH: a controller compares them with the daemon it carries for that platform. `bundle: true` is a Mac remote running from its app bundle; its `build` then also covers the bundle's `Info.plist` and `_CodeSignature/CodeResources`.
- **`self_update`** keeps remotes on the controller's daemon. Whenever the controller (re)connects to a remote and the remote's `build` differs from the controller's `Contents/Resources/linux-<arch>/charm-bak-daemon`, it sends that file in order, at most 1 MiB of `data` (base64) per call, `offset` 0 first. The remote stages it beside its executable, and on the piece that reaches `size` checks the SHA-256, renames it over its executable and restarts in place: a clean shutdown, then exec of the new file with the same arguments (same process, so a service manager sees nothing; if exec fails it exits 75 and `Restart=on-failure` starts it). `busy` while a job runs (the next reconnect tries again); out-of-order pieces or a hash mismatch give `bad_request` and change nothing. A remote whose hello says `bundle` gets the whole `darwin-<arch>/charm-bak.app` as a tar.gz instead (`bundle: true` on every piece): it unpacks it beside its own bundle (folders and regular files inside the bundle only), checks for `Contents/MacOS/<its executable>`, swaps the two and restarts, so the bundle's icon, Info.plist and signature seal follow the app. A Mac remote whose hello doesn't say `bundle` isn't sent an update (a new executable in its bundle would fail launchd's signature check at exec); it's reinstalled with Add Remote. Each build is sent to a remote at most once per controller run in each form. The Mac's daemon refuses (`bad_request`): it ships inside the signed app.
- **`status`**: `repository` is `{location, stored_bytes, logical_bytes}` or null before setup; `state` is `idle`, `running`, `pruning` or `paused`; `job` is the running Job or null.
- **`ls`** returns one directory's direct children (restic's `ls` without `--recursive`), sorted with directories first and then by name. Default `limit` 1000; `next_offset` is null on the last page. `snapshot: "live"` lists the disk as it is now (absolute path, no repository needed; entries that vanish mid-listing are skipped): the add-source picker browses a remote's disk this way.
- **`changes`** is what `snapshot` has that `base`, an earlier snapshot of the same source, doesn't: every path added, removed or changed between them through the whole tree (`change`: `added`, `removed`, `changed`, or `touched` for a file with the same contents and different details such as its timestamp, which is why a backup can store new folder records with no contents changed; touched folders aren't listed; a folder's path ends in `/`), in path order, at most 5000 of them; the counts are of all. It reads only the repository, so for a remote the controller answers it itself, like `ls`. Answers are kept in the daemon's cache database: two snapshots never change.
- **`dump`**: `data` is base64 of at most `max` bytes (the app caps `max` at 1 MiB, and uses that when `max` is absent or 0); `size` is the file's full size; `binary` is the app's guess from the first bytes, and when true `data` is empty. `snapshot: "live"` reads the file as it is on disk now (a regular file, absolute path; `not_found` if it's gone), so the TUI can diff a snapshot's copy against it without disk access of its own.
- **`diff`** compares one snapshot directory with the same path on disk: `change` is `added` (on disk, not in the snapshot), `removed` (in the snapshot, not on disk), `changed` (size or mtime differs) or `same`.
- **`restore`**: `paths` are absolute paths in the snapshot (empty: the whole snapshot). `target` is `"original"` or an absolute directory; each path lands there under its own name, keeping its place below the deepest folder the paths share (`/a/b/f.txt` to `/tmp/x` makes `/tmp/x/f.txt`), so `"original"` puts things back where they were. restic creates the target if it's missing. `overwrite` is `never`, `if_newer` or `always` (restic's `--overwrite`; `always` still skips files whose contents already match).
- **`source_set`** changes only the fields it carries. `schedule: {"every": 0}` makes the source manual-only. `excludes: []` clears the excludes (absent leaves them). `inherit: ["schedule", "retention"]` resets those fields to the defaults.
- **`source_rewrite`** takes `excludes` (absolute paths) out of every snapshot the source already has. `dry_run` answers how many snapshots have them (`snapshots`) and changes nothing; it reads every snapshot, so it can take a while. Otherwise it queues a `rewrite` job: `restic rewrite --forget --tag src:<id> --exclude …` (each changed snapshot is replaced by a new one whose `original` is the old ID; exclusive lock, so reads answer `busy` while it runs, as during a prune), then each replaced snapshot's note and pin move to its replacement, then `prune` frees the space. It doesn't change the source's own excludes: send `source_set` for that. The menu bar app posts a notification when a `rewrite` finishes, either way.
- **`source_remove`** with `keep_snapshots: false` forgets the source's snapshots; their data goes at the next prune.

## Events

| event | fields |
|---|---|
| `job_started` | `job: Job` |
| `progress` | `job_id`, `percent` (0–1), `files_done`, `total_files`, `bytes_done`, `total_bytes`, `current` (path or "") |
| `job_finished` | `run: Run` |
| `source_changed` | `source: Source` |
| `state_changed` | `state`, `job` (Job or null) |
| `host_changed` | `host_info: Host` |
| `aws_step` | `for` (the machine whose key is being made), `step` (`signed_in`, `bucket`, `manager`, `user`, `key_works`, in that order; without signing in only the last two), `account`, `bucket` |

`progress` is restic's `status` line relabeled, at most one per second per job.

## Connections and jobs

- Jobs belong to the app. A client disconnecting, the TUI quitting, or the TUI crashing never cancels a backup or a restore; only `cancel` does. On reconnect, `status` and `history` show what happened meanwhile.
- `cancel` stops restic with SIGINT, which lets restic remove its lock and keep what it already finished; the run ends with status `cancelled`.

## Versions

`protocol` is an integer. Adding fields, methods, events or error codes doesn't change it; removing or changing the meaning of one does.
