package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"charm-bak/internal/ui/styles"
)

// modal is a popup that owns the keyboard while it's open: the folder
// picker, the source form, a confirmation. update gets every key and
// every message the App doesn't handle itself (a picker's directory
// reads); a modal closes itself by setting a.modal.
type modal interface {
	update(a *App, msg tea.Msg) tea.Cmd
	view(a *App) string
	// typing reports whether a text field has focus, so q types a q
	// instead of quitting.
	typing() bool
}

// modalHeight is how many rows a popup may use: the column area, less
// its border.
func (a *App) modalHeight() int { return max(a.bodyHeight()-2, 8) }

// popupTitle is a popup's first line.
// Titles sit flush with the content under them (Alok, 2026-11-24: a title
// padded one cell in made everything below look outdented).
func popupTitle(s string) string { return styles.FocusedTitleStyle().Render(strings.TrimSpace(s)) }

// hints is a popup's last line: key, what it does; muted.
func hints(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, styles.HotkeyStyle().Render(pairs[i])+" "+styles.MutedStyle().Render(pairs[i+1]))
	}
	return strings.Join(parts, styles.MutedStyle().Render(" · "))
}

// boxed renders popup content at a fixed inner width.
func boxed(content string, inner int) string {
	return styles.PopupBorderStyle().Render(lipgloss.NewStyle().Width(inner).MaxWidth(inner).Render(content))
}

// errorLine is how a popup shows the app's refusal.
func errorLine(err error) string {
	if err == nil {
		return ""
	}
	return styles.HealthStyle(styles.Fail).Render(err.Error())
}

// ask opens a popup that an answer from a daemon called for (a count
// that took minutes, a removal that finished). If another popup is open
// it waits its turn: replacing a half-filled form, or the recovery key
// shown once, with a question would lose it.
func (a *App) ask(m modal) {
	if a.modal == nil {
		a.modal = m
		return
	}
	a.waiting = append(a.waiting, m)
}

// nextWaiting opens the oldest waiting popup once nothing else is open.
func (a *App) nextWaiting() {
	if a.modal == nil && len(a.waiting) > 0 {
		a.modal, a.waiting = a.waiting[0], a.waiting[1:]
	}
}
