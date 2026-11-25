package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// newOfflineApp is this Mac (studio) with pantry paired and not
// answering; saved is whether the daemon holds pantry's saved sources and
// repository.
func newOfflineApp(t *testing.T, w, h int, saved bool) (*App, *backup.Fake, *backup.Fake) {
	t.Helper()
	f := backup.NewFake(testNow)
	pantry := f.AddRemote("pantry", false)
	f.SetOffline("pantry", true, saved)
	a := NewApp(f, func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	drain(a, a.Init())
	return a, f, pantry
}

// pantrySource puts the cursor on pantry's source and loads its snapshots.
func pantrySource(t *testing.T, a *App) protocol.Source {
	t.Helper()
	for _, s := range a.sources {
		if s.Host == "pantry" {
			cursorTo(t, a, sourceKey(s))
			return s
		}
	}
	t.Fatal("no pantry source listed")
	return protocol.Source{}
}

// A remote that isn't answering still lists its sources (the daemon's
// saved copy), and its snapshots, folders and files read as any other's.
func TestOfflineRemoteIsBrowsable(t *testing.T) {
	a, _, _ := newOfflineApp(t, 160, 40, true)
	if h := a.findHost("pantry"); h.State != protocol.HostUnreachable {
		t.Fatalf("pantry's state %q", h.State)
	}
	src := pantrySource(t, a)
	if len(a.snaps) == 0 || a.snapsFor != sourceKey(src) {
		t.Fatalf("snapshots for %q: %d", a.snapsFor, len(a.snaps))
	}
	if _, ok := a.repos["pantry"]; !ok {
		t.Error("no saved repository for pantry's header")
	}
	press(a, "right", "right")
	if a.col != colTree || len(a.levels) == 0 || len(a.levels[0].entries) == 0 {
		t.Fatalf("tree: col %d, %d levels", a.col, len(a.levels))
	}
	// Walk to a file: its preview is a read from the repository too.
	for i := 0; i < 6; i++ {
		if n, ok := a.selectedEntry(); ok && !n.IsDir() {
			break
		}
		press(a, "right")
	}
	n, ok := a.selectedEntry()
	if !ok || n.IsDir() {
		t.Skip("the fixture's first entries are all folders")
	}
	if a.file.err != nil || a.file.key != n.key() {
		t.Errorf("preview of %s: err %v", n.path, a.file.err)
	}
}

// With nothing saved for it, an offline remote is a header and nothing
// under it, as before.
func TestOfflineRemoteWithNothingSaved(t *testing.T) {
	a, _, _ := newOfflineApp(t, 160, 40, false)
	for _, s := range a.sources {
		if s.Host == "pantry" {
			t.Fatalf("listed %s for a remote with nothing saved", s.Name)
		}
	}
	cursorTo(t, a, "#pantry")
	if !a.onHeader() {
		t.Error("no header for pantry")
	}
}

// Everything that needs the remote says so in a dialog and sends it
// nothing; OK or Esc closes it.
func TestOfflineActionsOpenTheDialog(t *testing.T) {
	a, _, pantry := newOfflineApp(t, 160, 40, true)
	src := pantrySource(t, a)
	before := pantry.Refused
	blocked := func(where, k, what string) {
		t.Helper()
		press(a, k)
		c, ok := a.modal.(*confirm)
		if !ok {
			t.Fatalf("%s %s: modal %T, want the offline dialog", where, k, a.modal)
		}
		if c.title != "pantry" || !strings.Contains(c.body, "pantry is offline") || !strings.Contains(c.body, what) || len(c.answers) != 1 {
			t.Errorf("%s %s: %q / %q %v", where, k, c.title, c.body, c.answers)
		}
		press(a, "esc")
		if a.modal != nil {
			t.Fatalf("%s %s: esc left %T open", where, k, a.modal)
		}
	}
	blocked("source", "a", "Adding a source")
	blocked("source", "e", "Editing a source")
	blocked("source", "x", "Removing a source")
	blocked("source", "b", "Backing up now")
	press(a, "right")
	if a.col != colSnapshots {
		t.Fatalf("col %d", a.col)
	}
	blocked("snapshot", "e", "Editing a note")
	blocked("snapshot", "p", "Pinning")
	press(a, "right")
	blocked("tree", "x", "Excluding")
	blocked("tree", "d", "Comparing")
	press(a, "left", "left")
	cursorTo(t, a, "#pantry")
	blocked("header", "a", "Adding a source")

	// The hero's pickers are the same editors: the same answer.
	cursorTo(t, a, sourceKey(src))
	press(a, "tab")
	for i, s := range a.heroStops() {
		if s.label != "Schedule" && s.label != "Retention" {
			continue
		}
		a.heroIdx = i
		press(a, "enter")
		if _, ok := a.modal.(*confirm); !ok {
			t.Errorf("hero %s: modal %T", s.label, a.modal)
		}
		press(a, "enter")
	}
	if a.modal != nil {
		t.Fatalf("left open: %T", a.modal)
	}
	if pantry.Refused != before {
		t.Errorf("%d calls reached the offline remote", pantry.Refused-before)
	}
	after, _ := pantry.Sources(t.Context())
	if len(after) != 1 || after[0].ID != src.ID {
		t.Errorf("pantry's sources changed: %+v", after)
	}
}

// The machine popup still opens for an offline remote (its Address tab is
// the fix for a moved machine); saving its defaults says it can't, and
// comes back to the popup.
func TestOfflineMachineEdit(t *testing.T) {
	a, _, pantry := newOfflineApp(t, 160, 40, true)
	cursorTo(t, a, "#pantry")
	press(a, "e")
	p, ok := a.modal.(*machinePopup)
	if !ok {
		t.Fatalf("e on the header: %T", a.modal)
	}
	if p.keep == nil {
		t.Fatal("the saved defaults didn't load")
	}
	before := pantry.Refused
	press(a, "enter")
	if c, ok := a.modal.(*confirm); !ok || !strings.Contains(c.body, "Changing its defaults") {
		t.Fatalf("enter: %T", a.modal)
	}
	press(a, "enter")
	if a.modal != p {
		t.Errorf("OK went to %T, want the machine popup back", a.modal)
	}
	if pantry.Refused != before {
		t.Error("the save reached the offline remote")
	}
}

// The action bar dims exactly the actions the offline machine blocks, and
// none for one that's answering.
func TestActionBarDimsBlockedActions(t *testing.T) {
	dimmed := func(a *App) string {
		var out []string
		for _, act := range a.actions() {
			if a.blocked(act) {
				out = append(out, act.label)
			}
			// What's drawn agrees: a dimmed button has no hotkey colour.
			drawn := renderButton(act, a.blocked(act))
			plain := renderButton(act, false)
			if a.blocked(act) == (drawn == plain) {
				t.Errorf("%s: blocked %v but drawn the same as normal: %v", act.label, a.blocked(act), drawn == plain)
			}
		}
		return strings.Join(out, " ")
	}
	a, _, _ := newOfflineApp(t, 160, 40, true)
	pantrySource(t, a)
	if got := dimmed(a); got != "Add Edit Remove Backup now" {
		t.Errorf("offline source: dimmed %q", got)
	}
	if got := actionLabels(a); got != "Add Edit Remove Backup now Add Remote Info" {
		t.Errorf("offline source: bar %q", got)
	}
	press(a, "right")
	if got := dimmed(a); got != "Edit Pin" {
		t.Errorf("offline snapshot: dimmed %q", got)
	}
	press(a, "right")
	if got := dimmed(a); got != "Diff Exclude" {
		t.Errorf("offline tree: dimmed %q", got)
	}
	press(a, "left", "left")
	cursorTo(t, a, "#pantry")
	if got := dimmed(a); got != "Add" {
		t.Errorf("offline header: dimmed %q", got)
	}
	// This Mac's own rows, in the same app: nothing dimmed.
	cursorTo(t, a, "#studio")
	press(a, "down")
	if got := dimmed(a); got != "" {
		t.Errorf("this Mac's source: dimmed %q", got)
	}

	// An answering remote: nothing dimmed in any column.
	b, _ := newRemoteApp(t)
	pantrySource(t, b)
	for _, k := range []string{"", "right", "right"} {
		if k != "" {
			press(b, k)
		}
		if got := dimmed(b); got != "" {
			t.Errorf("online remote, col %d: dimmed %q", b.col, got)
		}
	}
}

// Restore from an offline remote lands on this Mac: sent to this Mac's
// daemon with From, never to the remote, never in place.
func TestRestoreFromOfflineRemote(t *testing.T) {
	a, local, pantry := newOfflineApp(t, 160, 40, true)
	pantrySource(t, a)
	press(a, "right", "r")
	f, ok := a.modal.(*restoreForm)
	if !ok {
		t.Fatalf("r: %T", a.modal)
	}
	if !f.here || !f.offline || f.field != rfFolder {
		t.Fatalf("here %v, offline %v, field %d", f.here, f.offline, f.field)
	}
	view := ansi.Strip(f.view(a))
	if !strings.Contains(view, "this Mac") || !strings.Contains(view, "pantry is offline") || !strings.Contains(view, "lands on this Mac as ~/Downloads") {
		t.Errorf("popup doesn't say where it lands:\n%s", view)
	}
	press(a, "enter")
	if a.modal != nil {
		t.Fatalf("enter: still %T, err %v", a.modal, f.err)
	}
	if len(pantry.Restores) != 0 || len(local.Restores) != 1 {
		t.Fatalf("restores: local %d, pantry %d", len(local.Restores), len(pantry.Restores))
	}
	p := local.Restores[0]
	if p.From != "pantry" || p.Target == protocol.RestoreOriginal || !strings.Contains(p.Target, "/Downloads/Restored ") || p.Overwrite != protocol.OverwriteNever {
		t.Errorf("sent %+v", p)
	}
}

// An answering remote offers both machines, and the popup keeps its size
// whichever is chosen.
func TestRestoreToEitherMachine(t *testing.T) {
	a, pantry := newRemoteApp(t)
	local := a.repo.(*backup.Fake)
	pantrySource(t, a)
	press(a, "right", "r")
	f := a.modal.(*restoreForm)
	if f.here || f.field != rfTo {
		t.Fatalf("starts here %v at field %d, want on pantry at Restore to", f.here, f.field)
	}
	there := f.view(a)
	press(a, "right")
	if !f.here {
		t.Fatal("→ didn't switch to this Mac")
	}
	here := f.view(a)
	if lipgloss.Height(here) != lipgloss.Height(there) || lipgloss.Width(here) != lipgloss.Width(there) {
		t.Errorf("popup resized: %dx%d on pantry, %dx%d here", lipgloss.Width(there), lipgloss.Height(there), lipgloss.Width(here), lipgloss.Height(here))
	}
	if !strings.Contains(ansi.Strip(here), "lands on this Mac as") {
		t.Errorf("here:\n%s", ansi.Strip(here))
	}
	press(a, "enter")
	if a.modal != nil {
		t.Fatalf("still %T: %v", a.modal, f.err)
	}
	if len(local.Restores) != 1 || local.Restores[0].From != "pantry" || len(pantry.Restores) != 0 {
		t.Fatalf("to this Mac: local %+v, pantry %+v", local.Restores, pantry.Restores)
	}

	// On pantry: its own daemon, a full path there, no From.
	press(a, "r")
	f = a.modal.(*restoreForm)
	press(a, "down", "down")
	if f.field != rfFolder {
		t.Fatalf("field %d", f.field)
	}
	typeText(a, "/data/restored")
	press(a, "enter")
	if a.modal != nil {
		t.Fatalf("still %T: %v", a.modal, f.err)
	}
	if len(pantry.Restores) != 1 || pantry.Restores[0].From != "" || pantry.Restores[0].Target != "/data/restored" || len(local.Restores) != 1 {
		t.Errorf("on pantry: pantry %+v, local %d", pantry.Restores, len(local.Restores))
	}
}

// Frames still fit with an offline remote selected, its dialog and its
// restore popup open.
func TestOfflineFramesFit(t *testing.T) {
	for _, size := range [][2]int{{100, 26}, {120, 30}} {
		a, _, _ := newOfflineApp(t, size[0], size[1], true)
		check := func(what string) {
			t.Helper()
			frame := a.View().Content
			if w, h := lipgloss.Width(frame), lipgloss.Height(frame); w != size[0] || h != size[1] {
				t.Errorf("%dx%d %s: frame %dx%d", size[0], size[1], what, w, h)
			}
		}
		pantrySource(t, a)
		check("source")
		press(a, "b")
		check("dialog")
		press(a, "enter", "right", "r")
		check("restore")
		press(a, "esc")
		cursorTo(t, a, "#pantry")
		check("header")
	}
}

// An offline remote's hero says "offline" in a word; the connection
// error, however long, is only in the info popup.
func TestOfflineHeroIsShort(t *testing.T) {
	h := protocol.Host{Name: "pantry", State: protocol.HostUnreachable, Error: "dial tcp 192.0.2.10:7457: connect: connection refused"}
	if text, _ := hostState(h, ""); text != "✗ offline" {
		t.Errorf("hero %q", text)
	}
	if why := hostWhy(h, ""); !strings.Contains(why, "offline: dial tcp") {
		t.Errorf("info %q", why)
	}
}

// Before the first sources reply the column says it's loading, never
// "none yet".
func TestSourcesSayLoadingBeforeTheFirstReply(t *testing.T) {
	a := NewApp(backup.NewFake(time.Now()), time.Now)
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if frame := a.View().Content; !strings.Contains(frame, "loading…") || strings.Contains(frame, "none yet: press") {
		t.Error("before the first reply: want loading, not none yet")
	}
	a.Update(sourcesMsg{})
	if frame := a.View().Content; strings.Contains(frame, "loading…") || !strings.Contains(frame, "none yet: press") {
		t.Error("after an empty reply: want none yet")
	}
}

// Add Remote offers the Windows command too: ←→ switch, c copies the one
// shown, and the popup keeps its size either way.
func TestAddRemoteWindowsCommand(t *testing.T) {
	a := newTestApp(t, 120, 30)
	press(a, "r")
	typeText(a, "gamer")
	press(a, "enter")
	m, ok := a.modal.(*addRemote)
	if !ok || m.res.WindowsCommand == "" {
		t.Fatalf("modal %T, windows command %q", a.modal, m.res.WindowsCommand)
	}
	linux := m.view(a)
	if !strings.Contains(m.shown(), "| sudo sh") {
		t.Errorf("starts on %q", m.shown())
	}
	press(a, "right")
	if !m.win || !strings.Contains(m.shown(), "powershell") {
		t.Fatalf("→ shows %q", m.shown())
	}
	win := m.view(a)
	if lipgloss.Height(win) != lipgloss.Height(linux) || !strings.Contains(win, "administrator PowerShell") {
		t.Errorf("heights %d and %d", lipgloss.Height(linux), lipgloss.Height(win))
	}
	press(a, "c")
	if !m.copied {
		t.Error("c didn't copy")
	}
}
