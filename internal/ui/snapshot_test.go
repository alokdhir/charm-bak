package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// p pins and unpins the selected snapshot; the row and the fake follow.
func TestPinToggles(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right") // Snapshots
	if a.col != colSnapshots || a.snaps[0].Pinned {
		t.Fatalf("setup: col %d, pinned %v", a.col, a.snaps[0].Pinned)
	}
	press(a, "p")
	if !a.snaps[0].Pinned {
		t.Fatalf("p didn't pin; status %q", a.statusMsg)
	}
	press(a, "p")
	if a.snaps[0].Pinned {
		t.Fatalf("second p didn't unpin")
	}
}

// e edits the note; Enter saves, an empty note clears it, Esc changes
// nothing.
func TestEditSnapshotNote(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "e")
	if _, ok := a.modal.(*noteForm); !ok {
		t.Fatalf("e: modal %T, want the note form", a.modal)
	}
	press(a, "h", "i", "enter")
	if a.modal != nil || a.snaps[0].Note != "hi" {
		t.Fatalf("note %q, modal %T", a.snaps[0].Note, a.modal)
	}
	press(a, "e", "x", "esc")
	if a.snaps[0].Note != "hi" {
		t.Fatalf("esc changed the note to %q", a.snaps[0].Note)
	}
	press(a, "e")
	nf := a.modal.(*noteForm)
	nf.note.SetValue("")
	press(a, "enter")
	if a.snaps[0].Note != "" {
		t.Fatalf("empty didn't clear: %q", a.snaps[0].Note)
	}
	// The fake kept it too: a reload shows the same.
	snaps, _ := a.repo.Snapshots(t.Context(), selSrc(t, a).ID)
	if snaps[0].Note != "" {
		t.Errorf("repo note %q", snaps[0].Note)
	}
}

// A pinned snapshot stays red under the cursor, on the cursor's purple.
func TestPinnedRowKeepsRedUnderCursor(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right")
	i := slices.IndexFunc(a.snaps, func(s protocol.Snapshot) bool { return s.Pinned })
	if i < 0 {
		t.Fatal("fixture has no pinned snapshot")
	}
	for range i {
		press(a, "down")
	}
	row := strings.Split(a.renderColumn(colSnapshots, 30, 10), "\n")[1+i]
	want := styles.SelectedStyle().Foreground(styles.Pinned).Bold(true).Render(ansi.Strip(row))
	if !strings.Contains(row, want) {
		t.Errorf("pinned cursor row %q, want %q", row, want)
	}
}
