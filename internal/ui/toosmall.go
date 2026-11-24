package ui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"charm-bak/internal/ui/styles"
)

// The floor below which the layout stops being usable: Sources and
// Snapshots columns plus one tree column plus a preview need about 100
// columns; the hero, action bar and footer take 6 rows before any list
// row shows.
const (
	minTerminalWidth  = 100
	minTerminalHeight = 20
)

// renderTooSmall replaces the whole view below the floor, naming both the
// current and the required size so it's clear which dimension is short.
func renderTooSmall(width, height int) string {
	var b strings.Builder
	b.WriteString(styles.WarnTitleStyle().Render("Terminal too small"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "charm-bak needs at least %d x %d\n", minTerminalWidth, minTerminalHeight)
	fmt.Fprintf(&b, "this terminal is %s\n", styles.WarnTitleStyle().Render(fmt.Sprintf("%d x %d", width, height)))
	b.WriteString("\n")
	b.WriteString(styles.FieldLabelStyle().Render("resize, or q to quit"))
	return lipgloss.Place(max(width, 1), max(height, 1), lipgloss.Center, lipgloss.Center, b.String())
}

// tooSmall reports whether the terminal is below the floor (a size not
// yet known counts as fine).
func (a *App) tooSmall() bool {
	return a.width > 0 && (a.width < minTerminalWidth || a.height < minTerminalHeight)
}
