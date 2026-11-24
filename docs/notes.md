# Test notes

What was tested on macOS and restic while building charm-bak, and what the results mean for its design. restic 0.19.1 and macOS 27 unless a section says otherwise; items marked unverified were not checked.

## Mount, diff and cherry-pick

A mounted snapshot is a normal read-only directory. That allows `diff -r`, `rsync -avn`, a visual diff tool (Kaleidoscope, FileMerge, VS Code) against the live directory, and cherry-picking with `cp` or by merging hunks. Restic's own `diff` only compares two snapshots. The alternative to a mount is restoring the old files to a scratch directory and diffing against that, which avoids macFUSE.

A mounted snapshot is slower than local disk, because the first read of each file fetches chunks from S3.

## macFUSE status

- The kernel-extension route needs Recovery Mode and Reduced Security on Apple Silicon. That's the old painful path.
- macFUSE 5.0.0 (announced 2025-05-05) added an experimental FSKit backend that runs in user space with no kernel extension. It's opt-in with the mount option `-o backend=fskit`. Sources: https://macfuse.github.io/2025/05/05/macfuse-5.0.0.html and https://github.com/macfuse/macfuse/wiki/FUSE-Backends.
- 5.2.0 (2026-04-09) added an open-source API for mounting with the FSKit backend.
- 5.5.0 (2026-10-17) is the latest seen. Its notes include read-only mount support on macOS 26.4 and later, and note that FSKit has no per-file fsync callback.

## Full Disk Access test (2026-11-19)

Tested with the real app skeleton (`app/`, built and signed by `app/build.sh` with the Apple Development identity, bundle ID `io.github.alokdhir.charm-bak`, installed to `~/Applications/charm-bak.app`). The probe (`AccessProbe.swift`, run with `open -a … --args --probe --quit`) reads `~/Library/Mail`, `Messages` and `Safari`, which only Full Disk Access unlocks and which fail quietly without it (no consent prompt). It also runs `/bin/ls` and `restic backup --dry-run` on a temp repo as child processes.

| | ungranted (separate bundle ID, never granted) | granted | granted, then rebuilt with changed code |
|---|---|---|---|
| app reads the three folders | denied (Cocoa error 257) | ok | ok |
| child `/bin/ls ~/Library/Mail` | denied (exit 1) | ok | ok |
| child `restic backup --dry-run ~/Library/Safari` | exit 3 (couldn't read some files) | (not in that build) | ok (exit 0) |

- **The grant survives rebuilds** when the bundle ID and signing identity stay the same, even though the binary (and its hash) changed.
- **Children inherit it:** restic started by the app runs under the app's grant, so restic itself never needs one.
- **A control from a terminal proves nothing** if that terminal app has Full Disk Access (this one does): its shells inherit it.
- Not tested: ad-hoc signing and a bare binary. They don't matter now that the signed app works; `build.sh` keeps one identity.

## restic output and locking (2026-11-19, restic 0.19.1, local throwaway repo)

- **`backup --json`** prints `{"message_type":"status",…}` lines about once a second while it runs (`percent_done`, `total_files`, `files_done`, `total_bytes`, `bytes_done`, `current_files`), then one `{"message_type":"summary",…}` (`files_new/changed/unmodified`, `dirs_*`, `data_added`, `data_added_packed`, `total_files_processed`, `total_bytes_processed`, `total_duration`, `backup_start`, `backup_end`, `snapshot_id`). A backup that finishes within a second prints only the summary.
- **`snapshots --json`**: an array of `{time, parent, tree, paths, hostname, username, uid, gid, tags, program_version, summary, id, short_id}`; `summary` holds the same counters as the backup summary, so per-snapshot size and file counts come for free.
- **`ls --json <snap> <dir>`**: first line is the snapshot, then one `{"message_type":"node", name, type (dir|file|symlink…), path (absolute), size (files), mode, permissions, mtime, atime, ctime, uid, gid, inode}` per entry. Given a directory it lists that directory's node plus its direct children only (`--recursive` would descend), which is exactly one tree column; the app drops the first node (the directory itself).
- **Keep reasons exist.** `forget --dry-run --json` returns one object per group: `{tags, host, paths, keep, remove, reasons}`, where each reason is `{snapshot, matches}` and `matches` reads like `["last snapshot", "daily snapshot"]`. That fills the hero's "Kept" cell; the app runs it after each backup and caches the result.
- **Reads fail during prune.** While `prune` holds the exclusive lock, `snapshots` exits 11 with `"repository is already locked exclusively by PID …"`; `--retry-lock 5s` waits and then fails the same way. So: `forget` after every backup (brief), `prune` on its own schedule (weekly, overnight), and while it runs the app answers reads with a "busy: pruning since …" error that the TUI shows instead of a failure. `--no-lock` reads during prune are possible but can hit packs being deleted. **Revised 2026-11-20:** reads now use `--no-lock`, because the lock cost about 0.4s of every ~1.2s call on S3, and the daemon already refuses reads while its own prune runs (one repository per machine, so no other charm-bak prunes it). Only a prune run by hand at the same moment can make a read fail; it can't damage anything, since reads write nothing.
- **S3 credentials:** the S3 backend uses the standard AWS chain since restic 0.9.1: static keys, env vars, then `~/.aws/credentials` (CHANGELOG, Enhancement #1782). Selecting a named profile via `AWS_PROFILE` isn't mentioned in the changelog: unverified until tested against a real bucket.

## restic re-includes need the parents open (2026-11-20)

Tested on restic 0.19.1 with a throwaway local repository:

- `--exclude /d/a/big --exclude '!/d/a/big/keep'` backs up nothing under `big`, `keep` included: restic doesn't descend into an excluded directory, so the re-include never sees anything.
- `--exclude '/d/a/big/*' --exclude '!/d/a/big/keep'` backs up `big/keep` and everything in it, and nothing else in `big`.
- Two levels down: `big/*`, `!big/x`, `big/x/*`, `!big/x/y`, then `big/x/y/tmp.log` backs up exactly `big/x/y` minus `tmp.log`.
- `\[` in a pattern matches a literal `[`: `--exclude '/d/a/Shot \[1].png'` excluded that file and kept `Shot 1.png`.

The source compiler (`internal/backup/selection.go`) emits exactly these forms.
