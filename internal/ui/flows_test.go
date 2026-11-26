package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// A home folder with something to tick: two folders and a file.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"docs", "junk"} {
		if err := os.Mkdir(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

func sourceNamed(a *App, name string) (protocol.Source, bool) {
	for _, s := range a.sources {
		if s.Name == name {
			return s, true
		}
	}
	return protocol.Source{}, false
}

// Add: tick the home folder, untick junk inside it, save.
func TestAddSourceWithAnException(t *testing.T) {
	home := fakeHome(t)
	a := newTestApp(t, 160, 40)
	press(a, "a")
	if _, ok := a.modal.(*folderPicker); !ok {
		t.Fatalf("a: modal %T, want the picker", a.modal)
	}
	// Rows: this folder, docs/, junk/, notes.txt.
	press(a, "space")         // tick ~ itself; cursor moves to docs/
	press(a, "down", "space") // untick junk/
	press(a, "enter")
	f, ok := a.modal.(*sourceForm)
	if !ok {
		t.Fatalf("enter in the picker: modal %T, want the form", a.modal)
	}
	if got := f.name.Value(); got != "home" {
		t.Errorf("default name %q, want home", got)
	}
	pressSave(a)
	if a.modal != nil {
		t.Fatalf("after save: modal still open, err %v", a.modal.(*sourceForm).err)
	}
	s, ok := sourceNamed(a, "home")
	if !ok {
		t.Fatalf("no source named home in %v", a.sources)
	}
	if !slices.Equal(s.Paths, []string{home}) || !slices.Equal(s.Excludes, []string{filepath.Join(home, "junk")}) {
		t.Errorf("paths %q excludes %q", s.Paths, s.Excludes)
	}
	if selSrc(t, a).ID != s.ID {
		t.Errorf("the new source isn't selected")
	}
}

// Edit: the form reads the source back into ticks, and a schedule change
// is all that's sent.
func TestEditSourceSchedule(t *testing.T) {
	a := newTestApp(t, 160, 40)
	before := a.sources[0]
	press(a, "e")
	f, ok := a.modal.(*sourceForm)
	if !ok {
		t.Fatalf("e: modal %T", a.modal)
	}
	if paths, _ := f.sel.Compile(); !slices.Equal(paths, before.Paths) {
		t.Errorf("form ticks %q, source paths %q", paths, before.Paths)
	}
	press(a, "tab", "tab", "tab") // name → folders → also skip → schedule
	if f.field != ffSchedule {
		t.Fatalf("field %d, want schedule", f.field)
	}
	press(a, "right") // hourly → every 2h
	pressSave(a)
	if a.modal != nil {
		t.Fatalf("save failed: %v", f.err)
	}
	after := selSrc(t, a)
	if after.ID != before.ID || after.Schedule == nil || after.Schedule.Every != 7200 || slices.Contains(after.Inherited, "schedule") {
		t.Errorf("after: schedule %v inherited %v", after.Schedule, after.Inherited)
	}
	if !slices.Equal(after.Excludes, before.Excludes) || after.Name != before.Name {
		t.Errorf("other fields changed: %q %q", after.Name, after.Excludes)
	}
}

func TestRemoveSourceKeepsSnapshotsByDefault(t *testing.T) {
	a := newTestApp(t, 160, 40)
	n := len(a.sources)
	gone := a.sources[0]
	press(a, "x")
	if _, ok := a.modal.(*confirm); !ok {
		t.Fatalf("x: modal %T", a.modal)
	}
	press(a, "enter")
	if len(a.sources) != n-1 {
		t.Fatalf("sources %d, want %d", len(a.sources), n-1)
	}
	if _, still := sourceNamed(a, gone.Name); still {
		t.Errorf("%s still listed", gone.Name)
	}
	// Esc on the dangerous path cancels.
	press(a, "x", "right", "enter") // delete snapshots → second question
	press(a, "esc")
	if len(a.sources) != n-1 {
		t.Errorf("esc on the second question removed a source")
	}
}

// Back up now, then live events: progress while it runs, a fresh last
// run when it's done.
func TestBackupNowFollowsEvents(t *testing.T) {
	a := newTestApp(t, 160, 40)
	src := a.sources[0]
	press(a, "b")
	sawProgress := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pumpEvents(a, 100*time.Millisecond)
		if running, p := a.jobFor(src); running && p > 0 {
			sawProgress = true
			if !strings.Contains(a.renderColumns(), "%") {
				t.Errorf("progress not shown in the Sources column")
			}
		}
		if strings.HasPrefix(a.statusMsg, "Finished backup") {
			break
		}
	}
	if !sawProgress {
		t.Errorf("no progress seen")
	}
	if !strings.HasPrefix(a.statusMsg, "Finished backup") {
		t.Fatalf("status %q, want a finished note", a.statusMsg)
	}
	if s, _ := a.sourceByID(src.ID); s.LastRun == nil || time.Since(s.LastRun.Ended) > time.Minute {
		t.Errorf("last run not updated: %+v", s.LastRun)
	}
}
