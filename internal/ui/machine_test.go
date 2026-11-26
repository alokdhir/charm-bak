package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm.land/lipgloss/v2"
)

// openMachine opens the first header's popup (this Mac) and loads it.
func openMachine(t *testing.T, a *App) *machinePopup {
	t.Helper()
	cursorTo(t, a, "#"+a.srcRows()[0].host)
	press(a, "e")
	p, ok := a.modal.(*machinePopup)
	if !ok {
		t.Fatalf("e on the header: modal %T", a.modal)
	}
	return p
}

// e on this Mac's header edits its defaults: a schedule change saves and
// reaches the sources that inherit it.
func TestMachineDefaults(t *testing.T) {
	a := newTestApp(t, 160, 40)
	p := openMachine(t, a)
	before := defaultScheduleChoices()[p.schedule]
	press(a, "right")
	pressSave(a)
	if a.modal != nil {
		t.Fatalf("enter: still open, err %v", p.err)
	}
	res, _ := a.repo.Repository(t.Context())
	after := int64(0)
	if res.Defaults.Schedule != nil {
		after = res.Defaults.Schedule.Every
	}
	if after == before || after != defaultScheduleChoices()[p.schedule] {
		t.Errorf("schedule %d -> %d", before, after)
	}
	if a.statusMsg != "Defaults saved" {
		t.Errorf("status %q", a.statusMsg)
	}
}

// Keep is one choice: ←→ cycles the presets and custom, custom shows
// the counts (digits only), and Enter saves the chosen one.
func TestMachineKeep(t *testing.T) {
	a := newTestApp(t, 160, 40)
	p := openMachine(t, a)
	press(a, "down")
	if p.field != mpKeep {
		t.Fatalf("field %d", p.field)
	}
	for !p.keep.custom() {
		press(a, "right")
	}
	press(a, "down", "x")
	if p.field != mpCount0 || strings.Contains(p.keep.counts[0].Value(), "x") {
		t.Errorf("field %d, newest %q", p.field, p.keep.counts[0].Value())
	}
	press(a, "up")
	for p.keep.preset() != 1 {
		press(a, "right")
	}
	if p.keep.text() != "1 year" || !strings.Contains(p.fieldHelp(a), "one a month for a year") {
		t.Errorf("text %q, help %q", p.keep.text(), p.fieldHelp(a))
	}
	pressSave(a)
	if a.modal != nil {
		t.Fatalf("still open: %v", p.err)
	}
	res, _ := a.repo.Repository(t.Context())
	if res.Defaults.Retention != keepPresets[1].r {
		t.Errorf("saved %+v", res.Defaults.Retention)
	}
}

// A retention that matches a preset shows as it; anything else as custom
// with its counts; forever is restic's unlimited.
func TestKeepEditor(t *testing.T) {
	k := newKeepEditor(keepPresets[3].r, protocol.Retention{}, false, false)
	if k.text() != "forever" || k.custom() {
		t.Errorf("forever reads %q", k.text())
	}
	if r, _, _ := k.retention(); r.Yearly != protocol.KeepUnlimited {
		t.Errorf("forever %+v", r)
	}
	odd := protocol.Retention{Last: 10, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 12, Yearly: 3}
	k = newKeepEditor(odd, protocol.Retention{}, false, false)
	if !k.custom() || k.counts[2].Value() != "7" {
		t.Errorf("odd retention: custom %v, days %q", k.custom(), k.counts[2].Value())
	}
	k = newKeepEditor(odd, keepPresets[1].r, true, true)
	if k.text() != "default (1 year)" {
		t.Errorf("inherited reads %q", k.text())
	}
	if _, inherit, _ := k.retention(); !inherit {
		t.Error("default doesn't inherit")
	}
	if got := retentionReach(keepPresets[3].r, true); got != "forever (default)" {
		t.Errorf("reach %q", got)
	}
}

// Skip is a list, one pattern per row: x removes the selected one, the
// add line adds, and Enter on an empty add line saves.
func TestMachineSkipList(t *testing.T) {
	a := newTestApp(t, 160, 40)
	p := openMachine(t, a)
	if strings.Join(p.skip.items, "|") != "*.log" {
		t.Fatalf("skip %q, want *.log", p.skip.items)
	}
	for p.field != mpSkip {
		press(a, "down")
	}
	if p.field != mpSkip || p.skip.cur != 0 || p.typing() {
		t.Fatalf("field %d, cur %d", p.field, p.skip.cur)
	}
	if !strings.Contains(p.skip.help(), "named like *.log") {
		t.Errorf("help %q", p.skip.help())
	}
	press(a, "x") // removes *.log; the cursor is on the add line
	typeText(a, "Screenshot *")
	press(a, "enter")
	typeText(a, "node_modules")
	press(a, "enter", "up", "up", "enter")
	if p.skip.edit != 0 {
		t.Fatalf("enter on a pattern: edit %d", p.skip.edit)
	}
	press(a, "backspace", "backspace", "*", "enter")
	pressSave(a)
	if a.modal != nil {
		t.Fatalf("still open: %v", p.err)
	}
	res, _ := a.repo.Repository(t.Context())
	if strings.Join(res.Defaults.Excludes, "|") != "Screenshot*|node_modules" {
		t.Errorf("saved %q", res.Defaults.Excludes)
	}
}

// A remote's popup keeps one size on both tabs (Alok: the popup changing
// shape was jarring).
func TestMachineSameSizeOnBothTabs(t *testing.T) {
	for _, w := range []int{100, 120, 160} {
		a, _ := newRemoteApp(t)
		a.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		cursorTo(t, a, "#pantry")
		press(a, "e")
		p := a.modal.(*machinePopup)
		defs := p.view(a)
		press(a, "tab")
		addr := p.view(a)
		if lipgloss.Height(addr) != lipgloss.Height(defs) || lipgloss.Width(addr) != lipgloss.Width(defs) {
			t.Errorf("width %d: defaults %dx%d, address %dx%d", w,
				lipgloss.Width(defs), lipgloss.Height(defs), lipgloss.Width(addr), lipgloss.Height(addr))
		}
	}
}

func TestExplainPattern(t *testing.T) {
	for p, want := range map[string]string{
		"node_modules":  "named node_modules, in any folder",
		"Screenshot *":  "named like Screenshot *, in any folder",
		"/Users/a/tmp":  "exactly /Users/a/tmp",
		"Library/Cache": "Library/Cache in any folder",
		"!keep":         "Brings back keep",
	} {
		if got := explainPattern(p); !strings.Contains(got, want) {
			t.Errorf("%q: %q", p, got)
		}
	}
}

// Custom starts from the choice before it.
func TestKeepCustomStartsFromPreset(t *testing.T) {
	k := newKeepEditor(keepPresets[3].r, protocol.Retention{}, false, false)
	k.cycle(1)
	if !k.custom() || k.counts[4].Value() != "36" || k.counts[5].Value() != "∞" {
		t.Errorf("custom after forever: months %q years %q", k.counts[4].Value(), k.counts[5].Value())
	}
	if r, _, _ := k.retention(); r != keepPresets[3].r {
		t.Errorf("custom reads %+v", r)
	}
}

// With info open, ↑↓ move the column's cursor and the popup follows the
// new selection (like Quick Look); ↑ on the top row stays out of the hero.
func TestInfoFollowsCursor(t *testing.T) {
	a := newTestApp(t, 160, 40)
	cursorTo(t, a, "#"+a.srcRows()[0].host)
	press(a, "i")
	if !a.showInfo || a.infoFields()[0].label != "Host" {
		t.Fatalf("info on the header: %v", a.showInfo)
	}
	press(a, "down")
	s, ok := a.selSource()
	if !a.showInfo || !ok || a.infoFields()[0].value != s.Name {
		t.Fatalf("after down: open %v, first %+v", a.showInfo, a.infoFields()[0])
	}
	press(a, "up", "up")
	if !a.showInfo || a.area != areaColumns || !a.onHeader() {
		t.Errorf("up past the top: open %v, area %v", a.showInfo, a.area)
	}
	press(a, "space")
	if a.showInfo {
		t.Error("space didn't close it")
	}
	press(a, "space")
	if !a.showInfo {
		t.Error("space didn't open it")
	}
}

func init() { infoTickEvery = 0 } // step the spring without the clock

// The info popup springs to each selection's height and settles exactly
// on it; mid-spring it draws at the in-between height with the hints last.
func TestInfoHeightSprings(t *testing.T) {
	a := newTestApp(t, 160, 40)
	cursorTo(t, a, "#"+a.srcRows()[0].host)
	press(a, "i")
	if a.infoAnim.ticking || a.infoShownRows() != a.infoRows() {
		t.Fatalf("after opening: ticking %v, shown %d, want %d", a.infoAnim.ticking, a.infoShownRows(), a.infoRows())
	}
	header := a.infoRows()
	press(a, "down")
	if a.infoRows() == header {
		t.Fatalf("source and header both %d rows: pick fixtures that differ", header)
	}
	if a.infoAnim.ticking || a.infoShownRows() != a.infoRows() {
		t.Errorf("after down: shown %d, want %d", a.infoShownRows(), a.infoRows())
	}
	a.infoAnim.pos, a.infoAnim.ticking = float64(header), true
	popup := a.renderInfo()
	if lipgloss.Height(popup) != header+2 || !strings.Contains(popup, "esc to close") {
		t.Errorf("mid-spring: height %d, want %d", lipgloss.Height(popup), header+2)
	}
}

// Closing and reopening mid-spring leaves one tick chain: the old chain's
// tick is dropped, so the spring doesn't run at double speed.
func TestInfoReopenDropsOldTicks(t *testing.T) {
	a := newTestApp(t, 160, 40)
	a.Update(tea.KeyPressMsg{Code: 'i', Text: "i"}) // opens; its tick isn't run
	old := infoTickMsg{a.infoAnim.gen}
	a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.infoAnim.ticking {
		t.Error("closed, still ticking")
	}
	a.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	pos := a.infoAnim.pos
	if _, cmd := a.Update(old); cmd != nil || a.infoAnim.pos != pos {
		t.Errorf("the old chain's tick moved the spring: %v -> %v", pos, a.infoAnim.pos)
	}
	if _, cmd := a.Update(infoTickMsg{a.infoAnim.gen}); cmd == nil || a.infoAnim.pos == pos {
		t.Error("the new chain's tick did nothing")
	}
}

// Info opened from the hero and walked with ↓ leaves focus in the column.
func TestInfoFromHeroReturnsToColumn(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "tab")
	if a.area != areaHero {
		t.Fatalf("tab: area %v", a.area)
	}
	a.showInfo = true
	press(a, "down", "esc")
	if a.area != areaColumns || a.showInfo {
		t.Errorf("area %v, info %v", a.area, a.showInfo)
	}
}

// Info taller than the screen scrolls: the title and hints stay, the
// hints say there's more, and a click copies the row actually shown.
func TestInfoScrollsWhenTooTall(t *testing.T) {
	a := newTestApp(t, 100, 20)
	cursorTo(t, a, "#"+a.srcRows()[0].host)
	press(a, "i")
	over := a.infoScrollMax()
	if over == 0 {
		t.Fatalf("the header's info fits in %d rows: use a taller one", a.infoMaxRows())
	}
	lines, owner := a.infoBody()
	if len(lines) != a.infoMaxRows() || !strings.Contains(lines[len(lines)-1], "more below") || !strings.Contains(lines[0], "Info") {
		t.Fatalf("top: %d lines, hints %q", len(lines), lines[len(lines)-1])
	}
	first := owner[2]
	a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	press(a, "shift+down")
	if _, owner = a.infoBody(); a.infoAnim.scroll != min(2, over) || owner[2] == first && over > 1 {
		t.Errorf("scroll %d, row 2 still field %d", a.infoAnim.scroll, first)
	}
	for range over + 3 {
		press(a, "shift+down")
	}
	lines, owner = a.infoBody()
	all, allOwner := a.infoLayout(popupInnerWidth(a.width))
	if a.infoAnim.scroll != over || !strings.Contains(lines[len(lines)-1], "more above") ||
		owner[len(owner)-2] != allOwner[len(all)-2] {
		t.Errorf("bottom: scroll %d of %d, hints %q", a.infoAnim.scroll, over, lines[len(lines)-1])
	}
	press(a, "down")
	if a.infoAnim.scroll != 0 {
		t.Errorf("a new selection starts at the top: scroll %d", a.infoAnim.scroll)
	}
}
