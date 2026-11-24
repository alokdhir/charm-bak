package ui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// A snapshot's note and pin live in the daemon (snapshot_set); everything
// else about a snapshot is fixed.

// snapshotSetMsg is the daemon's answer to a note or pin change.
type snapshotSetMsg struct {
	snap protocol.Snapshot
	what string // footer text on success
	note bool   // the note form asked (else a pin, which the App applies)
	err  error
}

// selectedSnapshot is the snapshot under the Snapshots cursor.
func (a *App) selectedSnapshot() (protocol.Snapshot, bool) {
	if a.snapCur.pos < len(a.snaps) {
		return a.snaps[a.snapCur.pos], true
	}
	return protocol.Snapshot{}, false
}

func (a *App) setSnapshot(p protocol.SnapshotSetParams, what string) tea.Cmd {
	repo := a.srcRepo()
	return func() tea.Msg {
		s, err := repo.SetSnapshot(context.Background(), p)
		return snapshotSetMsg{snap: s, what: what, note: p.Note != nil, err: err}
	}
}

// togglePin pins or unpins the selected snapshot. No confirmation: it's
// one key to undo, and unpinning only hands the snapshot back to the
// source's retention.
func (a *App) togglePin() tea.Cmd {
	s, ok := a.selectedSnapshot()
	if !ok {
		return nil
	}
	pin := !s.Pinned
	what := "Pinned: kept until you unpin it"
	if !pin {
		what = "Unpinned: retention applies again"
	}
	return a.setSnapshot(protocol.SnapshotSetParams{ID: s.ID, Pinned: &pin}, what)
}

// applySnapshot puts a changed snapshot in place.
func (a *App) applySnapshot(msg snapshotSetMsg) {
	if msg.err != nil {
		a.statusMsg = "error: " + msg.err.Error()
		return
	}
	for i := range a.snaps {
		if a.snaps[i].ID == msg.snap.ID {
			a.snaps[i] = msg.snap
		}
	}
	a.statusMsg = msg.what
}

// noteForm edits a snapshot's note. Empty clears it.
type noteForm struct {
	snap    protocol.Snapshot
	note    textinput.Model
	err     error
	sending bool
}

func (a *App) startNote() tea.Cmd {
	s, ok := a.selectedSnapshot()
	if !ok {
		return nil
	}
	f := &noteForm{snap: s, note: newTextField(s.Note, "e.g. before the macOS update")}
	a.modal = f
	return f.note.Focus()
}

func (f *noteForm) typing() bool { return true }

func (f *noteForm) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case snapshotSetMsg:
		f.sending = false
		if msg.err != nil {
			f.err = msg.err
			return nil
		}
		a.modal = nil
		a.applySnapshot(msg)
		return nil
	case tea.KeyPressMsg:
		if f.sending {
			return nil
		}
		switch {
		case key.Matches(msg, keyEsc):
			a.modal = nil
			return nil
		case key.Matches(msg, keyEnter):
			note := strings.TrimSpace(f.note.Value())
			what := "Note saved"
			if note == "" {
				what = "Note cleared"
			}
			f.sending, f.err = true, nil
			return a.setSnapshot(protocol.SnapshotSetParams{ID: f.snap.ID, Note: &note}, what)
		}
		var cmd tea.Cmd
		f.note, cmd = f.note.Update(msg)
		f.err = nil
		return cmd
	}
	return nil
}

func (f *noteForm) view(a *App) string {
	inner := min(popupInnerWidth(a.width), 70)
	var b strings.Builder
	b.WriteString(popupTitle("Note for "+stamp(a.now(), f.snap.Time)) + "\n\n")
	f.note.SetWidth(inner)
	b.WriteString(f.note.View() + "\n\n")
	switch {
	case f.sending:
		b.WriteString(styles.MutedStyle().Render("asking the daemon…") + "\n")
	case f.err != nil:
		b.WriteString(errorLine(f.err) + "\n")
	default:
		b.WriteString(styles.MutedStyle().Render("Shown with the snapshot. Leave empty to clear it.") + "\n")
	}
	b.WriteString(hints("enter", "save", "esc", "cancel"))
	return boxed(b.String(), inner)
}
