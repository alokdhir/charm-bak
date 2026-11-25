// Package daemon is charm-bak-daemon: the store, scheduler, job queue and
// socket server for one machine, running every restic call behind the
// Engine interface. The same code runs on the Mac (started by the menu
// bar app) and on Linux remotes (under systemd).
package daemon

import (
	"context"
	"fmt"
	"time"

	"charm-bak/internal/protocol"
)

// Engine is everything the daemon needs from a backup engine and nothing
// about how it's done. ResticEngine is the only implementation; the core
// never builds an engine command itself.
//
// A source is identified to the engine by a tag (src:<id>): restic has no
// source object, so the tag rides on each snapshot.
type Engine interface {
	// Init creates an empty repository. Fails if one exists.
	Init(ctx context.Context) error
	// Snapshots lists snapshots, newest first. An empty tag means all.
	Snapshots(ctx context.Context, tag string) ([]EngineSnapshot, error)
	// List returns one directory's direct children, directories first.
	List(ctx context.Context, snapshot, dir string) ([]protocol.Entry, error)
	// Tree lists a whole snapshot: every directory's children (sorted as
	// List sorts them), keyed by the directory's absolute path, "/"
	// included. Empty directories map to an empty list.
	Tree(ctx context.Context, snapshot string) (map[string][]protocol.Entry, error)
	// Stats is the repository's size: what it stores (compressed, after
	// deduplication) and the same data uncompressed.
	Stats(ctx context.Context) (RepoStats, error)
	// Read returns the first max bytes of a file and its full size.
	Read(ctx context.Context, snapshot, file string, max int) ([]byte, int64, error)
	// Backup backs up a source, reporting progress as it goes.
	Backup(ctx context.Context, spec BackupSpec, progress func(Progress)) (BackupResult, error)
	// ApplyRetention drops snapshots the policy doesn't keep, except
	// pinned ones. All-zero retention keeps everything.
	ApplyRetention(ctx context.Context, tag string, r protocol.Retention, pinned map[string]bool) error
	// KeepReasons says why each snapshot is kept ("pinned" first for
	// pinned ones), without changing anything.
	KeepReasons(ctx context.Context, tag string, r protocol.Retention, pinned map[string]bool) (map[string][]string, error)
	// Forget drops these snapshots outright.
	Forget(ctx context.Context, ids []string) error
	// Maintain removes data no snapshot uses (restic prune). Blocks every
	// read while it runs.
	Maintain(ctx context.Context) error
	// Rewrite drops excludes (absolute paths, as a backup's excludes)
	// from every snapshot tagged tag: each changed one is replaced by a
	// new snapshot whose Original is the old ID (restic rewrite --forget;
	// takes the exclusive lock). It returns how many snapshots changed
	// (would change, for dryRun). The data goes at the next Maintain.
	Rewrite(ctx context.Context, tag string, excludes []string, dryRun bool) (int, error)
	// Added is how much data snapshot to has that from doesn't (restic
	// diff's "added" bytes): what a backup's "new" would be between them.
	Added(ctx context.Context, from, to string) (int64, error)
	// Changes lists what snapshot to has that from doesn't: every path
	// added, removed or changed between them, in path order.
	Changes(ctx context.Context, from, to string) (EngineChanges, error)
	// Check verifies the repository, reading back readPercent of the data.
	Check(ctx context.Context, readPercent int) error
	// Probe says what's at the location: a repository this password
	// opens, one it doesn't, nothing yet, or an error (bad credentials,
	// unreachable), without waiting out restic's retries.
	Probe(ctx context.Context) (ProbeResult, error)
	// Restore restores paths from a snapshot to target ("" = where they
	// came from). Each path lands in target under its own name, keeping
	// its place below the deepest folder the paths share: restoring
	// /a/b/f.txt to /tmp/x makes /tmp/x/f.txt, not /tmp/x/a/b/f.txt.
	Restore(ctx context.Context, snapshot string, paths []string, target, overwrite string, progress func(Progress)) error
}

// EngineChanges is what differs between two snapshots.
type EngineChanges struct {
	Paths      []protocol.PathChange
	BytesAdded int64
}

// EngineSnapshot is what every engine knows about a snapshot.
type EngineSnapshot struct {
	ID, ShortID string
	Time        time.Time
	End         *time.Time
	Paths       []string
	Hostname    string
	Tags        []string
	Original    string // the snapshot this one replaced (rewrite), or ""
	Files       int64
	Bytes       int64
	BytesAdded  int64
}

// BackupSpec is what to back up, compiled from a source.
type BackupSpec struct {
	Tag      string
	Paths    []string
	Excludes []string
}

// Progress is a running job's progress.
type Progress struct {
	Percent               float64
	FilesDone, TotalFiles int64
	BytesDone, TotalBytes int64
	Current               string
}

// BackupResult is a finished backup.
type BackupResult struct {
	// Incomplete: some files couldn't be read (restic exit 3); the
	// snapshot exists but is missing them.
	Incomplete bool
	// Unread names the first files that couldn't be read, with the
	// reason ("C:\\pagefile.sys (in use)"); UnreadCount is all of them.
	Unread       []string
	UnreadCount  int
	Snapshot     string
	FilesNew     int64
	FilesChanged int64
	BytesAdded   int64
	// Files and Bytes are everything the snapshot covers.
	Files int64
	Bytes int64
}

// RepositoryConfig is where the repository is and how to unlock it.
// Secrets are never stored here, only references: Keychain items or
// owner-only files (see secrets.go).
type RepositoryConfig struct {
	Location string `json:"location"`
	// PasswordCommand prints the repository password (restic's
	// --password-command), e.g. a Keychain lookup.
	PasswordCommand string `json:"password_command,omitempty"`
	// Environment is extra, non-secret environment for the engine.
	Environment map[string]string `json:"environment,omitempty"`
	// AWSKeychain names a Keychain item ("service/account") holding
	// "KEYID:SECRET"; fetched per run and passed as AWS_* env.
	AWSKeychain string `json:"aws_keychain,omitempty"`
	// PasswordFile and AWSFile are owner-only files holding the password
	// and "KEYID:SECRET", where there's no Keychain (a Linux remote):
	// repository_set writes them into the state directory. restic reads
	// the password file itself (RESTIC_PASSWORD_FILE).
	PasswordFile string `json:"password_file,omitempty"`
	AWSFile      string `json:"aws_file,omitempty"`
	// EnvKeychain and EnvFile hold a backend's credentials as a JSON
	// object of environment variables (internal/backends): a Keychain
	// item ("service/account", the JSON base64url-encoded, since the item
	// is written through `security -i`) or an owner-only file. They
	// replace AWSKeychain/AWSFile for repositories set up since
	// 2026-11-22; both still work.
	EnvKeychain string `json:"env_keychain,omitempty"`
	EnvFile     string `json:"env_file,omitempty"`

	// Password and SecretEnv are in memory only, never stored: a probe
	// or a setup runs restic before anything is saved.
	Password  string            `json:"-"`
	SecretEnv map[string]string `json:"-"`
}

// EngineError says why an engine call failed, in terms the core acts on.
type EngineError struct {
	Kind    ErrKind
	Code    int // engine exit code, when there is one
	Message string
}

// ErrKind classifies engine failures.
type ErrKind int

const (
	ErrFailed        ErrKind = iota
	ErrLocked                // locked by another job (restic exit 11)
	ErrNoRepository          // nothing at the location (restic exit 10)
	ErrWrongPassword         // restic exit 12
	ErrCancelled             // interrupted (restic exit 130)
	ErrNotFound              // a path or snapshot that isn't there
)

func (e *EngineError) Error() string {
	switch e.Kind {
	case ErrLocked:
		return "The repository is busy: " + e.Message
	case ErrNoRepository:
		return "No repository there: " + e.Message
	case ErrWrongPassword:
		return "The repository password is wrong."
	case ErrCancelled:
		return "Cancelled."
	case ErrNotFound:
		return e.Message
	}
	return fmt.Sprintf("Engine failed (exit %d): %s", e.Code, e.Message)
}

// RepoStats is the repository's size, from restic stats --mode raw-data.
type RepoStats struct {
	Stored       int64 // bytes of unique data as stored, compressed
	Uncompressed int64 // the same data before compression
}

// ProbeResult is what Probe found.
type ProbeResult struct {
	State   string // protocol.Probe*
	Message string // restic's reason, for ProbeFailed
}
