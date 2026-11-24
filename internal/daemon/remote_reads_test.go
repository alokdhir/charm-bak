package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// realPantry is a pair whose remote has a real restic repository, set up
// through the controller (which keeps its copy of the secrets), with one
// source backed up once.
func realPantry(t *testing.T) (p *remotePair, keys *fakeKeychain, source protocol.Source, data string) {
	t.Helper()
	needRestic(t)
	p = newRemotePair(t)
	p.pantry.store.db.Exec("DELETE FROM settings WHERE key = 'repository'")
	keys = &fakeKeychain{items: map[string]string{}}
	p.mac.secrets = keys
	p.mac.makeEngine = ResticEngines
	p.pantry.SecretDir = filepath.Join(t.TempDir(), "state")
	p.pantry.makeEngine = ResticEngines
	waitConnected(t, p)

	loc := filepath.Join(t.TempDir(), "pantry-repo")
	call[protocol.RepositorySetupResult](t, p.tui, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Host: "pantry", Location: loc, Init: true})
	data = filepath.Join(t.TempDir(), "videos")
	if err := os.MkdirAll(filepath.Join(data, "kept"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(data, "hello.txt"), []byte("hello from pantry\n"), 0o644)
	os.WriteFile(filepath.Join(data, "kept", "deep.txt"), []byte("deep\n"), 0o644)
	pantry := p.tui.WithHost("pantry")
	source = call[protocol.SourceResult](t, pantry, protocol.MethodSourceAdd, protocol.SourceAddParams{Name: "videos", Paths: []string{data}}).Source
	call[protocol.JobResult](t, pantry, protocol.MethodBackupNow, protocol.BackupNowParams{Source: source.ID})
	p.pantry.WaitIdle()
	return p, keys, source, data
}

// pantryOff points the controller at an address nothing listens on and
// waits until it knows pantry isn't answering.
func pantryOff(t *testing.T, p *remotePair) {
	t.Helper()
	call[protocol.HostResult](t, p.tui, protocol.MethodHostSet, protocol.HostSetParams{Name: "pantry", Address: "127.0.0.1:1"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := call[protocol.HostsResult](t, p.tui, protocol.MethodHosts, nil).Hosts[1]
		if h.State == protocol.HostUnreachable && h.Error != "connecting" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pantry still %+v", h)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A remote's backups are read by the controller from the remote's
// repository: with the remote switched off, its sources, snapshots (pin
// and note included), folders and files still answer, and a restore
// lands on the controller.
func TestRemoteReadsWithoutTheRemote(t *testing.T) {
	p, _, source, data := realPantry(t)
	pantry := p.tui.WithHost("pantry")

	snaps := call[protocol.SnapshotsResult](t, pantry, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: source.ID}).Snapshots
	if len(snaps) != 1 {
		t.Fatalf("snapshots while up: %+v", snaps)
	}
	id := snaps[0].ID
	note := "before the move"
	on := true
	call[protocol.SnapshotResult](t, pantry, protocol.MethodSnapshotSet, protocol.SnapshotSetParams{ID: id, Note: &note, Pinned: &on})
	// Listing again while it's up asks pantry for its pins and notes,
	// and saves them.
	snaps = call[protocol.SnapshotsResult](t, pantry, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: source.ID}).Snapshots
	if len(snaps) != 1 || !snaps[0].Pinned || snaps[0].Note != note {
		t.Fatalf("pin and note while up: %+v", snaps)
	}
	if !slices.Contains(snaps[0].KeptBy, "pinned") {
		t.Errorf("kept by %v, want pinned among them", snaps[0].KeptBy)
	}
	// The saved sources are there once the controller has synced.
	deadline := time.Now().Add(5 * time.Second)
	for len(p.mac.remoteSources("pantry")) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("pantry's sources never saved")
		}
		time.Sleep(20 * time.Millisecond)
	}

	pantryOff(t, p)
	p.pantrySrv.Close()

	if got := call[protocol.SourcesResult](t, pantry, protocol.MethodSources, nil).Sources; len(got) != 1 || got[0].ID != source.ID || got[0].Host != "pantry" {
		t.Errorf("sources while off: %+v", got)
	}
	if got := call[protocol.RepositoryResult](t, pantry, protocol.MethodRepository, nil); got.Repository == nil || got.Repository.Location == "" {
		t.Errorf("repository while off: %+v", got)
	}
	if e := callErr(pantry, protocol.MethodStatus, nil); e != protocol.ErrHostUnreachable {
		t.Errorf("status while off: %q", e)
	}
	if e := callErr(pantry, protocol.MethodBackupNow, protocol.BackupNowParams{Source: source.ID}); e != protocol.ErrHostUnreachable {
		t.Errorf("backup_now while off: %q", e)
	}
	// Dropping the lists proves the next ones come from restic run here.
	p.mac.mu.Lock()
	p.mac.dropRemoteListsLocked("pantry")
	p.mac.mu.Unlock()
	snaps = call[protocol.SnapshotsResult](t, pantry, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: source.ID}).Snapshots
	if len(snaps) != 1 || snaps[0].ID != id || !snaps[0].Pinned || snaps[0].Note != note {
		t.Fatalf("snapshots while off: %+v", snaps)
	}
	ls := call[protocol.LsResult](t, pantry, protocol.MethodLs, protocol.LsParams{Snapshot: id, Path: data})
	if len(ls.Entries) != 2 || ls.Entries[0].Name != "kept" || ls.Entries[1].Name != "hello.txt" {
		t.Errorf("ls while off: %+v", ls.Entries)
	}
	dump := call[protocol.DumpResult](t, pantry, protocol.MethodDump, protocol.DumpParams{Snapshot: id, Path: filepath.Join(data, "hello.txt"), Max: 100})
	if string(dump.Data) != "hello from pantry\n" {
		t.Errorf("dump while off: %q", dump.Data)
	}
	// Its live disk is the remote's to read.
	if e := callErr(pantry, protocol.MethodLs, protocol.LsParams{Snapshot: protocol.LiveSnapshot, Path: data}); e != protocol.ErrHostUnreachable {
		t.Errorf("live ls while off: %q", e)
	}

	// Restore: onto the controller, from pantry's repository.
	if e := callErr(p.tui, protocol.MethodRestore, protocol.RestoreParams{From: "pantry", Snapshot: id, Paths: []string{data}, Target: protocol.RestoreOriginal, Overwrite: "always"}); e != protocol.ErrBadRequest {
		t.Errorf("restore from pantry to the original place: %q", e)
	}
	if e := callErr(p.tui, protocol.MethodRestore, protocol.RestoreParams{From: "nobody", Snapshot: id, Paths: []string{data}, Target: t.TempDir(), Overwrite: "always"}); e != protocol.ErrNotFound {
		t.Errorf("restore from an unknown host: %q", e)
	}
	target := filepath.Join(t.TempDir(), "restored")
	call[protocol.JobResult](t, p.tui, protocol.MethodRestore, protocol.RestoreParams{From: "pantry", Snapshot: id, Paths: []string{filepath.Join(data, "hello.txt")}, Target: target, Overwrite: "always"})
	p.mac.WaitIdle()
	if b, err := os.ReadFile(filepath.Join(target, "hello.txt")); err != nil || string(b) != "hello from pantry\n" {
		t.Errorf("restored here: %q %v", b, err)
	}
	// Relayed restore (onto pantry itself) still needs pantry.
	if e := callErr(pantry, protocol.MethodRestore, protocol.RestoreParams{Snapshot: id, Paths: []string{data}, Target: target, Overwrite: "always"}); e != protocol.ErrHostUnreachable {
		t.Errorf("restore on pantry while off: %q", e)
	}

	// Unpairing drops what was saved about it.
	call[protocol.HostRemoveResult](t, p.tui, protocol.MethodHostRemove, protocol.HostRemoveParams{Name: "pantry"})
	if _, ok, _ := p.mac.store.RemoteCache("pantry", cacheSources); ok {
		t.Error("pantry's saved sources outlived the pairing")
	}
	if _, ok, _ := p.mac.store.RemoteCache("pantry", cacheMeta+source.ID); ok {
		t.Error("pantry's saved notes outlived the pairing")
	}
	if p.mac.remoteLocation("pantry") != "" {
		t.Error("pantry's saved repository outlived the pairing")
	}
}

// A backup finishing on the remote drops the controller's cached list,
// so the new snapshot shows without waiting out the cache.
func TestRemoteListFollowsItsJobs(t *testing.T) {
	p, _, source, data := realPantry(t)
	pantry := p.tui.WithHost("pantry")
	list := func() []protocol.Snapshot {
		return call[protocol.SnapshotsResult](t, pantry, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: source.ID}).Snapshots
	}
	if n := len(list()); n != 1 {
		t.Fatalf("%d snapshots", n)
	}
	os.WriteFile(filepath.Join(data, "more.txt"), []byte("more\n"), 0o644)
	call[protocol.JobResult](t, pantry, protocol.MethodBackupNow, protocol.BackupNowParams{Source: source.ID})
	p.pantry.WaitIdle()
	deadline := time.Now().Add(5 * time.Second)
	for len(list()) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the second snapshot never showed: %d", len(list()))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readEngine answers Snapshots with one snapshot named for whoever read.
type readEngine struct {
	Engine
	who string
}

func (e *readEngine) Snapshots(context.Context, string) ([]EngineSnapshot, error) {
	return []EngineSnapshot{{ID: e.who, ShortID: e.who}}, nil
}

func (e *readEngine) KeepReasons(context.Context, string, protocol.Retention, map[string]bool) (map[string][]string, error) {
	return nil, nil
}

func (e *readEngine) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }

// Tree is the warm-up that follows a snapshot listing.
func (e *readEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}

// Without the remote's password here the controller can't open its
// repository, so its snapshots are relayed as before; once it holds the
// password (and knows the location), it reads them itself.
func TestRemoteReadsRelayWithoutSecrets(t *testing.T) {
	p := newRemotePair(t, func(mac, pantry *Core) {
		mac.makeEngine = func(RepositoryConfig) Engine { return &readEngine{who: "read-by-studio"} }
		pantry.makeEngine = func(RepositoryConfig) Engine { return &readEngine{who: "read-by-pantry"} }
	})
	keys := &fakeKeychain{items: map[string]string{}}
	p.mac.secrets = keys
	waitConnected(t, p)
	pantry := p.tui.WithHost("pantry")
	src := call[protocol.SourceResult](t, pantry, protocol.MethodSourceAdd, protocol.SourceAddParams{Name: "videos", Paths: []string{"/srv/videos"}}).Source
	who := func() string {
		t.Helper()
		got := call[protocol.SnapshotsResult](t, pantry, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: src.ID}).Snapshots
		if len(got) != 1 {
			t.Fatalf("snapshots %+v", got)
		}
		return got[0].ID
	}
	// The location arrives with the first sync; still no password.
	deadline := time.Now().Add(5 * time.Second)
	for p.mac.remoteLocation("pantry") != "pantry-repo" {
		if time.Now().After(deadline) {
			t.Fatal("pantry's repository never saved")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := who(); got != "read-by-pantry" {
		t.Errorf("without its password here: %s", got)
	}
	keys.add(repoKeychainService, "pantry", "pw")
	p.mac.forgetRemoteView("pantry") // as setting it up through the controller does
	if got := who(); got != "read-by-studio" {
		t.Errorf("with its password here: %s", got)
	}
}

// snapshot_meta is a daemon's own pins, notes and recalculated sizes.
func TestSnapshotMeta(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	core := NewCore("test", store, func(RepositoryConfig) Engine { return &readEngine{who: "x"} }, nil)
	c, _ := serve(t, core)
	store.SetPinned("aaa", "s1", true)
	store.SetPinned("bbb", "s2", true)
	store.SetNote("aaa", "first")
	store.SetAdded("ccc", 42)
	got := call[protocol.SnapshotMetaResult](t, c, protocol.MethodSnapshotMeta, protocol.SnapshotMetaParams{Source: "s1"})
	if len(got.Pinned) != 1 || got.Pinned[0] != "aaa" || got.Notes["aaa"] != "first" || got.Added["ccc"] != 42 {
		t.Errorf("meta %+v", got)
	}
}
