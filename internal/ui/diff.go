package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// diffBytes caps how much of each side a file diff reads.
const diffBytes = 256 << 10

// diffView is the popup comparing a snapshot's file or folder with the
// same path on disk: a line diff for a text file, the changed entries
// for a folder. Scrolls; Esc, q or d closes it. The same popup lists what
// a snapshot changed from the one before it (changes.go): then title and
// sub are set, key identifies the load, and there's nothing to restore.
type diffView struct {
	n       node
	when    string // the snapshot's time, for the subtitle
	title   string // set for a changes list: the whole title
	sub     string // and its subtitle
	key     string // what a changes list is of, as its reply says
	lines   []string
	loading bool
	err     error
	off     int

	// A notification's details (noticedetail.go): Esc goes back to the
	// list it came from, Enter does enter (go to the source).
	back        modal
	enter       func() tea.Cmd
	loadingText string
}

// diffLoadedMsg carries a finished comparison to the open popup.
type diffLoadedMsg struct {
	key   string
	lines []string
	sub   string // a changes list: its subtitle, now with the counts
	err   error
}

func (d *diffView) typing() bool { return false }

// loadKey is what a reply must name to be this popup's.
func (d *diffView) loadKey() string {
	if d.title != "" {
		return d.key
	}
	return d.n.key()
}

// startDiff opens the popup for the selected tree entry.
func (a *App) startDiff() tea.Cmd {
	if a.col < colTree {
		return nil
	}
	n, ok := a.selectedEntry()
	if !ok {
		return nil
	}
	if n.Type != protocol.TypeDir && n.Type != protocol.TypeFile {
		a.statusMsg = "Only files and folders can be compared"
		return nil
	}
	when := ""
	for _, s := range a.snaps {
		if s.ID == n.snap {
			when = stamp(a.now(), s.Time)
		}
	}
	a.modal = &diffView{n: n, when: when, loading: true}
	repo := a.srcRepo()
	return func() tea.Msg {
		ctx := context.Background()
		var lines []string
		var err error
		if n.IsDir() {
			lines, err = folderDiff(ctx, repo, n)
		} else {
			lines, err = fileDiff(ctx, repo, n)
		}
		return diffLoadedMsg{key: n.key(), lines: lines, err: err}
	}
}

// folderDiff lists what differs between a snapshot folder and disk:
// changed, then gone, then new, with a count of the rest.
func folderDiff(ctx context.Context, repo backup.Repo, n node) ([]string, error) {
	entries, err := repo.Diff(ctx, n.snap, n.path)
	if err != nil {
		return nil, err
	}
	groups := []struct {
		change, mark, label string
		style               func(...string) string
	}{
		{protocol.ChangeChanged, "~", "changed on disk", styles.HealthStyle(styles.Changed).Render},
		{protocol.ChangeRemoved, "-", "gone from disk", styles.HealthStyle(styles.Removed).Render},
		{protocol.ChangeAdded, "+", "new on disk, not in the snapshot", styles.HealthStyle(styles.Added).Render},
	}
	var lines []string
	same := 0
	for _, g := range groups {
		var names []string
		for _, e := range entries {
			if e.Change == g.change {
				names = append(names, entryName(protocol.Entry{Name: e.Name, Type: e.Type}))
			}
		}
		if len(names) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, styles.FieldLabelStyle().Render(fmt.Sprintf("%d %s", len(names), g.label)))
		for _, name := range names {
			lines = append(lines, g.style(g.mark+" "+name))
		}
	}
	for _, e := range entries {
		if e.Change == protocol.ChangeSame {
			same++
		}
	}
	if len(lines) == 0 {
		return []string{styles.HealthStyle(styles.OK).Render("Everything here matches the disk.")},
			nil
	}
	if same > 0 {
		lines = append(lines, "", styles.MutedStyle().Render(fmt.Sprintf("%d unchanged (folders count as unchanged if they exist; open one to compare inside)", same)))
	}
	return lines, nil
}

// fileDiff is a line diff from the snapshot's copy to the file on disk.
func fileDiff(ctx context.Context, repo backup.Repo, n node) ([]string, error) {
	saved, err := repo.Read(ctx, n.snap, n.path, diffBytes)
	if err != nil {
		return nil, err
	}
	live, err := repo.Read(ctx, protocol.LiveSnapshot, n.path, diffBytes)
	var perr *protocol.Error
	if errors.As(err, &perr) && perr.Code == protocol.ErrNotFound {
		return []string{styles.HealthStyle(styles.Removed).Render("Gone from disk: only the snapshot has it.")}, nil
	}
	if err != nil {
		return nil, err
	}
	if saved.Binary || live.Binary || bytes.IndexByte(saved.Data, 0) >= 0 || bytes.IndexByte(live.Data, 0) >= 0 {
		if saved.Size == live.Size && bytes.Equal(saved.Data, live.Data) {
			return []string{styles.HealthStyle(styles.OK).Render("Binary file, same size; contents not compared.")}, nil
		}
		return []string{styles.HealthStyle(styles.Changed).Render(fmt.Sprintf("Binary file: %s in the snapshot, %s on disk.", humanBytes(saved.Size), humanBytes(live.Size)))}, nil
	}
	if bytes.Equal(saved.Data, live.Data) && saved.Size == live.Size {
		return []string{styles.HealthStyle(styles.OK).Render("Same as on disk.")}, nil
	}
	var lines []string
	if saved.Truncated || live.Truncated {
		lines = append(lines, styles.HealthStyle(styles.Warn).Render(fmt.Sprintf("Only the first %s of each side compared.", humanBytes(diffBytes))), "")
	}
	for _, h := range lineDiff(splitLines(saved.Data), splitLines(live.Data), 3) {
		if h.gap {
			lines = append(lines, styles.MutedStyle().Render(fmt.Sprintf("⋯ line %d", h.line)))
			continue
		}
		switch h.op {
		case '-':
			lines = append(lines, styles.HealthStyle(styles.Removed).Render("- "+h.text))
		case '+':
			lines = append(lines, styles.HealthStyle(styles.Added).Render("+ "+h.text))
		default:
			lines = append(lines, styles.FieldValueStyle().Render("  "+h.text))
		}
	}
	return lines, nil
}

// splitLines splits text into display-safe lines: tabs expanded, escape
// sequences stripped, no trailing empty line.
func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = ansi.Strip(strings.ReplaceAll(strings.TrimSuffix(l, "\r"), "\t", "    "))
	}
	return lines
}

// diffLine is one line of a line diff: an op ('-' only in a, '+' only
// in b, ' ' both), or a gap marker before the hunk that starts at line
// (1-based, in b).
type diffLine struct {
	op   byte
	text string
	gap  bool
	line int
}

// lcsMax caps the middle (after the common head and tail) that lineDiff
// aligns line by line; past it, the middle shows as all removed then all
// added, which is still correct, just less tidy.
const lcsMax = 3000

// lineDiff diffs a against b and keeps ctx unchanged lines around each
// change, with a gap marker wherever lines are skipped.
func lineDiff(a, b []string, ctx int) []diffLine {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	ma, mb := a[head:len(a)-tail], b[head:len(b)-tail]

	var all []diffLine
	for _, l := range a[:head] {
		all = append(all, diffLine{op: ' ', text: l})
	}
	if len(ma) > lcsMax || len(mb) > lcsMax {
		for _, l := range ma {
			all = append(all, diffLine{op: '-', text: l})
		}
		for _, l := range mb {
			all = append(all, diffLine{op: '+', text: l})
		}
	} else {
		all = append(all, lcsDiff(ma, mb)...)
	}
	for _, l := range a[len(a)-tail:] {
		all = append(all, diffLine{op: ' ', text: l})
	}

	// Keep changes plus ctx lines either side; mark each skip.
	keep := make([]bool, len(all))
	for i, l := range all {
		if l.op != ' ' {
			for j := max(i-ctx, 0); j <= min(i+ctx, len(all)-1); j++ {
				keep[j] = true
			}
		}
	}
	var out []diffLine
	bLine, skipped := 0, false
	for i, l := range all {
		if l.op != '-' {
			bLine++
		}
		if !keep[i] {
			skipped = true
			continue
		}
		if skipped || (i > 0 && len(out) == 0) {
			line := bLine
			if l.op == '-' {
				line++
			}
			out = append(out, diffLine{gap: true, line: line})
		}
		skipped = false
		out = append(out, l)
	}
	return out
}

// lcsDiff aligns a and b by their longest common subsequence.
func lcsDiff(a, b []string) []diffLine {
	n, m := len(a), len(b)
	// t[i][j] is the LCS length of a[i:] and b[j:].
	t := make([][]uint16, n+1)
	for i := range t {
		t[i] = make([]uint16, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffLine{op: ' ', text: a[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			out = append(out, diffLine{op: '-', text: a[i]})
			i++
		default:
			out = append(out, diffLine{op: '+', text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{op: '-', text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{op: '+', text: b[j]})
	}
	return out
}

func (d *diffView) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case diffLoadedMsg:
		if msg.key == d.loadKey() {
			d.lines, d.err, d.loading = msg.lines, msg.err, false
			if msg.sub != "" {
				d.sub = msg.sub
			}
		}
	case tea.KeyPressMsg:
		page := d.bodyRows(a)
		switch {
		case d.back != nil && key.Matches(msg, keyEsc):
			a.modal = d.back
		case d.enter != nil && key.Matches(msg, keyEnter):
			return d.enter()
		case key.Matches(msg, keyEsc, keyDiff, keyQuit), d.title != "" && key.Matches(msg, keyChanges):
			a.modal = nil
		case key.Matches(msg, keyRestore) && d.title == "":
			return a.startRestore(d.n, true)
		case key.Matches(msg, keyUp):
			d.off--
		case key.Matches(msg, keyDown):
			d.off++
		case key.Matches(msg, keyPageUp):
			d.off -= page
		case key.Matches(msg, keyPageDown), msg.String() == "space":
			d.off += page
		case key.Matches(msg, keyTop):
			d.off = 0
		case key.Matches(msg, keyBottom):
			d.off = len(d.lines)
		}
		d.off = min(max(d.off, 0), max(len(d.lines)-page, 0))
	}
	return nil
}

// diffChrome is the popup's rows besides the diff itself: title,
// subtitle, a blank line, a blank line and the hints, plus the border.
const diffChrome = 7

func (d *diffView) bodyRows(a *App) int {
	room := max(a.bodyHeight()-diffChrome, 1)
	if d.back != nil && !d.loading {
		// A notification's details: as tall as they are, up to the room.
		return min(max(len(d.lines), 1), room)
	}
	return room
}

// diffInnerWidth is wide, unlike other popups: diffs need the room.
func diffInnerWidth(width int) int { return max(width-8, 24) }

func (d *diffView) view(a *App) string {
	inner := diffInnerWidth(a.width)
	rows := d.bodyRows(a)
	var b strings.Builder
	if d.title != "" {
		b.WriteString(popupTitle(d.title) + "\n")
		b.WriteString(styles.MutedStyle().Render(ansi.Truncate(d.sub, inner, "…")) + "\n\n")
	} else {
		name := entryName(d.n.Entry)
		b.WriteString(popupTitle("Compare "+midTrunc(name, inner-12)) + "\n")
		sub := "snapshot"
		if d.when != "" {
			sub += " (" + d.when + ")"
		}
		b.WriteString(styles.MutedStyle().Render(ansi.Truncate(sub+" → on disk now · "+d.n.path, inner, "…")) + "\n\n")
	}

	var body []string
	switch {
	case d.loading:
		body = []string{styles.MutedStyle().Render("comparing…")}
		if d.title != "" {
			body = []string{styles.MutedStyle().Render("reading what changed…")}
		}
		if d.loadingText != "" {
			body = []string{styles.MutedStyle().Render(d.loadingText)}
		}
	case d.err != nil:
		body = []string{errorLine(d.err)}
	default:
		end := min(d.off+rows, len(d.lines))
		for _, l := range d.lines[min(d.off, end):end] {
			body = append(body, ansi.Truncate(l, inner, "…"))
		}
	}
	for len(body) < rows {
		body = append(body, "")
	}
	b.WriteString(strings.Join(body, "\n") + "\n\n")

	pos := ""
	if len(d.lines) > rows {
		pos = fmt.Sprintf("  %d–%d of %d", d.off+1, min(d.off+rows, len(d.lines)), len(d.lines))
	}
	switch {
	case d.back != nil && d.enter != nil:
		b.WriteString(hints("↑↓", "scroll", "enter", "go to its source", "esc", "back") + styles.MutedStyle().Render(pos))
	case d.back != nil:
		b.WriteString(hints("↑↓", "scroll", "esc", "back") + styles.MutedStyle().Render(pos))
	case d.title != "":
		b.WriteString(hints("↑↓", "scroll", "pgup/pgdn", "page", "esc", "close") + styles.MutedStyle().Render(pos))
	default:
		b.WriteString(hints("↑↓", "scroll", "pgup/pgdn", "page", "r", "restore", "esc", "close") + styles.MutedStyle().Render(pos))
	}
	return boxed(b.String(), inner)
}
