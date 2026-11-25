// Package protocol is the wire format between the TUI and the menu bar
// app: newline-delimited JSON over a Unix socket. docs/protocol.md is the
// contract; testdata/protocol holds one example per method and event.
//
// No charm imports and no human-facing wording beyond what the app sends.
package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Version is the protocol version this client speaks.
const Version = 1

// SocketPath is where the daemon listens on this machine: in the app's
// support folder on the Mac; on Linux under /run for the root service
// (the usual remote), else the user's runtime directory.
func SocketPath() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		// A service's state folder, locked to administrators and SYSTEM
		// by the installer (Windows has AF_UNIX sockets since 10 1803).
		return filepath.Join(os.Getenv("ProgramData"), "charm-bak", "daemon.sock")
	}
	if runtime.GOOS != "linux" {
		return filepath.Join(home, "Library", "Application Support", "charm-bak", "app.sock")
	}
	if os.Geteuid() == 0 {
		return "/run/charm-bak/daemon.sock"
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "charm-bak", "daemon.sock")
	}
	return filepath.Join(home, ".local", "state", "charm-bak", "daemon.sock")
}

// Request is one call from the client.
type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	// Host names a paired remote to relay the request to; empty is the
	// daemon this connection reaches.
	Host string `json:"host,omitempty"`
}

// Reply answers a Request: exactly one of Result or Error is set.
type Reply struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is a failed request. Message is a sentence the TUI may show as is.
type Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// Error codes.
const (
	ErrBadRequest          = "bad_request"
	ErrUnknownMethod       = "unknown_method"
	ErrUnsupportedProtocol = "unsupported_protocol"
	ErrNotFound            = "not_found"
	ErrBusy                = "busy"
	ErrResticFailed        = "restic_failed"
	ErrNotConfigured       = "not_configured"
	ErrInternal            = "internal"
	ErrHostUnreachable     = "host_unreachable" // a relayed request's remote didn't answer
)

// BusyData is Error.Data for ErrBusy.
type BusyData struct {
	Job Job `json:"job"`
}

// UnsupportedProtocolData is Error.Data for ErrUnsupportedProtocol: the
// version the app speaks.
type UnsupportedProtocolData struct {
	Protocol int `json:"protocol"`
}

// ResticFailedData is Error.Data for ErrResticFailed.
type ResticFailedData struct {
	Exit   int    `json:"exit"`
	Stderr string `json:"stderr"`
}

// Retention is restic forget's --keep-* counts. KeepUnlimited (-1)
// means every period, however old (restic's "unlimited").
type Retention struct {
	Last    int `json:"last"`
	Hourly  int `json:"hourly"`
	Daily   int `json:"daily"`
	Weekly  int `json:"weekly"`
	Monthly int `json:"monthly"`
	Yearly  int `json:"yearly"`
}

// KeepUnlimited is a Retention count with no limit.
const KeepUnlimited = -1

// Schedule is how often a source runs. Every is seconds.
type Schedule struct {
	Every int64 `json:"every"`
}

// Source health.
const (
	HealthOK      = "ok"
	HealthOverdue = "overdue"
	HealthFailed  = "failed"
	// HealthSkipped: the last backup made a snapshot but couldn't read
	// some files (a warning, not a failure).
	HealthSkipped = "skipped"
	HealthNever   = "never"
)

// Source is one backup set; in the repository it's the tag "src:<ID>".
type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Host is the machine the source backs up, which runs its daemon:
	// filled in by the daemon that answers (its own name, or a remote's
	// when relaying).
	Host      string    `json:"host"`
	Paths     []string  `json:"paths"`
	Excludes  []string  `json:"excludes"`
	Schedule  *Schedule `json:"schedule"`
	Retention Retention `json:"retention"`
	Inherited []string  `json:"inherited"`
	Paused    bool      `json:"paused"`
	Health    string    `json:"health"`
	// Files and Bytes are the size of the source's newest snapshot (what
	// its last backup that made one covered); 0 before the first.
	Files   int64      `json:"files"`
	Bytes   int64      `json:"bytes"`
	LastRun *Run       `json:"last_run"`
	NextRun *time.Time `json:"next_run"`
}

// Snapshot is restic's snapshot plus charm-bak's fields.
type Snapshot struct {
	ID         string     `json:"id"`
	ShortID    string     `json:"short_id"`
	Time       time.Time  `json:"time"`
	End        *time.Time `json:"end"`
	Paths      []string   `json:"paths"`
	Hostname   string     `json:"hostname"`
	Tags       []string   `json:"tags"`
	Files      int64      `json:"files"`
	Bytes      int64      `json:"bytes"`
	BytesAdded int64      `json:"bytes_added"`
	KeptBy     []string   `json:"kept_by"`
	Pinned     bool       `json:"pinned"`
	Note       string     `json:"note"`
}

// Entry types.
const (
	TypeDir     = "dir"
	TypeFile    = "file"
	TypeSymlink = "symlink"
	TypeOther   = "other"
)

// Entry is one item in a directory listing.
type Entry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Size  int64     `json:"size"`
	Mode  string    `json:"mode"`
	MTime time.Time `json:"mtime"`
}

// Job kinds.
const (
	KindBackup  = "backup"
	KindForget  = "forget"
	KindPrune   = "prune"
	KindCheck   = "check"
	KindRestore = "restore"
	// KindState backs up the daemon's own database (sources, history,
	// notes, pins, peers) into its repository, daily.
	KindState = "state"
	// KindRewrite removes paths from a source's existing snapshots
	// (source_rewrite), then prunes.
	KindRewrite = "rewrite"
)

// Job is a running restic job. Source is empty for repository-wide jobs.
type Job struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Source  *string   `json:"source"`
	Started time.Time `json:"started"`
}

// Run statuses.
const (
	RunOK         = "ok"
	RunIncomplete = "incomplete"
	RunFailed     = "failed"
	RunCancelled  = "cancelled"
)

// Run is a finished job from the app's history.
type Run struct {
	Job          string    `json:"job"`
	Kind         string    `json:"kind"`
	Source       *string   `json:"source"`
	Started      time.Time `json:"started"`
	Ended        time.Time `json:"ended"`
	Status       string    `json:"status"`
	FilesNew     int64     `json:"files_new"`
	FilesChanged int64     `json:"files_changed"`
	BytesAdded   int64     `json:"bytes_added"`
	// Files and Bytes are everything a backup covered, changed or not.
	Files    int64  `json:"files"`
	Bytes    int64  `json:"bytes"`
	Snapshot string `json:"snapshot"`
	Error    string `json:"error"`
	// Skipped is how many files an incomplete backup couldn't read.
	Skipped int64 `json:"skipped,omitempty"`
}

// App states.
const (
	StateIdle    = "idle"
	StateRunning = "running"
	StatePruning = "pruning"
	StatePaused  = "paused"
)

// Repository is the configured repository and its size.
type Repository struct {
	Location     string `json:"location"`
	StoredBytes  int64  `json:"stored_bytes"`
	LogicalBytes int64  `json:"logical_bytes"`
}

// Method names, and each one's params and result.
const (
	MethodHello          = "hello"
	MethodStatus         = "status"
	MethodSources        = "sources"
	MethodSnapshots      = "snapshots"
	MethodLs             = "ls"
	MethodDump           = "dump"
	MethodDiff           = "diff"
	MethodRestore        = "restore"
	MethodBackupNow      = "backup_now"
	MethodCancel         = "cancel"
	MethodSourceAdd      = "source_add"
	MethodSourceSet      = "source_set"
	MethodSourceRewrite  = "source_rewrite"
	MethodSourceRemove   = "source_remove"
	MethodSnapshotSet    = "snapshot_set"
	MethodSnapshotMeta   = "snapshot_meta"
	MethodChanges        = "changes"
	MethodPause          = "pause"
	MethodResume         = "resume"
	MethodHistory        = "history"
	MethodSubscribe      = "subscribe"
	MethodRepository     = "repository"
	MethodDefaultsSet    = "defaults_set"
	MethodHosts          = "hosts"
	MethodPairStart      = "pair_start"
	MethodHostRepair     = "host_repair"
	MethodRepoSet        = "repository_set"
	MethodRepoProbe      = "repository_probe"
	MethodRepoSetup      = "repository_setup"
	MethodSelfUpdate     = "self_update"
	MethodHostSet        = "host_set"
	MethodHostRemove     = "host_remove"
	MethodUninstall      = "uninstall"
	MethodAccessSettings = "access_settings"
	MethodAWSStatus      = "aws_status"
	MethodAWSKeys        = "aws_keys"
	MethodRun            = "run"
)

// UntaggedSource is the pseudo-source for snapshots without a charm-bak tag.
const UntaggedSource = "untagged"

type HelloParams struct {
	Protocol int    `json:"protocol"`
	Client   string `json:"client"`
}

type HelloResult struct {
	Protocol int    `json:"protocol"`
	App      string `json:"app"`
	// Build is the SHA-256 (hex) of the daemon's own executable; with OS
	// and Arch (Go's GOOS/GOARCH) it tells a controller whether a remote
	// runs the daemon it carries, and which one to send (self_update).
	Build string `json:"build"`
	OS    string `json:"os"`
	Arch  string `json:"arch"`
	// Bundle: a Mac remote running from its app bundle, which takes the
	// whole bundle in self_update (bundle: true). Build then covers the
	// bundle's Info.plist and code-signature seal too.
	Bundle bool `json:"bundle,omitempty"`
}

type StatusResult struct {
	Repository *Repository `json:"repository"`
	State      string      `json:"state"`
	Job        *Job        `json:"job"`
	Paused     bool        `json:"paused"`
	// FullDiskAccess is, on a Mac, whether the daemon can read what only
	// Full Disk Access unlocks; absent elsewhere. Without it, backups
	// skip protected folders (Mail, Messages, Safari, other users').
	FullDiskAccess *bool `json:"full_disk_access,omitempty"`
}

type SourcesResult struct {
	Sources []Source `json:"sources"`
}

type SnapshotsParams struct {
	Source string `json:"source"`
}

type SnapshotsResult struct {
	Snapshots []Snapshot `json:"snapshots"`
}

type LsParams struct {
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type LsResult struct {
	Entries    []Entry `json:"entries"`
	Total      int     `json:"total"`
	NextOffset *int    `json:"next_offset"`
}

type DumpParams struct {
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
	Max      int    `json:"max"`
}

// DumpResult.Data is the file's first bytes (base64 on the wire, which
// encoding/json does for []byte).
type DumpResult struct {
	Data      []byte `json:"data"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

type DiffParams struct {
	Snapshot string `json:"snapshot"`
	Path     string `json:"path"`
}

// Diff changes.
const (
	ChangeAdded   = "added"
	ChangeRemoved = "removed"
	ChangeChanged = "changed"
	ChangeSame    = "same"
	// ChangeTouched (changes only): the same contents, different details
	// such as its timestamp or permissions.
	ChangeTouched = "touched"
)

type DiffEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Change string `json:"change"`
}

type DiffResult struct {
	Entries []DiffEntry `json:"entries"`
}

// Restore overwrite modes (restic's --overwrite).
const (
	OverwriteNever   = "never"
	OverwriteIfNewer = "if_newer"
	OverwriteAlways  = "always"
)

// LiveSnapshot as DumpParams.Snapshot reads the file as it is on disk
// now, for comparing a snapshot's copy with it.
const LiveSnapshot = "live"

// RestoreOriginal as RestoreParams.Target restores in place.
const RestoreOriginal = "original"

type RestoreParams struct {
	Snapshot  string   `json:"snapshot"`
	Paths     []string `json:"paths"`
	Target    string   `json:"target"`
	Overwrite string   `json:"overwrite"`
	// From names a paired remote whose repository to restore from, onto
	// the machine handling the request (the controller reads every
	// remote's repository itself). Target must then be an absolute path:
	// there is no original place here. Empty: this machine's repository.
	From string `json:"from,omitempty"`
}

// ChangesParams asks what snapshot has that base, an earlier snapshot of
// the same source, doesn't: the paths added, removed and changed between
// them, through the whole tree.
type ChangesParams struct {
	Snapshot string `json:"snapshot"`
	Base     string `json:"base"`
}

// PathChange is one path that differs; Change is ChangeAdded,
// ChangeRemoved, ChangeChanged or, for a file whose contents are the same
// and only its details differ, ChangeTouched. A folder's path ends in
// "/"; folders are never listed as touched.
type PathChange struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// ChangesResult lists at most the first 5000 changes, in path order; the
// counts are of all of them.
type ChangesResult struct {
	Changes []PathChange `json:"changes"`
	Added   int          `json:"added"`
	Removed int          `json:"removed"`
	Changed int          `json:"changed"`
	Touched int          `json:"touched"`
	Bytes   int64        `json:"bytes_added"`
}

// SnapshotMetaParams asks for what a daemon's database knows about a
// source's snapshots that the repository doesn't hold. The controller
// lists a remote's snapshots from its repository itself and asks the
// remote only for this.
type SnapshotMetaParams struct {
	Source string `json:"source"`
}

// SnapshotMetaResult is keyed by full snapshot id. Added has entries only
// for rewritten snapshots, whose own figure restic copied from the
// original.
type SnapshotMetaResult struct {
	Pinned []string          `json:"pinned"`
	Notes  map[string]string `json:"notes"`
	Added  map[string]int64  `json:"added"`
}

type JobResult struct {
	Job Job `json:"job"`
}

type BackupNowParams struct {
	Source string `json:"source"`
}

type CancelParams struct {
	Job string `json:"job"`
}

type SourceAddParams struct {
	Name      string     `json:"name"`
	Paths     []string   `json:"paths"`
	Excludes  []string   `json:"excludes"`
	Schedule  *Schedule  `json:"schedule,omitempty"`
	Retention *Retention `json:"retention,omitempty"`
	BackUpNow bool       `json:"back_up_now"`
}

// SourceSetParams changes only the fields that are set.
type SourceSetParams struct {
	ID    string   `json:"id"`
	Name  *string  `json:"name,omitempty"`
	Paths []string `json:"paths,omitempty"`
	// Excludes is a pointer so an empty list can be sent: [] clears them.
	Excludes  *[]string  `json:"excludes,omitempty"`
	Schedule  *Schedule  `json:"schedule,omitempty"`
	Retention *Retention `json:"retention,omitempty"`
	Paused    *bool      `json:"paused,omitempty"`
	// Inherit lists fields to reset to the defaults: "schedule", "retention".
	Inherit []string `json:"inherit,omitempty"`
}

// SourceRewriteParams removes Excludes (absolute paths) from every
// existing snapshot of source ID. DryRun only counts the snapshots that
// would change.
type SourceRewriteParams struct {
	ID       string   `json:"id"`
	Excludes []string `json:"excludes"`
	DryRun   bool     `json:"dry_run,omitempty"`
}

// SourceRewriteResult: for a dry run, how many snapshots would change;
// otherwise the queued job.
type SourceRewriteResult struct {
	Snapshots int  `json:"snapshots"`
	Job       *Job `json:"job,omitempty"`
}

type SourceResult struct {
	Source Source `json:"source"`
}

type SourceRemoveParams struct {
	ID            string `json:"id"`
	KeepSnapshots bool   `json:"keep_snapshots"`
}

type SnapshotSetParams struct {
	ID     string  `json:"id"`
	Note   *string `json:"note,omitempty"`
	Pinned *bool   `json:"pinned,omitempty"`
}

type SnapshotResult struct {
	Snapshot Snapshot `json:"snapshot"`
}

// RepositoryInfo is the repository as the Repository popup shows it.
// Credentials are described by where they come from, never sent.
type RepositoryInfo struct {
	Location        string     `json:"location"`
	PasswordFrom    string     `json:"password_from"` // "Keychain charm-bak-repo / studio"
	AWSFrom         string     `json:"aws_from"`      // "Keychain charm-bak-aws / studio", or "" when not S3 or from the environment
	StoredBytes     int64      `json:"stored_bytes"`
	LogicalBytes    int64      `json:"logical_bytes"`
	LastPrune       *Run       `json:"last_prune"`
	LastCheck       *Run       `json:"last_check"`
	NextMaintenance *time.Time `json:"next_maintenance"` // when prune + check are next due
}

// Defaults are the schedule and retention sources inherit. A nil
// schedule means manual.
type Defaults struct {
	Schedule  *Schedule `json:"schedule"`
	Retention Retention `json:"retention"`
	// Excludes are patterns every source skips, before its own
	// (restic --exclude), e.g. "*.log".
	Excludes []string `json:"excludes"`
}

// RepositorySetParams sets up a daemon's repository with its secrets,
// which it keeps in owner-only files in its state directory. Sent by the
// controller's daemon to a remote over the paired connection (during
// repository_setup for that host); clients never hold these secrets. Not on macOS, where
// secrets live in the Keychain.
type RepositorySetParams struct {
	Location string `json:"location"` // "s3:s3.amazonaws.com/bucket/host"
	Password string `json:"password"` // the repository password
	AWSKey   string `json:"aws_key"`  // "KEYID:SECRET", "" when not S3 (before env)
	Init     bool   `json:"init"`     // create the repository first
	// Env is the backend's credentials as environment variables
	// (internal/backends); replaces aws_key.
	Env map[string]string `json:"env,omitempty"`
}

// SelfUpdateParams carries a new daemon executable in pieces, in order:
// Offset 0 starts an update, the piece that reaches Size finishes it.
// The daemon checks the whole file against SHA256, puts it in place of
// its own executable and restarts (its service manager starts it again).
type SelfUpdateParams struct {
	SHA256 string `json:"sha256"` // hex, of the whole file
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"` // base64 in JSON; at most 1 MiB a piece
	// Bundle: the file is a tar.gz of the whole app bundle (hello's
	// bundle), not the bare executable. Set on every piece.
	Bundle bool `json:"bundle,omitempty"`
}

type SelfUpdateResult struct {
	Received   int64 `json:"received"`   // bytes so far
	Restarting bool  `json:"restarting"` // the last piece: verified, installed, restarting
}

// Probe states: what repository_probe found at a location.
const (
	ProbeEmpty    = "empty"    // nothing there yet: it can be created
	ProbeOpens    = "opens"    // a repository, and the password opens it
	ProbePassword = "password" // a repository the password doesn't open (or none was given)
	ProbeFailed   = "failed"   // couldn't tell: credentials, network, a drive; see message
)

// RepositoryProbeParams asks a daemon what's at a location, using these
// credentials, before anything is saved. Sent with the request's host,
// so it runs on the machine that will use the repository (its network,
// its drives, its SSH keys).
type RepositoryProbeParams struct {
	Location string            `json:"location"`
	Env      map[string]string `json:"env,omitempty"`      // the backend's credentials
	Password string            `json:"password,omitempty"` // to open an existing repository
}

// RepositoryProbeResult is what the probe found.
type RepositoryProbeResult struct {
	State   string `json:"state"` // Probe*
	Message string `json:"message,omitempty"`
}

// RepositorySetupParams sets up a machine's repository from the setup
// flow, on this machine's daemon (the controller): for itself, or for a
// paired remote, which it sends the secrets to. Init creates it (a new
// password is made unless one is given); otherwise password opens one
// already there. Refused when the machine already has a repository.
type RepositorySetupParams struct {
	Host     string            `json:"host"` // "" or this machine's name for itself
	Location string            `json:"location"`
	Env      map[string]string `json:"env,omitempty"`
	Password string            `json:"password,omitempty"`
	Init     bool              `json:"init"`
}

// RepositorySetupResult is the new repository, and, when one was made,
// its password: shown once so the user can keep a copy somewhere other
// than this machine. Without it the backups can't be read if the
// machine is lost.
type RepositorySetupResult struct {
	Repository *RepositoryInfo `json:"repository"`
	Password   string          `json:"password,omitempty"`
}

// AWSStatusResult says what the controller can do in AWS by itself: CLI
// is whether the aws command is installed (aws_keys runs it), Manager the
// key manager aws_keys set up, if any.
type AWSStatusResult struct {
	CLI     bool        `json:"cli"`
	Manager *AWSManager `json:"manager,omitempty"`
}

// AWSManager is the controller's key manager: an IAM user that can only
// make users at path /charm-bak/ whose permissions boundary limits them
// to one of Buckets (every bucket a sign-in set it up for; Bucket is the
// latest, the one offered first). Its key stays in the controller's
// Keychain.
type AWSManager struct {
	Account string   `json:"account"`
	Bucket  string   `json:"bucket"`
	Buckets []string `json:"buckets,omitempty"`
	Region  string   `json:"region"`
}

// Serves reports whether the key manager can make keys for bucket.
func (m AWSManager) Serves(bucket string) bool {
	if bucket == m.Bucket {
		return true
	}
	for _, b := range m.Buckets {
		if b == bucket {
			return true
		}
	}
	return false
}

// AWSKeysParams asks the controller for a key that can only reach
// Bucket/Host/. SignIn signs in with the browser (aws login) first, makes
// the bucket if it isn't there and sets up the key manager; otherwise the
// key manager makes the key. Bucket "" with SignIn means
// charm-bak-<account id>.
type AWSKeysParams struct {
	Host   string `json:"host"`
	Bucket string `json:"bucket,omitempty"`
	Region string `json:"region,omitempty"`
	SignIn bool   `json:"sign_in"`
}

// AWSKeysResult is the key, as the backend's environment, for the setup
// flow to check and set up with like a typed one.
type AWSKeysResult struct {
	Bucket        string            `json:"bucket"`
	Region        string            `json:"region"`
	Env           map[string]string `json:"env"`
	BucketCreated bool              `json:"bucket_created,omitempty"`
}

type RepositoryResult struct {
	Repository *RepositoryInfo `json:"repository"` // null before setup
	Defaults   Defaults        `json:"defaults"`
}

// DefaultsSetParams changes the defaults; omitted fields stay. A schedule
// with every 0 means manual.
type DefaultsSetParams struct {
	Schedule  *Schedule  `json:"schedule,omitempty"`
	Retention *Retention `json:"retention,omitempty"`
	Excludes  *[]string  `json:"excludes,omitempty"` // [] clears them
}

// Host is a machine whose daemon this one can reach: itself, or a paired
// remote it relays to.
type Host struct {
	Name     string     `json:"name"`
	Local    bool       `json:"local"`
	Address  string     `json:"address"` // "pantry.lan:7457"; "" for the local daemon
	State    string     `json:"state"`   // "ok" or "unreachable"
	Error    string     `json:"error"`   // why it's unreachable
	LastSeen *time.Time `json:"last_seen"`
}

// Host states.
const (
	HostOK          = "ok"
	HostUnreachable = "unreachable"
)

// PairStartParams starts pairing a new remote called Name.
type PairStartParams struct {
	Name string `json:"name"`
}

// HostRepairParams asks for a command that reinstalls paired remote
// Name's daemon there (result: PairStartResult), keeping its state.
type HostRepairParams struct {
	Name string `json:"name"`
}

// PairStartResult is what to run on the remote: Command, as root, before
// Expires. The code in it works once.
type PairStartResult struct {
	Command string `json:"command"`
	// WindowsCommand is the same for a Windows remote, pasted in an
	// administrator PowerShell.
	WindowsCommand string    `json:"windows_command,omitempty"`
	Expires        time.Time `json:"expires"`
}

type HostsResult struct {
	Hosts []Host `json:"hosts"` // the local daemon first, then remotes by name
}

// HostSetParams changes where a paired remote is reached (its address
// changed); the pairing (its key) stays. Address is "host" or
// "host:port" (default port 7457).
type HostSetParams struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type HostResult struct {
	Host Host `json:"host"`
}

type DefaultsResult struct {
	Defaults Defaults `json:"defaults"`
}

type HistoryParams struct {
	Since time.Time `json:"since"`
}

type HistoryResult struct {
	Runs []Run `json:"runs"`
}

// RunParams names one run from history, for its details.
type RunParams struct {
	Job string `json:"job"`
}

// RunResult is a run with what history leaves out: every file an
// incomplete backup couldn't read, with the reason (up to 1000).
type RunResult struct {
	Run          Run      `json:"run"`
	SkippedFiles []string `json:"skipped_files"`
}

// Empty is the result of methods that return nothing.
type Empty struct{}

// Event names.
const (
	EventJobStarted    = "job_started"
	EventProgress      = "progress"
	EventJobFinished   = "job_finished"
	EventSourceChanged = "source_changed"
	EventStateChanged  = "state_changed"
	EventHostChanged   = "host_changed"
	EventAWSStep       = "aws_step"
)

// AWS steps, in order, as aws_keys gets through them.
const (
	AWSStepSignedIn = "signed_in" // the browser sign-in finished
	AWSStepBucket   = "bucket"    // the bucket is there
	AWSStepManager  = "manager"   // the key manager and its boundary are set up
	AWSStepUser     = "user"      // the machine's user and key are made
	AWSStepKeyWorks = "key_works" // AWS accepts the new key
)

// Event is any pushed event: Name says which of the other fields is set.
// On the wire the payload's fields sit beside "event" in one object.
type Event struct {
	Name string
	// Host is the remote the event came from; empty for the local daemon.
	Host          string
	JobStarted    *JobStartedEvent
	Progress      *Progress
	JobFinished   *JobFinishedEvent
	SourceChanged *SourceChangedEvent
	StateChanged  *StateChangedEvent
	HostChanged   *HostChangedEvent
	AWSStep       *AWSStepEvent
}

// AWSStepEvent says how far aws_keys has got making For's key, so the
// setup flow can say so while it waits. Account is set from signed_in.
type AWSStepEvent struct {
	For     string `json:"for"`
	Step    string `json:"step"`
	Account string `json:"account,omitempty"`
	Bucket  string `json:"bucket,omitempty"`
}

// HostChangedEvent says a paired remote became reachable or stopped
// answering.
type HostChangedEvent struct {
	Host Host `json:"host_info"`
}

type JobStartedEvent struct {
	Job Job `json:"job"`
}

// Progress is restic's status line, relabeled.
type Progress struct {
	JobID      string  `json:"job_id"`
	Percent    float64 `json:"percent"`
	FilesDone  int64   `json:"files_done"`
	TotalFiles int64   `json:"total_files"`
	BytesDone  int64   `json:"bytes_done"`
	TotalBytes int64   `json:"total_bytes"`
	Current    string  `json:"current"`
}

type JobFinishedEvent struct {
	Run Run `json:"run"`
}

type SourceChangedEvent struct {
	Source Source `json:"source"`
}

type StateChangedEvent struct {
	State string `json:"state"`
	Job   *Job   `json:"job"`
}

// payload returns a pointer to the field Name selects, allocating it, or
// nil for an unknown event.
func (e *Event) payload() any {
	switch e.Name {
	case EventJobStarted:
		if e.JobStarted == nil {
			e.JobStarted = &JobStartedEvent{}
		}
		return e.JobStarted
	case EventProgress:
		if e.Progress == nil {
			e.Progress = &Progress{}
		}
		return e.Progress
	case EventJobFinished:
		if e.JobFinished == nil {
			e.JobFinished = &JobFinishedEvent{}
		}
		return e.JobFinished
	case EventSourceChanged:
		if e.SourceChanged == nil {
			e.SourceChanged = &SourceChangedEvent{}
		}
		return e.SourceChanged
	case EventStateChanged:
		if e.StateChanged == nil {
			e.StateChanged = &StateChangedEvent{}
		}
		return e.StateChanged
	case EventHostChanged:
		if e.HostChanged == nil {
			e.HostChanged = &HostChangedEvent{}
		}
		return e.HostChanged
	case EventAWSStep:
		if e.AWSStep == nil {
			e.AWSStep = &AWSStepEvent{}
		}
		return e.AWSStep
	}
	return nil
}

// UnmarshalJSON reads "event", then the payload it names. Unknown events
// decode to just a Name, so newer apps don't break older clients.
func (e *Event) UnmarshalJSON(b []byte) error {
	var head struct {
		Name string `json:"event"`
		Host string `json:"host"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	*e = Event{Name: head.Name, Host: head.Host}
	if p := e.payload(); p != nil {
		return json.Unmarshal(b, p)
	}
	return nil
}

// MarshalJSON writes the payload's fields with "event" beside them.
func (e Event) MarshalJSON() ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if p := e.payload(); p != nil {
		b, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &fields); err != nil {
			return nil, err
		}
	}
	name, _ := json.Marshal(e.Name)
	fields["event"] = name
	if e.Host != "" {
		fields["host"], _ = json.Marshal(e.Host)
	}
	return json.Marshal(fields)
}

// HostRemoveParams removes a paired remote, on the controller: the remote
// is told to uninstall itself (its sources, schedule, service and files
// go), then it's unpaired. Its backups stay unless DeleteBackups, which
// needs the remote to answer.
type HostRemoveParams struct {
	Name          string `json:"name"`
	DeleteBackups bool   `json:"delete_backups,omitempty"`
}

// HostRemoveResult says whether the remote uninstalled itself; when it
// didn't answer it's unpaired anyway and Message says what to run there.
type HostRemoveResult struct {
	Uninstalled bool   `json:"uninstalled"`
	Message     string `json:"message,omitempty"`
}

// UninstallParams is sent by the controller to a remote it's removing.
// The remote replies, then (deleting every snapshot first if asked)
// removes its service, executable and state directory, and exits.
type UninstallParams struct {
	DeleteBackups bool `json:"delete_backups,omitempty"`
}
