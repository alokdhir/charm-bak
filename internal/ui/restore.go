package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// restoreForm is the Restore popup: what (fixed), where (a copy in a
// folder, or in place) and, in place, what to do with files on disk.
// Copies never overwrite; replacing in place asks first.
//
// A remote's snapshot can land on either machine ("Restore to"): on the
// remote, its own daemon restores (a full path there, or in place); on
// this Mac, this Mac's daemon reads the remote's repository itself and
// writes a copy in a folder here (RestoreParams.From), which is the only
// choice while the remote is offline.
type restoreForm struct {
	n        node
	host     string // the source's machine
	remote   bool   // host is another machine
	offline  bool   // and it isn't answering: only this Mac can restore
	here     bool   // a remote's snapshot, restored onto this Mac
	home     string // this Mac's home, for ~ in a folder here
	snapTime time.Time
	field    int
	inPlace  bool
	replace  bool // in place: overwrite files that differ
	// folder is the copy's folder on the machine it lands on: one input
	// per machine, so switching "Restore to" keeps what was typed.
	folderHere, folderThere textinput.Model
	buttons                 formButtons
	err                     error
	sending                 bool
}

const (
	rfTo = iota // a remote's snapshot: which machine
	rfWhere
	rfFolder
	rfExisting
	rfButtons
)

// local reports whether the restore writes this Mac's disk.
func (f *restoreForm) local() bool { return !f.remote || f.here }

// folder is the input for the machine the restore lands on.
func (f *restoreForm) folder() *textinput.Model {
	if f.local() {
		return &f.folderHere
	}
	return &f.folderThere
}

// restoreQueuedMsg is the app's answer to a restore request.
type restoreQueuedMsg struct {
	job  protocol.Job
	what string // "main.go to ~/Downloads/…", for the footer
	err  error
}

// restoreTarget picks what r restores: the selected tree entry, or the
// whole snapshot when Snapshots has focus.
func (a *App) restoreTarget() (node, bool) {
	switch {
	case a.col == colSnapshots && a.snapCur.pos < len(a.snaps):
		return snapshotRoot(a.snaps[a.snapCur.pos]), true
	case a.col >= colTree:
		return a.selectedEntry()
	}
	return node{}, false
}

// startRestore opens the popup for n. inPlace preselects putting it back
// where it was (from the diff popup, where that's the point).
func (a *App) startRestore(n node, inPlace bool) tea.Cmd {
	var when time.Time
	for _, s := range a.snaps {
		if s.ID == n.snap {
			when = s.Time
		}
	}
	host := a.localHost()
	if src, ok := a.selSource(); ok && src.Host != "" {
		host = src.Host
	}
	f := &restoreForm{n: n, host: host, remote: a.isRemote(host), offline: a.hostDown(host), snapTime: when, inPlace: inPlace, replace: true, buttons: newFormButtons("Restore")}
	f.home, _ = os.UserHomeDir()
	// A folder here defaults to this Mac's ~/Downloads; one on a remote
	// is a full path there, with nothing filled in (this Mac's home
	// isn't a place on it).
	def := filepath.Join(f.home, "Downloads", "Restored "+when.Local().Format("2006-01-02 15.04"))
	// The trailing / says it's a folder the copy goes into, not a file
	// by that name (Alok, 2026-11-24).
	f.folderHere = newTextField(shortPath(f.home, def)+"/", "a folder")
	f.folderThere = newTextField("", "a folder on "+host+", e.g. /data/restored or C:\\Restored")
	if f.offline {
		f.here, f.inPlace = true, false
	}
	f.field = f.fields()[0]
	a.modal = f
	return f.focus()
}

// fields are the cursor's stops. "Restore to" is one only when there's a
// choice; Where isn't one for a copy onto this Mac from a remote (a
// remote's files have no original place here).
func (f *restoreForm) fields() []int {
	var fs []int
	if f.remote && !f.offline {
		fs = append(fs, rfTo)
	}
	if !f.here {
		fs = append(fs, rfWhere)
	}
	if f.inPlace && !f.here {
		return append(fs, rfExisting, rfButtons)
	}
	return append(fs, rfFolder, rfButtons)
}

func (f *restoreForm) focus() tea.Cmd {
	f.folderHere.Blur()
	f.folderThere.Blur()
	if f.field == rfFolder {
		return f.folder().Focus()
	}
	return nil
}

func (f *restoreForm) typing() bool { return f.field == rfFolder }

func (f *restoreForm) move(delta int) tea.Cmd {
	fs := f.fields()
	i := 0
	for j, x := range fs {
		if x == f.field {
			i = j
		}
	}
	next := fs[min(max(i+delta, 0), len(fs)-1)]
	if next == rfButtons && f.field != rfButtons {
		f.buttons.pick = 0
	}
	f.field = next
	return f.focus()
}

func (f *restoreForm) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case restoreQueuedMsg:
		f.sending = false
		if msg.err != nil {
			f.err = msg.err
			return nil
		}
		a.modal = nil
		a.restoring[msg.job.ID] = msg.what
		a.statusMsg = "Restoring " + msg.what
		if a.job != nil && a.job.ID != msg.job.ID {
			a.statusMsg += ", after the running job"
		}
		return a.loadStatus()
	case tea.KeyPressMsg:
		if f.sending {
			return nil
		}
		return f.key(a, msg)
	}
	return nil
}

func (f *restoreForm) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, keyEsc):
		a.modal = nil
		return nil
	case key.Matches(msg, keyNext), msg.String() == "down":
		return f.move(1)
	case key.Matches(msg, keyPrev), msg.String() == "up":
		return f.move(-1)
	case f.field == rfButtons:
		switch f.buttons.key(msg) {
		case 0:
			return f.submit(a)
		case 1:
			a.modal = nil
		}
		return nil
	case key.Matches(msg, keyEnter):
		return f.move(1)
	}
	flip := msg.String() == "left" || msg.String() == "right" || msg.String() == "h" || msg.String() == "l" || msg.String() == "space"
	switch f.field {
	case rfTo:
		if flip {
			f.here = !f.here
			f.err = nil
		}
	case rfWhere:
		if flip {
			f.inPlace = !f.inPlace
			f.err = nil
		}
	case rfExisting:
		if flip {
			f.replace = !f.replace
		}
	case rfFolder:
		var cmd tea.Cmd
		in := f.folder()
		*in, cmd = in.Update(msg)
		f.err = nil
		return cmd
	}
	return nil
}

// params is the request the form describes, and how to describe it.
func (f *restoreForm) params() (protocol.RestoreParams, string, error) {
	p := protocol.RestoreParams{Snapshot: f.n.snap, Paths: []string{f.n.path}}
	name := entryName(f.n.Entry)
	on := ""
	switch {
	case f.here:
		p.From = f.host // this Mac's daemon reads the remote's repository
		on = " on this Mac"
	case f.remote:
		on = " on " + f.host
	}
	if f.inPlace && !f.here {
		p.Target = protocol.RestoreOriginal
		p.Overwrite = protocol.OverwriteNever
		if f.replace {
			p.Overwrite = protocol.OverwriteAlways
		}
		return p, name + " in place" + on, nil
	}
	home := ""
	if f.local() {
		home = f.home // ~ stands for nothing on a remote
	}
	dir := strings.TrimSpace(f.folder().Value())
	switch {
	case home == "":
	case dir == "~":
		dir = home
	case strings.HasPrefix(dir, "~/"):
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) && (f.local() || !isDrivePath(dir)) {
		if !f.local() {
			return p, "", fmt.Errorf("the folder needs a full path on %s, like /data/restored or C:\\Restored", f.host)
		}
		return p, "", fmt.Errorf("the folder needs a full path, like ~/Downloads/restored")
	}
	p.Target, p.Overwrite = filepath.Clean(dir), protocol.OverwriteNever
	return p, name + " to " + f.landsAs(home, p.Target) + on, nil
}

// landsAs is where the copy ends up inside folder target, as shown: a
// folder ends in /, so it never reads as a file.
func (f *restoreForm) landsAs(home, target string) string {
	if isDrivePath(target) {
		// A Windows folder: its own separators throughout.
		out := strings.TrimRight(strings.ReplaceAll(target, "/", `\`), `\`) + `\` + f.n.Name
		if f.n.IsDir() {
			out += `\`
		}
		return out
	}
	out := shortPath(home, filepath.Join(target, f.n.Name))
	if f.n.IsDir() && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

func (f *restoreForm) submit(a *App) tea.Cmd {
	p, what, err := f.params()
	if err != nil {
		f.err = err
		return nil
	}
	send := func(a *App) tea.Cmd {
		a.modal = f
		f.sending = true
		// Onto this Mac: its own daemon, even for a remote's snapshot.
		repo := a.repo
		if !f.local() {
			repo = a.repo.ForHost(f.host)
		}
		return func() tea.Msg {
			job, err := repo.Restore(context.Background(), p)
			return restoreQueuedMsg{job: job, what: what, err: err}
		}
	}
	if f.here || !f.inPlace || !f.replace {
		return send(a)
	}
	name := entryName(f.n.Entry)
	a.modal = &confirm{
		title:   "Replace on disk?",
		body:    fmt.Sprintf("Files in %s that differ from the %s copy will be overwritten. Files that aren't in the snapshot are left alone.", name, f.snapTime.Local().Format("Jan 2 15:04")),
		answers: []string{"Replace", "Cancel"},
		pick:    1, // Enter, Enter must not overwrite live files
		danger:  map[int]bool{0: true},
		act: func(a *App, answer int) tea.Cmd {
			if answer != 0 {
				a.modal = f
				return nil
			}
			return send(a)
		},
	}
	return nil
}

func (f *restoreForm) view(a *App) string {
	inner := popupInnerWidth(a.width)
	vw := inner - formLabelWidth
	var b formBuilder
	b.WriteString(popupTitle("Restore "+midTrunc(entryName(f.n.Entry), inner-12)) + "\n\n")

	row := func(field int, label, value string) {
		lab := styles.FieldLabelStyle()
		if f.field == field {
			lab = styles.HotkeyStyle()
		}
		b.Row(lab.Render(label), value)
	}
	choice := func(field int, text string) string {
		if f.field == field {
			return styles.StopStyle(true).Render("‹ " + text + " ›")
		}
		return styles.FieldValueStyle().Render(text)
	}

	// The snapshot's paths are the source machine's: ~ only for this Mac's.
	row(-1, "From", styles.FieldValueStyle().Render(midTrunc(shortPath(a.homeFor(f.host), f.n.path), vw)))
	row(-1, "Snapshot", styles.FieldValueStyle().Render(stamp(a.now(), f.snapTime)))
	if f.remote {
		switch {
		case f.offline:
			// No choice to make, so not a stop: only this Mac can.
			row(-1, "Restore to", styles.FieldValueStyle().Render("this Mac")+styles.MutedStyle().Render(midTrunc(" ("+f.host+" is offline)", vw-8)))
		case f.here:
			row(rfTo, "Restore to", choice(rfTo, "this Mac"))
		default:
			row(rfTo, "Restore to", choice(rfTo, f.host))
		}
	}
	where := "a copy in a folder"
	if f.inPlace && !f.here {
		where = "in place, where it came from"
	}
	if f.here {
		row(-1, "Where", styles.FieldValueStyle().Render(where))
	} else {
		row(rfWhere, "Where", choice(rfWhere, where))
	}
	if f.inPlace && !f.here {
		ex := "replace files that differ"
		if !f.replace {
			ex = "keep them, only add missing files"
		}
		row(rfExisting, "On disk", choice(rfExisting, ex))
		if f.remote {
			// The row a copy's "lands as" line takes, so flipping Where
			// or Restore to never resizes the popup.
			row(-1, "", styles.MutedStyle().Render(midTrunc("back onto "+f.host+"'s disk", vw)))
		}
	} else {
		in := f.folder()
		in.SetWidth(vw)
		row(rfFolder, "Folder", in.View())
		lands := "lands as "
		switch {
		case f.here:
			lands = "lands on this Mac as "
		case f.remote:
			lands = "lands on " + f.host + " as "
		}
		if p, _, err := f.params(); err == nil {
			home := ""
			if f.local() {
				home = f.home
			}
			row(-1, "", styles.MutedStyle().Render(midTrunc(lands+f.landsAs(home, p.Target), vw)))
		} else if f.remote {
			// The row is always there, so typing a path doesn't resize
			// the popup.
			row(-1, "", styles.MutedStyle().Render(midTrunc("restored onto "+f.host+"'s disk, not this Mac's", vw)))
		}
	}
	b.WriteString("\n" + f.buttons.view(f.field == rfButtons) + "\n\n")
	switch {
	case f.sending:
		b.WriteString(styles.MutedStyle().Render("asking the daemon…") + "\n")
	case f.err != nil:
		b.WriteString(errorLine(f.err) + "\n")
	default:
		b.WriteString(styles.MutedStyle().Render(f.fieldHelp()) + "\n")
	}
	if f.field == rfButtons {
		b.WriteString(hints("↑", "fields", "←→", "choose", "enter", "press", "esc", "cancel"))
	} else {
		b.WriteString(hints("↑↓", "field", "←→", "change", "enter", "next", "esc", "cancel"))
	}
	return boxed(b.String(), inner)
}

func (f *restoreForm) fieldHelp() string {
	switch {
	case f.field == rfTo && f.here:
		return "A copy on this Mac, read straight from " + f.host + "'s backups."
	case f.field == rfTo:
		return f.host + " restores it itself, onto its own disk."
	case f.field == rfWhere && f.inPlace:
		return "Puts it back at its original path."
	case f.field == rfWhere:
		return "Into a separate folder, so nothing already on disk changes."
	case f.field == rfFolder && !f.local():
		return "A full path on " + f.host + ". Created if missing; files already there are never overwritten."
	case f.field == rfFolder:
		return "Created if missing. Files already there are never overwritten."
	case f.field == rfExisting && f.replace:
		return "Overwrites changed files with the snapshot's copy; asks first."
	case f.field == rfExisting:
		return "Leaves every file on disk as it is; brings back only what's gone."
	case f.field == rfButtons && f.inPlace && !f.here && f.replace:
		return f.buttons.help("Restores it, after asking before anything is replaced.", "Closes without restoring.")
	case f.field == rfButtons:
		return f.buttons.help("Restores it.", "Closes without restoring.")
	}
	return ""
}

// isDrivePath reports whether p is a full Windows path (C:\x or C:/x):
// a Windows remote's restore folder, which its daemon takes as it is.
func isDrivePath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z')
}
