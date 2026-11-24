package ui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// startChanges opens the list of what the selected snapshot changed from
// the one before it (c in Snapshots): every path added, removed or
// changed, through the whole tree. It reads the repository, so it works
// for a machine that's offline. The oldest snapshot has nothing before
// it: everything in it is new.
func (a *App) startChanges() tea.Cmd {
	i := a.snapCur.pos
	if a.col != colSnapshots || i >= len(a.snaps) {
		return nil
	}
	if i+1 >= len(a.snaps) {
		a.statusMsg = "This is the first snapshot here: everything in it is new"
		return nil
	}
	snap, base := a.snaps[i], a.snaps[i+1] // newest first
	root := backup.Root(snap.Paths)
	key := snap.ID + "\x00" + base.ID
	since := "since " + stamp(a.now(), base.Time)
	a.modal = &diffView{loading: true, key: key,
		title: "Changes in " + stamp(a.now(), snap.Time),
		sub:   since + " · in " + root}
	repo := a.srcRepo()
	return func() tea.Msg {
		res, err := repo.Changes(context.Background(), snap.ID, base.ID)
		if err != nil {
			return diffLoadedMsg{key: key, err: err}
		}
		// The counts go in the subtitle, which stays put; the list
		// below is only paths.
		counts := changeCounts(res)
		return diffLoadedMsg{key: key, lines: changeLines(res, root), sub: since + " · " + counts + " · in " + root}
	}
}

// changeCounts is a changes reply in a line: what changed in content,
// then what was only touched (the same contents, different details such
// as a timestamp), then the new data, which is folder records when no
// contents changed.
func changeCounts(res protocol.ChangesResult) string {
	var parts []string
	if res.Added+res.Removed+res.Changed == 0 {
		parts = append(parts, "no contents changed")
	} else {
		parts = append(parts, fmt.Sprintf("%d changed, %d added, %d removed", res.Changed, res.Added, res.Removed))
	}
	if res.Touched > 0 {
		parts = append(parts, fmt.Sprintf("%d touched", res.Touched))
	}
	return strings.Join(append(parts, humanBytes(res.Bytes)+" new"), ", ")
}

// changeLines is a changes reply as the popup's lines: each path
// (relative to the snapshot's root) with its mark, in path order as the
// daemon sends them.
func changeLines(res protocol.ChangesResult, root string) []string {
	total := res.Added + res.Removed + res.Changed + res.Touched
	if total == 0 {
		return []string{styles.HealthStyle(styles.OK).Render("Nothing changed between these two snapshots.")}
	}
	var lines []string
	prefix := strings.TrimSuffix(root, "/") + "/"
	for _, ch := range res.Changes {
		mark, style := "~", styles.HealthStyle(styles.Changed)
		switch ch.Change {
		case protocol.ChangeAdded:
			mark, style = "+", styles.HealthStyle(styles.Added)
		case protocol.ChangeRemoved:
			mark, style = "-", styles.HealthStyle(styles.Removed)
		case protocol.ChangeTouched:
			// Same contents, different details: quieter than a change.
			mark, style = "·", styles.MutedStyle()
		}
		lines = append(lines, style.Render(mark+" "+strings.TrimPrefix(ch.Path, prefix)))
	}
	if more := total - len(res.Changes); more > 0 {
		lines = append(lines, "", styles.MutedStyle().Render(fmt.Sprintf("and %d more", more)))
	}
	return lines
}
