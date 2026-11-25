package ui

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// Column widths in cells, divider excluded. The preview takes whatever is
// left, but never less than previewMin: below that, columns slide off the
// left instead (Sources first, then Snapshots).
const (
	sourcesWidth   = 24
	snapshotsWidth = 24 // room for "Nov 20 18:09  +123.4 MB"
	treeMinWidth   = 18
	treeMaxWidth   = 40
	previewMin     = 30
)

// treeColWidth sizes a tree column to its longest row (name, a space, the
// size, one cell of padding each side) and its title, within
// treeMinWidth..treeMaxWidth, like widening a Finder column. Longer names
// are cut in the middle; the hero and the info popup show them whole.
func treeColWidth(lv dirLevel) int {
	w := ansi.StringWidth(lv.name+"/") + 2
	for _, e := range lv.entries {
		w = max(w, ansi.StringWidth(entryName(e))+1+len(entrySize(e))+2)
	}
	return min(max(w, treeMinWidth), treeMaxWidth)
}

// columnLayout returns the first visible column given the terminal width,
// each column's width, and which column has focus. Columns slide off the
// left until the preview gets previewMin, but the focused column always
// stays visible.
func columnLayout(width int, widths []int, focused int) (first int) {
	need := func(from int) int {
		n := previewMin
		for _, w := range widths[from:] {
			n += w + 1 // divider
		}
		return n
	}
	for first < focused && need(first) > width {
		first++
	}
	return first
}

// columnWidths is the width of each walked column (Sources, Snapshots,
// tree levels up to the focused one) and the first one on screen.
func (a *App) columnWidths() (widths []int, first int) {
	walked := min(a.nextLevel(), len(a.levels))
	widths = []int{sourcesWidth, snapshotsWidth}
	for _, lv := range a.levels[:walked] {
		widths = append(widths, treeColWidth(lv))
	}
	return widths, columnLayout(a.width, widths, a.col)
}

// breadcrumb names what the columns slid off the left had selected:
// "home › Nov 20 18:09 › Library/". Empty when nothing is hidden.
func (a *App) breadcrumb() string {
	_, first := a.columnWidths()
	var crumbs []string
	for i := range first {
		switch {
		case i == colSources:
			if s, ok := a.selSource(); ok {
				crumbs = append(crumbs, s.Name)
			}
		case i == colSnapshots && a.snapCur.pos < len(a.snaps):
			crumbs = append(crumbs, stamp(a.now(), a.snaps[a.snapCur.pos].Time))
		case i >= colTree:
			if lv := a.levels[i-colTree]; lv.cur.pos < len(lv.entries) {
				crumbs = append(crumbs, entryName(lv.entries[lv.cur.pos]))
			}
		}
	}
	return strings.Join(crumbs, " › ")
}

// renderColumns draws the columns walked so far (Sources, Snapshots, and
// every tree level up to the focused one), then one pane filling the rest:
// the selected folder's listing or the selected file. It's always exactly
// one pane, in the same place, so moving between a file and a folder
// doesn't change the layout's shape.
func (a *App) renderColumns() string {
	walked := min(a.nextLevel(), len(a.levels))
	widths, first := a.columnWidths()

	h := a.bodyHeight()
	var parts []string
	used := 0
	for i := first; i < len(widths); i++ {
		parts = append(parts, a.renderColumn(i, widths[i], h), verticalRule(h, i == a.col))
		used += widths[i] + 1
	}
	if pw := a.width - used; pw > 0 {
		var pane string
		switch {
		case walked < len(a.levels):
			pane = a.renderColumn(colTree+walked, pw, h)
		default:
			if e, ok := a.selectedEntry(); ok && !e.IsDir() {
				pane = a.renderFile(e, pw, h)
			}
		}
		parts = append(parts, lipgloss.NewStyle().Width(pw).MaxWidth(pw).Height(h).MaxHeight(h).Render(pane))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func verticalRule(height int, focused bool) string {
	return styles.DividerStyle(focused).Render(strings.TrimRight(strings.Repeat("│\n", height), "\n"))
}

// renderColumn draws column i: a title row, then its rows windowed by the
// column's cursor.
func (a *App) renderColumn(i, width, height int) string {
	focused := i == a.col && a.area == areaColumns
	var title string
	var cur cursor
	var rows []string
	var pinned []bool // Snapshots rows that keep their red under the cursor
	now := a.now()

	switch {
	case i == colSources:
		title, cur = "Sources", a.srcCur
		rows, cur = a.sourceRows(now, width)
	case i == colSnapshots:
		title, cur = "Snapshots", a.snapCur
		for _, s := range a.snaps {
			when := stamp(now, s.Time)
			if s.Pinned {
				when = styles.PinnedStyle().Render(when)
			}
			// What this run added: every snapshot holds everything, so
			// that's the size that says how much changed.
			rows = append(rows, fitRow(when, styles.MutedStyle().Render("+"+humanBytes(s.BytesAdded)), width-2))
			pinned = append(pinned, s.Pinned)
		}
	default:
		lv := a.levels[i-colTree]
		// A bare folder name here reads as the first entry, so the root
		// column is "Files" and deeper ones say whose contents they are.
		title, cur = "Files", lv.cur
		if i > colTree {
			title = "in " + lv.name + "/"
		}
		for _, e := range lv.entries {
			rows = append(rows, fitRow(entryName(e), styles.MutedStyle().Render(entrySize(e)), width-2))
		}
	}

	switch {
	case i == colSources && !a.sourcesKnown:
		// Before the first answer: "none yet" would be a wrong claim.
		rows = append(rows, styles.MutedStyle().Render("loading…"))
	case i == colSources && len(a.sources) == 0:
		rows = append(rows, styles.MutedStyle().Render("none yet: press a to add one"))
	}

	var b strings.Builder
	titleStyle := styles.MutedStyle()
	if i == a.col {
		titleStyle = styles.FocusedTitleStyle()
	}
	b.WriteString(titleStyle.Render(fitRow(" "+title, "", width)))
	for r := cur.off; r < min(cur.off+height-1, len(rows)); r++ {
		b.WriteString("\n")
		line := " " + rows[r] + " "
		// No cursor in the preview pane: it shows the folder under the
		// cursor, and nothing in it is selected until you move in. The
		// exception is Snapshots while Sources has focus: the files shown
		// come from its cursor's snapshot, so mark it in the cursor's color.
		if r == cur.pos && (i <= a.col || i == colSnapshots) {
			style := styles.SelectedDimStyle()
			if focused {
				style = styles.SelectedStyle()
			}
			if r < len(pinned) && pinned[r] {
				style = style.Foreground(styles.Pinned).Bold(true)
			}
			line = style.Render(ansi.Strip(line))
		}
		b.WriteString(line)
	}
	return lipgloss.NewStyle().Width(width).MaxWidth(width).Height(height).MaxHeight(height).Render(b.String())
}

// sourceRows are the Sources column's rows and the cursor over them.
// With remotes, each host gets a header row above its sources (one that
// isn't answering says so), and the cursor and scroll are mapped from
// source positions to rows.
func (a *App) sourceRows(now time.Time, width int) ([]string, cursor) {
	var rows []string
	for _, r := range a.srcRows() {
		if r.header() {
			rows = append(rows, a.hostHeaderRow(r.host, width-2))
			continue
		}
		s := a.sources[r.src]
		health := a.sourceHealth(now, s)
		name := s.Name
		if a.grouped() {
			name = "  " + name // under its host header
		}
		rows = append(rows, fitRow(midTrunc(name, width-2-lipgloss.Width(health)-1), health, width-2))
	}
	return rows, a.srcCur
}

// hostHeaderRow is a host's header in the Sources column: a fold mark and
// the name in the title orange, then "not answering", what it still
// needs (hostTodo: "setup", "disk access"; Enter there goes to it) or,
// folded, how many sources are hidden.
func (a *App) hostHeaderRow(name string, width int) string {
	h := a.findHost(name)
	mark := "▾ "
	right := ""
	if a.collapsed[name] {
		mark = "▸ "
		right = styles.MutedStyle().Render(fmt.Sprint(a.hostSourceCount(name)))
	}
	if h.State != protocol.HostOK {
		mark, c := hostMark(h, a.spinFrame())
		right = styles.HealthStyle(c).Render(mark)
	} else if todo := a.hostTodo(name); todo != "" {
		right = styles.HealthStyle(styles.Warn).Render(todo)
	}
	head := styles.FileTitleStyle().Render(mark + midTrunc(name, width-2-lipgloss.Width(right)-1))
	return fitRow(head, right, width)
}

// entryName is how a tree row names an entry: folders end in "/".
func entryName(e protocol.Entry) string {
	if e.Type == protocol.TypeDir {
		return e.Name + "/"
	}
	return e.Name
}

// entrySize is a tree row's size: blank for folders, since restic doesn't
// record a folder's total.
func entrySize(e protocol.Entry) string {
	if e.Type == protocol.TypeDir {
		return ""
	}
	return humanBytes(e.Size)
}

// lastEnded is when a source's last run ended, zero if it never ran.
func lastEnded(s protocol.Source) time.Time {
	if s.LastRun == nil {
		return time.Time{}
	}
	return s.LastRun.Ended
}

// sourceHealth is the right-hand figure on a source row: the running
// backup's progress, else time since the last run, colored and marked
// by health.
func (a *App) sourceHealth(now time.Time, s protocol.Source) string {
	if running, p := a.jobFor(s); running {
		return a.spinnerText(p)
	}
	if s.LastRun == nil {
		return styles.HealthStyle(styles.Warn).Render("never")
	}
	c, mark := sourceState(s)
	return styles.HealthStyle(c).Render(mark + " " + ago(now, s.LastRun.Ended))
}

// sourceState maps the app's health for a source to a color and mark.
func sourceState(s protocol.Source) (c color.Color, mark string) {
	switch s.Health {
	case protocol.HealthFailed:
		return styles.Fail, "✗"
	case protocol.HealthSkipped:
		return styles.Warn, "⚠"
	case protocol.HealthOverdue, protocol.HealthNever:
		return styles.Warn, "!"
	default:
		return styles.OK, "✓"
	}
}

// renderFile is the preview pane for a file: name, a details line, then
// the contents, cut to the pane. Binary files say so instead.
func (a *App) renderFile(e node, width, height int) string {
	lines := []string{
		" " + styles.FileTitleStyle().Render(fitRow(e.Name, "", width-1)),
		" " + styles.FieldValueStyle().Render(humanBytes(e.Size)) +
			styles.FieldLabelStyle().Render("  "+e.Mode+"  modified "+e.MTime.Local().Format("2006-01-02 15:04")),
		styles.MutedStyle().Render(" " + strings.Repeat("─", max(width-2, 0))),
	}
	switch {
	case a.file.key != e.key() || a.file.loading:
		lines = append(lines, styles.MutedStyle().Render(" loading…"))
	case a.file.err != nil:
		lines = append(lines, styles.HealthStyle(styles.Fail).Render(" "+a.file.err.Error()))
	case a.file.dump.Binary || bytes.IndexByte(a.file.dump.Data, 0) >= 0:
		lines = append(lines, styles.MutedStyle().Render(" binary file, not shown"))
	default:
		for _, l := range strings.Split(string(a.file.dump.Data), "\n") {
			if len(lines) >= height {
				break
			}
			l = strings.ReplaceAll(l, "\t", "    ")
			lines = append(lines, " "+ansi.Truncate(printable(ansi.Strip(l)), max(width-2, 0), ""))
		}
	}
	return strings.Join(lines, "\n")
}

// printable replaces the control characters a text file can carry (bell,
// backspace, form feed, a lone CR or ESC) with "·": written to the
// terminal as they are, they'd beep or move the cursor mid-frame.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '·'
		}
		return r
	}, s)
}

// progressText is a running job's short form: "⟳ 43%", or "⟳" before the
// first progress report.
// spinnerText is a running source's state: the spinner, then how far it
// is once restic knows.
func (a *App) spinnerText(p float64) string {
	text := styles.HealthStyle(styles.Spinning).Render(a.spin.View())
	if p > 0 {
		text += styles.HealthStyle(styles.Spinning).Render(fmt.Sprintf(" %d%%", int(p*100)))
	}
	return text
}

func progressText(p float64) string {
	if p <= 0 {
		return "⟳"
	}
	return fmt.Sprintf("⟳ %d%%", int(p*100))
}
