package ui

import (
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
)

// A remote that isn't answering is still browsable: this Mac's daemon
// reads every remote's repository itself (Alok, 2026-11-24: that's when
// its backups are needed most), so snapshots, files, previews and restore
// onto this Mac work as for any source. What changes the remote or reads
// its disk (add, edit, remove, back up now, pin, note, exclude, diff)
// needs it: those say so in a dialog instead of starting, and their
// action-bar buttons are dimmed. An action says it needs its machine by
// naming itself in action.needs; the dimming and the dialog both read that.

// hostDown reports whether host is a remote that isn't answering.
func (a *App) hostDown(host string) bool {
	return a.isRemote(host) && a.findHost(host).State != protocol.HostOK
}

// hostUp reports whether something that needs host's daemon can start.
// When it can't, it opens the dialog saying so (what names the action,
// "Backing up now") over whatever is open, which comes back on OK.
func (a *App) hostUp(host, what string) bool {
	if !a.hostDown(host) {
		return true
	}
	why := host + " is offline."
	if a.findHost(host).State == hostConnecting {
		why = host + " isn't connected yet."
	}
	back := a.modal
	a.modal = &confirm{
		title:   host,
		body:    why + " " + what + " needs " + host + ": try again when it's back.",
		answers: []string{"OK"},
		act: func(a *App, _ int) tea.Cmd {
			a.modal = back
			return nil
		},
	}
	return false
}

// selectionHost is the machine the focused selection belongs to: the
// header's, or the selected source's.
func (a *App) selectionHost() string {
	r, ok := a.selRow()
	if !ok {
		return a.localHost()
	}
	return r.host
}

// blocked reports whether act can't run now because its machine is
// offline: it's drawn dimmed, and its key opens the dialog.
func (a *App) blocked(act action) bool {
	return act.needs != "" && a.hostDown(a.selectionHost())
}
