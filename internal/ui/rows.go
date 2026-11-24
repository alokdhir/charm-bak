package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
	ltable "charm.land/lipgloss/v2/table"
)

// renderRows lays out label/value rows as a borderless table: the value
// column lines up whatever the labels' widths, and a long value wraps
// under itself. width is the whole table's width; 0 = as wide as the
// content. Cells may carry their own styling.
func renderRows(rows [][2]string, width int) string {
	if len(rows) == 0 {
		return ""
	}
	t := ltable.New().
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderHeader(false).BorderColumn(false).BorderRow(false).
		Headers("", "").
		StyleFunc(func(_, col int) lipgloss.Style {
			if col == 0 {
				return lipgloss.NewStyle().PaddingRight(2)
			}
			return lipgloss.NewStyle()
		})
	if width > 0 {
		t.Width(width).Wrap(true)
	}
	for _, r := range rows {
		t.Row(r[0], r[1])
	}
	// Headers("", "") with no borders still leaves a blank first line.
	_, out, _ := strings.Cut(t.String(), "\n")
	return out
}

// formBuilder is a strings.Builder for popups whose label/value rows sit
// between other lines (titles, notes, hints): consecutive Row calls
// become one renderRows table, flushed by the next WriteString.
type formBuilder struct {
	b    strings.Builder
	rows [][2]string
}

func (f *formBuilder) Row(label, value string) { f.rows = append(f.rows, [2]string{label, value}) }

func (f *formBuilder) flush() {
	if len(f.rows) > 0 {
		f.b.WriteString(renderRows(f.rows, 0) + "\n")
		f.rows = nil
	}
}

func (f *formBuilder) WriteString(s string) (int, error) {
	f.flush()
	return f.b.WriteString(s)
}

func (f *formBuilder) String() string {
	f.flush()
	return f.b.String()
}
