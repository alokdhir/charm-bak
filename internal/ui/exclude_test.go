package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/backup"
)

// x on a folder in the tree asks, then leaves it out of the source from
// then on, as unticking it in the picker would; the source keeps its
// other excludes.
func TestExcludeFromTree(t *testing.T) {
	a := newTestApp(t, 160, 40)
	src := selSrc(t, a)
	press(a, "right", "right") // into the tree
	n, ok := a.selectedEntry()
	if !ok {
		t.Fatal("no tree selection")
	}
	if !strings.Contains(actionLabels(a), "Exclude") {
		t.Fatalf("actions %q", actionLabels(a))
	}
	press(a, "x")
	if _, ok := a.modal.(*confirm); !ok {
		t.Fatalf("modal %T", a.modal)
	}
	_, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(a, cmd)
	var got []string
	for _, s := range a.sources {
		if s.ID == src.ID {
			got = s.Excludes
		}
	}
	if !slices.Contains(got, n.path) {
		t.Errorf("excludes %v lack %s", got, n.path)
	}
	for _, e := range src.Excludes {
		if !slices.Contains(got, e) {
			t.Errorf("lost exclude %q: %v", e, got)
		}
	}
	if !strings.Contains(a.statusMsg, "Excluded") {
		t.Errorf("status %q", a.statusMsg)
	}
	// Again: it's excluded already, so x goes straight to the past.
	press(a, "x")
	if c, ok := a.modal.(*confirm); !ok || c.title != "Remove from past snapshots" || c.pick != 1 {
		t.Errorf("again: modal %T, status %q", a.modal, a.statusMsg)
	}
}

// "Also from past snapshots" excludes it, counts the snapshots that have
// it, and asks again (Cancel preselected) before the rewrite job.
func TestExcludeFromPastSnapshots(t *testing.T) {
	f := backup.NewFake(testNow)
	a := NewApp(f, func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drain(a, a.Init())
	press(a, "right", "right")
	n, _ := a.selectedEntry()
	press(a, "x", "right")
	_, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(a, cmd)
	c, ok := a.modal.(*confirm)
	if !ok || c.pick != 1 || !strings.Contains(c.body, "snapshot") {
		t.Fatalf("second confirm: %T %+v", a.modal, c)
	}
	if len(f.Rewrites) != 0 {
		t.Fatal("rewrote before the second yes")
	}
	press(a, "left")
	_, cmd = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(a, cmd)
	if len(f.Rewrites) != 1 || !slices.Equal(f.Rewrites[0].Excludes, []string{n.path}) {
		t.Fatalf("rewrites %+v", f.Rewrites)
	}
	if !strings.Contains(a.statusMsg, "Removing") {
		t.Errorf("status %q", a.statusMsg)
	}
}
