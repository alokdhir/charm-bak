package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// Changes between two real snapshots: a file added, one removed and one
// edited, each reported once with its kind.
func TestResticChanges(t *testing.T) {
	fx := newResticFixture(t)
	e := fx.engine
	ctx := context.Background()
	e.Init(ctx)
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fx.data, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt", "same")
	write("edit.txt", "before")
	write("gone.txt", "bye")
	spec := BackupSpec{Tag: "src:c", Paths: []string{fx.data}}
	if _, err := e.Backup(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	write("edit.txt", "after, and longer")
	// Same contents, a different timestamp: touched, not changed.
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(fx.data, "keep.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	write("new.txt", "hello")
	os.Remove(filepath.Join(fx.data, "gone.txt"))
	if _, err := e.Backup(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	snaps, _ := e.Snapshots(ctx, "src:c") // newest first
	if len(snaps) != 2 {
		t.Fatalf("snapshots %+v", snaps)
	}
	got, err := e.Changes(ctx, snaps[1].ID, snaps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		filepath.Join(fx.data, "edit.txt"): protocol.ChangeChanged,
		filepath.Join(fx.data, "new.txt"):  protocol.ChangeAdded,
		filepath.Join(fx.data, "gone.txt"): protocol.ChangeRemoved,
	}
	seen := map[string]string{}
	for _, ch := range got.Paths {
		seen[ch.Path] = ch.Change
	}
	for p, kind := range want {
		if seen[p] != kind {
			t.Errorf("%s: %q, want %q (all: %v)", filepath.Base(p), seen[p], kind, got.Paths)
		}
	}
	if got := seen[filepath.Join(fx.data, "keep.txt")]; got != protocol.ChangeTouched {
		t.Errorf("a file with only a new timestamp: %q, want touched", got)
	}
	for p, kind := range seen {
		if kind == protocol.ChangeTouched && strings.HasSuffix(p, "/") {
			t.Errorf("a folder is listed as touched: %s", p)
		}
	}
	if got.BytesAdded <= 0 {
		t.Errorf("bytes added %d", got.BytesAdded)
	}
}
