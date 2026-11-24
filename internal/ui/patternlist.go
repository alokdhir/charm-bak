package ui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/ui/styles"
)

// patternList edits skip patterns one per row (Alok, 2026-11-24: in one
// comma-separated field, where a pattern ended and what its spaces meant
// was unreadable). The rows are the patterns, then an add line. ↑↓ move
// through them; on a pattern, Enter edits it and x or delete removes it;
// on the add line, typing and Enter adds. Keys it doesn't use (↑ on the
// first row, ↓ on the add line, Enter on an empty add line) are the
// form's.
type patternList struct {
	items []string
	cur   int // 0..len(items); len(items) is the add line
	edit  int // the item being edited, -1 for none
	input textinput.Model
}

func newPatternList(items []string, placeholder string) *patternList {
	l := &patternList{items: slices.Clone(items), edit: -1, input: newTextField("", placeholder)}
	l.cur = len(l.items)
	return l
}

// values is the patterns, including one typed but not yet added.
func (l *patternList) values() []string {
	out := slices.Clone(l.items)
	if l.edit >= 0 {
		out = slices.Delete(out, l.edit, l.edit+1)
	}
	if v := strings.TrimSpace(l.input.Value()); v != "" {
		if l.edit >= 0 {
			out = slices.Insert(out, l.edit, v)
		} else {
			out = append(out, v)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// typing reports whether keys go to the text input.
func (l *patternList) typing() bool { return l.cur == len(l.items) || l.edit >= 0 }

// focus is called when the form moves onto the list (from above when
// down, else from below).
func (l *patternList) focus(down bool) tea.Cmd {
	l.cur = len(l.items)
	if down && len(l.items) > 0 {
		l.cur = 0
	}
	return l.sync()
}

func (l *patternList) blur() {
	l.commit()
	l.input.Blur()
}

// sync focuses the input when the cursor is on the add line or editing.
func (l *patternList) sync() tea.Cmd {
	if l.typing() {
		l.input.CursorEnd()
		return l.input.Focus()
	}
	l.input.Blur()
	return nil
}

// commit ends an edit, or adds what's typed on the add line.
func (l *patternList) commit() {
	v := strings.TrimSpace(l.input.Value())
	switch {
	case l.edit >= 0 && v == "":
		l.items = slices.Delete(l.items, l.edit, l.edit+1)
		l.cur = min(l.edit, len(l.items))
	case l.edit >= 0:
		l.items[l.edit] = v
	case v != "":
		// One pattern per row: a comma is part of the pattern.
		l.items = append(l.items, v)
		l.cur = len(l.items)
	}
	l.edit = -1
	l.input.SetValue("")
}

// key handles msg and reports whether it used it.
func (l *patternList) key(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch s := msg.String(); {
	case s == "esc" && l.edit >= 0:
		l.edit = -1
		l.input.SetValue("")
		return l.sync(), true
	case s == "up" || s == "down":
		if l.edit >= 0 || strings.TrimSpace(l.input.Value()) != "" {
			l.commit()
		}
		next := l.cur - 1
		if s == "down" {
			next = l.cur + 1
		}
		if next < 0 || next > len(l.items) {
			return nil, false
		}
		l.cur = next
		return l.sync(), true
	case s == "enter":
		switch {
		case l.edit >= 0:
			l.commit()
			return l.sync(), true
		case l.cur < len(l.items):
			l.edit = l.cur
			l.input.SetValue(l.items[l.cur])
			return l.sync(), true
		case strings.TrimSpace(l.input.Value()) != "":
			l.commit()
			return nil, true
		}
		return nil, false // an empty add line: the form saves
	case l.edit < 0 && l.cur < len(l.items):
		if s == "x" || s == "delete" || s == "backspace" {
			l.items = slices.Delete(l.items, l.cur, l.cur+1)
			l.cur = min(l.cur, len(l.items))
			return l.sync(), true
		}
		return nil, false // the form's (esc, tab, ←→)
	}
	var cmd tea.Cmd
	l.input, cmd = l.input.Update(msg)
	return cmd, true
}

// rows renders the list as form values, one per row: the patterns, then
// the add line. focused is whether the form's cursor is on the list;
// room is the most rows to draw (0: all of them), what the form's popup
// has left, and the rows scroll with the cursor.
func (l *patternList) rows(focused bool, width, room int) []string {
	var out []string
	for i, p := range l.items {
		switch {
		case focused && i == l.edit:
			l.input.SetWidth(width)
			out = append(out, l.input.View())
		case focused && i == l.cur:
			out = append(out, styles.StopStyle(true).Render(midTrunc(p, width)))
		default:
			out = append(out, styles.FieldValueStyle().Render(midTrunc(p, width)))
		}
	}
	switch {
	case focused && l.cur == len(l.items) && l.edit < 0:
		l.input.SetWidth(width)
		out = append(out, l.input.View())
	case len(l.items) == 0:
		out = append(out, styles.MutedStyle().Render("none"))
	default:
		out = append(out, styles.MutedStyle().Render("+ add"))
	}
	if room <= 0 || len(out) <= room {
		return out
	}
	// More rows than there's room for: a window around the cursor (from
	// the top when the cursor is elsewhere), its first and last rows
	// saying how many are out of sight. The cursor's row is never one of
	// those: it sits in the middle of the window except at the ends.
	off := 0
	if focused {
		off = min(max(l.cur-room/2, 0), len(out)-room)
	}
	shown := slices.Clone(out[off : off+room])
	if room >= 3 {
		if off > 0 {
			shown[0] = styles.MutedStyle().Render(fmt.Sprintf("↑ %d more", off+1))
		}
		if below := len(out) - off - room; below > 0 {
			shown[room-1] = styles.MutedStyle().Render(fmt.Sprintf("↓ %d more", below+1))
		}
	}
	return shown
}

// help explains the row under the cursor: what a pattern matches, or how
// to add one.
func (l *patternList) help() string {
	switch {
	case l.edit >= 0:
		return "enter keeps it, esc undoes, empty removes it"
	case l.cur < len(l.items):
		// Which one of how many: a long list shows only some of its rows.
		return fmt.Sprintf("%d of %d: ", l.cur+1, len(l.items)) + lowerFirst(explainPattern(l.items[l.cur])) + " · enter edits, x removes"
	}
	return "One pattern per line, e.g. node_modules or *.tmp; enter adds it"
}

// explainPattern says what a restic exclude pattern skips. Without a
// leading / it matches at any depth (tested with restic 0.19.1: a/b
// skipped both a/b and x/a/b); with one it's a full path.
func explainPattern(p string) string {
	if rest, ok := strings.CutPrefix(p, "!"); ok {
		return "Brings back " + rest + " if a pattern above skipped it"
	}
	wild := strings.ContainsAny(p, "*?[")
	switch {
	case strings.HasPrefix(p, "/"):
		if wild {
			return "Skips paths matching " + p
		}
		return "Skips exactly " + p
	case strings.Contains(strings.TrimSuffix(p, "/"), "/"):
		return "Skips " + p + " in any folder"
	case wild:
		return "Skips anything named like " + p + ", in any folder"
	}
	return "Skips anything named " + p + ", in any folder"
}
