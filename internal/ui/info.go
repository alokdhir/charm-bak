package ui

import (
	"time"

	"charm-bak/internal/backup"

	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/ui/styles"
)

// infoField is one row of the info popup; clicking it copies value.
type infoField struct{ label, value string }

// selectionPath is the live path of what's selected: the source's folder
// for a source or snapshot (the common parent if it has several), the
// entry's own path in the tree. restic stores absolute paths, so a
// snapshot path is the live path.
func (a *App) selectionPath() (string, bool) {
	if a.col < colTree {
		s, ok := a.selSource()
		if !ok {
			return "", false
		}
		return backup.Root(s.Paths), true
	}
	n, ok := a.selectedEntry()
	if !ok {
		return "", false
	}
	return n.path, true
}

// infoFields describes the focused column's selection in full.
func (a *App) infoFields() []infoField {
	now := a.now()
	path, _ := a.selectionPath()
	switch {
	case a.col == colSources && a.onHeader():
		h, _ := a.selHost()
		return a.hostInfoFields(h)
	case a.col == colSources && len(a.sources) > 0:
		s, _ := a.selSource()
		r := s.Retention
		f := []infoField{{"Name", s.Name}, {"Paths", strings.Join(s.Paths, ", ")}}
		if len(s.Excludes) > 0 {
			f = append(f, infoField{"Excludes", strings.Join(s.Excludes, ", ")})
		}
		if s.Bytes > 0 || s.Files > 0 {
			f = append(f, infoField{"Size", fmt.Sprintf("%s in %d files (newest snapshot)", humanBytes(s.Bytes), s.Files)})
		}
		if s.LastRun != nil {
			last := stamp(now, s.LastRun.Ended) + ", " + s.LastRun.Status
			if s.LastRun.Error != "" {
				last += ": " + s.LastRun.Error
			}
			f = append(f, infoField{"Last run", last})
		}
		if s.NextRun != nil {
			f = append(f, infoField{"Next", stamp(now, *s.NextRun)})
		}
		if s.Schedule != nil {
			f = append(f, infoField{"Every", every(time.Duration(s.Schedule.Every) * time.Second)})
		} else {
			f = append(f, infoField{"Every", "manual"})
		}
		f = append(f, infoField{"Retention", fmt.Sprintf("%d latest, %d hourly, %d daily, %d weekly, %d monthly, %d yearly", r.Last, r.Hourly, r.Daily, r.Weekly, r.Monthly, r.Yearly)})
		if len(s.Inherited) > 0 {
			f = append(f, infoField{"Defaults", strings.Join(s.Inherited, ", ")})
		}
		if r := a.repoStatus.Repository; r != nil {
			f = append(f, infoField{"Destination", r.Location})
		}
		return append(f, infoField{"Tag", "src:" + s.ID})
	case a.col == colSnapshots && len(a.snaps) > 0:
		s := a.snaps[a.snapCur.pos]
		f := []infoField{
			{"Snapshot", s.ID},
			{"Paths", strings.Join(s.Paths, ", ")},
			{"Started", s.Time.Local().Format("2006-01-02 15:04:05")},
		}
		if s.End != nil {
			f = append(f, infoField{"Took", s.End.Sub(s.Time).Round(time.Second).String()})
		}
		f = append(f,
			infoField{"Size", fmt.Sprintf("%s in %d files, %s new", humanBytes(s.Bytes), s.Files, humanBytes(s.BytesAdded))},
			infoField{"Kept by", strings.Join(s.KeptBy, ", ")},
			infoField{"Note", s.Note},
			infoField{"Host", s.Hostname},
			infoField{"Tags", strings.Join(s.Tags, " ")})
		return f
	case a.col >= colTree:
		n, ok := a.selectedEntry()
		if !ok {
			return nil
		}
		f := []infoField{{"Name", n.Name}, {"Path", path}}
		if !n.IsDir() {
			f = append(f, infoField{"Size", humanBytes(n.Size)})
		}
		f = append(f, infoField{"Mode", n.Mode}, infoField{"Modified", n.MTime.Local().Format("2006-01-02 15:04:05")})
		if len(a.snaps) > 0 {
			s := a.snaps[a.snapCur.pos]
			f = append(f, infoField{"Snapshot", stamp(now, s.Time) + "  " + s.ShortID})
		}
		return f
	}
	return nil
}

// infoGap is the space between the info popup's labels and values.
const infoGap = 3

// infoLayout lays the popup out for a content width: the rendered lines,
// and for each line the index of the field it belongs to (-1 for title,
// spacing and hints). Rendering and click hit-testing both use it, so they
// can't disagree. Long values wrap, preferring to break after / - _ . .
func (a *App) infoLayout(inner int) (lines []string, owner []int) {
	add := func(l string, o int) { lines, owner = append(lines, l), append(owner, o) }
	add(popupTitle("Info"), -1)
	add("", -1)
	// The label column fits the longest label (up to a third of the
	// popup); a field with no label is a blank line between groups.
	fields := a.infoFields()
	lw := 0
	for _, f := range fields {
		lw = max(lw, lipgloss.Width(f.label))
	}
	lw = min(lw, inner/3) + infoGap
	label := styles.FieldLabelStyle().Width(lw).MaxWidth(lw)
	vw := max(inner-lw, 8)
	for i, f := range fields {
		if f.label == "" && f.value == "" {
			add("", -1)
			continue
		}
		wrapped := strings.Split(ansi.Wrap(f.value, vw, "/-_."), "\n")
		for j, part := range wrapped {
			l := ""
			if j == 0 {
				l = midTrunc(f.label, lw-infoGap)
			}
			add(label.Render(l)+styles.FieldValueStyle().Render(part), i)
		}
	}
	add("", -1)
	add(styles.MutedStyle().Render("click a row to copy it · esc to close"), -1)
	return lines, owner
}

func (a *App) renderInfo() string {
	lines, _ := a.infoBody()
	return renderPopup(strings.Join(lines, "\n"), a.width)
}

// infoFieldAt finds the field under screen cell (x, y) by laying the popup
// out again, never from positions recorded during a render.
func (a *App) infoFieldAt(x, y int) (infoField, bool) {
	popup := a.renderInfo()
	pw, ph := lipgloss.Width(popup), lipgloss.Height(popup)
	top := heroHeight + actionHeight + max((a.bodyHeight()-ph)/2, 0)
	left := max((a.width-pw)/2, 0)
	if x < left || x >= left+pw {
		return infoField{}, false
	}
	_, owner := a.infoBody()
	line := y - top - 1 // the border's top row
	if line < 0 || line >= len(owner) || owner[line] < 0 {
		return infoField{}, false
	}
	return a.infoFields()[owner[line]], true
}
