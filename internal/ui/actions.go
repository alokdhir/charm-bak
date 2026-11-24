package ui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// runAction does what an action-bar button (or its hotkey) says.
func (a *App) runAction(act action) tea.Cmd {
	a.statusMsg = ""
	// An action that needs its machine says so when it's offline.
	if act.needs != "" && !a.hostUp(a.selectionHost(), act.needs) {
		return nil
	}
	switch {
	case act.key.Keys()[0] == keyRemote.Keys()[0] && a.col == colSources: // r is Restore elsewhere
		return a.startAddRemote()
	case act.key.Keys()[0] == keyAdd.Keys()[0] && a.col == colSources:
		return a.startAdd()
	case act.key.Keys()[0] == keyEdit.Keys()[0] && a.col == colSources && a.onHeader():
		return a.startMachineEdit()
	case act.key.Keys()[0] == keyEdit.Keys()[0] && a.col == colSources:
		return a.startEdit(ffName)
	case act.key.Keys()[0] == keyRemove.Keys()[0] && a.col == colSources && a.onHeader():
		return a.startHostRemove()
	case act.key.Keys()[0] == keyRemove.Keys()[0] && a.col == colSources:
		return a.startRemove()
	case act.key.Keys()[0] == keyBackup.Keys()[0] && a.col == colSources:
		return a.backupNow()
	case act.key.Keys()[0] == keyEdit.Keys()[0] && a.col == colSnapshots:
		return a.startNote()
	case act.key.Keys()[0] == keyPin.Keys()[0] && a.col == colSnapshots:
		return a.togglePin()
	case act.key.Keys()[0] == keyChanges.Keys()[0] && a.col == colSnapshots:
		return a.startChanges()
	case act.key.Keys()[0] == keyDiff.Keys()[0]:
		return a.startDiff()
	case act.key.Keys()[0] == keyExclude.Keys()[0] && a.col >= colTree:
		return a.startExclude()
	case act.key.Keys()[0] == keyFinder.Keys()[0]:
		return a.showInFinder()
	case act.key.Keys()[0] == keyRestore.Keys()[0]:
		if n, ok := a.restoreTarget(); ok {
			return a.startRestore(n, false)
		}
		return nil
	}
	a.statusMsg = act.label + " isn't built yet"
	return nil
}

// startAdd opens the picker at ~; once something is ticked, the form.
func (a *App) startAdd() tea.Cmd { return a.startAddFor(a.addHost()) }

// startAddFor adds sources on host.
func (a *App) startAddFor(host string) tea.Cmd {
	start := a.homeFor(host)
	if start == "" {
		start = "/"
	}
	p := a.newFolderPicker(host, start, backup.NewSelection(), func(a *App, sel *backup.Selection) tea.Cmd {
		f := a.newSourceForm(host, nil, sel)
		f.name.SetValue(defaultSourceName(a.homeFor(host), mustPaths(sel)))
		a.modal = f
		return f.focus()
	}, nil)
	a.modal = p
	return p.load()
}

// startEdit opens the selected source's form at field.
func (a *App) startEdit(field int) tea.Cmd {
	src, ok := a.selSource()
	if !ok {
		return nil
	}
	f := a.newSourceForm(src.Host, &src, nil)
	f.field = field
	a.modal = f
	return f.focus()
}

func (a *App) startRemove() tea.Cmd {
	src, ok := a.selSource()
	if !ok {
		return nil
	}
	count := "its snapshots"
	if a.snapsFor == sourceKey(src) {
		count = "its " + pluralize(len(a.snaps), "snapshot")
	}
	a.modal = &confirm{
		title:   "Remove " + src.Name,
		body:    fmt.Sprintf("Backups of %s stop. Keep %s in the repository, browsable and restorable, or delete them?", src.Name, count),
		answers: []string{"Keep snapshots", "Delete snapshots", "Cancel"},
		danger:  map[int]bool{1: true},
		act: func(a *App, answer int) tea.Cmd {
			switch answer {
			case 0:
				return a.removeSource(src, true)
			case 1:
				a.modal = &confirm{
					title:   "Delete snapshots of " + src.Name,
					body:    fmt.Sprintf("Delete %s of %s? Their data is removed at the next weekly cleanup. This can't be undone.", count, src.Name),
					answers: []string{"Delete", "Cancel"},
					pick:    1,
					danger:  map[int]bool{0: true},
					act: func(a *App, answer int) tea.Cmd {
						if answer == 0 {
							return a.removeSource(src, false)
						}
						return nil
					},
				}
			}
			return nil
		},
	}
	return nil
}

type changedMsg struct {
	note string
	err  error
}

func (a *App) removeSource(src protocol.Source, keep bool) tea.Cmd {
	repo := a.repo.ForHost(src.Host)
	return func() tea.Msg {
		err := repo.RemoveSource(context.Background(), src.ID, keep)
		note := "Removed " + src.Name + "; its snapshots are kept"
		if !keep {
			note = "Removed " + src.Name + " and deleted its snapshots"
		}
		return changedMsg{note, err}
	}
}

func (a *App) backupNow() tea.Cmd {
	src, ok := a.selSource()
	if !ok {
		return nil
	}
	if running, _ := a.jobFor(src); running {
		a.statusMsg = src.Name + " is already backing up"
		return nil
	}
	repo := a.repo.ForHost(src.Host)
	queued := a.job != nil
	return func() tea.Msg {
		_, err := repo.BackupNow(context.Background(), src.ID)
		note := "Backing up " + src.Name
		if queued {
			note = "Backup of " + src.Name + " queued after the running job"
		}
		return changedMsg{note, err}
	}
}

// mustPaths is what a selection backs up, nothing if it doesn't compile.
func mustPaths(sel *backup.Selection) []string {
	paths, _ := sel.Compile()
	return paths
}
