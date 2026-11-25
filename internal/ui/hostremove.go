package ui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
)

type hostRemovedMsg struct {
	host string
	res  protocol.HostRemoveResult
	err  error
}

// startHostRemove asks before removing the selected remote altogether:
// keep its backups (the default) or delete them, with a second confirm
// for deleting. One that isn't answering can only be unpaired here.
func (a *App) startHostRemove() tea.Cmd {
	h, ok := a.selHost()
	if !ok || h.Local {
		return nil
	}
	name := h.Name
	sources := pluralize(a.hostSourceCount(name), "source")
	if h.State != protocol.HostOK {
		a.modal = &confirm{
			title:   "Remove " + name,
			body:    fmt.Sprintf("%s isn't answering, so it can only be unpaired here. It keeps running there, with its %s, until it's removed by hand; the commands are shown next. Its backups stay.", name, sources),
			answers: []string{"Unpair", "Cancel"},
			danger:  map[int]bool{0: true},
			act: func(a *App, answer int) tea.Cmd {
				if answer == 0 {
					return a.removeHost(name, false)
				}
				return nil
			},
		}
		return nil
	}
	where := "its repository"
	if st, ok := a.hostStatus[name]; ok && st.Repository != nil {
		where = st.Repository.Location
	}
	a.modal = &confirm{
		title: "Remove " + name,
		body: fmt.Sprintf("charm-bak is removed from %s: its %s, its schedule and the service itself. It's unpaired from this Mac. "+
			"Keep its backups in %s, to restore from or connect to later, or delete them?", name, sources, where),
		answers: []string{"Keep backups", "Delete backups", "Cancel"},
		danger:  map[int]bool{1: true},
		act: func(a *App, answer int) tea.Cmd {
			switch answer {
			case 0:
				return a.removeHost(name, false)
			case 1:
				a.modal = &confirm{
					title:   "Delete " + name + "'s backups",
					body:    fmt.Sprintf("Delete every backup of %s, and this Mac's copy of its recovery key? This can't be undone.", name),
					answers: []string{"Delete", "Cancel"},
					pick:    1,
					danger:  map[int]bool{0: true},
					act: func(a *App, answer int) tea.Cmd {
						if answer == 0 {
							return a.removeHost(name, true)
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

func (a *App) removeHost(name string, deleteBackups bool) tea.Cmd {
	a.statusMsg = "Removing " + name + "…"
	repo := a.repo
	return func() tea.Msg {
		res, err := repo.RemoveHost(context.Background(), protocol.HostRemoveParams{Name: name, DeleteBackups: deleteBackups})
		return hostRemovedMsg{name, res, err}
	}
}

// hostRemoved reports the outcome: a status line, or, when the remote
// couldn't uninstall itself, what to run there.
func (a *App) hostRemoved(msg hostRemovedMsg) tea.Cmd {
	switch {
	case msg.err != nil:
		a.statusMsg = "error: " + msg.err.Error()
		return nil
	case msg.res.Message != "":
		// Not uninstalled: what to run there. Uninstalled: what's left
		// elsewhere (its AWS user, when it couldn't be deleted).
		title := msg.host + " is still installed"
		a.statusMsg = msg.host + " unpaired"
		if msg.res.Uninstalled {
			title, a.statusMsg = msg.host+" removed", msg.host+" removed"
		}
		a.ask(&confirm{title: title, body: msg.res.Message, answers: []string{"OK"},
			act: func(*App, int) tea.Cmd { return nil }})
	default:
		a.statusMsg = msg.host + " removed"
	}
	delete(a.collapsed, msg.host)
	return tea.Batch(a.loadSources(), a.loadStatus())
}
