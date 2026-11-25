// Package backup is what the TUI reads: the Repo interface over the
// protocol's types (internal/protocol), implemented by the socket client
// (internal/client) and by Fake for tests and offline work. No charm
// imports and no human-facing wording here; internal/ui decides how things
// look.
package backup

import (
	"context"
	"path"
	"strings"
	"time"

	"charm-bak/internal/protocol"
)

// Repo is everything the TUI asks of the app.
type Repo interface {
	Status(ctx context.Context) (protocol.StatusResult, error)
	// Hosts is the daemon's own machine first, then its paired remotes.
	Hosts(ctx context.Context) ([]protocol.Host, error)
	// ForHost is a Repo whose calls go to host's daemon (relayed by this
	// one); this Repo itself for its own host or "".
	ForHost(host string) Repo
	Sources(ctx context.Context) ([]protocol.Source, error)
	// Snapshots returns a source's snapshots, newest first.
	// protocol.UntaggedSource lists those taken outside charm-bak.
	Snapshots(ctx context.Context, source string) ([]protocol.Snapshot, error)
	// List returns one directory's direct children, directories first,
	// then by name.
	List(ctx context.Context, snapshot, dir string) ([]protocol.Entry, error)
	// Read returns the first max bytes of a file, for previews.
	// protocol.LiveSnapshot reads the file as it is on disk now.
	Read(ctx context.Context, snapshot, file string, max int) (protocol.DumpResult, error)
	// Diff compares one snapshot directory with the same path on disk.
	Diff(ctx context.Context, snapshot, dir string) ([]protocol.DiffEntry, error)
	// Changes is what snapshot has that base, an earlier snapshot of the
	// same source, doesn't: paths added, removed and changed.
	Changes(ctx context.Context, snapshot, base string) (protocol.ChangesResult, error)

	// Changes. The app runs them; the TUI only asks.
	AddSource(ctx context.Context, p protocol.SourceAddParams) (protocol.Source, error)
	SetSource(ctx context.Context, p protocol.SourceSetParams) (protocol.Source, error)
	// RewriteSource takes paths out of a source's existing snapshots
	// (DryRun: counts them).
	RewriteSource(ctx context.Context, p protocol.SourceRewriteParams) (protocol.SourceRewriteResult, error)
	RemoveSource(ctx context.Context, id string, keepSnapshots bool) error
	BackupNow(ctx context.Context, source string) (protocol.Job, error)
	Restore(ctx context.Context, p protocol.RestoreParams) (protocol.Job, error)
	// SetSnapshot changes a snapshot's note or pin and returns it.
	SetSnapshot(ctx context.Context, p protocol.SnapshotSetParams) (protocol.Snapshot, error)
	// Repository describes the repository and the defaults sources inherit.
	Repository(ctx context.Context) (protocol.RepositoryResult, error)
	// SetDefaults changes the defaults; omitted fields stay.
	SetDefaults(ctx context.Context, p protocol.DefaultsSetParams) (protocol.Defaults, error)
	// ProbeRepository says what's at a location with these credentials,
	// on this Repo's host (its network, drives and keys), saving nothing.
	ProbeRepository(ctx context.Context, p protocol.RepositoryProbeParams) (protocol.RepositoryProbeResult, error)
	// SetupRepository sets up p.Host's repository (this daemon's own, or
	// a paired remote's). Call it on the controller, not a ForHost Repo.
	SetupRepository(ctx context.Context, p protocol.RepositorySetupParams) (protocol.RepositorySetupResult, error)
	// PairStart starts pairing a new machine: the command to run on it.
	PairStart(ctx context.Context, p protocol.PairStartParams) (protocol.PairStartResult, error)
	// HostRepair: the command that reinstalls a paired remote's daemon.
	HostRepair(ctx context.Context, name string) (protocol.PairStartResult, error)
	// OpenAccessSettings opens Full Disk Access in System Settings on
	// this Repo's host (a Mac), with charm-bak shown in Finder.
	OpenAccessSettings(ctx context.Context) error
	// AWSStatus says whether the controller can make AWS keys itself (the
	// aws command, a key manager). Call it on the controller.
	AWSStatus(ctx context.Context) (protocol.AWSStatusResult, error)
	// AWSKeys makes p.Host a key that only reaches its folder in the
	// bucket, signing in with the browser first when p.SignIn. It can
	// take minutes (the person signs in). Call it on the controller.
	AWSKeys(ctx context.Context, p protocol.AWSKeysParams) (protocol.AWSKeysResult, error)
	// RemoveHost removes a paired remote altogether: it uninstalls
	// itself, then it's unpaired. Call it on the controller.
	RemoveHost(ctx context.Context, p protocol.HostRemoveParams) (protocol.HostRemoveResult, error)
	// SetHostAddress changes where a paired remote is reached.
	SetHostAddress(ctx context.Context, p protocol.HostSetParams) (protocol.Host, error)
	Cancel(ctx context.Context, job string) error
	// History is the runs that ended since since, oldest first.
	History(ctx context.Context, since time.Time) ([]protocol.Run, error)
	// RunDetail is one run and the files it couldn't read.
	RunDetail(ctx context.Context, job string) (protocol.RunResult, error)

	// Events delivers the app's events until ctx ends or the app goes
	// away; the channel closes then.
	Events(ctx context.Context) (<-chan protocol.Event, error)
}

// Root is where a snapshot's tree opens: its one path, or the deepest
// directory all its paths share. restic stores absolute paths, so the
// empty directories above a source are skipped this way.
func Root(paths []string) string {
	if len(paths) == 0 {
		return "/"
	}
	if len(paths) == 1 {
		return path.Clean(paths[0])
	}
	common := strings.Split(path.Clean(paths[0]), "/")
	for _, p := range paths[1:] {
		parts := strings.Split(path.Clean(p), "/")
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	if len(common) <= 1 {
		return "/"
	}
	return strings.Join(common, "/")
}

// Join is a child's path in a snapshot.
func Join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
