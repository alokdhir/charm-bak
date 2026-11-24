package ui

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"

	"charm-bak/internal/ui/styles"
)

// popupMaxWidth caps a popup's inner width so it reads as a small centered
// box rather than filling the terminal.
const popupMaxWidth = 76

// renderPopup boxes content at popupInnerWidth for a terminal availWidth
// wide.
func renderPopup(content string, availWidth int) string {
	body := lipgloss.NewStyle().Width(popupInnerWidth(availWidth)).Render(content)
	return styles.PopupBorderStyle().Render(body)
}

// popupInnerWidth is the content width renderPopup lays out at, clamped so
// the box never overflows a narrow terminal.
func popupInnerWidth(availWidth int) int {
	return max(min(popupMaxWidth, availWidth-8), 24)
}

// compositeOverlay draws popup centered over background with lipgloss's
// layer compositor, leaving every cell it doesn't cover untouched.
//
// A popup taller than the area is cut to it (its top, then its last
// content line, the key hints, and the bottom edge): forms window their
// long lists to fit, but on the smallest terminals some still can't, and
// a frame taller than the terminal scrolls and corrupts the screen.
func compositeOverlay(background, popup string, width, height int) string {
	if lines := strings.Split(popup, "\n"); len(lines) > height && height >= 3 {
		popup = strings.Join(append(slices.Clone(lines[:height-2]), lines[len(lines)-2:]...), "\n")
	}
	row := max((height-lipgloss.Height(popup))/2, 0)
	col := max((width-lipgloss.Width(popup))/2, 0)
	bg := lipgloss.NewLayer(background).ID("background")
	fg := lipgloss.NewLayer(popup).ID("popup").X(col).Y(row).Z(1)
	return lipgloss.NewCompositor(bg, fg).Render()
}
