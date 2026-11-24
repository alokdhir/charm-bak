package ui

import (
	"os"
	"os/exec"
	"path/filepath"

	tea "charm.land/bubbletea/v2"
)

// openFinder reveals path in Finder (open -R selects it in its folder).
// A variable so tests don't open windows.
var openFinder = func(path string) error { return exec.Command("open", "-R", path).Run() }

// withFinder adds Finder to a snapshot or tree column's actions when the
// selection's source is on this Mac: Finder shows this Mac's disk only.
func (a *App) withFinder(acts []action) []action {
	if src, ok := a.selSource(); ok && src.Host == a.localHost() {
		return append(acts, action{label: "Open in Finder", key: keyFinder})
	}
	return acts
}

type finderMsg struct{ status string }

// showInFinder reveals the selection's live copy on this Mac. When it's
// gone from disk, the nearest folder above it that's still there.
func (a *App) showInFinder() tea.Cmd {
	src, ok := a.selSource()
	if !ok || src.Host != a.localHost() {
		return nil
	}
	n, ok := a.restoreTarget()
	if !ok || n.path == "" {
		return nil
	}
	home := a.homeFor(src.Host)
	return func() tea.Msg {
		path, status := filepath.Clean(n.path), ""
		for {
			if _, err := os.Lstat(path); err == nil {
				break
			}
			parent := filepath.Dir(path)
			if parent == path {
				return finderMsg{shortPath(home, n.path) + " isn't on disk, nor any folder above it"}
			}
			path = parent
			status = shortPath(home, n.path) + " isn't on disk now; showing " + shortPath(home, path)
		}
		if err := openFinder(path); err != nil {
			return finderMsg{"Finder: " + err.Error()}
		}
		return finderMsg{status}
	}
}
