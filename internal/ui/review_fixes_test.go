package ui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backends"
	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// Tests for the 2026-11-24 whole-codebase review's findings in the TUI.

// ^N's Enter (goToSource) shows the source it went to: its snapshots,
// not the ones of the source the cursor was on.
func TestGoToSourceLoadsItsSnapshots(t *testing.T) {
	a := newTestApp(t, 160, 40)
	from := selSrc(t, a)
	var to protocol.Source
	for _, s := range a.sources {
		if s.ID != from.ID && s.Host == from.Host {
			to = s
		}
	}
	if to.ID == "" {
		t.Fatal("fixture: one source only")
	}
	press(a, "right", "right") // into the first source's tree
	drain(a, a.goToSource(to.Host, to.ID))
	if got := selSrc(t, a); got.ID != to.ID {
		t.Fatalf("selected %s, want %s", got.Name, to.Name)
	}
	if a.snapsFor != sourceKey(to) {
		t.Fatalf("snapshots shown are %q's, want %q's", a.snapsFor, sourceKey(to))
	}
	want, _ := a.repo.Snapshots(t.Context(), to.ID)
	if len(a.snaps) != len(want) || len(want) > 0 && a.snaps[0].ID != want[0].ID {
		t.Errorf("%d snapshots, first %v; want %d of %s's", len(a.snaps), a.snaps, len(want), to.Name)
	}
	if n, ok := a.restoreTarget(); ok && a.col == colSnapshots && n.snap != a.snaps[a.snapCur.pos].ID {
		t.Errorf("restore would take snapshot %s", n.snap)
	}
}

// When the snapshot being browsed is gone from a reload (a rewrite gave
// it a new id), focus comes back to Snapshots instead of staying in a
// tree column that no longer exists.
func TestSnapshotGoneBringsFocusBack(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right", "right")
	if a.col < colTree {
		t.Fatalf("col %d", a.col)
	}
	snaps := slices.Clone(a.snaps)
	for i := range snaps {
		snaps[i].ID = "new-" + snaps[i].ID
	}
	_, cmd := a.Update(snapshotsMsg{source: a.selectedSourceID(), snaps: snaps})
	drain(a, cmd)
	if a.col != colSnapshots || a.focusedCursor() == nil {
		t.Fatalf("col %d, cursor %v", a.col, a.focusedCursor())
	}
	press(a, "down")
	if a.snapCur.pos != 1 {
		t.Errorf("↓ did nothing: pos %d", a.snapCur.pos)
	}
}

// The hero highlights the stop the cursor is on at every width, also
// where the grid's last cells are cut off the right edge.
func TestHeroHighlightMatchesStops(t *testing.T) {
	for _, w := range []int{100, 110, 120, 160} {
		a := newTestApp(t, w, 40)
		plain := a.renderHero()
		a.area = areaHero
		stops := a.heroStops()
		if len(stops) == 0 {
			t.Fatalf("width %d: no stops", w)
		}
		for i, s := range stops {
			a.heroIdx = i
			got := strings.Split(a.renderHero(), "\n")
			was := strings.Split(plain, "\n")
			// Line 0 is the box's top edge; the bottom edge's hint
			// differs with focus, the content lines only with the stop.
			for line := range 2 {
				differs := got[1+line] != was[1+line]
				if differs != (line == s.line) {
					t.Errorf("width %d, stop %d (%s, line %d): hero line %d highlighted=%v", w, i, s.label, s.line, line, differs)
				}
			}
		}
	}
}

// A remote that stops answering isn't still "backing up": its job goes
// when a reload no longer has it.
func TestRemoteJobClearedWhenRemoteGoes(t *testing.T) {
	a, _ := newRemoteApp(t)
	a.setRemoteJob("pantry", &protocol.Job{ID: "j-9", Kind: protocol.KindBackup, Started: testNow})
	if !a.anyRunning() {
		t.Fatal("setup: nothing running")
	}
	hosts := slices.Clone(a.hosts)
	var sources []protocol.Source
	for _, s := range a.sources {
		if s.Host != "pantry" {
			sources = append(sources, s)
		}
	}
	for i := range hosts {
		if hosts[i].Name == "pantry" {
			hosts[i].State = protocol.HostUnreachable
		}
	}
	a.Update(sourcesMsg{hosts: hosts, sources: sources, remoteJobs: map[string]*protocol.Job{}})
	if a.remoteJobs["pantry"] != nil {
		t.Errorf("pantry still running: %+v", a.remoteJobs["pantry"])
	}
}

// After the event stream drops and comes back, everything is read again:
// a job that finished meanwhile isn't left "running".
func TestResubscribeReloads(t *testing.T) {
	a := newTestApp(t, 160, 40)
	src := a.sources[0]
	a.job = &protocol.Job{ID: "j-gone", Kind: protocol.KindBackup, Source: &src.ID, Started: testNow}
	a.Update(eventsClosedMsg{}) // its retry timer isn't run here
	ch := make(chan protocol.Event)
	_, cmd := a.Update(eventsReadyMsg{ch})
	drainSkippingWait(a, cmd)
	if a.job != nil {
		t.Errorf("job %s still shown after resubscribing", a.job.ID)
	}
	// A first subscription doesn't reload (Init already loads).
	_, cmd = a.Update(eventsReadyMsg{ch})
	if a.eventsLost {
		t.Error("eventsLost stuck")
	}
	_ = cmd
}

// A second answer for a folder already listed is dropped: applying it
// would collapse the columns walked since.
func TestDuplicateListingDropped(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right") // into the first tree column
	if len(a.levels) == 0 {
		t.Fatal("no tree")
	}
	root := a.levels[0]
	for !func() bool { n, ok := a.selectedEntry(); return ok && n.IsDir() }() {
		before := a.levels[0].cur.pos
		press(a, "down")
		if a.levels[0].cur.pos == before {
			t.Skip("fixture root has no folder")
		}
	}
	press(a, "right")
	depth, col, pos := len(a.levels), a.col, a.levels[0].cur.pos
	n, _ := a.wantEntry(0)
	a.Update(dirMsg{level: 0, dir: n, entries: root.entries})
	if len(a.levels) != depth || a.col != col || a.levels[0].cur.pos != pos {
		t.Errorf("levels %d→%d, col %d→%d, pos %d→%d", depth, len(a.levels), col, a.col, pos, a.levels[0].cur.pos)
	}
}

// A pin's answer arriving while the note form is open is the App's: the
// form stays, and the pin shows.
func TestPinReplyDoesNotCloseNoteForm(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right")
	s, _ := a.selectedSnapshot()
	pin := !s.Pinned
	reply := a.setSnapshot(protocol.SnapshotSetParams{ID: s.ID, Pinned: &pin}, "Pinned")() // asked, not yet answered
	press(a, "e")
	f, ok := a.modal.(*noteForm)
	if !ok {
		t.Fatalf("e: modal %T", a.modal)
	}
	a.Update(reply)
	if a.modal != f {
		t.Fatalf("the pin's answer closed the note form: modal %T", a.modal)
	}
	if got, _ := a.selectedSnapshot(); got.Pinned != pin {
		t.Error("pin not applied")
	}
	// The form's own save still closes it.
	typeText(a, "x")
	press(a, "enter")
	if a.modal != nil {
		t.Errorf("note save: modal %T, err %v", a.modal, f.err)
	}
}

// Below the minimum size the screen only says to resize: keys other than
// quit do nothing.
func TestTooSmallIgnoresKeys(t *testing.T) {
	a := newTestApp(t, 160, 40)
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	pos := a.srcCur.pos
	press(a, "x", "enter", "b", "e", "down", "i")
	if a.modal != nil || a.srcCur.pos != pos || a.showInfo {
		t.Errorf("keys acted while too small: modal %T, pos %d→%d, info %v", a.modal, pos, a.srcCur.pos, a.showInfo)
	}
	if f := a.repo.(*backup.Fake); len(f.Restores) != 0 {
		t.Error("restored")
	}
	if _, cmd := a.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd == nil {
		t.Error("q doesn't quit")
	}
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	press(a, "down")
	if a.srcCur.pos == pos {
		t.Error("keys still dead after resizing back")
	}
}

// Control characters in a previewed file never reach the frame.
func TestPreviewHasNoControlCharacters(t *testing.T) {
	if got := printable("a\ab\bc\fd\re\x1bf\x7f"); got != "a·b·c·d·e·f·" {
		t.Errorf("printable: %q", got)
	}
	a := newTestApp(t, 160, 40)
	e := node{Entry: protocol.Entry{Name: "notes.txt", Type: protocol.TypeFile}, snap: "s", path: "/notes.txt"}
	a.file = filePreview{key: e.key(), dump: protocol.DumpResult{Data: []byte("one\atwo\bthree\f\nfour\x1b[31mred\r\n")}}
	out := a.renderFile(e, 60, 10)
	for _, r := range ansi.Strip(out) {
		if r != '\n' && (r < 0x20 || r == 0x7f) {
			t.Fatalf("control character %q in the preview:\n%q", r, out)
		}
	}
}

// x on a folder that already has something excluded inside excludes the
// folder (not: brings all of it back).
func TestExcludeFolderWithExclusionInside(t *testing.T) {
	a := newTestApp(t, 160, 40)
	src := selSrc(t, a)
	press(a, "right", "right")
	for !func() bool { n, ok := a.selectedEntry(); return ok && n.IsDir() }() {
		before := a.levels[0].cur.pos
		press(a, "down")
		if a.levels[0].cur.pos == before {
			t.Skip("fixture root has no folder")
		}
	}
	dir, _ := a.selectedEntry()
	press(a, "right")
	child, ok := a.selectedEntry()
	if !ok {
		t.Skip("fixture folder is empty")
	}
	press(a, "x", "enter") // the child, from now on
	press(a, "left")
	if n, _ := a.selectedEntry(); n.path != dir.path {
		t.Fatalf("← landed on %s", n.path)
	}
	press(a, "x")
	if _, ok := a.modal.(*confirm); !ok {
		t.Fatalf("x on the folder: modal %T, status %q", a.modal, a.statusMsg)
	}
	press(a, "enter")
	var got protocol.Source
	for _, s := range a.sources {
		if s.ID == src.ID {
			got = s
		}
	}
	sel, _ := backup.FromSource(got.Paths, got.Excludes)
	if sel.Included(dir.path) || sel.Included(child.path) {
		t.Errorf("after excluding %s: excludes %v (folder in: %v, child in: %v)", dir.path, got.Excludes, sel.Included(dir.path), sel.Included(child.path))
	}
}

// Every backend's every field can be focused in the setup flow (the
// popup measures itself as S3, which has fewer fields than some).
func TestSetupEveryBackendEveryField(t *testing.T) {
	a, _ := newBareApp(t, 160, 40)
	f, ok := a.modal.(*setupFlow)
	if !ok {
		t.Fatalf("modal %T", a.modal)
	}
	for pick := range len(backends.All) {
		f.pick, f.inputs, f.field = pick, nil, 0
		f.prepare()
		f.step = sfDetails
		for field := range len(f.inputs) {
			f.field = field
			_ = a.View() // panicked for a field index past S3's last
		}
	}
}

// Edit on a machine whose daemon doesn't answer with its repository
// says so instead of crashing, on both tabs, and Tab still reaches the
// Address tab.
func TestMachinePopupWhenLoadFails(t *testing.T) {
	a, pantry := newRemoteApp(t)
	pantry.FailRepository(&protocol.Error{Code: protocol.ErrHostUnreachable, Message: "pantry isn't answering"})
	cursorTo(t, a, "#pantry")
	press(a, "e")
	p, ok := a.modal.(*machinePopup)
	if !ok {
		t.Fatalf("e: modal %T", a.modal)
	}
	view := ansi.Strip(a.View().Content)
	if !strings.Contains(view, "Couldn't read pantry's defaults") || !strings.Contains(view, "isn't answering") {
		t.Errorf("defaults tab:\n%s", ansi.Strip(p.view(a)))
	}
	press(a, "enter", "down", "right", "x") // nothing to edit, nothing to crash
	press(a, "tab")
	if p.tab != mpAddress {
		t.Fatalf("tab: on %d", p.tab)
	}
	if view := ansi.Strip(a.View().Content); !strings.Contains(view, "Address") {
		t.Errorf("address tab:\n%s", view)
	}
	press(a, "tab", "esc")
	if a.modal != nil {
		t.Errorf("esc: modal %T", a.modal)
	}

	// This Mac's, with no Address tab.
	a2 := newTestApp(t, 160, 40)
	a2.repo.(*backup.Fake).FailRepository(errors.New("database is locked"))
	cursorTo(t, a2, "#"+a2.srcRows()[0].host)
	press(a2, "e")
	_ = a2.View()
	press(a2, "enter", "tab", "down", "esc")
	if a2.modal != nil {
		t.Errorf("esc: modal %T", a2.modal)
	}
}

// One machine's late answer doesn't land in another machine's popup.
func TestMachinePopupDropsOtherMachinesReplies(t *testing.T) {
	a, pantry := newRemoteApp(t)
	cursorTo(t, a, "#studio")
	press(a, "e")
	p := a.modal.(*machinePopup)
	mine := p.schedule
	theirs, _ := pantry.Repository(t.Context())
	theirs.Defaults.Schedule = &protocol.Schedule{Every: 24 * 3600}
	a.Update(repoLoadedMsg{host: "pantry", res: theirs})
	if p.schedule != mine {
		t.Errorf("pantry's defaults replaced studio's: schedule %d→%d", mine, p.schedule)
	}
	a.Update(defaultsSavedMsg{host: "pantry"})
	if a.modal != p {
		t.Errorf("pantry's save closed studio's popup")
	}
	a.Update(hostSetMsg{name: "pantry"})
	if a.modal != p {
		t.Errorf("pantry's address answer closed studio's popup")
	}
}

// The folder picker survives keys pressed between ← and the listing.
func TestPickerKeysBeforeListing(t *testing.T) {
	for _, k := range []string{"space", "right", "enter", "down"} {
		a := newTestApp(t, 160, 40)
		done := false
		p := a.newFolderPicker("", t.TempDir(), backup.NewSelection(), func(*App, *backup.Selection) tea.Cmd { done = true; return nil }, nil)
		a.modal = p
		p.dir = "/data/sub"
		p.entries = []pickerEntry{{name: "a", dir: true}, {name: "b", dir: true}, {name: "c"}}
		p.back = []cursor{{pos: 3}}
		a.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // the load isn't run: no listing yet
		if p.entries != nil {
			t.Fatal("setup: entries kept")
		}
		press(a, k) // panicked: the restored cursor was past the (empty) rows
		_ = done
	}
	// And the cursor does come back once the listing arrives.
	a := newTestApp(t, 160, 40)
	p := a.newFolderPicker("", "/data/sub", backup.NewSelection(), func(*App, *backup.Selection) tea.Cmd { return nil }, nil)
	a.modal = p
	p.back = []cursor{{pos: 2}}
	a.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	a.Update(pickerDirMsg{id: p.loadID, entries: []pickerEntry{{name: "a", dir: true}, {name: "b", dir: true}, {name: "c"}}})
	if p.cur.pos != 2 {
		t.Errorf("cursor %d after the listing, want 2", p.cur.pos)
	}
}

// A copy restored from a remote's snapshot lands on the remote: the
// folder is a full path there, never this Mac's home, and the popup says
// which machine.
func TestRestoreFromRemoteNamesItsDisk(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	cursorTo(t, a, "pantry/"+src[0].ID)
	press(a, "right", "r")
	f, ok := a.modal.(*restoreForm)
	if !ok {
		t.Fatalf("r: modal %T", a.modal)
	}
	if !f.remote || f.host != "pantry" || f.folder().Value() != "" {
		t.Fatalf("remote %v, host %q, folder %q", f.remote, f.host, f.folder().Value())
	}
	if view := ansi.Strip(f.view(a)); !strings.Contains(view, "on pantry") {
		t.Errorf("popup doesn't say where:\n%s", view)
	}
	for _, bad := range []string{"", "~/Downloads/x", "restored"} {
		f.folder().SetValue(bad)
		if _, _, err := f.params(); err == nil {
			t.Errorf("folder %q accepted for a remote", bad)
		}
	}
	f.folder().SetValue("/data/restored")
	p, what, err := f.params()
	if err != nil || p.Target != "/data/restored" || !strings.Contains(what, "on pantry") {
		t.Fatalf("params %+v, %q, %v", p, what, err)
	}
	press(a, "down", "enter")
	if a.modal != nil {
		t.Fatalf("restore: modal %T, err %v", a.modal, f.err)
	}
	if got := pantry.Restores; len(got) != 1 || got[0].Target != "/data/restored" {
		t.Errorf("pantry's restores %+v", got)
	}
	if got := a.repo.(*backup.Fake).Restores; len(got) != 0 {
		t.Errorf("this Mac restored: %+v", got)
	}
}

// Adding on a remote shows that remote's default Keep, not this Mac's.
func TestAddOnRemoteShowsItsDefaults(t *testing.T) {
	a, pantry := newRemoteApp(t)
	month := keepPresets[0].r
	if _, err := pantry.SetDefaults(t.Context(), protocol.DefaultsSetParams{Retention: &month}); err != nil {
		t.Fatal(err)
	}
	drain(a, a.loadSources())
	_, mine := a.defaultPolicy(a.localHost())
	if mine == month {
		t.Fatal("fixture: both machines default to 1 month")
	}
	f := a.newSourceForm("pantry", nil, backup.NewSelection())
	if f.host != "pantry" || f.keep.def != month || f.keep.text() != "default (1 month)" {
		t.Errorf("host %q, keep %q (default %+v)", f.host, f.keep.text(), f.keep.def)
	}
}

// A question a daemon's answer calls for waits while another popup is
// open, then shows.
func TestAsyncConfirmWaitsForOpenPopup(t *testing.T) {
	a := newTestApp(t, 160, 40)
	src := selSrc(t, a)
	press(a, "e")
	form := a.modal
	if _, ok := form.(*sourceForm); !ok {
		t.Fatalf("e: modal %T", form)
	}
	a.Update(excludedMsg{source: src, shown: "docs/", path: "/x/docs", past: true, count: 3})
	if a.modal != form {
		t.Fatalf("the count's answer replaced the form: modal %T", a.modal)
	}
	a.Update(hostRemovedMsg{host: "pantry", res: protocol.HostRemoveResult{Message: "run this there"}})
	if a.modal != form {
		t.Fatalf("the removal's answer replaced the form: modal %T", a.modal)
	}
	press(a, "esc")
	c, ok := a.modal.(*confirm)
	if !ok || c.title != "Remove from past snapshots" || c.pick != 1 {
		t.Fatalf("after the form closed: modal %T %+v", a.modal, c)
	}
	press(a, "esc")
	if c, ok := a.modal.(*confirm); !ok || !strings.Contains(c.title, "still installed") {
		t.Fatalf("then: modal %T", a.modal)
	}
	press(a, "enter")
	if a.modal != nil {
		t.Errorf("left open: %T", a.modal)
	}
}

// The add line takes one pattern: a comma is part of it.
func TestPatternListKeepsCommas(t *testing.T) {
	l := newPatternList(nil, "")
	l.focus(true)
	for _, r := range "Notes, old" {
		l.key(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := l.values(); len(got) != 1 || got[0] != "Notes, old" {
		t.Errorf("values before adding: %q", got)
	}
	l.key(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(l.items) != 1 || l.items[0] != "Notes, old" {
		t.Errorf("items %q", l.items)
	}
}

// A long skip list scrolls inside its form: the frame fits the terminal
// with 30-odd patterns, the cursor's row is always shown, and what's out
// of sight is counted.
func TestLongSkipListsFit(t *testing.T) {
	var many []string
	for i := range 34 {
		many = append(many, fmt.Sprintf("pattern-%02d", i))
	}
	for _, size := range [][2]int{{100, 26}, {120, 30}, {160, 40}} {
		w, h := size[0], size[1]
		a := newTestApp(t, w, h)
		src := selSrc(t, a)
		if _, err := a.repo.SetSource(t.Context(), protocol.SourceSetParams{ID: src.ID, Excludes: &many}); err != nil {
			t.Fatal(err)
		}
		drain(a, a.loadSources())
		press(a, "e")
		f, ok := a.modal.(*sourceForm)
		if !ok {
			t.Fatalf("e: modal %T", a.modal)
		}
		checkFrame(t, a, w, h, fmt.Sprintf("source form %dx%d", w, h))
		for f.field != ffExcludes {
			press(a, "down")
		}
		for i := range len(many) + 1 {
			view := ansi.Strip(a.View().Content)
			want := "pattern-" + fmt.Sprintf("%02d", i)
			if i == len(many) {
				want = "add a pattern"
			}
			if !strings.Contains(view, want) {
				t.Fatalf("%dx%d: cursor on row %d (%s) not shown", w, h, i, want)
			}
			checkFrame(t, a, w, h, fmt.Sprintf("source form %dx%d row %d", w, h, i))
			press(a, "down")
		}
		press(a, "esc")

		// The machine's defaults, with as many.
		if _, err := a.repo.SetDefaults(t.Context(), protocol.DefaultsSetParams{Excludes: &many}); err != nil {
			t.Fatal(err)
		}
		p := openMachine(t, a)
		checkFrame(t, a, w, h, fmt.Sprintf("machine popup %dx%d", w, h))
		for p.field != mpSkip {
			press(a, "down")
		}
		for range 20 {
			press(a, "down")
			checkFrame(t, a, w, h, fmt.Sprintf("machine popup %dx%d, skip row %d", w, h, p.skip.cur))
		}
		if view := ansi.Strip(a.View().Content); !strings.Contains(view, fmt.Sprintf("pattern-%02d", p.skip.cur)) || !strings.Contains(view, fmt.Sprintf("%d of %d", p.skip.cur+1, len(many))) {
			t.Errorf("%dx%d: machine skip row %d not shown, or nothing says which of how many:\n%s", w, h, p.skip.cur, view)
		}
	}
}

// A snapshot whose one path is a file opens at the folder holding it:
// listing the file itself gives nothing (or not found, once the daemon
// has the tree in memory).
func TestSingleFileSourceOpensAtItsFolder(t *testing.T) {
	for name, reply := range map[string]dirMsg{
		"empty":     {},
		"not found": {err: &protocol.Error{Code: protocol.ErrNotFound, Message: "isn't in snapshot"}},
	} {
		a := newTestApp(t, 160, 40)
		a.snaps = []protocol.Snapshot{{ID: "s-file", Time: testNow.Add(-time.Hour), Paths: []string{"/etc/hosts"}}}
		a.snapCur, a.levels, a.col = cursor{}, nil, colSnapshots
		root, _ := a.wantEntry(0)
		if root.path != "/etc/hosts" {
			t.Fatalf("root %q", root.path)
		}
		reply.level, reply.dir = 0, root
		_, cmd := a.Update(reply)
		if cmd == nil {
			t.Fatalf("%s: no second listing asked for", name)
		}
		if got, _ := a.wantEntry(0); got.path != "/etc" || got.Name != "etc" {
			t.Errorf("%s: tree opens at %q (%q), want /etc", name, got.path, got.Name)
		}
		if a.err != nil {
			t.Errorf("%s: error shown: %v", name, a.err)
		}
		// Restore and diff of the snapshot still mean the file.
		if n, _ := a.restoreTarget(); n.path != "/etc/hosts" {
			t.Errorf("%s: restore target %q", name, n.path)
		}
		// An empty folder one level up isn't walked up again.
		up, _ := a.wantEntry(0)
		_, cmd = a.Update(dirMsg{level: 0, dir: up})
		if got, _ := a.wantEntry(0); got.path != "/etc" {
			t.Errorf("%s: walked up twice, to %q", name, got.path)
		}
		_ = cmd
	}
}

// On the smallest terminals a form with every row open is taller than
// the area under the hero: it's cut to fit instead of pushing the frame
// past the terminal.
func TestTallFormsCutToFit(t *testing.T) {
	for _, size := range [][2]int{{100, 20}, {100, 26}} {
		a := newTestApp(t, size[0], size[1])
		press(a, "e")
		f := a.modal.(*sourceForm)
		for f.field != ffRetention {
			press(a, "down")
		}
		for !f.keep.custom() {
			press(a, "right")
		}
		checkFrame(t, a, size[0], size[1], "custom")
		press(a, "esc")
		p := openMachine(t, a)
		press(a, "down")
		for !p.keep.custom() {
			press(a, "right")
		}
		checkFrame(t, a, size[0], size[1], "machine custom")
	}
}
