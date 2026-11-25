package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// seg is one hero value. A seg with a stop is a cursor stop: Enter on it
// does something (AGENTS.md: a field is a stop if and only if Enter does
// something there).
type seg struct {
	text  string
	style lipgloss.Style
	stop  string // stop label, "" if not focusable
}

// heroCell is one label/value pair in the hero grid. The label is muted
// and the value bright, so no colon is needed.
type heroCell struct {
	label string
	value seg
	// optional cells are dropped, last first, when the grid is too wide,
	// before anything gets cut. Only cells the info popup also shows.
	optional bool
	// whole cells are never cut: what they say only reads in full (a
	// source's last run, or its backup's progress).
	whole bool
	// under names a first-line cell this one sits beneath, on the second
	// line, where there's room (Alok, 2026-11-25: a source's Retention,
	// under Schedule).
	under string
}

// secondLine is the hero's second line: the host's cells, then the
// selection's under cells each beneath its first-line cell, empty cells
// filling the columns between.
func secondLine(repo, top, cells []heroCell) []heroCell {
	line := slices.Clone(repo)
	for _, c := range cells {
		if c.under == "" {
			continue
		}
		col := slices.IndexFunc(top, func(t heroCell) bool { return t.label == c.under })
		if col < len(line) {
			col = len(line) // its column is taken: the next free one
		}
		for len(line) < col {
			line = append(line, heroCell{})
		}
		line = append(line, c)
	}
	return line
}

// firstLine is the selection's cells less those on the second line.
func firstLine(cells []heroCell) []heroCell {
	return slices.DeleteFunc(slices.Clone(cells), func(c heroCell) bool { return c.under != "" })
}

// heroStop is a focusable hero cell and where it sits: line 0 is the
// selection, line 1 the repository; x and w are content columns of the
// whole value cell, used for arrow moves and highlighting.
type heroStop struct {
	label string
	line  int
	x, w  int
}

// Hero grid spacing: between a label and its value, and between a value
// and the next label.
const (
	heroLabelGap = 2
	heroCellGap  = 3
)

// heroSelectionCells describes item i of the focused column's list, or
// nil when there's nothing there.
func (a *App) heroSelectionCells(i int) []heroCell {
	now := a.now()
	val := styles.FieldValueStyle()

	switch {
	case a.col == colSources && i < len(a.srcRows()) && a.srcRows()[i].header():
		return a.hostHeroCells(a.srcRows()[i].host)
	case a.col == colSources && i < len(a.srcRows()):
		s := a.sources[a.srcRows()[i].src]
		c, mark := sourceState(s)
		last := "never"
		if s.LastRun != nil {
			last = fmt.Sprintf("%s %s, took %s", mark, agoText(now, s.LastRun.Ended), took(s.LastRun))
			// Why is the notification's, ^N's and the run details' to say.
			switch s.LastRun.Status {
			case protocol.RunFailed:
				last = mark + " " + agoText(now, s.LastRun.Ended) + ", failed"
			case protocol.RunIncomplete:
				last = mark + " " + agoText(now, s.LastRun.Ended) + ", " + skippedText(s.LastRun.Skipped)
			case protocol.RunCancelled:
				last = mark + " " + agoText(now, s.LastRun.Ended) + ", cancelled"
			}
		}
		// A backup in progress shows in the Daemon cell below; this one
		// keeps saying how the last one went.
		lastStyle := styles.HealthStyle(c)
		next := "manual"
		if s.Paused {
			next = "paused"
		} else if s.NextRun != nil && s.Schedule != nil {
			next = every(time.Duration(s.Schedule.Every)*time.Second) + ", next " + whenNext(now, *s.NextRun)
		}
		r := s.Retention
		size := "–"
		if s.Bytes > 0 || s.Files > 0 {
			size = humanBytes(s.Bytes)
		}
		return []heroCell{
			{label: "Source", value: seg{text: s.Name, style: styles.FileTitleStyle(), stop: "Info"}},
			{label: "Size", value: seg{text: size, style: val}},
			{label: "Last run", value: seg{text: last, style: lastStyle, stop: "Last run"}, whole: true},
			{label: "Schedule", value: seg{text: next, style: val, stop: "Schedule"}},
			{label: "Retention", value: seg{text: retentionReach(r, false), style: val, stop: "Retention"}, under: "Schedule"},
		}
	case a.col == colSnapshots && i < len(a.snaps):
		s := a.snaps[i]
		note := s.Note
		if note == "" {
			note = "none"
		}
		pin, pinStyle := "no", val
		if s.Pinned {
			pin, pinStyle = "pinned", styles.PinnedStyle()
		}
		took := ""
		if s.End != nil {
			took = ", took " + s.End.Sub(s.Time).Round(time.Second).String()
		}
		return []heroCell{
			{label: "Snapshot", value: seg{text: stamp(now, s.Time) + took, style: styles.FileTitleStyle(), stop: "Info"}},
			{label: "Size", value: seg{text: fmt.Sprintf("%s, %s new", humanBytes(s.Bytes), humanBytes(s.BytesAdded)), style: val}},
			{label: "Note", value: seg{text: note, style: val, stop: "Description"}},
			{label: "Pin", value: seg{text: pin, style: pinStyle, stop: "Pin"}},
		}
	case a.col >= colTree:
		lv := a.col - colTree
		if lv >= len(a.levels) || i >= len(a.levels[lv].entries) {
			return nil
		}
		e := a.levels[lv].entries[i]
		return []heroCell{
			{label: "Name", value: seg{text: e.Name, style: styles.FileTitleStyle(), stop: "Info"}},
			{label: "Size", value: seg{text: humanBytes(e.Size), style: val}},
			{label: "Modified", value: seg{text: e.MTime.Local().Format("Jan 2 15:04"), style: val}},
		}
	}
	return nil
}

// every describes a snapshot interval: "hourly", "daily", "every 6h".
func every(d time.Duration) string {
	switch d {
	case 0:
		return "manual"
	case time.Hour:
		return "hourly"
	case 24 * time.Hour:
		return "daily"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("every %dh", d/time.Hour)
	}
	return fmt.Sprintf("every %dm", d/time.Minute)
}

// retentionReach sums a retention rule up as how far back it reaches,
// which is what it means in practice; the rule itself is in the editor
// (Enter) and the info popup.
func retentionReach(r protocol.Retention, inherited bool) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	var reach string
	switch p := presetFor(r); {
	case p >= 0:
		reach = keepPresets[p].name
	case r.Yearly == protocol.KeepUnlimited || r.Monthly == protocol.KeepUnlimited || r.Weekly == protocol.KeepUnlimited ||
		r.Daily == protocol.KeepUnlimited || r.Hourly == protocol.KeepUnlimited || r.Last == protocol.KeepUnlimited:
		reach = "forever"
	case r.Yearly > 0:
		reach = plural(r.Yearly, "year")
	case r.Monthly > 0:
		reach = plural(r.Monthly, "month")
	case r.Weekly > 0:
		reach = plural(r.Weekly, "week")
	case r.Daily > 0:
		reach = plural(r.Daily, "day")
	case r.Hourly > 0:
		reach = plural(r.Hourly, "hour")
	case r.Last > 0:
		reach = "last " + fmt.Sprint(r.Last)
	default:
		return "keeps everything"
	}
	if inherited {
		reach += " (default)"
	}
	return reach
}

// heroRepoCells is the hero's fixed second line: the repository and
// what the app is doing.
func (a *App) heroRepoCells() []heroCell {
	// The selected row's host: a remote's source shows its repository
	// and its daemon, not this machine's.
	if r, ok := a.selRow(); ok && a.isRemote(r.host) {
		return a.remoteRepoCells(r.host)
	}
	st := a.repoStatus
	val := styles.FieldValueStyle()
	app := seg{text: "● idle", style: styles.HealthStyle(styles.OK), stop: "Daemon"}
	switch st.State {
	case protocol.StateRunning:
		app = seg{text: "● " + a.jobText(), style: styles.HealthStyle(styles.Busy), stop: "Daemon"}
	case protocol.StatePruning:
		app = seg{text: "● cleaning up", style: styles.HealthStyle(styles.Busy), stop: "Daemon"}
	case protocol.StatePaused:
		app = seg{text: "○ paused", style: styles.HealthStyle(styles.Warn), stop: "Daemon"}
	}
	if a.err != nil && st.State == "" {
		app = seg{text: "○ not reachable", style: styles.HealthStyle(styles.Fail), stop: "Daemon"}
	}
	return []heroCell{
		{label: "Stored", value: seg{text: storedText(st.Repository), style: val}},
		{label: "Daemon", value: app},
	}
}

// storedText is the Stored cell: unique data after deduplication (restic
// compresses it on top), with the uncompressed size. The destination
// isn't shown: it's set once per host, and the repository popup and a
// source's info have it.
func storedText(r *protocol.Repository) string {
	switch {
	case r == nil:
		return "no repository yet"
	case r.StoredBytes <= 0:
		return "–"
	}
	stored := humanBytes(r.StoredBytes)
	if r.LogicalBytes > r.StoredBytes {
		stored += " (" + humanBytes(r.LogicalBytes) + " uncompressed)"
	}
	return stored
}

// remoteRepoCells is the second row for a remote's selection: its
// repository and what its daemon is doing (from its status and events).
// Daemon isn't a stop: launching and the run log are this machine's.
func (a *App) remoteRepoCells(host string) []heroCell {
	val := styles.FieldValueStyle()
	st, known := a.hostStatus[host]
	app := seg{text: "● idle", style: styles.HealthStyle(styles.OK)}
	switch {
	case a.findHost(host).State != protocol.HostOK:
		text, c := hostState(a.findHost(host), a.spinFrame())
		app = seg{text: text, style: styles.HealthStyle(c)}
	case !known:
		app = seg{text: "…", style: styles.MutedStyle()}
	case a.remoteJobs[host] != nil:
		app = seg{text: "● " + a.remoteJobText(host), style: styles.HealthStyle(styles.Busy)}
	case st.State == protocol.StatePruning:
		app = seg{text: "● cleaning up", style: styles.HealthStyle(styles.Busy)}
	case st.State == protocol.StatePaused:
		app = seg{text: "○ paused", style: styles.HealthStyle(styles.Warn)}
	}
	stored := "…"
	if known {
		stored = storedText(st.Repository)
	} else if r, ok := a.repos[host]; ok && r.Repository != nil {
		// Offline: the size as of its last answer.
		stored = storedText(&protocol.Repository{StoredBytes: r.Repository.StoredBytes, LogicalBytes: r.Repository.LogicalBytes})
	}
	return []heroCell{
		{label: "Stored", value: seg{text: stored, style: val}},
		{label: "Daemon", value: app},
	}
}

// remoteJobText says what a remote is running: "backing up data 42%".
func (a *App) remoteJobText(host string) string {
	j := a.remoteJobs[host]
	text := jobKindText(j.job.Kind)
	if j.job.Source != nil {
		for _, s := range a.sources {
			if s.Host == host && s.ID == *j.job.Source {
				text += " " + s.Name
			}
		}
	}
	if j.progress != nil && j.progress.Percent > 0 {
		text += fmt.Sprintf(" %d%%", int(j.progress.Percent*100))
	}
	return text
}

// sameHeroKind reports whether items i and j of the focused list get the
// same hero cells: in the Sources column a host header and a source
// don't, so each sizes the grid only over its own kind.
func (a *App) sameHeroKind(i, j int) bool {
	if a.col != colSources {
		return true
	}
	rows := a.srcRows()
	return i < len(rows) && j < len(rows) && rows[i].header() == rows[j].header()
}

// focusedListLen is the number of items in the focused column's list.
func (a *App) focusedListLen() int {
	switch {
	case a.col == colSources:
		return len(a.srcRows())
	case a.col == colSnapshots:
		return len(a.snaps)
	}
	if lv := a.col - colTree; lv < len(a.levels) {
		return len(a.levels[lv].entries)
	}
	return 0
}

// focusedListPos is the cursor position in the focused column's list.
func (a *App) focusedListPos() int {
	if c := a.focusedCursor(); c != nil {
		return c.pos
	}
	return 0
}

// heroGrid is the hero laid out: both rows' cells, each grid column's
// label and value widths, and each column's x. Value widths cover every
// item in the focused list, not just the selection, so the grid holds
// still while the cursor moves. The first value column (the name) gives
// way when the grid is too wide, and its text is cut in the middle.
type heroGrid struct {
	rows      [2][]heroCell
	labelW    []int
	valueW    []int
	labelX    []int
	valueX    []int
	fullWidth int // before any cell was cut
}

func (a *App) heroGrid() heroGrid {
	// Drop optional cells, last first, until the grid fits at full width.
	drop := map[string]bool{}
	for {
		g := a.heroGridDropping(drop)
		if g.fullWidth <= a.heroInner() {
			break
		}
		dropped := false
		for _, c := range slices.Backward(a.heroSelectionCells(a.focusedListPos())) {
			if c.optional && !drop[c.label] {
				drop[c.label], dropped = true, true
				break
			}
		}
		if !dropped {
			break
		}
	}
	return a.heroGridDropping(drop)
}

// withoutDropped filters cells whose labels are in drop.
func withoutDropped(cells []heroCell, drop map[string]bool) []heroCell {
	return slices.DeleteFunc(cells, func(c heroCell) bool { return drop[c.label] })
}

func (a *App) heroGridDropping(drop map[string]bool) heroGrid {
	var g heroGrid
	repo := a.heroRepoCells()
	sel := withoutDropped(a.heroSelectionCells(a.focusedListPos()), drop)
	g.rows[0] = firstLine(sel)
	g.rows[1] = secondLine(repo, g.rows[0], sel)
	// Size over every item of the same kind, so the grid holds still as
	// the cursor moves. Items can differ in how many cells they have (a
	// remote's host header has Link, this machine's doesn't), so the grid
	// is as wide as the longest.
	pos := a.focusedListPos()
	measured := [][]heroCell{g.rows[1]}
	for i := range a.focusedListLen() {
		if a.sameHeroKind(i, pos) {
			cells := withoutDropped(a.heroSelectionCells(i), drop)
			top := firstLine(cells)
			measured = append(measured, top, secondLine(repo, top, cells))
		}
	}
	n := len(g.rows[0])
	for _, cells := range measured {
		n = max(n, len(cells))
	}
	g.labelW, g.valueW = make([]int, n), make([]int, n)
	g.labelX, g.valueX = make([]int, n), make([]int, n)
	for _, cells := range measured {
		for i, c := range cells {
			g.labelW[i] = max(g.labelW[i], lipgloss.Width(c.label))
			g.valueW[i] = max(g.valueW[i], lipgloss.Width(c.value.text))
		}
	}

	// Too wide: take a cell at a time from the widest value column, never
	// below 8 cells, so the long ones give way and the short ones stay.
	// The second row (the host's repository and daemon) is never cut: it's
	// one line whatever the cursor is on, so its column keeps its width
	// and the first row's cells give way instead.
	floor := make([]int, n)
	for i := range floor {
		floor[i] = 8
		if i < len(g.rows[1]) {
			floor[i] = max(floor[i], lipgloss.Width(g.rows[1][i].value.text))
		}
	}
	for _, cells := range measured {
		for i, c := range cells {
			if c.whole {
				floor[i] = max(floor[i], g.valueW[i])
			}
		}
	}
	total := func() int {
		t := 0
		for i := range n {
			t += g.labelW[i] + heroLabelGap + g.valueW[i]
		}
		return t + heroCellGap*max(n-1, 0)
	}()
	g.fullWidth = total
	for over := total - a.heroInner(); over > 0; over-- {
		w := -1
		for i := range n {
			if g.valueW[i] > floor[i] && (w < 0 || g.valueW[i] > g.valueW[w]) {
				w = i
			}
		}
		if w < 0 {
			break
		}
		g.valueW[w]--
	}
	x := 0
	for i := range n {
		g.labelX[i] = x
		g.valueX[i] = x + g.labelW[i] + heroLabelGap
		x = g.valueX[i] + g.valueW[i] + heroCellGap
	}
	for r := range g.rows {
		for i := range g.rows[r] {
			c := &g.rows[r][i].value
			c.text = midTrunc(c.text, g.valueW[i])
		}
	}
	return g
}

// heroStops lists the hero's focusable cells in reading order, with
// their positions as rendered.
func (a *App) heroStops() []heroStop {
	g := a.heroGrid()
	inner := a.heroInner()
	var stops []heroStop
	for line, cells := range g.rows {
		for i, c := range cells {
			if c.value.stop != "" && g.valueX[i] < inner {
				stops = append(stops, heroStop{label: c.value.stop, line: line, x: g.valueX[i], w: min(g.valueW[i], inner-g.valueX[i])})
			}
		}
	}
	return stops
}

// heroInner is the hero's content width: the terminal less border and
// padding.
func (a *App) heroInner() int { return a.width - 4 }

// heroContentX and heroSelectionY locate the hero's first line on screen.
const (
	heroContentX   = 2
	heroSelectionY = 1
)

// heroNameAt reports whether screen cell (x, y) is on the selection's
// name cell in the hero.
func (a *App) heroNameAt(x, y int) bool {
	g := a.heroGrid()
	if len(g.rows[0]) == 0 || y != heroSelectionY {
		return false
	}
	x -= heroContentX
	return x >= g.valueX[0] && x < g.valueX[0]+g.valueW[0]
}

// renderHero boxes the grid at full width, highlighting the focused cell
// when the hero has focus.
func (a *App) renderHero() string {
	g := a.heroGrid()
	inner := a.heroInner()
	stop := 0
	lab := styles.FieldLabelStyle()
	render := func(cells []heroCell) string {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString(strings.Repeat(" ", heroCellGap))
			}
			b.WriteString(lab.Render(padRight(c.label, g.labelW[i])))
			b.WriteString(strings.Repeat(" ", heroLabelGap))
			text := padRight(c.value.text, g.valueW[i])
			switch {
			case c.value.stop != "" && g.valueX[i] < inner && a.area == areaHero && stop == a.heroIdx:
				b.WriteString(styles.StopStyle(true).Render(text))
			default:
				b.WriteString(c.value.style.Render(text))
			}
			// Counted as heroStops counts: a stop cut off the right edge
			// isn't one, or the highlight lands on a cell the cursor
			// isn't on.
			if c.value.stop != "" && g.valueX[i] < inner {
				stop++
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	body := fitRow(render(g.rows[0]), "", inner) + "\n" + fitRow(render(g.rows[1]), "", inner)
	box := styles.PopupBorderStyle().Width(a.width).Render(body)
	// The app's name sits in the box's top edge, at its left.
	edge := lipgloss.NewStyle().Foreground(styles.Selected).Background(styles.Background)
	name := " " + styles.GradientBlockLabel("charm-bak", true) + " "
	top := edge.Render("╭─") + name + edge.Render(strings.Repeat("─", max(a.width-3-lipgloss.Width(name), 0))+"╮")
	// Its bottom edge says how to get in (and out), at its right.
	verb := "edit"
	if a.area == areaHero {
		verb = "back"
	}
	hint := " " + styles.MutedStyle().Render("tab "+verb) + " "
	bottom := edge.Render("╰"+strings.Repeat("─", max(a.width-3-lipgloss.Width(hint), 0))) + hint + edge.Render("─╯")
	_, rest, _ := strings.Cut(box, "\n")
	if i := strings.LastIndex(rest, "\n"); i >= 0 {
		rest = rest[:i+1] + bottom
	}
	return top + "\n" + rest
}

// jobText says what the daemon is doing: "backing up home 43%".
func (a *App) jobText() string {
	if a.job == nil {
		return "backing up"
	}
	text := jobKindText(a.job.Kind)
	if a.job.Source != nil {
		if s, ok := a.sourceByID(*a.job.Source); ok {
			text += " " + s.Name
		}
	}
	if a.progress != nil && a.progress.Percent > 0 {
		text += fmt.Sprintf(" %d%%", int(a.progress.Percent*100))
	}
	return text
}

// jobKindText names a job kind as the hero says it.
func jobKindText(kind string) string {
	switch kind {
	case protocol.KindBackup:
		return "backing up"
	case protocol.KindRestore:
		return "restoring"
	case protocol.KindForget:
		return "applying retention"
	case protocol.KindPrune:
		return "cleaning up"
	case protocol.KindCheck:
		return "checking"
	case protocol.KindState:
		return "saving its settings"
	case protocol.KindRewrite:
		return "removing from past snapshots of"
	}
	return kind
}
