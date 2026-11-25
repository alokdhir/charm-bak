package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm-bak/internal/client"
	"charm-bak/internal/protocol"
)

// shortSocket is a socket path short enough for macOS (104 bytes);
// t.TempDir paths can be longer.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// serve starts a daemon over core on a fresh socket and returns a client.
func serve(t *testing.T, core *Core) (*client.Client, string) {
	t.Helper()
	sock := shortSocket(t)
	srv, err := Listen(core, sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close(); core.Shutdown() })
	c, err := client.Dial(context.Background(), sock, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, sock
}

func call[T any](t *testing.T, c *client.Client, method string, params any) T {
	t.Helper()
	var out T
	if err := c.Call(context.Background(), method, params, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

func callErr(c *client.Client, method string, params any) string {
	err := c.Call(context.Background(), method, params, nil)
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// The whole daemon over a real socket, with real restic and a throwaway
// repository: what the TUI sees.
func TestDaemonEndToEnd(t *testing.T) {
	fx := newResticFixture(t)
	if err := fx.engine.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := memStore(t)
	store.SetRepository(fx.engine.Repo)
	core := NewCore("test", store, func(r RepositoryConfig) Engine {
		return &ResticEngine{Binary: fx.engine.Binary, Repo: r}
	}, nil)
	c, sock := serve(t, core)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	events, err := client.Subscribe(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}

	added := call[protocol.SourceResult](t, c, protocol.MethodSourceAdd, protocol.SourceAddParams{
		Name: "data", Paths: []string{fx.data}, Excludes: []string{"node_modules"}, BackUpNow: true})
	id := added.Source.ID
	if !slices.Equal(added.Source.Inherited, []string{"schedule", "retention"}) || added.Source.Health != protocol.HealthNever {
		t.Errorf("added %+v", added.Source)
	}

	var run *protocol.Run
	deadline := time.After(60 * time.Second)
	for run == nil {
		select {
		case e := <-events:
			if e.Name == protocol.EventJobFinished {
				run = &e.JobFinished.Run
			}
		case <-deadline:
			t.Fatal("no job_finished event")
		}
	}
	if run.Status != protocol.RunOK || run.FilesNew != 3 {
		t.Errorf("run %+v", run)
	}
	core.WaitIdle()

	srcs := call[protocol.SourcesResult](t, c, protocol.MethodSources, nil)
	if len(srcs.Sources) != 1 || srcs.Sources[0].Health != protocol.HealthOK {
		t.Errorf("sources %+v", srcs)
	}
	if s := srcs.Sources[0]; s.Files == 0 || s.Bytes == 0 {
		t.Errorf("source size after a backup: %d files, %d bytes", s.Files, s.Bytes)
	}

	snaps, err := c.Snapshots(context.Background(), id)
	if err != nil || len(snaps) != 1 || !slices.Contains(snaps[0].KeptBy, "last snapshot") {
		t.Fatalf("snapshots %+v %v", snaps, err)
	}
	snap := snaps[0].ID

	ls := call[protocol.LsResult](t, c, protocol.MethodLs, protocol.LsParams{Snapshot: snap, Path: fx.data, Limit: 1})
	if len(ls.Entries) != 1 || ls.Entries[0].Name != "docs" || ls.Total != 2 || ls.NextOffset == nil || *ls.NextOffset != 1 {
		t.Errorf("ls page %+v", ls)
	}
	all, err := c.List(context.Background(), snap, fx.data)
	if err != nil || len(all) != 2 {
		t.Errorf("all pages %v %v", all, err)
	}
	d, err := c.Read(context.Background(), snap, filepath.Join(fx.data, "docs", "a.txt"), 100)
	if err != nil || string(d.Data) != "hello\n" || d.Binary || d.Truncated {
		t.Errorf("dump %+v %v", d, err)
	}
	if b, _ := c.Read(context.Background(), snap, filepath.Join(fx.data, "blob.bin"), 100); !b.Binary {
		t.Errorf("blob.bin not binary")
	}

	os.WriteFile(filepath.Join(fx.data, "added.txt"), []byte("new\n"), 0o644)
	os.Remove(filepath.Join(fx.data, "blob.bin"))
	diff := call[protocol.DiffResult](t, c, protocol.MethodDiff, protocol.DiffParams{Snapshot: snap, Path: fx.data})
	changes := map[string]string{}
	for _, e := range diff.Entries {
		changes[e.Name] = e.Change
	}
	want := map[string]string{"docs": "same", "blob.bin": "removed", "added.txt": "added", "node_modules": "added"}
	if len(changes) != len(want) {
		t.Errorf("diff %v", changes)
	}
	for k, v := range want {
		if changes[k] != v {
			t.Errorf("diff %s = %q, want %q", k, changes[k], v)
		}
	}

	note, pin := "first", true
	set := call[protocol.SnapshotResult](t, c, protocol.MethodSnapshotSet, protocol.SnapshotSetParams{ID: snap[:8], Note: &note, Pinned: &pin})
	if !set.Snapshot.Pinned || set.Snapshot.Note != "first" {
		t.Errorf("snapshot_set %+v", set.Snapshot)
	}

	manual := call[protocol.SourceResult](t, c, protocol.MethodSourceSet, protocol.SourceSetParams{ID: id, Schedule: &protocol.Schedule{Every: 0}})
	if manual.Source.Schedule != nil || manual.Source.NextRun != nil {
		t.Errorf("manual %+v", manual.Source)
	}
	back := call[protocol.SourceResult](t, c, protocol.MethodSourceSet, protocol.SourceSetParams{ID: id, Inherit: []string{"schedule"}})
	if back.Source.Schedule == nil || back.Source.Schedule.Every != 3600 {
		t.Errorf("inherit %+v", back.Source)
	}

	hist := call[protocol.HistoryResult](t, c, protocol.MethodHistory, protocol.HistoryParams{Since: time.Unix(0, 0)})
	if len(hist.Runs) != 1 {
		t.Errorf("history %+v", hist)
	}
	if code := callErr(c, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: "nope"}); code != protocol.ErrNotFound {
		t.Errorf("missing source: %s", code)
	}
	if code := callErr(c, protocol.MethodLs, protocol.LsParams{Snapshot: snap, Path: "/definitely/not/there"}); code != protocol.ErrNotFound {
		t.Errorf("missing path: %s", code)
	}
}

func TestDaemonWithoutRepository(t *testing.T) {
	c, _ := serve(t, NewCore("test", memStore(t), ResticEngines, nil))
	if code := callErr(c, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: protocol.UntaggedSource}); code != protocol.ErrNotConfigured {
		t.Errorf("snapshots: %s", code)
	}
	if code := callErr(c, protocol.MethodSourceAdd, protocol.SourceAddParams{Name: "x", Paths: []string{"/tmp"}, BackUpNow: true}); code != "" {
		t.Errorf("source_add without repo: %s", code)
	}
	if code := callErr(c, protocol.MethodSourceAdd, protocol.SourceAddParams{Name: "x", Paths: []string{"relative"}}); code != protocol.ErrBadRequest {
		t.Errorf("relative path: %s", code)
	}
	if code := callErr(c, protocol.MethodRestore, protocol.RestoreParams{Paths: []string{"/tmp"}, Target: protocol.RestoreOriginal}); code != protocol.ErrBadRequest {
		t.Errorf("restore without a snapshot: %s", code)
	}
	if code := callErr(c, "nope", nil); code != protocol.ErrUnknownMethod {
		t.Errorf("unknown method: %s", code)
	}
	st := call[protocol.StatusResult](t, c, protocol.MethodStatus, nil)
	if st.Repository != nil || st.State != protocol.StateIdle {
		t.Errorf("status %+v", st)
	}
}

// dump with snapshot "live" reads the disk and needs no repository.
func TestDumpLive(t *testing.T) {
	c, _ := serve(t, NewCore("test", memStore(t), ResticEngines, nil))
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := call[protocol.DumpResult](t, c, protocol.MethodDump, protocol.DumpParams{Snapshot: protocol.LiveSnapshot, Path: f, Max: 5})
	if string(r.Data) != "hello" || r.Size != 11 || !r.Truncated {
		t.Errorf("live dump %+v", r)
	}
	for path, want := range map[string]string{
		filepath.Join(dir, "gone"): protocol.ErrNotFound,
		dir:                        protocol.ErrBadRequest,
		"relative":                 protocol.ErrBadRequest,
	} {
		if code := callErr(c, protocol.MethodDump, protocol.DumpParams{Snapshot: protocol.LiveSnapshot, Path: path, Max: 5}); code != want {
			t.Errorf("%s: %s, want %s", path, code, want)
		}
	}
}

// ls with snapshot "live" lists the disk, folders first, with no
// repository: the add-source picker browses a remote's disk this way.
func TestLsLive(t *testing.T) {
	c, _ := serve(t, NewCore("test", memStore(t), ResticEngines, nil))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello"), 0o644)
	os.Mkdir(filepath.Join(dir, "z-dir"), 0o755)
	os.Symlink("b.txt", filepath.Join(dir, "a-link"))
	r := call[protocol.LsResult](t, c, protocol.MethodLs, protocol.LsParams{Snapshot: protocol.LiveSnapshot, Path: dir})
	var got []string
	for _, e := range r.Entries {
		got = append(got, e.Name+":"+e.Type)
	}
	if strings.Join(got, " ") != "z-dir:dir a-link:symlink b.txt:file" || r.Entries[2].Size != 5 || r.NextOffset != nil {
		t.Errorf("live ls %v %+v", got, r)
	}
	for path, want := range map[string]string{filepath.Join(dir, "gone"): protocol.ErrNotFound, "relative": protocol.ErrBadRequest} {
		if code := callErr(c, protocol.MethodLs, protocol.LsParams{Snapshot: protocol.LiveSnapshot, Path: path}); code != want {
			t.Errorf("%s: %s, want %s", path, code, want)
		}
	}
}

func TestServerSocketRules(t *testing.T) {
	core := NewCore("test", memStore(t), ResticEngines, nil)
	_, sock := serve(t, core)
	if fi, _ := os.Stat(sock); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(sock)); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode().Perm())
	}
	if _, err := Listen(core, sock); !errors.Is(err, ErrAlreadyServing) {
		t.Errorf("second daemon: %v", err)
	}

	stale := shortSocket(t)
	os.WriteFile(stale, nil, 0o600)
	srv, err := Listen(core, stale)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	srv.Close()
}

// A client that sends its requests and closes its writing side (nc does)
// still gets every reply.
func TestHalfClosedClientGetsReplies(t *testing.T) {
	_, sock := serve(t, NewCore("test", memStore(t), ResticEngines, nil))
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte(`{"id":1,"method":"status"}` + "\n" + `{"id":2,"method":"hello","params":{"protocol":1,"client":"t"}}` + "\n"))
	conn.(*net.UnixConn).CloseWrite()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	sc := bufio.NewScanner(conn)
	ids := map[int64]bool{}
	for sc.Scan() {
		var r protocol.Reply
		json.Unmarshal(sc.Bytes(), &r)
		ids[r.ID] = true
	}
	if !ids[1] || !ids[2] {
		t.Errorf("replies for %v", ids)
	}
}

// blockingEngine is a fake whose prune waits until released.
type blockingEngine struct {
	Engine
	release chan struct{}
	started chan struct{}
	trees   atomic.Int32
}

func (b *blockingEngine) Maintain(ctx context.Context) error {
	close(b.started)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil
}

func (b *blockingEngine) Check(context.Context, int) error { return nil }

func (b *blockingEngine) List(context.Context, string, string) ([]protocol.Entry, error) {
	return []protocol.Entry{}, nil
}

func (b *blockingEngine) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }

func (b *blockingEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	b.trees.Add(1)
	return map[string][]protocol.Entry{"/": {{Name: "a", Type: protocol.TypeFile}}}, nil
}

func TestReadsAreBusyDuringPrune(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	eng := &blockingEngine{release: make(chan struct{}), started: make(chan struct{})}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	c, _ := serve(t, core)
	core.Tick() // no prune has ever run: maintenance is due
	<-eng.started
	err := c.Call(context.Background(), protocol.MethodLs, protocol.LsParams{Snapshot: "x", Path: "/"}, nil)
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.ErrBusy {
		t.Fatalf("ls during prune: %v", err)
	}
	var busy protocol.BusyData
	if json.Unmarshal(pe.Data, &busy) != nil || busy.Job.Kind != protocol.KindPrune {
		t.Errorf("busy data %s", pe.Data)
	}
	st := call[protocol.StatusResult](t, c, protocol.MethodStatus, nil)
	if st.State != protocol.StatePruning {
		t.Errorf("state %s", st.State)
	}
	close(eng.release)
	core.WaitIdle()
	if err := c.Call(context.Background(), protocol.MethodLs, protocol.LsParams{Snapshot: "x", Path: "/"}, nil); err != nil {
		t.Errorf("ls after prune: %v", err)
	}

	// That ls started loading x's whole tree; once loaded, folders come
	// from memory, even during the next prune, and the tree loads once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, loaded, _ := core.trees.lookup("x", "/"); loaded || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	eng.release, eng.started = make(chan struct{}), make(chan struct{})
	core.enqueue(protocol.KindPrune, nil, func(ctx context.Context, _ protocol.Job) outcome { return result(eng.Maintain(ctx)) })
	<-eng.started
	r := call[protocol.LsResult](t, c, protocol.MethodLs, protocol.LsParams{Snapshot: "x", Path: "/"})
	if len(r.Entries) != 1 || r.Entries[0].Name != "a" {
		t.Errorf("cached ls during prune: %+v", r)
	}
	if code := callErr(c, protocol.MethodLs, protocol.LsParams{Snapshot: "x", Path: "/nope"}); code != protocol.ErrNotFound {
		t.Errorf("missing folder from the cache: %s", code)
	}
	close(eng.release)
	core.WaitIdle()
	if n := eng.trees.Load(); n != 1 {
		t.Errorf("tree loaded %d times", n)
	}
}

// testClock is a clock the test moves by hand.
type testClock struct {
	mu   sync.Mutex
	t    time.Time
	tick time.Duration // added after every reading, like a real clock moving
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.t
	c.t = c.t.Add(c.tick)
	return t
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestSchedulerRunsDueSourcesAndMaintenance(t *testing.T) {
	fx := newResticFixture(t)
	fx.engine.Init(context.Background())
	store := memStore(t)
	store.SetRepository(fx.engine.Repo)
	// The clock moves between readings: a source that never ran must
	// still be due (it once looked a moment in the future, never due).
	clock := &testClock{t: time.Unix(1_900_000_000, 0), tick: time.Millisecond}
	core := NewCore("test", store, func(r RepositoryConfig) Engine {
		return &ResticEngine{Binary: fx.engine.Binary, Repo: r}
	}, clock.now)
	src, _ := store.AddSource(StoredSource{Name: "d", Paths: []string{fx.data}, Schedule: &protocol.Schedule{Every: 3600}}, clock.now())
	kinds := func() []string {
		runs, _ := store.Runs(time.Unix(0, 0))
		var k []string
		for _, r := range runs {
			k = append(k, r.Kind+":"+r.Status)
		}
		return k
	}
	step := func() { core.Tick(); core.WaitIdle() }

	step() // never run: due now; maintenance waits behind it
	if got := kinds(); !slices.Equal(got, []string{"backup:ok"}) {
		t.Fatalf("first tick %v", got)
	}
	step() // idle and no prune ever: maintenance
	if got := kinds(); !slices.Equal(got, []string{"backup:ok", "prune:ok", "check:ok"}) {
		t.Fatalf("second tick %v", got)
	}
	clock.advance(30 * time.Minute)
	step()
	if n := len(kinds()); n != 3 {
		t.Errorf("within the hour: %d runs", n)
	}
	clock.advance(31 * time.Minute)
	step()
	if got := kinds(); len(got) != 4 || got[3] != "backup:ok" {
		t.Errorf("after the hour %v", got)
	}
	clock.advance(2 * time.Hour)
	core.SetPaused(true)
	step()
	core.SetPaused(false)
	src.Paused = true
	store.UpdateSource(src)
	step()
	if n := len(kinds()); n != 4 {
		t.Errorf("paused: %d runs", n)
	}
}

type statsEngine struct {
	Engine
	calls atomic.Int32
}

func (s *statsEngine) Stats(context.Context) (RepoStats, error) {
	n := int64(s.calls.Add(1))
	return RepoStats{Stored: 100 * n, Uncompressed: 150 * n}, nil
}

// status reads the repository's size in the background the first time,
// says so with a state_changed event, and answers from memory after.
func TestStatusRepositorySize(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	eng := &statsEngine{}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	events, unsub := core.Subscribe()
	defer unsub()
	c, _ := serve(t, core)
	st := call[protocol.StatusResult](t, c, protocol.MethodStatus, nil)
	if st.Repository.StoredBytes != 0 {
		t.Fatalf("first status already has a size: %+v", st.Repository)
	}
	select {
	case e := <-events:
		if e.Name != protocol.EventStateChanged {
			t.Fatalf("event %s", e.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no state_changed after the size was read")
	}
	st = call[protocol.StatusResult](t, c, protocol.MethodStatus, nil)
	if st.Repository.StoredBytes != 100 || st.Repository.LogicalBytes != 150 || eng.calls.Load() != 1 {
		t.Errorf("repository %+v after %d reads", st.Repository, eng.calls.Load())
	}
}

// repository describes credentials by where they come from, never more;
// defaults_set changes what inheriting sources run on.
func TestRepositoryAndDefaults(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake", PasswordCommand: "security find-generic-password -s charm-bak-repo -a studio -w", AWSKeychain: "charm-bak-aws/studio"})
	core := NewCore("test", store, func(RepositoryConfig) Engine { return &statsEngine{} }, nil)
	c, _ := serve(t, core)
	r := call[protocol.RepositoryResult](t, c, protocol.MethodRepository, nil)
	if r.Repository == nil || r.Repository.PasswordFrom != "Keychain charm-bak-repo / studio" || r.Repository.AWSFrom != "Keychain charm-bak-aws / studio" {
		t.Fatalf("repository %+v", r.Repository)
	}
	if r.Repository.NextMaintenance == nil || r.Defaults.Schedule == nil || r.Defaults.Schedule.Every != 3600 {
		t.Errorf("maintenance %v, defaults %+v", r.Repository.NextMaintenance, r.Defaults)
	}
	if got := describePasswordCommand("echo hunter2"); strings.Contains(got, "hunter2") {
		t.Errorf("password command shown: %q", got)
	}

	src := call[protocol.SourceResult](t, c, protocol.MethodSourceAdd, protocol.SourceAddParams{Paths: []string{"/tmp"}})
	d := call[protocol.DefaultsResult](t, c, protocol.MethodDefaultsSet, protocol.DefaultsSetParams{Schedule: &protocol.Schedule{Every: 1800}})
	if d.Defaults.Schedule == nil || d.Defaults.Schedule.Every != 1800 || d.Defaults.Retention != BuiltinDefaults.Retention {
		t.Fatalf("defaults %+v", d.Defaults)
	}
	got := call[protocol.SourcesResult](t, c, protocol.MethodSources, nil)
	if len(got.Sources) != 1 || got.Sources[0].ID != src.Source.ID || got.Sources[0].Schedule == nil || got.Sources[0].Schedule.Every != 1800 {
		t.Errorf("inheriting source after defaults_set: %+v", got.Sources)
	}
	d = call[protocol.DefaultsResult](t, c, protocol.MethodDefaultsSet, protocol.DefaultsSetParams{Schedule: &protocol.Schedule{Every: 0}})
	if d.Defaults.Schedule != nil {
		t.Errorf("every 0 should be manual: %+v", d.Defaults.Schedule)
	}
}

// specEngine records what each backup was asked to skip.
type specEngine struct {
	statsEngine
	mu    sync.Mutex
	specs []BackupSpec
}

func (e *specEngine) Backup(_ context.Context, spec BackupSpec, _ func(Progress)) (BackupResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.specs = append(e.specs, spec)
	return BackupResult{Snapshot: "s1"}, nil
}
func (e *specEngine) ApplyRetention(context.Context, string, protocol.Retention, map[string]bool) error {
	return nil
}
func (e *specEngine) KeepReasons(context.Context, string, protocol.Retention, map[string]bool) (map[string][]string, error) {
	return nil, nil
}

// A backup skips the defaults' excludes (none built in), then the
// source's own; defaults_set changes or clears them.
func TestDefaultExcludes(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	eng := &specEngine{}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	c, _ := serve(t, core)
	if r := call[protocol.RepositoryResult](t, c, protocol.MethodRepository, nil); r.Defaults.Excludes == nil || len(r.Defaults.Excludes) != 0 {
		t.Fatalf("built-in excludes %q", r.Defaults.Excludes)
	}
	src := call[protocol.SourceResult](t, c, protocol.MethodSourceAdd, protocol.SourceAddParams{Paths: []string{"/tmp"}, Excludes: []string{"node_modules"}})
	backup := func() []string {
		call[protocol.JobResult](t, c, protocol.MethodBackupNow, protocol.BackupNowParams{Source: src.Source.ID})
		core.WaitIdle()
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.specs[len(eng.specs)-1].Excludes
	}
	if got := backup(); !slices.Equal(got, []string{"node_modules"}) {
		t.Errorf("backup excludes %q", got)
	}
	d := call[protocol.DefaultsResult](t, c, protocol.MethodDefaultsSet, protocol.DefaultsSetParams{Excludes: &[]string{"*.tmp", "*.log"}})
	if !slices.Equal(d.Defaults.Excludes, []string{"*.tmp", "*.log"}) || d.Defaults.Schedule == nil {
		t.Errorf("after set: %+v", d.Defaults)
	}
	if got := backup(); !slices.Equal(got, []string{"*.tmp", "*.log", "node_modules"}) {
		t.Errorf("backup excludes %q", got)
	}
	d = call[protocol.DefaultsResult](t, c, protocol.MethodDefaultsSet, protocol.DefaultsSetParams{Excludes: &[]string{}})
	if d.Defaults.Excludes == nil || len(d.Defaults.Excludes) != 0 {
		t.Errorf("cleared: %#v", d.Defaults.Excludes)
	}
	if got := backup(); !slices.Equal(got, []string{"node_modules"}) {
		t.Errorf("backup excludes after clearing %q", got)
	}
}

// Tree is the warm-up that follows a snapshot listing.
func (*statsEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}

// Progress divides by the last backup's size until restic's scanner has
// found more, and never goes backwards.
func TestSteadyPercent(t *testing.T) {
	steady := steadyPercent(1000)
	for _, c := range []struct {
		done, total int64
		want        float64
	}{
		{100, 120, 0.1},  // restic would say 83%
		{150, 900, 0.15}, // the scanner catching up
		{120, 900, 0.15}, // never backwards
		{800, 1000, 0.8},
		{1100, 1200, 1100.0 / 1200}, // more than last time: restic's total
	} {
		if got := steady(Progress{BytesDone: c.done, TotalBytes: c.total, Percent: float64(c.done) / float64(c.total)}); got != c.want {
			t.Errorf("%d of %d: %v, want %v", c.done, c.total, got, c.want)
		}
	}
	// No last backup: restic's own figure, still never backwards.
	first := steadyPercent(0)
	if a, b := first(Progress{BytesDone: 80, TotalBytes: 100}), first(Progress{BytesDone: 90, TotalBytes: 900}); a != 0.8 || b != 0.8 {
		t.Errorf("first backup: %v then %v", a, b)
	}
}

// A clicked notification is announced, then taken by one TUI only; one
// nobody takes in time is dropped.
func TestNoticeShowTake(t *testing.T) {
	now := time.Now()
	core := NewCore("test", memStore(t), ResticEngines, nil)
	core.now = func() time.Time { return now }
	events, unsub := core.Subscribe()
	defer unsub()
	c, _ := serve(t, core)
	call[protocol.Empty](t, c, protocol.MethodNoticeShow, protocol.Notice{Host: "pantry", Job: "j-41"})
	select {
	case e := <-events:
		if e.Name != protocol.EventNoticeShow || e.NoticeShow.Notice.Job != "j-41" {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notice_show event")
	}
	if got := call[protocol.NoticeTakeResult](t, c, protocol.MethodNoticeTake, nil); got.Notice == nil || got.Notice.Host != "pantry" {
		t.Fatalf("take %+v", got)
	}
	if got := call[protocol.NoticeTakeResult](t, c, protocol.MethodNoticeTake, nil); got.Notice != nil {
		t.Fatalf("taken twice: %+v", got)
	}
	call[protocol.Empty](t, c, protocol.MethodNoticeShow, protocol.Notice{Job: "j-1"})
	now = now.Add(noticeKeep + time.Second)
	if got := call[protocol.NoticeTakeResult](t, c, protocol.MethodNoticeTake, nil); got.Notice != nil {
		t.Errorf("stale notice taken: %+v", got)
	}
}
