package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/protocol"
)

func diffText(ls []diffLine) string {
	var b strings.Builder
	for _, l := range ls {
		if l.gap {
			b.WriteString("@" + string(rune('0'+l.line)) + "\n")
			continue
		}
		b.WriteString(string(l.op) + l.text + "\n")
	}
	return b.String()
}

func TestLineDiff(t *testing.T) {
	a := strings.Split("1 2 3 4 5 6 7 8 9", " ")
	b := strings.Split("1 2 3 4 X 6 7 8 9 10", " ")
	got := diffText(lineDiff(a, b, 1))
	want := "@4\n 4\n-5\n+X\n 6\n@9\n 9\n+10\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := lineDiff(a, a, 3); len(got) != 0 {
		t.Errorf("identical: %v", got)
	}
	// Past lcsMax the middle is all removed then all added, still correct.
	big := make([]string, lcsMax+1)
	other := make([]string, lcsMax+1)
	for i := range big {
		big[i], other[i] = "a", "b"
	}
	d := lineDiff(big, other, 0)
	if len(d) != 2*(lcsMax+1) {
		t.Errorf("big diff: %d lines", len(d))
	}
}

// d on a fixture file opens the popup with a line diff; d on a folder
// lists what differs; esc closes.
func TestDiffPopup(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right") // Snapshots: d does nothing here (c lists changes)
	lines, err := folderDiff(context.Background(), a.repo, snapshotRoot(a.snaps[0]))
	if err != nil {
		t.Fatal(err)
	}
	if text := ansi.Strip(strings.Join(lines, "\n")); !strings.Contains(text, "+ new-on-disk.txt") {
		t.Errorf("folder diff misses the new file:\n%s", text)
	}

	// Pick a file in the root folder that the fake says was edited.
	press(a, "right")
	lv := a.levels[0]
	changes, err := a.repo.Diff(context.Background(), lv.snap, lv.path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, e := range lv.entries {
		for _, c := range changes {
			if c.Name == e.Name && e.Type == protocol.TypeFile && c.Change == protocol.ChangeChanged && !strings.HasSuffix(e.Name, ".pdf") && !strings.HasSuffix(e.Name, ".xlsx") {
				a.levels[0].cur.pos, found = i, true
			}
		}
	}
	if !found {
		t.Fatal("no edited text file in the fixture's root folder")
	}
	press(a, "d")
	d := a.modal.(*diffView)
	text := ansi.Strip(strings.Join(d.lines, "\n"))
	for _, want := range []string{"+ line 12, edited on disk", "- line 40 of", "+ a line added on disk", "⋯ line"} {
		if !strings.Contains(text, want) {
			t.Errorf("file diff misses %q:\n%s", want, text)
		}
	}
}
