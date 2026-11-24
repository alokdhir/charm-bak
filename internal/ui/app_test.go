package ui

import (
	"charm.land/bubbles/v2/spinner"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

var testNow = time.Date(2026, 11, 18, 22, 0, 0, 0, time.UTC)

// newTestApp builds an App over the fixture repo at w x h and runs every
// load to completion, the way the Bubble Tea runtime would.
func newTestApp(t *testing.T, w, h int) *App {
	t.Helper()
	a := NewApp(backup.NewFake(testNow), func() time.Time { return testNow })
	a.Update(tea.WindowSizeMsg{Width: w, Height: h})
	drain(a, a.Init())
	return a
}

// drain runs cmd and every command its messages produce.
func drain(a *App, cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		// The event subscription waits forever; tests deliver events
		// with pumpEvents instead.
		if ready, ok := msg.(eventsReadyMsg); ok {
			testEvents[a] = ready.ch
			continue
		}
		// The spinner ticks for as long as a job runs: not here.
		if _, ok := msg.(spinner.TickMsg); ok {
			continue
		}
		_, next := a.Update(msg)
		queue = append(queue, next)
	}
}

// testEvents is each test App's event channel, held back by drain.
var testEvents = map[*App]<-chan protocol.Event{}

// pumpEvents applies events from the app until none arrives for wait.
func pumpEvents(a *App, wait time.Duration) {
	ch := testEvents[a]
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			_, cmd := a.Update(eventMsg{ch, e})
			// The command re-arms waitEvent; run the rest, skip that.
			drainSkippingWait(a, cmd)
		case <-time.After(wait):
			return
		}
	}
}

// drainSkippingWait is drain for an event's follow-ups: everything but
// the blocking wait for the next event.
func drainSkippingWait(a *App, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				drainSkippingWait(a, c)
			}
			return
		}
		if _, ok := msg.(eventMsg); ok {
			return // pumpEvents reads the channel itself
		}
		_, next := a.Update(msg)
		drainSkippingWait(a, next)
	case <-time.After(50 * time.Millisecond):
		// The wait for the next event: leave it.
	}
}

func press(a *App, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "left":
			msg = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "space":
			msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		case "shift+up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift}
		case "shift+down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift}
		case "ctrl+o":
			msg = tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}
		case "ctrl+t":
			msg = tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}
		case "ctrl+n":
			msg = tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		_, cmd := a.Update(msg)
		drain(a, cmd)
	}
}

// typeText types s one character at a time.
func typeText(a *App, s string) {
	for _, r := range s {
		press(a, string(r))
	}
}

func TestCursorMoveAndScroll(t *testing.T) {
	var c cursor
	c.move(5, 10, 3)
	if c.pos != 5 || c.off != 3 {
		t.Errorf("down 5 in a 3-row window: pos %d off %d, want 5 3", c.pos, c.off)
	}
	c.move(100, 10, 3)
	if c.pos != 9 || c.off != 7 {
		t.Errorf("past the end: pos %d off %d, want 9 7", c.pos, c.off)
	}
	c.move(-100, 10, 3)
	if c.pos != 0 || c.off != 0 {
		t.Errorf("past the start: pos %d off %d, want 0 0", c.pos, c.off)
	}
	c.move(1, 0, 3)
	if c.pos != 0 {
		t.Errorf("empty list: pos %d, want 0", c.pos)
	}
}

func TestColumnLayoutSlidesLeftButKeepsFocus(t *testing.T) {
	const treeWidth = 26
	widths := []int{sourcesWidth, snapshotsWidth, treeWidth, treeWidth, treeWidth}
	// Everything plus a preview: 24+24+3*26 + 5 dividers + 30 = 161.
	if got := columnLayout(162, widths, 4); got != 0 {
		t.Errorf("wide terminal: first %d, want 0", got)
	}
	// 120 wide: drop Sources (25), then Snapshots (25) to fit 161-50=111.
	if got := columnLayout(120, widths, 4); got != 2 {
		t.Errorf("120 wide: first %d, want 2", got)
	}
	// Never slide the focused column off, even if the preview starves.
	if got := columnLayout(40, widths, 1); got != 1 {
		t.Errorf("narrow with Snapshots focused: first %d, want 1", got)
	}
}

// Columns slid off the left show up as a breadcrumb on the action bar's
// row; nothing hidden, no breadcrumb.
func TestBreadcrumbNamesHiddenColumns(t *testing.T) {
	a := newTestApp(t, 160, 40)
	if a.breadcrumb() != "" {
		t.Fatalf("wide: breadcrumb %q", a.breadcrumb())
	}
	a = newTestApp(t, 160, 30)
	press(a, "right", "right", "right", "right") // into the tree, then deeper
	// Narrowed afterwards: below the minimum size keys do nothing, and
	// the layout math is the same.
	a.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	_, first := a.columnWidths()
	if first == 0 {
		t.Fatal("setup: nothing slid off at 80 wide")
	}
	crumb := a.breadcrumb()
	if !strings.HasPrefix(crumb, selSrc(t, a).Name+" › ") {
		t.Errorf("breadcrumb %q", crumb)
	}
	bar := a.renderActionBar()
	if lipgloss.Width(bar) != 80 || !strings.Contains(ansi.Strip(bar), "›") {
		t.Errorf("action bar %q", ansi.Strip(bar))
	}
}

// The app decides health; the TUI only maps it to a mark.
func TestSourceState(t *testing.T) {
	for health, want := range map[string]string{
		protocol.HealthOK: "✓", protocol.HealthOverdue: "!", protocol.HealthNever: "!", protocol.HealthFailed: "✗",
	} {
		if _, m := sourceState(protocol.Source{Health: health}); m != want {
			t.Errorf("%s: %q, want %q", health, m, want)
		}
	}
}

func TestFitRowIsExactWidth(t *testing.T) {
	for _, w := range []int{5, 12, 30} {
		for _, c := range [][2]string{{"short", "9d"}, {"a much longer name than fits", "123 KB"}, {"", ""}} {
			if got := lipgloss.Width(fitRow(c[0], c[1], w)); got != w {
				t.Errorf("fitRow(%q, %q, %d) is %d wide", c[0], c[1], w, got)
			}
		}
	}
}

func TestStartupLoadsOnlyTheSnapshotRoot(t *testing.T) {
	a := newTestApp(t, 160, 40)
	if len(a.sources) != 4 || len(a.snaps) == 0 {
		t.Fatalf("sources %d snaps %d", len(a.sources), len(a.snaps))
	}
	// Sources focused: the one pane right of the walked columns is the
	// newest snapshot's root. Nothing inside it is opened.
	if len(a.levels) != 1 || a.levels[0].name != "charm-bak" {
		t.Fatalf("levels %v, want just charm-bak", levelNames(a))
	}
}

func levelNames(a *App) []string {
	var names []string
	for _, l := range a.levels {
		names = append(names, l.name)
	}
	return names
}

// Regression: opening a folder used to open its first child, and that
// child's first child, all the way down.
func TestNeverLoadsMoreThanOnePaneBeyondFocus(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right") // into charm-bak/, cursor on docs/
	if got := levelNames(a); !slices.Equal(got, []string{"charm-bak", "docs"}) {
		t.Fatalf("levels %v, want charm-bak, docs", got)
	}
	press(a, "down") // internal/
	if got := levelNames(a); !slices.Equal(got, []string{"charm-bak", "internal"}) {
		t.Errorf("on internal/: levels %v, want charm-bak, internal", got)
	}
	press(a, "left") // back to Snapshots: the root is the pane again
	if got := levelNames(a); !slices.Equal(got, []string{"charm-bak"}) {
		t.Errorf("after left: levels %v, want charm-bak", got)
	}
}

func TestFileShowsContentsInTheSamePane(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right") // cursor on docs/ in charm-bak/
	dirCols := strings.Count(strings.Split(a.renderColumns(), "\n")[0], "│")
	press(a, "down", "down") // AGENTS.md
	if a.file.dump.Data == nil || a.file.loading {
		t.Fatalf("file preview not loaded: %+v", a.file)
	}
	if got := levelNames(a); !slices.Equal(got, []string{"charm-bak"}) {
		t.Errorf("on a file: levels %v, want only charm-bak", got)
	}
	fileCols := strings.Count(strings.Split(a.renderColumns(), "\n")[0], "│")
	if dirCols != fileCols {
		t.Errorf("dividers: %d on a folder, %d on a file; the layout changed shape", dirCols, fileCols)
	}
}

func TestStaleFileReplyIsDropped(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right", "down", "down") // AGENTS.md
	e, _ := a.selectedEntry()
	cmd := a.loadFile(e)
	press(a, "down") // go.mod
	a.Update(cmd())
	if a.file.key == e.key() {
		t.Errorf("late AGENTS.md reply replaced the go.mod preview")
	}
}

func TestMovingASourceReloadsEverythingRightOfIt(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "down")
	if a.snapsFor != sourceKey(a.sources[1]) {
		t.Errorf("snapshots are for %q, want %q", a.snapsFor, a.sources[1].ID)
	}
	if a.levels[0].name != "charm-cli" {
		t.Errorf("tree root %q, want charm-cli", a.levels[0].name)
	}
}

func TestStaleRepliesAreDropped(t *testing.T) {
	a := newTestApp(t, 160, 40)
	cmd := a.loadSnapshots() // for source 0
	press(a, "down")         // cursor moves to source 1 first
	a.Update(cmd())          // the old reply lands late
	if a.snapsFor != sourceKey(a.sources[1]) {
		t.Errorf("late reply overwrote snapshots with %q", a.snapsFor)
	}
}

func TestFocusRoundTrip(t *testing.T) {
	a := newTestApp(t, 160, 40)
	cursorTo(t, a, a.rowKey(a.srcRows()[0]))
	press(a, "up")
	if a.area != areaHero || a.heroStops()[a.heroIdx].line != 1 {
		t.Fatalf("up from the top row: area %v, want the hero's bottom line", a.area)
	}
	press(a, "enter")
	if !strings.Contains(a.statusMsg, strings.ToLower(a.heroStops()[a.heroIdx].label)) {
		t.Errorf("enter on %q: status %q", a.heroStops()[a.heroIdx].label, a.statusMsg)
	}
	press(a, "down")
	if a.area != areaColumns {
		t.Errorf("down: area %v, want columns", a.area)
	}
}

// Arrows in the hero go to the nearest stop in that direction, or nowhere.
func TestHeroArrowsAreSpatial(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "tab") // hero, top line
	stops := a.heroStops()
	if a.area != areaHero || stops[a.heroIdx].line != 0 {
		t.Fatalf("up x3: area %v, line %d", a.area, stops[a.heroIdx].line)
	}
	press(a, "left", "left", "left", "left", "left", "left")
	if a.heroIdx != 0 {
		t.Fatalf("left to the start: stop %d", a.heroIdx)
	}
	press(a, "up", "left")
	if a.area != areaHero || a.heroIdx != 0 {
		t.Errorf("up/left from the top-left stop moved: area %v, stop %d", a.area, a.heroIdx)
	}
	press(a, "right", "right", "right", "right", "right", "right", "right", "right")
	if stops[a.heroIdx].line != 0 {
		t.Errorf("right past the end of line 0 wrapped to line %d", stops[a.heroIdx].line)
	}
	press(a, "left", "left", "left", "left", "left", "left", "down")
	if a.area != areaHero || stops[a.heroIdx].line != 1 {
		t.Errorf("down from the top-left stop: area %v, line %d, want hero line 1", a.area, stops[a.heroIdx].line)
	}
	press(a, "right", "right", "right", "right")
	if stops[a.heroIdx].line != 1 {
		t.Errorf("right past the end of line 1 left the line")
	}
	press(a, "down")
	if a.area != areaColumns {
		t.Errorf("down from line 1: area %v, want columns", a.area)
	}
}

func TestHotkeysEitherCase(t *testing.T) {
	fakeHome(t)
	a := newTestApp(t, 160, 40)
	press(a, "A")
	if _, ok := a.modal.(*folderPicker); !ok {
		t.Errorf("A: modal %T, want the picker", a.modal)
	}
}

func TestHotkeysWorkFromAnyArea(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "tab", "b") // in the hero, Sources focused
	if !strings.HasPrefix(a.statusMsg, "Backing up") {
		t.Errorf("b from the hero: status %q", a.statusMsg)
	}
}

func TestEveryActionKeyIsInHelp(t *testing.T) {
	a := newTestApp(t, 160, 40)
	for col := colSources; col <= colTree; col++ {
		a.col = col
		for _, act := range a.actions() {
			if !slices.ContainsFunc(helpBindings, func(b2 key.Binding) bool {
				return slices.Equal(b2.Keys(), act.key.Keys())
			}) {
				t.Errorf("action %q (%v) is missing from help", act.label, act.key.Keys())
			}
		}
	}
}

// Every frame must fit the terminal exactly: a line too wide wraps and a
// frame too tall scrolls, and either corrupts the screen for good.
func TestFrameFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{100, 20}, {120, 30}, {160, 40}, {220, 60}, {80, 24}, {99, 19}} {
		w, h := size[0], size[1]
		a := newTestApp(t, w, h)
		r := NewRoot(a)
		r.Update(tea.WindowSizeMsg{Width: w, Height: h})
		for _, keys := range [][]string{nil, {"right", "right", "right", "right"}, {"?"}, {"up", "up"}, {"right", "right", "i"}, {"right", "d"}, {"right", "right", "right", "d", "pgdown"}, {"right", "right", "r"}, {"right", "right", "d", "r", "enter"}, {"up", "e"}, {"up", "e", "down", "down", "down", "down", "down", "down", "down"}, {"ctrl+n"}} {
			press(a, keys...)
			frame := r.View().Content
			if got := lipgloss.Height(frame); got != h {
				t.Errorf("%dx%d after %v: %d lines", w, h, keys, got)
			}
			for i, line := range strings.Split(frame, "\n") {
				if lw := lipgloss.Width(line); lw > w {
					t.Errorf("%dx%d after %v: line %d is %d wide", w, h, keys, i, lw)
					break
				}
			}
			press(a, "esc", "esc", "esc")
		}
	}
}

// The pane right of the focused column is a preview: its cursor row must
// not be highlighted, or it reads as if the cursor were already in it.
func TestPreviewPaneHasNoCursorHighlight(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right") // focus charm-bak/, pane shows docs/
	pane := a.renderColumn(colTree+1, 40, 10)
	plain := a.renderColumn(colTree+1, 40, 10)
	a.col = colTree + 1 // pretend focus moved in: now it must highlight
	focused := a.renderColumn(colTree+1, 40, 10)
	if pane != plain || pane == focused {
		t.Errorf("preview pane rendered with a cursor highlight")
	}
}

// With Sources focused, the Files pane shows the snapshot under the
// Snapshots cursor, so that row is marked.
func TestSnapshotCursorShowsWhileSourcesFocused(t *testing.T) {
	a := newTestApp(t, 160, 40)
	if a.col != colSources || len(a.snaps) == 0 {
		t.Fatalf("setup: col %d, %d snapshots", a.col, len(a.snaps))
	}
	col := a.renderColumn(colSnapshots, 30, 10)
	row := strings.Split(col, "\n")[1]
	if !strings.Contains(row, styles.SelectedDimStyle().Render(ansi.Strip(row))) {
		t.Errorf("first snapshot row isn't marked: %q", row)
	}
}

func TestMidTruncKeepsTheEnd(t *testing.T) {
	got := midTrunc("quarterly-report-2026-final-v2.pdf", 20)
	if lipgloss.Width(got) != 20 || !strings.HasSuffix(got, "v2.pdf") || !strings.HasPrefix(got, "quarterly") {
		t.Errorf("midTrunc = %q", got)
	}
	if got := midTrunc("short", 20); got != "short" {
		t.Errorf("short name changed: %q", got)
	}
}

func TestTreeColWidthFitsNamesWithinBounds(t *testing.T) {
	short := dirLevel{name: "x", entries: []protocol.Entry{{Name: "a", Type: protocol.TypeFile, Size: 1}}}
	if got := treeColWidth(short); got != treeMinWidth {
		t.Errorf("short names: %d, want min %d", got, treeMinWidth)
	}
	long := dirLevel{name: "x", entries: []protocol.Entry{{Name: strings.Repeat("n", 80), Type: protocol.TypeFile, Size: 1}}}
	if got := treeColWidth(long); got != treeMaxWidth {
		t.Errorf("long names: %d, want max %d", got, treeMaxWidth)
	}
	mid := dirLevel{name: "x", entries: []protocol.Entry{{Name: strings.Repeat("n", 20), Type: protocol.TypeFile, Size: 2048}}}
	if got, want := treeColWidth(mid), 20+1+len("2.0 KB")+2; got != want {
		t.Errorf("fitted: %d, want %d", got, want)
	}
}

func TestSelectionPathIsTheLivePath(t *testing.T) {
	a := newTestApp(t, 160, 40)
	src := a.sources[0].Paths[0]
	press(a, "right", "right", "down", "right") // charm-bak/internal/, cursor on backup/
	got, ok := a.selectionPath()
	if want := src + "/internal/backup"; !ok || got != want {
		t.Errorf("selectionPath = %q, want %q", got, want)
	}
}

func click(a *App, x, y int) tea.Cmd {
	_, cmd := a.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

// heroNameX is the screen column where the hero's name cell starts.
func heroNameX(a *App) int { return heroContentX + a.heroGrid().valueX[0] }

func TestClickHeroNameCopiesFullPath(t *testing.T) {
	a := newTestApp(t, 160, 40)
	press(a, "right", "right")
	if cmd := click(a, heroContentX, heroSelectionY); cmd != nil {
		t.Error("click on the Name label copied")
	}
	if cmd := click(a, heroNameX(a), heroSelectionY); cmd == nil {
		t.Fatal("click on the hero name did nothing")
	}
	want, _ := a.selectionPath()
	if a.statusMsg != "copied "+midTrunc(want, 60) {
		t.Errorf("status %q, want copied %s", a.statusMsg, want)
	}
	if cmd := click(a, a.width-3, heroSelectionY); cmd != nil {
		t.Errorf("click far right of the name copied something")
	}
}

func TestClickThatFocusesTheWindowIsIgnored(t *testing.T) {
	a := newTestApp(t, 160, 40)
	a.Update(tea.FocusMsg{})
	if cmd := click(a, heroNameX(a), heroSelectionY); cmd != nil {
		t.Error("the focusing click copied")
	}
	if cmd := click(a, heroNameX(a), heroSelectionY); cmd == nil {
		t.Error("the next click was swallowed too")
	}
}

// A wrapped value spans several popup lines; clicking any of them copies
// the whole value, never one line of it.
func TestInfoRowsCopyTheirWholeValue(t *testing.T) {
	a := newTestApp(t, 120, 40)
	press(a, "right", "right", "right") // into docs/
	for i, e := range a.levels[1].entries {
		if strings.Contains(e.Name, "fit-nowhere") {
			a.levels[1].cur.pos = i
		}
	}
	press(a, "i")
	if !a.showInfo {
		t.Fatal("i did not open info")
	}
	lines, owner := a.infoLayout(popupInnerWidth(a.width))
	fields := a.infoFields()
	popup := a.renderInfo()
	top := heroHeight + actionHeight + max((a.bodyHeight()-lipgloss.Height(popup))/2, 0)
	left := max((a.width-lipgloss.Width(popup))/2, 0)
	nameLines := 0
	for i, o := range owner {
		if o < 0 || fields[o].label != "Name" {
			continue
		}
		nameLines++
		f, ok := a.infoFieldAt(left+3, top+1+i)
		if !ok || f.value != fields[o].value {
			t.Errorf("line %d (%q): got %+v", i, lines[i], f)
		}
	}
	if nameLines < 2 {
		t.Errorf("long name took %d lines; expected it to wrap", nameLines)
	}
	press(a, "esc")
	if a.showInfo {
		t.Error("esc did not close info")
	}
}

// The hero's grid holds still while the cursor moves within a column.
func TestHeroGridStableAcrossCursor(t *testing.T) {
	a := newTestApp(t, 200, 40)
	press(a, "right", "right") // tree root
	g0 := a.heroGrid()
	press(a, "down", "down", "down")
	g1 := a.heroGrid()
	if !slices.Equal(g0.valueX, g1.valueX) || !slices.Equal(g0.valueW, g1.valueW) {
		t.Errorf("grid moved with the cursor: %v/%v -> %v/%v", g0.valueX, g0.valueW, g1.valueX, g1.valueW)
	}
	// Both rows share columns: the repo row's second label starts where
	// the selection row's does.
	if g1.labelX[1] != g1.valueX[0]+g1.valueW[0]+heroCellGap {
		t.Errorf("column 1 label at %d, want %d", g1.labelX[1], g1.valueX[0]+g1.valueW[0]+heroCellGap)
	}
}

func TestConnectScreenStartsTheApp(t *testing.T) {
	up := false
	launched := 0
	r := NewConnectRoot(Connector{
		Dial: func(context.Context) (backup.Repo, error) {
			if !up {
				return nil, errors.New("the charm-bak app isn't running")
			}
			return backup.NewFake(testNow), nil
		},
		Launch: func() error { launched++; up = true; return nil },
	}, func() time.Time { return testNow })
	r.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	var run func(cmd tea.Cmd)
	run = func(cmd tea.Cmd) {
		for cmd != nil {
			msg := cmd()
			if b, ok := msg.(tea.BatchMsg); ok {
				for _, c := range b {
					run(c)
				}
				return
			}
			if _, ok := msg.(eventsReadyMsg); ok {
				return // the event wait never ends; not needed here
			}
			_, cmd = r.Update(msg)
		}
	}
	run(r.Init())
	if r.app != nil || r.conn != connDown {
		t.Fatalf("app down: state %v, app %v", r.conn, r.app != nil)
	}
	if !strings.Contains(ansi.Strip(r.View().Content), "isn't running") {
		t.Errorf("connect screen doesn't say the app isn't running")
	}
	_, cmd := r.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	run(cmd)
	if launched != 1 || r.app == nil {
		t.Fatalf("after enter: launched %d, app %v", launched, r.app != nil)
	}
	if len(r.app.sources) == 0 {
		t.Errorf("app didn't load sources after connecting")
	}
}

func TestConnectScreenProtocolMismatch(t *testing.T) {
	r := NewConnectRoot(Connector{
		Dial: func(context.Context) (backup.Repo, error) {
			return nil, &protocol.Error{Code: protocol.ErrUnsupportedProtocol, Message: "This app speaks protocol 2; update charm-bak."}
		},
		Launch: func() error { t.Error("launch on a mismatch"); return nil },
	}, func() time.Time { return testNow })
	r.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	r.Update(r.Init()())
	r.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if r.conn != connFatal || !strings.Contains(ansi.Strip(r.View().Content), "protocol 2") {
		t.Errorf("state %v, view %q", r.conn, ansi.Strip(r.View().Content))
	}
}

func TestConnectScreenFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{100, 20}, {160, 50}, {60, 10}} {
		r := NewConnectRoot(Connector{
			Dial:   func(context.Context) (backup.Repo, error) { return nil, errors.New("down") },
			Launch: func() error { return nil },
		}, func() time.Time { return testNow })
		r.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		r.Update(r.Init()())
		lines := strings.Split(r.View().Content, "\n")
		if len(lines) != size[1] {
			t.Errorf("%v: %d lines", size, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > size[0] {
				t.Errorf("%v: line %d is %d wide", size, i, w)
			}
		}
	}
}

// → from a source with no snapshots stays put: focus in an empty column
// looks like a lost cursor.
func TestRightSkipsEmptyColumn(t *testing.T) {
	a := newTestApp(t, 160, 40)
	snaps := a.snaps
	a.snaps = nil
	press(a, "right")
	if a.col != colSources {
		t.Errorf("moved into an empty Snapshots column (col %d)", a.col)
	}
	a.snaps = snaps
	press(a, "right")
	if a.col != colSnapshots {
		t.Errorf("didn't move into Snapshots (col %d)", a.col)
	}
}

// selSrc is the source under the Sources cursor.
func selSrc(t *testing.T, a *App) protocol.Source {
	t.Helper()
	s, ok := a.selSource()
	if !ok {
		t.Fatal("no source selected")
	}
	return s
}
