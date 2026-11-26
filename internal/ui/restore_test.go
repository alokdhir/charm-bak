package ui

import (
	"path/filepath"
	"strings"
	"testing"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// r in a tree column restores a copy into ~/Downloads by default, never
// overwriting.
func TestRestoreCopy(t *testing.T) {
	home := fakeHome(t)
	a := newTestApp(t, 160, 40)
	press(a, "right", "right") // into the tree
	n, ok := a.selectedEntry()
	if !ok {
		t.Fatal("no tree entry")
	}
	press(a, "r")
	if _, ok := a.modal.(*restoreForm); !ok {
		t.Fatalf("r: modal %T, want the restore form", a.modal)
	}
	pressSave(a)
	if a.modal != nil {
		t.Fatalf("enter: modal %T still open", a.modal)
	}
	got := a.repo.(*backup.Fake).Restores
	if len(got) != 1 {
		t.Fatalf("restores %v", got)
	}
	p := got[0]
	if p.Snapshot != n.snap || len(p.Paths) != 1 || p.Paths[0] != n.path || p.Overwrite != protocol.OverwriteNever {
		t.Errorf("params %+v for %s", p, n.path)
	}
	if !strings.HasPrefix(p.Target, filepath.Join(home, "Downloads", "Restored ")) {
		t.Errorf("target %q", p.Target)
	}
	if !strings.HasPrefix(a.statusMsg, "Restoring "+entryName(n.Entry)+" to ~/Downloads/") {
		t.Errorf("status %q", a.statusMsg)
	}
}

// r in the diff popup restores in place; replacing asks first, and
// Cancel goes back to the form without restoring.
func TestRestoreInPlaceFromDiff(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right", "d", "r")
	f, ok := a.modal.(*restoreForm)
	if !ok || !f.inPlace {
		t.Fatalf("r in diff: modal %T, want the form in place", a.modal)
	}
	pressSave(a)
	if _, ok := a.modal.(*confirm); !ok {
		t.Fatalf("replace in place didn't ask: modal %T", a.modal)
	}
	press(a, "esc") // Cancel
	if a.modal != f || len(a.repo.(*backup.Fake).Restores) != 0 {
		t.Fatalf("cancel: modal %T, restores %v", a.modal, a.repo.(*backup.Fake).Restores)
	}
	press(a, "enter", "enter") // Cancel is preselected: Enter, Enter overwrites nothing
	if a.modal != f || len(a.repo.(*backup.Fake).Restores) != 0 {
		t.Fatalf("enter on the preselected answer restored: %v", a.repo.(*backup.Fake).Restores)
	}
	press(a, "enter", "left", "enter") // Replace has to be chosen
	got := a.repo.(*backup.Fake).Restores
	if len(got) != 1 || got[0].Target != protocol.RestoreOriginal || got[0].Overwrite != protocol.OverwriteAlways {
		t.Fatalf("restores %+v", got)
	}
}

// A relative folder is refused in the form, before asking the daemon.
func TestRestoreRejectsRelativeFolder(t *testing.T) {
	fakeHome(t)
	a := newTestApp(t, 160, 40)
	press(a, "right", "r", "down")
	f := a.modal.(*restoreForm)
	f.folder().SetValue("somewhere")
	pressSave(a)
	if f.err == nil || len(a.repo.(*backup.Fake).Restores) != 0 {
		t.Errorf("err %v, restores %v", f.err, a.repo.(*backup.Fake).Restores)
	}
}

// The folder a copy goes into is shown ending in /, so it can't be read
// as a file by that name; a restored folder's landing path ends in / too,
// a file's doesn't, and the request's target has no trailing slash.
func TestRestoreShowsFoldersWithSlash(t *testing.T) {
	fakeHome(t)
	a := newTestApp(t, 160, 40)
	press(a, "right", "right")
	n, ok := a.selectedEntry()
	if !ok {
		t.Fatal("no tree entry")
	}
	press(a, "r")
	f := a.modal.(*restoreForm)
	if v := f.folder().Value(); !strings.HasSuffix(v, "/") || !strings.HasPrefix(v, "~/Downloads/Restored ") {
		t.Errorf("folder %q", v)
	}
	p, what, err := f.params()
	if err != nil || strings.HasSuffix(p.Target, "/") {
		t.Fatalf("target %q, %v", p.Target, err)
	}
	if got := strings.HasSuffix(what, "/"); got != n.IsDir() {
		t.Errorf("%q: ends in / %v, a folder %v", what, got, n.IsDir())
	}
	f.n.Type = protocol.TypeDir
	if !strings.HasSuffix(f.landsAs("", p.Target), "/") {
		t.Error("a folder's landing path has no /")
	}
	f.n.Type = protocol.TypeFile
	if strings.HasSuffix(f.landsAs("", p.Target), "/") {
		t.Error("a file's landing path ends in /")
	}
}

// A remote's restore folder may be a Windows path; it's shown with
// Windows separators and sent as typed.
func TestRestoreToWindowsFolder(t *testing.T) {
	f := &restoreForm{n: node{Entry: protocol.Entry{Name: "photos", Type: protocol.TypeDir}}}
	if !isDrivePath(`C:\Restored`) || !isDrivePath("d:/x") || isDrivePath("C:") || isDrivePath("/data") {
		t.Error("isDrivePath")
	}
	if got := f.landsAs("", `C:\Restored\`); got != `C:\Restored\photos\` {
		t.Errorf("landsAs %q", got)
	}
}
