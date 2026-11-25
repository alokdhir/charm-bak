package daemon

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// resticFixture is a throwaway repository and a small data tree, using the
// real restic binary. Tests skip when restic isn't installed.
type resticFixture struct {
	root, data string
	engine     *ResticEngine
}

func newResticFixture(t *testing.T) *resticFixture {
	t.Helper()
	bin := FindRestic()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("restic not installed")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(data, "docs"), 0o755))
	must(os.MkdirAll(filepath.Join(data, "node_modules", "pkg"), 0o755))
	must(os.WriteFile(filepath.Join(data, "docs", "a.txt"), []byte("hello\n"), 0o644))
	must(os.WriteFile(filepath.Join(data, "docs", "b.txt"), make([]byte, 5000), 0o644))
	must(os.WriteFile(filepath.Join(data, "blob.bin"), []byte{0, 1, 2, 3}, 0o644))
	must(os.WriteFile(filepath.Join(data, "node_modules", "pkg", "x.js"), []byte("junk"), 0o644))
	// Tests only: the password from the environment. The daemon uses a
	// Keychain lookup via PasswordCommand.
	e := &ResticEngine{Binary: bin, Repo: RepositoryConfig{
		Location:    filepath.Join(root, "repo"),
		Environment: map[string]string{"RESTIC_PASSWORD": "test-only"},
	}}
	return &resticFixture{root: root, data: data, engine: e}
}

var keepOne = protocol.Retention{Last: 1}

func TestResticBackupBrowseRetain(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	if err := e.Init(ctx); err != nil {
		t.Fatal(err)
	}
	spec := BackupSpec{Tag: "src:test", Paths: []string{fx.data}, Excludes: []string{"node_modules"}}
	first, err := e.Backup(ctx, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Incomplete || first.FilesNew != 3 {
		t.Errorf("first backup %+v, want 3 new files (node_modules excluded)", first)
	}
	if _, err := e.Backup(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}

	snaps, err := e.Snapshots(ctx, "src:test")
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots %v %v", snaps, err)
	}
	if snaps[0].Time.Before(snaps[1].Time) || snaps[0].Files != 3 || !slices.Contains(snaps[0].Tags, "src:test") {
		t.Errorf("snapshots %+v", snaps)
	}
	if other, _ := e.Snapshots(ctx, "src:other"); len(other) != 0 {
		t.Errorf("other tag: %v", other)
	}

	top, err := e.List(ctx, snaps[0].ShortID, fx.data+"/")
	if err != nil {
		t.Fatal(err)
	}
	if names := entryNames(top); !slices.Equal(names, []string{"docs", "blob.bin"}) || top[0].Size != 0 {
		t.Errorf("top %v", top)
	}
	docs, _ := e.List(ctx, snaps[0].ID, filepath.Join(fx.data, "docs"))
	if names := entryNames(docs); !slices.Equal(names, []string{"a.txt", "b.txt"}) || docs[1].Size != 5000 {
		t.Errorf("docs %v", docs)
	}
	var ee *EngineError
	if _, err := e.List(ctx, snaps[0].ID, filepath.Join(fx.data, "nope")); !errors.As(err, &ee) || ee.Kind != ErrNotFound {
		t.Errorf("missing path: %v", err)
	}

	data, size, err := e.Read(ctx, snaps[0].ID, filepath.Join(fx.data, "docs", "a.txt"), 1024)
	if err != nil || string(data) != "hello\n" || size != 6 {
		t.Errorf("read a.txt: %q %d %v", data, size, err)
	}
	data, size, err = e.Read(ctx, snaps[0].ID, filepath.Join(fx.data, "docs", "b.txt"), 100)
	if err != nil || len(data) != 100 || size != 5000 {
		t.Errorf("capped read: %d bytes of %d, %v", len(data), size, err)
	}

	reasons, err := e.KeepReasons(ctx, "src:test", keepOne, nil)
	if err != nil || !slices.Equal(reasons[snaps[0].ID], []string{"last snapshot"}) || reasons[snaps[1].ID] != nil {
		t.Errorf("reasons %v %v", reasons, err)
	}
	if err := e.ApplyRetention(ctx, "src:test", keepOne, nil); err != nil {
		t.Fatal(err)
	}
	if left, _ := e.Snapshots(ctx, "src:test"); len(left) != 1 || left[0].ID != snaps[0].ID {
		t.Errorf("after retention %v", left)
	}

	// All-zero retention keeps everything (restic alone would forget all).
	e.Backup(ctx, spec, nil)
	if err := e.ApplyRetention(ctx, "src:test", protocol.Retention{}, nil); err != nil {
		t.Fatal(err)
	}
	if left, _ := e.Snapshots(ctx, "src:test"); len(left) != 2 {
		t.Errorf("all-zero retention removed snapshots: %d left", len(left))
	}
	if err := e.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(ctx, 100); err != nil {
		t.Fatal(err)
	}
}

func TestResticPinnedSurvivesRetention(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	spec := BackupSpec{Tag: "src:p", Paths: []string{fx.data}}
	pinned, _ := e.Backup(ctx, spec, nil)
	e.Backup(ctx, spec, nil)
	latest, _ := e.Backup(ctx, spec, nil)
	pins := map[string]bool{pinned.Snapshot: true}
	reasons, _ := e.KeepReasons(ctx, "src:p", keepOne, pins)
	if !slices.Equal(reasons[pinned.Snapshot], []string{"pinned"}) || !slices.Equal(reasons[latest.Snapshot], []string{"last snapshot"}) {
		t.Errorf("reasons %v", reasons)
	}
	if err := e.ApplyRetention(ctx, "src:p", keepOne, pins); err != nil {
		t.Fatal(err)
	}
	left, _ := e.Snapshots(ctx, "src:p")
	ids := []string{left[0].ID, left[1].ID}
	slices.Sort(ids)
	want := []string{pinned.Snapshot, latest.Snapshot}
	slices.Sort(want)
	if len(left) != 2 || !slices.Equal(ids, want) {
		t.Errorf("left %v", left)
	}
}

func TestResticErrors(t *testing.T) {
	fx := newResticFixture(t)
	ctx := context.Background()
	var ee *EngineError
	if _, err := fx.engine.Snapshots(ctx, ""); !errors.As(err, &ee) || ee.Kind != ErrNoRepository {
		t.Errorf("no repo: %v", err)
	}
	fx.engine.Init(ctx)
	wrong := &ResticEngine{Binary: fx.engine.Binary, Repo: RepositoryConfig{Location: fx.engine.Repo.Location,
		Environment: map[string]string{"RESTIC_PASSWORD": "nope"}}}
	if _, err := wrong.Snapshots(ctx, ""); !errors.As(err, &ee) || ee.Kind != ErrWrongPassword {
		t.Errorf("wrong password: %v", err)
	}
}

func TestResticRestoreToDirectory(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	snap, _ := e.Backup(ctx, BackupSpec{Tag: "src:r", Paths: []string{fx.data}}, nil)
	target := filepath.Join(fx.root, "restored")
	file := filepath.Join(fx.data, "docs", "a.txt")
	if err := e.Restore(ctx, snap.Snapshot, []string{file}, target, protocol.OverwriteNever, nil); err != nil {
		t.Fatal(err)
	}
	// The file lands in the target under its own name.
	got, err := os.ReadFile(filepath.Join(target, "a.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Errorf("restored %q %v", got, err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 1 {
		t.Errorf("restored more than asked: %v", entries)
	}

	// A folder lands as target/docs with its contents.
	target2 := filepath.Join(fx.root, "restored2")
	if err := e.Restore(ctx, snap.Snapshot, []string{filepath.Join(fx.data, "docs")}, target2, protocol.OverwriteNever, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target2, "docs", "a.txt")); err != nil {
		t.Errorf("folder restore: %v", err)
	}

	// In place: "always" puts the snapshot's copy back over an edit,
	// "never" leaves the edit alone.
	if err := os.WriteFile(file, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Restore(ctx, snap.Snapshot, []string{file}, "", protocol.OverwriteNever, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); string(got) != "edited\n" {
		t.Errorf("never overwrote: %q", got)
	}
	if err := e.Restore(ctx, snap.Snapshot, []string{file}, "", protocol.OverwriteAlways, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(file); string(got) != "hello\n" {
		t.Errorf("in-place restore: %q", got)
	}
}

func TestRestoreBase(t *testing.T) {
	for want, paths := range map[string][]string{
		"/":        nil,
		"/a/b":     {"/a/b/f.txt"},
		"/a":       {"/a/b/f.txt", "/a/c"},
		"/x":       {"/x/y", "/x/yz/q"},
		"/a/b/c/d": {"/a/b/c/d/e"},
	} {
		if got := restoreBase(paths); got != want {
			t.Errorf("restoreBase(%q) = %q, want %q", paths, got, want)
		}
	}
}

func TestResticCancelStopsBackup(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	e.Init(context.Background())
	// Enough incompressible data that the backup is still running when
	// the cancel lands a moment after it starts.
	f, err := os.Create(filepath.Join(fx.data, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	chunk := make([]byte, 1<<20)
	for range 384 {
		for i := 0; i < len(chunk); i += 8 {
			binary.LittleEndian.PutUint64(chunk[i:], rng.Uint64())
		}
		f.Write(chunk)
	}
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(700*time.Millisecond, cancel)
	_, err = e.Backup(ctx, BackupSpec{Tag: "src:c", Paths: []string{fx.data}}, nil)
	var ee *EngineError
	if err == nil {
		t.Skip("backup finished before the cancel landed")
	}
	if !errors.As(err, &ee) || ee.Kind != ErrCancelled {
		t.Errorf("cancelled backup: %v", err)
	}
	// The lock was released: another command works straight away.
	if _, err := e.Snapshots(context.Background(), ""); err != nil {
		t.Errorf("after cancel: %v", err)
	}
}

func entryNames(es []protocol.Entry) []string {
	var n []string
	for _, e := range es {
		n = append(n, e.Name)
	}
	return n
}

// Tree is List for every directory at once: same entries, same order,
// empty directories included.
func TestResticTreeMatchesList(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	if err := os.MkdirAll(filepath.Join(fx.data, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap, err := e.Backup(ctx, BackupSpec{Tag: "src:t", Paths: []string{fx.data}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := e.Tree(ctx, snap.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/", fx.data, filepath.Join(fx.data, "docs"), filepath.Join(fx.data, "node_modules", "pkg"), filepath.Join(fx.data, "empty")} {
		want, err := e.List(ctx, snap.Snapshot, dir)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := tree[dir]
		if !ok {
			t.Errorf("%s missing from the tree", dir)
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%s: tree %v, list %v", dir, got, want)
			continue
		}
		for i := range want {
			if got[i].Name != want[i].Name || got[i].Type != want[i].Type || got[i].Size != want[i].Size || !got[i].MTime.Equal(want[i].MTime) {
				t.Errorf("%s[%d]: tree %+v, list %+v", dir, i, got[i], want[i])
			}
		}
	}
}

// Stats reports something stored after a backup, and no more stored than
// uncompressed.
func TestResticStats(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	before, err := e.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Backup(ctx, BackupSpec{Tag: "src:t", Paths: []string{fx.data}}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := e.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Stored <= before.Stored || after.Uncompressed < after.Stored {
		t.Errorf("before %+v, after %+v", before, after)
	}
}

// --exclude-cloud-files only where restic has it.
func TestBackupFlagsPerOS(t *testing.T) {
	for goos, want := range map[string]bool{"darwin": true, "windows": true, "linux": false, "freebsd": false} {
		if got := slices.Contains(backupFlags(goos), "--exclude-cloud-files"); got != want {
			t.Errorf("%s: cloud flag %v", goos, got)
		}
	}
}

// The cache folder main sets for a service without HOME reaches restic;
// other RESTIC_ variables from the daemon's environment don't.
func TestResticEnvKeepsCacheDir(t *testing.T) {
	t.Setenv("RESTIC_CACHE_DIR", "/state/cache/restic")
	t.Setenv("RESTIC_REPOSITORY", "/somewhere/else")
	env, err := (&ResticEngine{Repo: RepositoryConfig{Location: "/repo"}}).env()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "RESTIC_CACHE_DIR=/state/cache/restic") {
		t.Error("RESTIC_CACHE_DIR dropped")
	}
	if slices.Contains(env, "RESTIC_REPOSITORY=/somewhere/else") || !slices.Contains(env, "RESTIC_REPOSITORY=/repo") {
		t.Errorf("repository env %v", env)
	}
}

// Rewrite takes a path out of a source's snapshots: a dry run counts
// them and changes nothing; the real one replaces each, the new one
// naming the old as Original, and other sources' snapshots are untouched.
func TestResticRewrite(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	spec := BackupSpec{Tag: "src:r", Paths: []string{fx.data}}
	first, _ := e.Backup(ctx, spec, nil)
	os.WriteFile(filepath.Join(fx.data, "docs", "c.txt"), []byte("more"), 0o644)
	e.Backup(ctx, spec, nil)
	other, _ := e.Backup(ctx, BackupSpec{Tag: "src:o", Paths: []string{fx.data}}, nil)
	gone := filepath.Join(fx.data, "node_modules")

	n, err := e.Rewrite(ctx, "src:r", []string{gone}, true)
	if err != nil || n != 2 {
		t.Fatalf("dry run: %d %v", n, err)
	}
	if snaps, _ := e.Snapshots(ctx, "src:r"); len(snaps) != 2 || snaps[1].ID != first.Snapshot {
		t.Fatalf("dry run changed snapshots: %+v", snaps)
	}
	if n, err = e.Rewrite(ctx, "src:r", []string{gone}, false); err != nil || n != 2 {
		t.Fatalf("rewrite: %d %v", n, err)
	}
	snaps, _ := e.Snapshots(ctx, "src:r")
	if len(snaps) != 2 || snaps[1].Original != first.Snapshot {
		t.Fatalf("after: %+v", snaps)
	}
	for _, s := range snaps {
		entries, err := e.List(ctx, s.ID, fx.data)
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range entries {
			if en.Name == "node_modules" {
				t.Errorf("%s still has node_modules", s.ShortID)
			}
		}
	}
	if o, _ := e.Snapshots(ctx, "src:o"); len(o) != 1 || o[0].ID != other.Snapshot {
		t.Errorf("other source's snapshots %+v", o)
	}
	if n, _ := e.Rewrite(ctx, "src:r", []string{gone}, true); n != 0 {
		t.Errorf("again: %d", n)
	}
	// What the second snapshot adds over the first: c.txt and the trees
	// above it (restic counts tree blobs as added data too), a few KB.
	if n, err := e.Added(ctx, snaps[1].ID, snaps[0].ID); err != nil || n < 4 || n > 20000 {
		t.Errorf("added %d %v", n, err)
	}
}

// A file restic can't open makes the backup incomplete, and the result
// names it with the system's reason.
func TestResticBackupUnread(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	fx := newResticFixture(t)
	ctx := context.Background()
	if err := fx.engine.Init(ctx); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(fx.data, "locked.txt")
	if err := os.WriteFile(locked, []byte("no"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })
	res, err := fx.engine.Backup(ctx, BackupSpec{Tag: "src:test", Paths: []string{fx.data}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := locked + " (permission denied)"
	if !res.Incomplete || res.UnreadCount != 1 || len(res.Unread) != 1 || res.Unread[0] != want {
		t.Fatalf("result %+v, want %q", res, want)
	}
	if got := unreadSummary(res); got != "Couldn't read 1 file: "+want+"." {
		t.Errorf("summary %q", got)
	}
}
