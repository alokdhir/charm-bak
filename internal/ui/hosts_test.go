package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// newRemoteApp is the fixture Mac paired with pantry (answering, one
// source) and tower (not answering).
func newRemoteApp(t *testing.T) (*App, *backup.Fake) {
	t.Helper()
	f := backup.NewFake(testNow)
	pantry := f.AddRemote("pantry", false)
	f.AddRemote("tower", true)
	a := NewApp(f, func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drain(a, a.Init())
	return a, pantry
}

// Sources come from every answering host, grouped under host headers, in
// the order the cursor walks them.
func TestSourcesGroupedByHost(t *testing.T) {
	a, pantry := newRemoteApp(t)
	local, _ := a.repo.Sources(t.Context())
	remote, _ := pantry.Sources(t.Context())
	if len(a.sources) != len(local)+len(remote) {
		t.Fatalf("%d sources, want %d + %d", len(a.sources), len(local), len(remote))
	}
	col := ansi.Strip(a.renderColumn(colSources, sourcesWidth, 30))
	lines := strings.Split(col, "\n")
	if !strings.HasPrefix(strings.TrimSpace(lines[1]), "▾ studio") {
		t.Errorf("first row %q, want the studio header", lines[1])
	}
	if !strings.Contains(col, "pantry") || !strings.Contains(col, "tower") {
		t.Errorf("column:\n%s", col)
	}
	for _, l := range lines {
		if strings.Contains(l, "tower") && !strings.HasSuffix(strings.TrimSpace(l), "✗") {
			t.Errorf("unreachable tower's header %q, want ✗", l)
		}
	}
	last := a.sources[len(a.sources)-1]
	if last.Host != "pantry" {
		t.Errorf("last source is %s's, want pantry's", last.Host)
	}
}

// cursorTo walks the Sources cursor down to the row with key.
func cursorTo(t *testing.T, a *App, key string) {
	t.Helper()
	a.col, a.area = colSources, areaColumns
	for a.srcCur.pos > 0 {
		press(a, "up")
	}
	for range len(a.srcRows()) {
		if a.selectedRowKey() == key {
			return
		}
		press(a, "down")
	}
	t.Fatalf("no row %q", key)
}

// Selecting pantry's source loads its snapshots from pantry.
func TestRemoteSourceSnapshotsComeFromItsHost(t *testing.T) {
	a, pantry := newRemoteApp(t)
	remote, _ := pantry.Sources(t.Context())
	cursorTo(t, a, sourceKey(remote[0]))
	src, _ := a.selSource()
	want, _ := pantry.Snapshots(t.Context(), src.ID)
	if len(want) == 0 || a.snapsFor != sourceKey(src) || len(a.snaps) != len(want) || a.snaps[0].ID != want[0].ID {
		t.Errorf("snapshots for %q: %d, want %d from pantry", a.snapsFor, len(a.snaps), len(want))
	}
	rows, cur := a.sourceRows(testNow, sourcesWidth)
	if !strings.Contains(ansi.Strip(rows[cur.pos]), src.Name) {
		t.Errorf("cursor row %q, want %s", ansi.Strip(rows[cur.pos]), src.Name)
	}
}

// The cursor stops on host headers; Enter folds one and unfolds it, ← and
// → fold and unfold, and a header has no snapshots or source actions.
func TestHostHeadersFold(t *testing.T) {
	a, pantry := newRemoteApp(t)
	remote, _ := pantry.Sources(t.Context())
	if a.selectedRowKey() != "studio/a1b2c3d4" {
		t.Fatalf("first row %q, want studio's first source", a.selectedRowKey())
	}
	cursorTo(t, a, "#studio")
	if len(a.snaps) != 0 || a.snapsFor != "" {
		t.Errorf("header loaded snapshots for %q", a.snapsFor)
	}
	if got := actionLabels(a); got != "Add Edit Adopt key Add Remote Info" {
		t.Errorf("local header actions %q", got)
	}
	n := len(a.srcRows())
	press(a, "enter")
	if !a.collapsed["studio"] || len(a.srcRows()) != n-a.hostSourceCount("studio") || a.selectedRowKey() != "#studio" {
		t.Errorf("after enter: collapsed %v, %d rows, on %q", a.collapsed, len(a.srcRows()), a.selectedRowKey())
	}
	if a.col != colSources {
		t.Errorf("enter on a header moved focus to column %d", a.col)
	}
	col := ansi.Strip(a.renderColumn(colSources, sourcesWidth, 30))
	if !strings.Contains(col, "▸ studio") {
		t.Errorf("folded header missing:\n%s", col)
	}
	// Down from a folded header goes straight to the next host.
	press(a, "down")
	if a.selectedRowKey() != "#pantry" {
		t.Errorf("down from folded studio: on %q", a.selectedRowKey())
	}
	if got := actionLabels(a); got != "Add Edit Remove Adopt key Add Remote Info" {
		t.Errorf("answering remote header actions %q, want Add Edit Remove Adopt key Add Remote Info", got)
	}
	press(a, "left")
	if !a.collapsed["pantry"] {
		t.Error("left didn't fold pantry")
	}
	press(a, "right")
	if a.collapsed["pantry"] {
		t.Error("right didn't unfold pantry")
	}
	press(a, "down")
	if a.selectedRowKey() != sourceKey(remote[0]) || a.snapsFor != sourceKey(remote[0]) {
		t.Errorf("on %q with snapshots for %q, want pantry's source", a.selectedRowKey(), a.snapsFor)
	}
	// A reload keeps a folded host folded and the cursor where it was.
	press(a, "up")
	press(a, "enter")
	a.Update(a.loadSources()())
	if a.selectedRowKey() != "#pantry" || !a.collapsed["pantry"] {
		t.Errorf("after reload: on %q, collapsed %v", a.selectedRowKey(), a.collapsed)
	}
}

// o on a remote without a repository opens the setup flow, starting
// beside this machine's repository: same backend and bucket, pantry's own
// folder. It's checked from pantry, created through this machine's daemon,
// shows the recovery key once, and ends in pantry's folder picker.
func TestRemoteRepositorySetup(t *testing.T) {
	a, pantry := newRemoteApp(t)
	pantry.NoRepository()
	drain(a, a.loadSources())
	cursorTo(t, a, "#pantry")
	if got := ansi.Strip(a.hostHeaderRow("pantry", 30)); !strings.Contains(got, "setup") {
		t.Errorf("pantry's header %q", got)
	}
	press(a, "enter") // the header says setup
	f, ok := a.modal.(*setupFlow)
	if !ok || f.host != "pantry" || !f.remote || f.step != sfWhere {
		t.Fatalf("modal %T %+v", a.modal, f)
	}
	press(a, "enter", "down", "enter") // S3, I have a key
	if f.step != sfDetails || f.backend.ID != "s3" {
		t.Fatalf("step %d backend %q", f.step, f.backend.ID)
	}
	v := f.values()
	if v["bucket"] != "charm-bak" || v["folder"] != "pantry" || f.backend.Fields[f.field].Key != "AWS_ACCESS_KEY_ID" {
		t.Fatalf("prefill %v, cursor on %s", v, f.backend.Fields[f.field].Key)
	}
	press(a, "AKIA1", "tab", "s3cret", "enter")
	if f.step != sfCreate || f.location != "s3:s3.amazonaws.com/charm-bak/pantry" {
		t.Fatalf("after check: step %d at %q (note %q, err %v)", f.step, f.location, f.note, f.err)
	}
	press(a, "enter")
	if f.step != sfKey || f.key == "" || !strings.Contains(ansi.Strip(f.view(a)), f.key) {
		t.Fatalf("after create: step %d key %q err %v", f.step, f.key, f.err)
	}
	// A click on the key copies it, like c.
	popup := f.view(a)
	top := heroHeight + actionHeight + max((a.bodyHeight()-lipgloss.Height(popup))/2, 0)
	for i, l := range strings.Split(popup, "\n") {
		if strings.Contains(ansi.Strip(l), f.key) {
			a.Update(tea.MouseClickMsg{X: a.width / 2, Y: top + i, Button: tea.MouseLeft})
		}
	}
	if !f.copied {
		t.Error("clicking the key didn't copy it")
	}
	press(a, "esc") // the key can't be skipped
	if f.step != sfKey {
		t.Fatalf("esc left the key step")
	}
	press(a, "enter", "enter")
	if p, ok := a.modal.(*folderPicker); !ok || p.host != "pantry" {
		t.Fatalf("after done: modal %T", a.modal)
	}
	if got, _ := pantry.Repository(t.Context()); got.Repository == nil || got.Repository.Location != f.location {
		t.Errorf("pantry's repository %+v", got.Repository)
	}
}

// Add on pantry's header browses pantry's disk (through its daemon) and
// saves the new source on pantry.
func TestAddSourceOnRemote(t *testing.T) {
	a, pantry := newRemoteApp(t)
	before, _ := pantry.Sources(t.Context())
	cursorTo(t, a, "#pantry")
	press(a, "a")
	p, ok := a.modal.(*folderPicker)
	if !ok || p.host != "pantry" || p.dir != "/" || p.repo == nil {
		t.Fatalf("modal %T %+v", a.modal, p)
	}
	if len(p.entries) != 2 || p.entries[0].name != "data" || !p.entries[0].dir {
		t.Fatalf("pantry's / listed as %+v (err %v)", p.entries, p.err)
	}
	if v := ansi.Strip(p.view(a)); !strings.Contains(v, "on pantry") {
		t.Errorf("picker doesn't say whose disk:\n%s", v)
	}
	press(a, "down", "right") // into /data
	if p.dir != "/data" || len(p.entries) != 3 {
		t.Fatalf("in %s: %+v", p.dir, p.entries)
	}
	press(a, "up", "space", "enter") // tick /data itself
	f, ok := a.modal.(*sourceForm)
	if !ok || f.host != "pantry" || f.name.Value() != "data" {
		t.Fatalf("form %T host %q name %q", a.modal, f.host, f.name.Value())
	}
	press(a, "enter")
	if a.modal != nil {
		t.Fatalf("save: %v", a.modal.(*sourceForm).err)
	}
	after, _ := pantry.Sources(t.Context())
	if len(after) != len(before)+1 || after[len(after)-1].Paths[0] != "/data" {
		t.Fatalf("pantry's sources %+v", after)
	}
	if mine, _ := a.repo.Sources(t.Context()); slices.ContainsFunc(mine, func(s protocol.Source) bool { return s.Name == "data" }) {
		t.Error("the source landed on this machine too")
	}
	if a.selectedSourceID() != sourceKey(after[len(after)-1]) {
		t.Errorf("selected %q, want the new source", a.selectedSourceID())
	}
	// A host that isn't answering keeps Add in its bar, dimmed: pressing
	// it says why it can't, and adds nothing anywhere.
	cursorTo(t, a, "#tower")
	if got := actionLabels(a); got != "Add Edit Remove Add Remote Info" {
		t.Errorf("tower's header actions %q", got)
	}
	press(a, "a")
	if c, ok := a.modal.(*confirm); !ok || !strings.Contains(c.body, "tower is offline") {
		t.Errorf("a on tower's header: modal %T", a.modal)
	}
	press(a, "enter")
}

// Every frame fits with several hosts, including a paired remote with no
// sources and no repository yet (a fresh pairing) and one that isn't
// answering: host headers have different hero cells than sources, and
// than each other.
func TestFramesFitWithHosts(t *testing.T) {
	f := backup.NewFake(testNow)
	pantry := f.AddRemote("pantry", false)
	src, _ := pantry.Sources(t.Context())
	pantry.RemoveSource(t.Context(), src[0].ID, false)
	pantry.NoRepository()
	f.AddRemote("tower", true)
	for _, size := range [][2]int{{160, 40}, {100, 26}} {
		a := NewApp(f, func() time.Time { return testNow })
		a.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		drain(a, a.Init())
		check := func(after string) {
			t.Helper()
			lines := strings.Split(a.View().Content, "\n")
			if len(lines) != size[1] {
				t.Errorf("%v after %s: %d lines", size, after, len(lines))
			}
			for _, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Errorf("%v after %s: line %d wide", size, after, w)
				}
			}
		}
		check("start")
		for i := range len(a.srcRows()) + 1 {
			press(a, "down")
			check(fmt.Sprintf("down %d (on %q)", i+1, a.selectedRowKey()))
		}
		cursorTo(t, a, "#pantry")
		for _, k := range []string{"enter", "enter", "up", "up", "e", "esc", "i", "esc"} {
			press(a, k)
			check(k + " on pantry")
		}
	}
}

// A remote's running job shows on its source, from its events (and from
// its status when the TUI opens mid-run), not as the last run's result;
// it clears when the job finishes.
func TestRemoteJobShowsAsRunning(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	s := src[0]
	s.Host = "pantry"
	job := protocol.Job{ID: "j-1", Kind: protocol.KindBackup, Source: &s.ID, Started: testNow}
	a.handleEvent(protocol.Event{Name: protocol.EventJobStarted, Host: "pantry", JobStarted: &protocol.JobStartedEvent{Job: job}})
	a.handleEvent(protocol.Event{Name: protocol.EventProgress, Host: "pantry", Progress: &protocol.Progress{JobID: "j-1", Percent: 0.42}})
	running, p := a.jobFor(s)
	if !running || p != 0.42 {
		t.Fatalf("running %v at %v", running, p)
	}
	if h := ansi.Strip(a.sourceHealth(testNow, s)); !strings.Contains(h, "42%") {
		t.Errorf("health %q, want progress", h)
	}
	// The same id on this machine isn't running.
	local := s
	local.Host = a.localHost()
	if running, _ := a.jobFor(local); running {
		t.Error("pantry's job shows on this machine's source")
	}
	a.handleEvent(protocol.Event{Name: protocol.EventJobFinished, Host: "pantry", JobFinished: &protocol.JobFinishedEvent{Run: protocol.Run{Job: "j-1", Source: &s.ID}}})
	if running, _ := a.jobFor(s); running {
		t.Error("still running after job_finished")
	}
	// Opened mid-run: the sources reload carries pantry's status.
	a.Update(sourcesMsg{hosts: a.hosts, sources: a.sources, remoteJobs: map[string]*protocol.Job{"pantry": &job}})
	if running, _ := a.jobFor(s); !running {
		t.Error("status job not picked up")
	}
}

// The hero's second row follows the selected row's host: a remote's
// repository and daemon, not this machine's.
func TestHeroSecondRowFollowsHost(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	cursorTo(t, a, "#pantry")
	a.Update(sourcesMsg{hosts: a.hosts, sources: a.sources, hostStatus: map[string]protocol.StatusResult{
		"pantry": {State: protocol.StateIdle, Repository: &protocol.Repository{Location: "s3:x/pantry", StoredBytes: 142_000_000_000, LogicalBytes: 151_000_000_000}},
	}})
	cursorTo(t, a, "pantry/"+src[0].ID)
	row := ansi.Strip(strings.Join(func() []string {
		var out []string
		for _, c := range a.heroRepoCells() {
			out = append(out, c.label+" "+c.value.text)
		}
		return out
	}(), " | "))
	if !strings.Contains(row, "142.0 GB (151.0 GB uncompressed)") || !strings.Contains(row, "idle") {
		t.Errorf("pantry's second row: %s", row)
	}
	cursorTo(t, a, "#studio")
	if row := a.heroRepoCells()[0].value.text; strings.Contains(row, "142.0") {
		t.Errorf("studio shows pantry's repository: %s", row)
	}
}

// A running source shows the spinner (and its percent) instead of its
// last run; the spinner ticks only while something runs.
func TestSpinnerWhileRunning(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	s := src[0]
	if a.spinning {
		t.Fatal("spinning with nothing running")
	}
	job := protocol.Job{ID: "j-1", Kind: protocol.KindBackup, Source: &s.ID, Started: testNow}
	_, cmd := a.Update(sourcesMsg{hosts: a.hosts, sources: a.sources, remoteJobs: map[string]*protocol.Job{"pantry": &job}})
	if !a.spinning || cmd == nil {
		t.Fatal("spinner didn't start")
	}
	if h := ansi.Strip(a.sourceHealth(testNow, s)); !strings.HasPrefix(h, a.spin.View()) || strings.Contains(h, "✓") {
		t.Errorf("health %q, want the spinner", h)
	}
	a.Update(sourcesMsg{hosts: a.hosts, sources: a.sources, remoteJobs: map[string]*protocol.Job{"pantry": nil}})
	if _, cmd := a.Update(a.spin.Tick()); a.spinning || cmd != nil {
		t.Error("spinner still ticking with nothing running")
	}
}

// When the hero is too wide, the first row gives way: the second row's
// repository sizes are never cut while the first row has room to give.
func TestHeroSecondRowNotCut(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	a.Update(sourcesMsg{hosts: a.hosts, sources: a.sources, hostStatus: map[string]protocol.StatusResult{
		"pantry": {State: protocol.StatePaused, Repository: &protocol.Repository{Location: "s3:x/pantry", StoredBytes: 120_100_000_000, LogicalBytes: 142_000_000_000}},
	}})
	cursorTo(t, a, "pantry/"+src[0].ID)
	for _, w := range []int{100, 120, 140, 160} {
		a.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		if hero := ansi.Strip(a.renderHero()); !strings.Contains(hero, "120.1 GB (142.0 GB uncompressed)") {
			t.Errorf("width %d: stored cut:\n%s", w, hero)
		}
	}
}

// Tab jumps from a source far down the list straight into the hero and
// back; the hero's bottom edge says so.
func TestTabToHero(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	cursorTo(t, a, "pantry/"+src[0].ID)
	if !strings.Contains(ansi.Strip(a.renderHero()), "tab edit") {
		t.Error("no tab hint in the hero's edge")
	}
	press(a, "tab")
	if a.area != areaHero || a.heroIdx != 0 {
		t.Fatalf("tab: area %v stop %d", a.area, a.heroIdx)
	}
	if !strings.Contains(ansi.Strip(a.renderHero()), "tab back") {
		t.Error("hint doesn't say back")
	}
	press(a, "tab")
	if s, ok := a.selSource(); a.area != areaColumns || !ok || s.ID != src[0].ID {
		t.Fatalf("tab back: area %v", a.area)
	}
	press(a, "tab", "esc")
	if a.area != areaColumns {
		t.Fatalf("esc: area %v", a.area)
	}
}

// A host's link reads as one mark: ✓ connected, the spinner while
// connecting, ✗ with why.
func TestHostState(t *testing.T) {
	for _, c := range []struct {
		h    protocol.Host
		want string
	}{
		{protocol.Host{State: protocol.HostOK}, "✓ connected"},
		{protocol.Host{State: protocol.HostUnreachable, Error: "connecting"}, "* connecting"},
		{protocol.Host{State: protocol.HostUnreachable, Error: "dial tcp: refused"}, "✗ offline"},
		{protocol.Host{State: protocol.HostUnreachable}, "✗ offline"},
	} {
		if got, _ := hostState(c.h, "*"); got != c.want {
			t.Errorf("%+v: %q, want %q", c.h, got, c.want)
		}
	}
}

// A connecting host keeps the spinner going.
func TestSpinnerWhileConnecting(t *testing.T) {
	a, _ := newRemoteApp(t)
	hosts := slices.Clone(a.hosts)
	for i := range hosts {
		if !hosts[i].Local {
			hosts[i].State, hosts[i].Error = protocol.HostUnreachable, "connecting"
		}
	}
	_, cmd := a.Update(sourcesMsg{hosts: hosts, sources: a.sources})
	if !a.spinning || cmd == nil {
		t.Fatal("no spinner while connecting")
	}
}

// e on a header opens that machine's popup: the defaults, and for a
// remote an Address tab; the hero's Where opens the Address tab.
func TestHostAddressEdit(t *testing.T) {
	a, _ := newRemoteApp(t)
	cursorTo(t, a, "#studio")
	drain(a, a.runAction(action{label: "Edit", key: keyEdit}))
	if p, ok := a.modal.(*machinePopup); !ok || p.remote || p.host != "studio" {
		t.Fatalf("this Mac's Edit: %T %+v", a.modal, a.modal)
	}
	press(a, "esc")
	cursorTo(t, a, "#pantry")
	press(a, "e")
	p, ok := a.modal.(*machinePopup)
	if !ok || !p.remote || p.tab != mpDefaults {
		t.Fatalf("modal %T", a.modal)
	}
	drain(a, func() tea.Msg { return nil })
	press(a, "tab")
	f := p.addr
	if p.tab != mpAddress || f.addr.Value() != "pantry.lan:7457" {
		t.Fatalf("tab %d, address %q", p.tab, f.addr.Value())
	}
	f.addr.SetValue("")
	press(a, "1", "0", ".", "0", ".", "0", ".", "9")
	_, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(a, cmd)
	if a.modal != nil {
		t.Fatalf("still open: %v", f.err)
	}
	if h := a.findHost("pantry"); h.Address != "10.0.0.9:7457" {
		t.Errorf("pantry at %q after save", h.Address)
	}

	// The hero's Where opens the Address tab.
	press(a, "tab")
	for a.area == areaHero && a.heroStops()[a.heroIdx].label != "Address" {
		before := a.heroIdx
		press(a, "right")
		if a.heroIdx == before {
			t.Fatal("no Address stop in the hero")
		}
	}
	press(a, "enter")
	if p, ok := a.modal.(*machinePopup); !ok || p.tab != mpAddress {
		t.Errorf("Enter on Where opened %T", a.modal)
	}
}

// actionLabels is the action bar's buttons, space-separated.
func actionLabels(a *App) string {
	var labels []string
	for _, act := range a.actions() {
		labels = append(labels, act.label)
	}
	return strings.Join(labels, " ")
}

// A header shows its machine's repository in the hero, and all of it
// (credentials' source, upkeep, defaults) in the info popup.
func TestHeaderShowsRepository(t *testing.T) {
	a, _ := newRemoteApp(t)
	cursorTo(t, a, "#studio")
	var labels []string
	for _, c := range a.heroSelectionCells(a.srcCur.pos) {
		labels = append(labels, c.label)
	}
	if !slices.Contains(labels, "Repository") {
		t.Errorf("header hero %v", labels)
	}
	var info []string
	for _, f := range a.infoFields() {
		info = append(info, f.label)
	}
	for _, want := range []string{"Repository", "Password", "Checked", "Default schedule"} {
		if !slices.Contains(info, want) {
			t.Errorf("info has no %s: %v", want, info)
		}
	}
	if !strings.Contains(actionLabels(a), "Info") {
		t.Errorf("no Info in the action bar: %s", actionLabels(a))
	}
}

// Remove on a remote's header asks, keeps backups by default, and the
// host is gone afterwards; deleting asks twice. One that isn't answering
// can only be unpaired, and says what to run there.
func TestRemoveHost(t *testing.T) {
	a, _ := newRemoteApp(t)
	cursorTo(t, a, "#pantry")
	press(a, "x")
	c, ok := a.modal.(*confirm)
	if !ok || c.answers[0] != "Keep backups" || c.pick != 0 {
		t.Fatalf("modal %T %+v", a.modal, c)
	}
	press(a, "right", "enter")
	c, ok = a.modal.(*confirm)
	if !ok || c.pick != 1 || c.answers[c.pick] != "Cancel" {
		t.Fatalf("second confirm %T %+v", a.modal, c)
	}
	press(a, "esc")
	if a.rowIndex("#pantry") < 0 {
		t.Fatal("cancel removed pantry")
	}
	press(a, "x", "enter")
	if a.rowIndex("#pantry") >= 0 || a.statusMsg != "pantry removed" {
		t.Fatalf("after keep: status %q, rows %v", a.statusMsg, a.srcRows())
	}

	cursorTo(t, a, "#tower")
	press(a, "x")
	c = a.modal.(*confirm)
	if c.answers[0] != "Unpair" || len(c.answers) != 2 {
		t.Fatalf("down host's answers %v", c.answers)
	}
	press(a, "enter")
	c, ok = a.modal.(*confirm)
	if !ok || !strings.Contains(c.body, "systemctl") {
		t.Fatalf("no commands shown: %T", a.modal)
	}
	press(a, "enter")
	if len(a.hosts) != 1 {
		t.Errorf("hosts after unpairing: %v", a.hosts)
	}
}

// Add Remote: a name, the command (click or c copies), a live wait,
// and once the machine pairs and answers, the setup flow for it.
func TestAddRemote(t *testing.T) {
	a, _ := newRemoteApp(t)
	cursorTo(t, a, "#studio")
	press(a, "r")
	m, ok := a.modal.(*addRemote)
	if !ok || !m.typing() {
		t.Fatalf("modal %T", a.modal)
	}
	nameHeight := lipgloss.Height(m.view(a))
	press(a, "Bad Name!", "enter")
	if m.err == nil || m.host != "" {
		t.Fatalf("bad name: err %v host %q", m.err, m.host)
	}
	for range 9 {
		press(a, "backspace")
	}
	m.name.SetValue("Wren")
	press(a, "enter")
	if m.host != "wren" || !strings.Contains(m.res.Command, "| sudo sh") || !m.waiting(a.now()) {
		t.Fatalf("after start: host %q res %+v", m.host, m.res)
	}
	if h := lipgloss.Height(m.view(a)); h != nameHeight {
		t.Errorf("popup %d rows with the command, %d with the name", h, nameHeight)
	}
	view := ansi.Strip(m.view(a))
	if !strings.Contains(view, "waiting for wren") {
		t.Errorf("view:\n%s", view)
	}
	// A click on the command copies it.
	popup := m.view(a)
	top := heroHeight + actionHeight + max((a.bodyHeight()-lipgloss.Height(popup))/2, 0)
	for i, l := range strings.Split(popup, "\n") {
		if strings.Contains(ansi.Strip(l), "curl") {
			a.Update(tea.MouseClickMsg{X: a.width / 2, Y: top + i, Button: tea.MouseLeft})
		}
	}
	if !m.copied {
		t.Error("clicking the command didn't copy it")
	}
	// wren runs it: it pairs, answers, and has no repository yet.
	fake := a.repo.(*backup.Fake)
	wren := fake.AddRemote("wren", false)
	wren.NoRepository()
	drain(a, a.loadSources())
	f, ok := a.modal.(*setupFlow)
	if !ok || f.host != "wren" || a.statusMsg != "wren is paired" {
		t.Fatalf("after pairing: %T, status %q", a.modal, a.statusMsg)
	}
	// Expired codes offer a new one.
	a.modal = nil
	press(a, "r")
	m = a.modal.(*addRemote)
	m.name.SetValue("finch")
	press(a, "enter")
	m.res.Expires = testNow.Add(-time.Minute)
	if m.waiting(a.now()) || !strings.Contains(ansi.Strip(m.view(a)), "expired") {
		t.Error("expired code still waiting")
	}
}

// A Mac remote without Full Disk Access gets a step after pairing: Enter
// opens the setting there, and the grant, when it lands, goes on to the
// setup flow. s skips it, and its header then says so.
func TestAddRemoteMacAccess(t *testing.T) {
	a, _ := newRemoteApp(t)
	press(a, "r")
	m := a.modal.(*addRemote)
	height := lipgloss.Height(m.view(a))
	m.name.SetValue("wren")
	press(a, "enter")
	no, yes := false, true
	wren := a.repo.(*backup.Fake).AddRemote("wren", false)
	wren.NoRepository()
	wren.SetFullDiskAccess(&no)
	// Deliver the reload by hand: the poll it starts would wait.
	a.Update(a.loadSources()())
	if !m.access || a.modal != m {
		t.Fatalf("no access step: modal %T", a.modal)
	}
	if h := lipgloss.Height(m.view(a)); h != height {
		t.Errorf("access step %d rows, name step %d", h, height)
	}
	press(a, "enter")
	if wren.AccessOpened != 1 || !m.opened {
		t.Fatalf("settings not opened: %d %v", wren.AccessOpened, m.accessErr)
	}
	a.Update(accessPolledMsg{host: "wren", st: mustStatus(t, wren)})
	if a.modal != m {
		t.Fatal("left the step without access")
	}
	wren.SetFullDiskAccess(&yes)
	a.Update(accessPolledMsg{host: "wren", st: mustStatus(t, wren)})
	if f, ok := a.modal.(*setupFlow); !ok || f.host != "wren" {
		t.Fatalf("after the grant: %T", a.modal)
	}

	// Skipped: the header says so.
	a.modal = nil
	wren.SetFullDiskAccess(&no)
	drain(a, a.loadSources())
	if got := ansi.Strip(a.hostHeaderRow("wren", 30)); !strings.Contains(got, "setup") {
		t.Errorf("wren's header %q", got)
	}
	a.repo.(*backup.Fake).ForHost("wren").(*backup.Fake).SetupRepository(t.Context(), protocol.RepositorySetupParams{Location: "/srv/w", Init: true})
	drain(a, a.loadSources())
	if got := ansi.Strip(a.hostHeaderRow("wren", 30)); !strings.Contains(got, "disk access") {
		t.Errorf("wren's header %q", got)
	}
}

func mustStatus(t *testing.T, r backup.Repo) protocol.StatusResult {
	t.Helper()
	st, err := r.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Enter on a header that says what the machine still needs goes there:
// "setup" to the setup flow, "disk access" to the access step. Other
// headers still fold.
func TestHeaderTodoEnter(t *testing.T) {
	a, pantry := newRemoteApp(t)
	pantry.NoRepository()
	drain(a, a.loadSources())
	cursorTo(t, a, "#pantry")
	press(a, "enter")
	if f, ok := a.modal.(*setupFlow); !ok || f.host != "pantry" {
		t.Fatalf("enter on setup: %T", a.modal)
	}
	a.modal = nil
	no := false
	pantry.SetFullDiskAccess(&no)
	pantry.SetupRepository(t.Context(), protocol.RepositorySetupParams{Location: "/srv/p", Init: true})
	drain(a, a.loadSources())
	// Deliver the key by hand: the access step polls.
	a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m, ok := a.modal.(*addRemote); !ok || !m.access || m.host != "pantry" {
		t.Fatalf("enter on disk access: %T", a.modal)
	}
	a.modal = nil
	yes := true
	pantry.SetFullDiskAccess(&yes)
	drain(a, a.loadSources())
	press(a, "enter")
	if a.modal != nil || !a.collapsed["pantry"] {
		t.Errorf("enter on a ready header: modal %T, folded %v", a.modal, a.collapsed["pantry"])
	}
}

// With a key manager, a remote's key is made without signing in, for the
// manager's bucket, in the remote's own folder.
func TestRemoteSetupWithKeyManager(t *testing.T) {
	a, pantry := newRemoteApp(t)
	ctl := a.repo.(*backup.Fake)
	ctl.AWS.Manager = &protocol.AWSManager{Account: "123456789012", Bucket: "charm-bak-123456789012", Region: "us-east-1"}
	pantry.NoRepository()
	drain(a, a.loadSources())
	cursorTo(t, a, "#pantry")
	press(a, "enter", "enter")
	f := a.modal.(*setupFlow)
	if f.step != sfHow || f.howOptions()[0].from != keyManager {
		t.Fatalf("step %d options %+v", f.step, f.howOptions())
	}
	// Signing in stays on offer (for another bucket).
	if opts := f.howOptions(); len(opts) != 3 || opts[1].from != keySignIn {
		t.Errorf("options %+v", opts)
	}
	press(a, "enter")
	if v := f.values(); f.step != sfDetails || v["bucket"] != "charm-bak-123456789012" || v["folder"] != "pantry" {
		t.Fatalf("step %d values %v", f.step, v)
	}
	if view := ansi.Strip(f.view(a)); !strings.Contains(view, "IAM user charm-bak-pantry") {
		t.Errorf("doesn't say it creates pantry's user:\n%s", view)
	}
	press(a, "enter")
	if len(ctl.AWSCalls) != 1 || ctl.AWSCalls[0].SignIn || ctl.AWSCalls[0].Host != "pantry" {
		t.Fatalf("aws_keys %+v", ctl.AWSCalls)
	}
	if f.step != sfCreate || f.location != "s3:s3.amazonaws.com/charm-bak-123456789012/pantry" {
		t.Fatalf("step %d at %q (note %q err %v)", f.step, f.location, f.note, f.err)
	}

	// A bucket the manager doesn't serve: Enter signs in (it adds the
	// bucket), and the screen says so before.
	press(a, "esc")
	for i, fl := range f.backend.Fields {
		if fl.Key == "bucket" {
			f.field = i
		}
	}
	f.focus()
	f.input("bucket").SetValue("den-backup")
	f.input("bucket").CursorEnd()
	press(a, "s") // typed, so the key made for the other bucket is dropped
	view := ansi.Strip(f.view(a))
	if f.step != sfDetails || !strings.Contains(view, "Enter signs in") || !strings.Contains(view, "charm-bak-boundary-den-backups") || !strings.Contains(view, "enter sign in") {
		t.Fatalf("other bucket (step %d):\n%s", f.step, view)
	}
	press(a, "enter")
	if n := len(ctl.AWSCalls); n != 2 || !ctl.AWSCalls[1].SignIn || ctl.AWSCalls[1].Bucket != "den-backups" {
		t.Fatalf("aws_keys %+v", ctl.AWSCalls)
	}
	if m := f.aws.Manager; m == nil || !m.Serves("den-backups") || !m.Serves("charm-bak-123456789012") {
		t.Errorf("manager after signing in for den-backups: %+v", f.aws.Manager)
	}
}

// ↓ in a remote's Edit popup reaches Repair: the command to run there,
// then a wait that closes once the remote has gone and come back.
func TestHostRepair(t *testing.T) {
	a, _ := newRemoteApp(t)
	cursorTo(t, a, "#pantry")
	press(a, "e", "tab")
	popup := ansi.Strip(a.modal.view(a))
	press(a, "down")
	if h := lipgloss.Height(a.modal.view(a)); h != lipgloss.Height(popup) {
		t.Errorf("popup height %d on Repair, %d on the address", h, lipgloss.Height(popup))
	}
	_, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(a, cmd)
	m, ok := a.modal.(*addRemote)
	if !ok || !m.repair || !strings.Contains(m.res.Command, "| sudo sh") {
		t.Fatalf("modal %T %+v", a.modal, m)
	}
	if !strings.Contains(ansi.Strip(m.view(a)), "Repair pantry") {
		t.Errorf("view:\n%s", ansi.Strip(m.view(a)))
	}
	hosts := slices.Clone(a.hosts)
	set := func(state string) {
		for i := range hosts {
			if hosts[i].Name == "pantry" {
				hosts[i].State = state
			}
		}
		_, cmd := a.Update(sourcesMsg{hosts: slices.Clone(hosts), sources: a.sources})
		drain(a, cmd)
	}
	set(protocol.HostOK) // still the old daemon answering
	if a.modal != m {
		t.Fatal("closed before pantry went away")
	}
	set(protocol.HostUnreachable)
	set(protocol.HostOK)
	if a.modal != nil || !strings.Contains(a.statusMsg, "reinstalled") {
		t.Errorf("after pantry came back: modal %T, status %q", a.modal, a.statusMsg)
	}
}

// A source's Last run is never cut, at any width the TUI runs at: the
// name and the other cells give way.
func TestHeroLastRunNotCut(t *testing.T) {
	a, pantry := newRemoteApp(t)
	src, _ := pantry.Sources(t.Context())
	cursorTo(t, a, "pantry/"+src[0].ID)
	s, _ := a.selSource()
	r := *s.LastRun
	r.Status, r.Skipped = protocol.RunIncomplete, 80
	for i := range a.sources {
		if a.sources[i].ID == s.ID && a.sources[i].Host == s.Host {
			a.sources[i].LastRun, a.sources[i].Health = &r, protocol.HealthSkipped
		}
	}
	for _, w := range []int{100, 120, 140} {
		a.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		hero := ansi.Strip(a.renderHero())
		if !strings.Contains(hero, "80 files skipped") {
			t.Errorf("width %d: last run cut:\n%s", w, hero)
		}
		// Retention sits on the second line, beneath Schedule.
		lines := strings.Split(hero, "\n")
		if len(lines) < 3 || strings.Index(lines[2], "Retention") != strings.Index(lines[1], "Schedule") {
			t.Errorf("width %d: retention not under schedule:\n%s", w, hero)
		}
	}
}

// A machine whose AWS key charm-bak made offers New key on its header;
// it asks first, and Cancel changes nothing. Without a made key there's
// no such action.
func TestNewKeyOnHeader(t *testing.T) {
	a, _ := newRemoteApp(t)
	ctl := a.repo.(*backup.Fake)
	cursorTo(t, a, "#pantry")
	for _, act := range a.actions() {
		if act.label == "New key" {
			t.Fatal("New key offered for a key charm-bak didn't make")
		}
	}
	made := testNow.Add(-40 * 24 * time.Hour)
	for i := range a.hosts {
		if a.hosts[i].Name == "pantry" {
			a.hosts[i].KeyMade = &made
		}
	}
	if !strings.Contains(ansi.Strip(a.View().Content), "[N]ew key") {
		t.Fatalf("no New key in the action bar:\n%s", ansi.Strip(a.renderActionBar()))
	}
	press(a, "n")
	c, ok := a.modal.(*confirm)
	if !ok || !strings.Contains(c.body, "keeps the key it has") {
		t.Fatalf("modal %T", a.modal)
	}
	press(a, "esc") // Cancel
	if len(ctl.Rotated) != 0 {
		t.Fatalf("rotated on cancel: %v", ctl.Rotated)
	}
	press(a, "n")
	drain(a, a.modal.update(a, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if len(ctl.Rotated) != 1 || ctl.Rotated[0] != "pantry" {
		t.Fatalf("rotations asked %v", ctl.Rotated)
	}
	press(a, "i")
	if !strings.Contains(ansi.Strip(a.View().Content), "AWS key made") {
		t.Error("info doesn't show the key's age")
	}
}

// A machine on S3 with a key charm-bak didn't make is offered adoption
// once; Not now is remembered, Adopt key on its header asks again, and
// adopting goes to this Mac's daemon.
func TestAdoptOffer(t *testing.T) {
	shown := filepath.Join(t.TempDir(), "offered-pantry")
	adoptOfferedFile = func(host string) string {
		if host == "pantry" {
			return shown
		}
		return ""
	}
	defer func() { adoptOfferedFile = func(string) string { return "" } }()
	// The fake pantry is on S3 with a key it was given: offered at start.
	a, _ := newRemoteApp(t)
	ctl := a.repo.(*backup.Fake)
	c, ok := a.modal.(*confirm)
	if !ok || !strings.Contains(c.title, "Adopt pantry") {
		t.Fatalf("no offer: %T", a.modal)
	}
	press(a, "esc") // Not now
	if _, err := os.Stat(shown); err != nil || len(ctl.Adopted) != 0 {
		t.Fatalf("not now: recorded %v, adopted %v", err, ctl.Adopted)
	}
	a.adoptAsked = nil
	a.offerAdopt()
	if a.modal != nil {
		t.Fatal("offered twice")
	}
	cursorTo(t, a, "#pantry")
	if !strings.Contains(ansi.Strip(a.renderActionBar()), "Ad[o]pt key") {
		t.Fatalf("no Adopt key action:\n%s", ansi.Strip(a.renderActionBar()))
	}
	press(a, "o")
	drain(a, a.modal.update(a, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if len(ctl.Adopted) != 1 || ctl.Adopted[0] != "pantry" {
		t.Fatalf("adopted %v", ctl.Adopted)
	}
}

// Several machines waiting: one question, "both" for two, "all 3" from
// three on, and one adoption request for all of them.
func TestAdoptOfferSeveral(t *testing.T) {
	if many(2) != "both" || many(3) != "all 3" {
		t.Errorf("many: %q %q", many(2), many(3))
	}
	dir := t.TempDir()
	adoptOfferedFile = func(host string) string { return filepath.Join(dir, host) }
	defer func() { adoptOfferedFile = func(string) string { return "" } }()
	a, _ := newRemoteApp(t)
	ctl := a.repo.(*backup.Fake)
	c, ok := a.modal.(*confirm)
	if !ok || c.title != "Adopt both machines' AWS keys?" || c.answers[0] != "Adopt both" {
		t.Fatalf("offer: %T %+v", a.modal, c)
	}
	drain(a, a.modal.update(a, tea.KeyPressMsg{Code: tea.KeyEnter}))
	if len(ctl.Adopted) != 2 {
		t.Fatalf("adopted %v", ctl.Adopted)
	}
}
