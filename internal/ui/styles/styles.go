// Package styles centralizes the visual theme: Crush's Charmtone palette,
// with each color assigned exactly one role.
package styles

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/charmtone"
	"github.com/lucasb-eyer/go-colorful"
)

// Color roles. Each has one meaning; don't reuse a role for something
// else because the color happens to look right.
var (
	// Focused marks what has keyboard focus at the outermost level: the
	// focused column's title and divider, the focused action-bar button,
	// the focused hero field.
	Focused color.Color = charmtone.Dolly

	// Selected is the cursor row inside a column, in reverse video:
	// stronger than Focused so the two levels stay distinct.
	Selected color.Color = charmtone.Charple

	// Muted is unfocused or secondary text: unfocused titles, dividers,
	// dates, sizes.
	Muted color.Color = charmtone.Squid

	// Background is the app-wide terminal background (set via OSC 11 in
	// Root, never a string wrap): true black, bidkit's preferred theme
	// rather than Charmtone's off-black Pepper.
	Background color.Color = lipgloss.Color("#000000")

	// FieldLabel and FieldValue are the two halves of a label/value pair:
	// the label recedes, the value stands out, so no colon is needed.
	FieldLabel color.Color = charmtone.Squid
	FieldValue color.Color = charmtone.Salt

	// OK, Warn and Fail are backup health: last run clean, overdue, failed.
	OK   color.Color = charmtone.Julep
	Warn color.Color = charmtone.Mustard
	Fail color.Color = charmtone.Coral
	// Busy is a job in progress; the menu bar's dot uses the same color.
	Busy color.Color = charmtone.Charple
	// Spinning is the spinner on a source that's backing up right now.
	Spinning color.Color = charmtone.Malibu

	// Added, Changed and Removed mark a file's state compared with the
	// live directory. Same hues as health, on purpose: green is "fine /
	// new", red is "gone / broken".
	Added   color.Color = charmtone.Julep
	Changed color.Color = charmtone.Mustard
	Removed color.Color = charmtone.Coral

	// FileTitle is the name of the thing in view: the hero's first cell
	// and the top of the file pane (charm-aws's primary orange).
	FileTitle color.Color = lipgloss.Color("#F5A623")

	// Pinned is a pinned snapshot's row: kept forever, so it stands out,
	// on the cursor too. Pure red: charmtone's reds lean pink or salmon.
	Pinned color.Color = lipgloss.Color("#FF2A2A")
)

// GradientBlend blends two colors in HCL space at t (0=from, 1=to).
func GradientBlend(from, to color.Color, t float64) color.Color {
	c1, _ := colorful.MakeColor(from)
	c2, _ := colorful.MakeColor(to)
	return lipgloss.Color(c1.BlendHcl(c2, t).Hex())
}

var (
	gradientFadeLeft  = []rune("░▒▓")
	gradientFadeRight = []rune("▓▒░")
)

// GradientBlockLabel frames s in fading block glyphs and renders the whole
// thing as one pink-to-purple gradient (Focused to Selected). Used for the
// app name.
func GradientBlockLabel(s string, bold bool) string {
	label := []rune(" " + s + " ")
	runes := make([]rune, 0, len(gradientFadeLeft)+len(label)+len(gradientFadeRight))
	runes = append(runes, gradientFadeLeft...)
	runes = append(runes, label...)
	runes = append(runes, gradientFadeRight...)

	labelStart := len(gradientFadeLeft)
	labelEnd := len(runes) - len(gradientFadeRight)
	n := len(runes)

	var b strings.Builder
	for i, r := range runes {
		t := float64(i) / float64(max(n-1, 1))
		style := lipgloss.NewStyle().Foreground(GradientBlend(Focused, Selected, t))
		if bold && i >= labelStart && i < labelEnd {
			style = style.Bold(true)
		}
		b.WriteString(style.Render(string(r)))
	}
	return b.String()
}

// SelectedStyle is the cursor row in a column.
func SelectedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Background(Selected).Foreground(charmtone.Butter).Bold(true)
}

// SelectedDimStyle is the cursor row in a column that doesn't have focus:
// still visible, so the path through the columns reads at a glance.
func SelectedDimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(charmtone.Hazy).Bold(true)
}

// FocusedTitleStyle is the focused column's title: colored text, never a
// filled bar, so it can't be mistaken for the cursor.
func FocusedTitleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Focused).Bold(true)
}

// MutedStyle is unfocused titles and secondary text.
func MutedStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Muted)
}

// DividerStyle is a column's vertical divider.
func DividerStyle(focused bool) lipgloss.Style {
	if focused {
		return lipgloss.NewStyle().Foreground(Focused)
	}
	return MutedStyle()
}

// PopupBorderStyle is the box around every popup and the hero. It sets an
// explicit background: the compositor treats unstyled padding spaces as
// transparent, so without one whatever is behind a popup shows through.
func PopupBorderStyle() lipgloss.Style {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Selected).
		BorderBackground(Background).
		Background(Background).
		Padding(0, 1)
}

func FieldLabelStyle() lipgloss.Style { return lipgloss.NewStyle().Foreground(FieldLabel) }
func FieldValueStyle() lipgloss.Style { return lipgloss.NewStyle().Foreground(FieldValue) }
func FileTitleStyle() lipgloss.Style  { return lipgloss.NewStyle().Foreground(FileTitle).Bold(true) }
func PinnedStyle() lipgloss.Style     { return lipgloss.NewStyle().Foreground(Pinned).Bold(true) }

// HealthStyle colors a backup-health figure.
func HealthStyle(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// StopStyle is a focusable hero field or action-bar button: the same
// cursor as a list row when the cursor is on it (one cursor look
// everywhere), plain value text otherwise.
func StopStyle(focused bool) lipgloss.Style {
	if focused {
		return SelectedStyle()
	}
	return FieldValueStyle()
}

// DangerButtonStyle is the focused button of an answer that destroys
// data.
func DangerButtonStyle() lipgloss.Style {
	return lipgloss.NewStyle().Background(Fail).Foreground(charmtone.Pepper).Bold(true)
}

// HotkeyStyle is the hotkey letter inside an unfocused action-bar button.
func HotkeyStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Focused).Bold(true)
}

// WarnTitleStyle is the headline of a warning screen (terminal too small).
func WarnTitleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(Warn).Bold(true)
}
