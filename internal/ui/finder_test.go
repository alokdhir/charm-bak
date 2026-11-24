package ui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// f in Snapshots or the tree reveals the live copy on this Mac; one
// that's gone shows the nearest folder above it; a remote's sources have
// no Open in Finder.
func TestOpenInFinder(t *testing.T) {
	var opened []string
	old := openFinder
	openFinder = func(p string) error { opened = append(opened, p); return nil }
	t.Cleanup(func() { openFinder = old })

	a := newTestApp(t, 160, 40)
	press(a, "right")
	if !strings.Contains(actionLabels(a), "Open in Finder") {
		t.Fatalf("actions %q", actionLabels(a))
	}
	_, cmd := a.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	drain(a, cmd)
	root := selSrc(t, a).Paths[0]
	if len(opened) != 1 || opened[0] != root {
		t.Errorf("opened %v, want %s", opened, root)
	}

	// Something no longer on disk: the folder above that is.
	n, _ := a.restoreTarget()
	a.snaps[a.snapCur.pos].Paths = []string{filepath.Join(n.path, "gone-zz", "deeper")}
	opened = nil
	drain(a, a.showInFinder())
	if len(opened) != 1 || opened[0] != n.path || !strings.Contains(a.statusMsg, "isn't on disk now") {
		t.Errorf("gone: opened %v, status %q", opened, a.statusMsg)
	}
}

// A remote source's columns don't offer Finder.
func TestNoFinderForRemote(t *testing.T) {
	a, _ := newRemoteApp(t)
	a.col, a.area = colSources, areaColumns
	for range len(a.srcRows()) {
		if src, ok := a.selSource(); ok && src.Host == "pantry" {
			break
		}
		press(a, "down")
	}
	if src, ok := a.selSource(); !ok || src.Host != "pantry" {
		t.Fatal("no pantry source")
	}
	a.col = colSnapshots
	if strings.Contains(actionLabels(a), "Finder") {
		t.Errorf("remote offers Finder: %q", actionLabels(a))
	}
}
