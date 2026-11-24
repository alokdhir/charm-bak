package ui

import (
	"strings"
	"testing"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// c on a snapshot lists what it changed from the one before it (the next
// row down), marked + - ~ with paths under the snapshot's root; d no
// longer does anything there; the oldest snapshot has nothing to compare.
func TestChangesSinceThePreviousSnapshot(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right")
	if a.col != colSnapshots || len(a.snaps) < 2 {
		t.Fatalf("col %d, %d snapshots", a.col, len(a.snaps))
	}
	if bar := a.renderActionBar(); !strings.Contains(bar, "hanges") || strings.Contains(bar, "iff") {
		t.Errorf("snapshot actions: %q", bar)
	}
	press(a, "d")
	if a.modal != nil {
		t.Fatalf("d on a snapshot opened %T", a.modal)
	}
	press(a, "c")
	d, ok := a.modal.(*diffView)
	if !ok || d.loading || d.err != nil {
		t.Fatalf("c: modal %T, %+v", a.modal, d)
	}
	asked := a.repo.(*backup.Fake).ChangesAsked
	if len(asked) != 1 || asked[0] != [2]string{a.snaps[0].ID, a.snaps[1].ID} {
		t.Errorf("asked %v, want the selected snapshot against the one before", asked)
	}
	view := d.view(a)
	for _, want := range []string{"Changes in", "1 changed, 1 added, 1 removed, 1 touched", "~ app.go", "+ notes/", "- old.txt", "· touched.txt"} {
		if !strings.Contains(view, want) {
			t.Errorf("popup lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "restore") {
		t.Error("a changes list offers restore")
	}
	press(a, "c")
	if a.modal != nil {
		t.Error("c didn't close it")
	}

	for range len(a.snaps) {
		press(a, "down")
	}
	press(a, "c")
	if a.modal != nil || !strings.Contains(a.statusMsg, "first snapshot") {
		t.Errorf("oldest snapshot: modal %T, status %q", a.modal, a.statusMsg)
	}
}

// With info open, ←→ move across columns and it follows; c opens Changes
// over it.
func TestInfoFollowsAcrossColumns(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "i", "right")
	if !a.showInfo || a.col != colSnapshots {
		t.Fatalf("after →: info %v, col %d", a.showInfo, a.col)
	}
	// c opens Changes in front of info; closing it leaves info there.
	press(a, "c")
	if _, ok := a.modal.(*diffView); !ok || !a.showInfo {
		t.Fatalf("c with info open: modal %T, info %v", a.modal, a.showInfo)
	}
	if frame := a.View().Content; !strings.Contains(frame, "Changes in") {
		t.Error("the changes list isn't drawn in front")
	}
	press(a, "down") // scrolls the list, not the snapshot cursor
	if a.snapCur.pos != 0 {
		t.Errorf("a key meant for Changes moved the snapshot cursor to %d", a.snapCur.pos)
	}
	press(a, "esc")
	if a.modal != nil || !a.showInfo {
		t.Fatalf("after closing Changes: modal %T, info %v", a.modal, a.showInfo)
	}
	press(a, "left")
	if !a.showInfo || a.col != colSources {
		t.Errorf("after ←: info %v, col %d", a.showInfo, a.col)
	}
}

// A backup that only touched files (same contents, new timestamps) says
// so, so "nothing changed" beside new data doesn't look like a mistake.
func TestChangeCountsWhenOnlyTouched(t *testing.T) {
	got := changeCounts(protocol.ChangesResult{Touched: 2, Bytes: 140500})
	if got != "no contents changed, 2 touched, 140.5 KB new" {
		t.Errorf("%q", got)
	}
	lines := changeLines(protocol.ChangesResult{Touched: 1, Changes: []protocol.PathChange{{Path: "/etc/default/x", Change: protocol.ChangeTouched}}}, "/etc")
	if len(lines) != 1 || !strings.Contains(lines[0], "· default/x") {
		t.Errorf("lines %q", lines)
	}
}
