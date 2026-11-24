package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// Tests for the fixes from the 2026-11-24 review.

func TestLiteralPattern(t *testing.T) {
	for in, want := range map[string]string{
		"/a/plain name":   "/a/plain name",
		"/a/[2024] Taxes": `/a/\[2024] Taxes`,
		"/a/a*b":          `/a/a\*b`,
		"/a/q?z":          `/a/q\?z`,
		`/a/back\slash`:   `/a/back\\slash`,
		"/a/**":           `/a/\*\*`,
		"/a/br{a,b} $x !": "/a/br{a,b} $x !",
	} {
		if got := literalPattern(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Names with glob characters: rewrite takes out, and restore brings
// back, exactly the one named, never the sibling its name would match as
// a pattern (real restic).
func TestResticLiteralNames(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	// Each odd name beside the plain name it matches as a glob.
	pairs := [][2]string{
		{"[2024] Taxes", "2 Taxes"},
		{"a*b", "axxb"},
		{"q?z", "qyz"},
		{`back\slash`, "backslash"},
		{"img[1].jpg", "img1.jpg"},
	}
	for _, p := range pairs {
		for _, n := range p {
			if err := os.MkdirAll(filepath.Join(fx.data, n), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(fx.data, n, "f.txt"), []byte(n), 0o644)
		}
	}
	spec := BackupSpec{Tag: "src:g", Paths: []string{fx.data}}
	if _, err := e.Backup(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	snaps, _ := e.Snapshots(ctx, "src:g")
	if len(snaps) != 1 {
		t.Fatalf("snapshots %+v", snaps)
	}

	// Restore each odd name alone: only it lands.
	for i, p := range pairs {
		target := filepath.Join(fx.root, "out", string(rune('a'+i)))
		if err := e.Restore(ctx, snaps[0].ID, []string{filepath.Join(fx.data, p[0])}, target, protocol.OverwriteAlways, nil); err != nil {
			t.Fatalf("restore %q: %v", p[0], err)
		}
		got, _ := os.ReadDir(target)
		if len(got) != 1 || got[0].Name() != p[0] {
			t.Errorf("restoring %q landed %v", p[0], entryDirNames(got))
		}
	}

	// Remove each odd name from the snapshot: the plain siblings stay.
	var gone []string
	for _, p := range pairs {
		gone = append(gone, filepath.Join(fx.data, p[0]))
	}
	if n, err := e.Rewrite(ctx, "src:g", gone, false); err != nil || n != 1 {
		t.Fatalf("rewrite: %d %v", n, err)
	}
	snaps, _ = e.Snapshots(ctx, "src:g")
	entries, err := e.List(ctx, snaps[0].ID, fx.data)
	if err != nil {
		t.Fatal(err)
	}
	names := entryNames(entries)
	for _, p := range pairs {
		if slices.Contains(names, p[0]) {
			t.Errorf("%q is still in the snapshot", p[0])
		}
		if !slices.Contains(names, p[1]) {
			t.Errorf("%q was removed with %q", p[1], p[0])
		}
	}
}

func entryDirNames(es []os.DirEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// failingRewrite replaces old1 by new1, then fails (as a cancelled or
// interrupted restic rewrite does part-way).
type failingRewrite struct {
	rewriteEngine
	retained map[string]bool // the pins the last ApplyRetention got
}

func (e *failingRewrite) Rewrite(ctx context.Context, tag string, excludes []string, dry bool) (int, error) {
	e.rewriteEngine.Rewrite(ctx, tag, excludes, dry)
	return 0, &EngineError{Kind: ErrCancelled, Message: "interrupted"}
}

// A rewrite that stops part-way still moves the notes and pins of what
// it replaced: a pin left on the old ID wouldn't protect the new one.
func TestRewriteFailureStillMovesPins(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	src, _ := store.AddSource(StoredSource{Name: "s", Paths: []string{"/a"}}, time.Now())
	store.SetNote("old1", "keep me")
	store.SetPinned("old1", src.ID, true)
	eng := &failingRewrite{}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	core.keepCache[src.Tag()] = map[string][]string{"old1": {"pinned"}}
	c, _ := serve(t, core)
	call[protocol.SourceRewriteResult](t, c, protocol.MethodSourceRewrite, protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{"/a/x"}})
	core.WaitIdle()
	if pins, _ := store.Pinned(src.ID); !pins["new1"] || pins["old1"] {
		t.Errorf("pins %v", pins)
	}
	if n, _ := store.Note("new1"); n != "keep me" {
		t.Errorf("note %q", n)
	}
	if eng.pruned {
		t.Error("pruned after a rewrite that failed")
	}
	runs, _ := store.Runs(time.Unix(0, 0))
	if len(runs) != 1 || runs[0].Status != protocol.RunCancelled {
		t.Errorf("runs %+v", runs)
	}
	core.mu.Lock()
	_, cached := core.keepCache[src.Tag()]
	core.mu.Unlock()
	if cached {
		t.Error("keep reasons for the replaced IDs are still cached")
	}
}

// Weekly cleanup: a week after the last good one, at night, or at any
// hour once a day overdue; a failed one is retried after an hour, not a
// week.
func TestMaintenanceDue(t *testing.T) {
	store := memStore(t)
	core := NewCore("test", store, nil, nil)
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.Local) }
	if !core.maintenanceDue(at(1, 14)) {
		t.Error("never cleaned: not due")
	}
	store.AddRun(protocol.Run{Job: "p1", Kind: protocol.KindPrune, Started: at(1, 3), Ended: at(1, 3), Status: protocol.RunOK})
	for _, c := range []struct {
		day, hour int
		want      bool
		why       string
	}{
		{7, 3, false, "six days on"},
		{8, 2, false, "an hour short of a week"},
		{8, 4, true, "a week on, at night"},
		{8, 14, false, "a week on, mid-day: waits for the night"},
		{9, 2, true, "the next night"},
		{9, 4, true, "more than a day overdue"},
		{9, 14, true, "more than a day overdue, mid-day"},
	} {
		if got := core.maintenanceDue(at(c.day, c.hour)); got != c.want {
			t.Errorf("%s: due %v", c.why, got)
		}
	}
	// A failed try that night: again within the hour? No. After it? Yes.
	store.AddRun(protocol.Run{Job: "p2", Kind: protocol.KindPrune, Started: at(8, 4), Ended: at(8, 4), Status: protocol.RunFailed})
	if core.maintenanceDue(at(8, 4).Add(30 * time.Minute)) {
		t.Error("retried half an hour after failing")
	}
	if !core.maintenanceDue(at(9, 1)) {
		t.Error("a failed cleanup wasn't retried the next night")
	}
	// A good one resets the week.
	store.AddRun(protocol.Run{Job: "p3", Kind: protocol.KindPrune, Started: at(9, 1), Ended: at(9, 1), Status: protocol.RunOK})
	if core.maintenanceDue(at(10, 3)) {
		t.Error("due a day after a good cleanup")
	}
}

// ownEngine is a repository two machines share.
type ownEngine struct {
	Engine
	mu        sync.Mutex
	snaps     []EngineSnapshot
	forgot    []string
	pruned    bool
	forgetErr error
}

func (e *ownEngine) Snapshots(_ context.Context, tag string) ([]EngineSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []EngineSnapshot
	for _, s := range e.snaps {
		if tag == "" || slices.Contains(s.Tags, tag) {
			out = append(out, s)
		}
	}
	return out, nil
}
func (e *ownEngine) Forget(_ context.Context, ids []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.forgetErr != nil {
		return e.forgetErr
	}
	e.forgot = append(e.forgot, ids...)
	return nil
}
func (e *ownEngine) Maintain(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruned = true
	return nil
}
func (e *ownEngine) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }
func (e *ownEngine) KeepReasons(context.Context, string, protocol.Retention, map[string]bool) (map[string][]string, error) {
	return nil, nil
}

// Deleting a machine's backups forgets its own snapshots (its sources',
// kept ones of removed sources too, and its state backups) and leaves
// another machine's in the same repository.
func TestDeleteOwnSnapshotsLeavesForeign(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "shared"})
	src, _ := store.AddSource(StoredSource{Name: "s", Paths: []string{"/a"}}, time.Now())
	gone := "feedc0de" // a removed source: only its runs remain
	store.AddRun(protocol.Run{Job: "j", Kind: protocol.KindBackup, Source: &gone, Started: time.Now(), Ended: time.Now(), Status: protocol.RunOK})
	eng := &ownEngine{snaps: []EngineSnapshot{
		{ID: "mine", Tags: []string{src.Tag()}, Hostname: "pantry"},
		{ID: "mine-removed", Tags: []string{"src:" + gone}, Hostname: "pantry"},
		{ID: "mine-state", Tags: []string{stateTag}, Hostname: "pantry.lan"},
		{ID: "theirs", Tags: []string{"src:0therb0x"}, Hostname: "wren"},
		{ID: "their-state", Tags: []string{stateTag}, Hostname: "wren"},
		{ID: "by-hand", Hostname: "pantry"},
	}}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	core.Name = "pantry"
	if err := core.deleteOwnSnapshots(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(eng.forgot)
	if want := []string{"mine", "mine-removed", "mine-state"}; !slices.Equal(eng.forgot, want) {
		t.Errorf("forgot %v, want %v", eng.forgot, want)
	}
	if !eng.pruned {
		t.Error("not pruned")
	}
}

// uninstall with delete: when the deletion fails, the remote says so and
// stays installed, so the controller keeps the pairing and its copies of
// the secrets.
func TestHostRemoveDeleteFailsClosed(t *testing.T) {
	eng := &ownEngine{forgetErr: errors.New("repository is locked"), snaps: []EngineSnapshot{{ID: "s1", Tags: []string{"src:abc"}}}}
	p := newRemotePair(t, func(_, pantry *Core) {
		pantry.makeEngine = func(RepositoryConfig) Engine { return eng }
	})
	src := "abc"
	p.pantry.store.AddRun(protocol.Run{Job: "j", Kind: protocol.KindBackup, Source: &src, Started: time.Now(), Ended: time.Now(), Status: protocol.RunOK})
	keys := &fakeKeychain{items: map[string]string{"charm-bak-repo/pantry": "pw"}}
	p.mac.secrets = keys
	waitConnected(t, p)
	uninstalled := make(chan struct{}, 1)
	p.pantry.mu.Lock()
	p.pantry.Uninstall = func() { uninstalled <- struct{}{} }
	p.pantry.mu.Unlock()

	if e := callErr(p.tui, protocol.MethodHostRemove, protocol.HostRemoveParams{Name: "pantry", DeleteBackups: true}); e != protocol.ErrBadRequest {
		t.Fatalf("a failed deletion answered %q", e)
	}
	select {
	case <-uninstalled:
		t.Fatal("uninstalled although its backups weren't deleted")
	case <-time.After(500 * time.Millisecond):
	}
	if peers, _ := p.mac.store.Peers(); len(peers) != 1 {
		t.Errorf("unpaired: %+v", peers)
	}
	if keys.items["charm-bak-repo/pantry"] != "pw" {
		t.Error("the Mac dropped the password of backups that still exist")
	}
	p.pantry.mu.Lock()
	paused := p.pantry.paused
	p.pantry.mu.Unlock()
	if paused {
		t.Error("left paused after refusing")
	}

	// Once the deletion works, it goes through, deleting first.
	eng.mu.Lock()
	eng.forgetErr = nil
	eng.mu.Unlock()
	res := call[protocol.HostRemoveResult](t, p.tui, protocol.MethodHostRemove, protocol.HostRemoveParams{Name: "pantry", DeleteBackups: true})
	eng.mu.Lock()
	forgot, pruned := slices.Clone(eng.forgot), eng.pruned
	eng.mu.Unlock()
	if !res.Uninstalled || !slices.Equal(forgot, []string{"s1"}) || !pruned {
		t.Errorf("result %+v, forgot %v, pruned %v", res, forgot, pruned)
	}
	select {
	case <-uninstalled:
	case <-time.After(5 * time.Second):
		t.Fatal("never uninstalled")
	}
	if _, ok := keys.items["charm-bak-repo/pantry"]; ok {
		t.Error("deleted backups kept their password on the Mac")
	}
}

func TestCleanupByHandPerOS(t *testing.T) {
	if got := cleanupByHand("pantry", "linux"); !strings.Contains(got, "systemctl") || strings.Contains(got, "launchctl") {
		t.Errorf("linux: %s", got)
	}
	if got := cleanupByHand("basement", "darwin"); !strings.Contains(got, "launchctl bootout system/"+MacLabel) || strings.Contains(got, "systemctl") {
		t.Errorf("mac: %s", got)
	}
	if got := cleanupByHand("x", ""); !strings.Contains(got, "systemctl") || !strings.Contains(got, "launchctl") {
		t.Errorf("unknown: %s", got)
	}
}

// Snapshots of a source that's gone show under "untagged": nothing else
// lists them.
func TestOrphanedSnapshotsAreListed(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	src, _ := store.AddSource(StoredSource{Name: "s", Paths: []string{"/a"}}, time.Now())
	eng := &ownEngine{snaps: []EngineSnapshot{
		{ID: "live", Tags: []string{src.Tag()}},
		{ID: "orphan", Tags: []string{"src:removed1"}},
		{ID: "bare"},
		{ID: "state", Tags: []string{stateTag}},
	}}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	c, _ := serve(t, core)
	res := call[protocol.SnapshotsResult](t, c, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: protocol.UntaggedSource})
	var ids []string
	for _, s := range res.Snapshots {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"bare", "orphan"}) {
		t.Errorf("untagged shows %v", ids)
	}
}

// slowBackup blocks until cancelled, or makes a snapshot and then blocks
// in retention.
type slowBackup struct {
	Engine
	started   chan struct{}
	snapFirst bool
}

func (e *slowBackup) Backup(ctx context.Context, _ BackupSpec, _ func(Progress)) (BackupResult, error) {
	if e.snapFirst {
		return BackupResult{Snapshot: "snap1", Files: 3, Bytes: 300}, nil
	}
	close(e.started)
	<-ctx.Done()
	return BackupResult{}, &EngineError{Kind: ErrCancelled, Message: "cancelled"}
}
func (e *slowBackup) ApplyRetention(ctx context.Context, _ string, _ protocol.Retention, _ map[string]bool) error {
	close(e.started)
	<-ctx.Done()
	return errors.New("signal: interrupt")
}
func (e *slowBackup) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }

// A backup cut short by the daemon stopping leaves no run, so it's due
// at the next start; one cancelled by hand is recorded.
func TestShutdownCancelLeavesSourceDue(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	clock := &testClock{t: time.Unix(1_900_000_000, 0), tick: time.Millisecond}
	src, _ := store.AddSource(StoredSource{Name: "s", Paths: []string{"/a"}, Schedule: &protocol.Schedule{Every: 86400}}, clock.now())
	eng := &slowBackup{started: make(chan struct{})}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, clock.now)
	core.Tick()
	<-eng.started
	core.Shutdown()
	if last, _ := store.LastRun(src.ID, protocol.KindBackup); last != nil {
		t.Fatalf("a run was recorded: %+v", last)
	}
	core.mu.Lock()
	view, _ := core.sourceViewLocked(src)
	core.mu.Unlock()
	if view.NextRun == nil || view.NextRun.After(clock.now()) {
		t.Errorf("next run %v: not due", view.NextRun)
	}

	// Cancelled by hand: recorded, and the next run is a day on.
	eng.started = make(chan struct{})
	job := core.enqueueBackup(src)
	<-eng.started
	if err := core.cancelJob(job.ID); err != nil {
		t.Fatal(err)
	}
	core.WaitIdle()
	if last, _ := store.LastRun(src.ID, protocol.KindBackup); last == nil || last.Status != protocol.RunCancelled {
		t.Errorf("cancelled by hand: %+v", last)
	}
}

// Cancelling while old snapshots are being cleaned up keeps the record
// of the snapshot that was made.
func TestCancelDuringRetentionKeepsSnapshot(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	src, _ := store.AddSource(StoredSource{Name: "s", Paths: []string{"/a"}}, time.Now())
	eng := &slowBackup{started: make(chan struct{}), snapFirst: true}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	job := core.enqueueBackup(src)
	<-eng.started
	core.cancelJob(job.ID)
	core.WaitIdle()
	last, _ := store.LastSnapshotRun(src.ID)
	if last == nil || last.Snapshot != "snap1" || last.Bytes != 300 || last.Status != protocol.RunCancelled {
		t.Errorf("run %+v", last)
	}
}

// dumpEngine records the limit it was asked for.
type dumpEngine struct {
	Engine
	limit int
}

func (e *dumpEngine) Read(_ context.Context, _, _ string, max int) ([]byte, int64, error) {
	e.limit = max
	return []byte("x"), 1, nil
}
func (e *dumpEngine) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }

// dump without a max reads at most the cap, never the whole file.
func TestDumpWithoutMaxIsCapped(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	eng := &dumpEngine{}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	c, _ := serve(t, core)
	call[protocol.DumpResult](t, c, protocol.MethodDump, protocol.DumpParams{Snapshot: "s", Path: "/a"})
	if eng.limit != 1<<20 {
		t.Errorf("limit %d", eng.limit)
	}
}

// Reading stored defaults doesn't write through the built-in ones.
func TestDefaultsLeaveBuiltinAlone(t *testing.T) {
	store := memStore(t)
	before := *BuiltinDefaults.Schedule
	store.SetDefaults(Defaults{Schedule: &protocol.Schedule{Every: 60}, Retention: protocol.Retention{Last: 1}})
	d, err := store.Defaults()
	if err != nil || d.Schedule == nil || d.Schedule.Every != 60 {
		t.Fatalf("defaults %+v %v", d, err)
	}
	if *BuiltinDefaults.Schedule != before {
		t.Errorf("built-in schedule became %+v", *BuiltinDefaults.Schedule)
	}
	if d.Schedule == BuiltinDefaults.Schedule {
		t.Error("stored defaults share the built-in schedule")
	}
}

func TestKeychainAddLine(t *testing.T) {
	line, err := keychainAddLine("svc", "acct", "a label", "plain-pw_1")
	if err != nil || !strings.Contains(line, `-w "plain-pw_1"`) {
		t.Errorf("plain: %q %v", line, err)
	}
	// A quote or backslash can't sit inside the quoted form: hex.
	line, err = keychainAddLine("svc", "acct", "l", `a"b\c`)
	if err != nil || !strings.Contains(line, "-X 6122625c63") || strings.Contains(line, `a"b`) {
		t.Errorf("quoted: %q %v", line, err)
	}
	for _, bad := range []string{"", "two\nlines", "tab\there"} {
		if _, err := keychainAddLine("svc", "acct", "l", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := keychainAddLine(`s"vc`, "acct", "l", "pw"); err == nil {
		t.Error("a quote in the service accepted")
	}
}

// probeEngine opens only with the right password.
type probeEngine struct {
	Engine
	cfg RepositoryConfig
}

func (e *probeEngine) Probe(context.Context) (ProbeResult, error) {
	if e.cfg.Password == "right" && e.cfg.SecretEnv["AWS_ACCESS_KEY_ID"] == "AKID" {
		return ProbeResult{State: protocol.ProbeOpens}, nil
	}
	return ProbeResult{State: protocol.ProbePassword}, nil
}
func (e *probeEngine) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }

// repository_set that doesn't open the repository leaves the working
// secrets alone; one that does replaces them, and drops a credentials
// file the new repository doesn't use.
func TestRepositorySetChecksBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	store := memStore(t)
	core := NewCore("test", store, func(r RepositoryConfig) Engine { return &probeEngine{cfg: r} }, nil)
	core.SecretDir = dir
	c, _ := serve(t, core)
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	set := protocol.RepositorySetParams{Location: "s3:x/y", Password: "right", AWSKey: "AKID:SECRET", Env: map[string]string{"B2_ACCOUNT_ID": "old"}}
	call[protocol.RepositoryResult](t, c, protocol.MethodRepoSet, set)
	if read(passwordFileName) != "right" || read(awsFileName) != "AKID:SECRET" || read(envFileName) == "" {
		t.Fatalf("after a good set: %q %q %q", read(passwordFileName), read(awsFileName), read(envFileName))
	}
	bad := set
	bad.Password = "typo"
	if e := callErr(c, protocol.MethodRepoSet, bad); e != protocol.ErrBadRequest {
		t.Fatalf("a wrong password answered %q", e)
	}
	if read(passwordFileName) != "right" {
		t.Errorf("a failed set replaced the password file: %q", read(passwordFileName))
	}
	again := set
	again.Env = nil
	call[protocol.RepositoryResult](t, c, protocol.MethodRepoSet, again)
	if _, err := os.Stat(filepath.Join(dir, envFileName)); !os.IsNotExist(err) {
		t.Error("the old credentials file is still there")
	}
}

// failInitEngine fails to create the repository.
type failInitEngine struct{ Engine }

func (failInitEngine) Init(context.Context) error { return errors.New("bucket not found") }

// Creating this machine's repository stores the password first and takes
// it back (to what was there) when creating fails.
func TestSetupOwnStoresPasswordBeforeInit(t *testing.T) {
	store := memStore(t)
	core := NewCore("test", store, func(RepositoryConfig) Engine { return failInitEngine{} }, nil)
	core.Name = "studio"
	keys := &fakeKeychain{items: map[string]string{}}
	core.secrets = keys
	c, _ := serve(t, core)
	if e := callErr(c, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: "/tmp/nowhere", Init: true}); e == "" {
		t.Fatal("a failed init succeeded")
	}
	if v, ok := keys.items["charm-bak-repo/studio"]; ok {
		t.Errorf("a password stayed after a failed init: %q", v)
	}
	keys.items["charm-bak-repo/studio"] = "an older repository's"
	callErr(c, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: "/tmp/nowhere", Init: true})
	if keys.items["charm-bak-repo/studio"] != "an older repository's" {
		t.Errorf("the item that was there became %q", keys.items["charm-bak-repo/studio"])
	}
}

// Tree is the warm-up that follows a snapshot listing.
func (*ownEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}

// Tree is the warm-up that follows a snapshot listing.
func (*slowBackup) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}

// Tree is the warm-up that follows a snapshot listing.
func (*dumpEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}

// Tree is the warm-up that follows a snapshot listing.
func (*probeEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}
