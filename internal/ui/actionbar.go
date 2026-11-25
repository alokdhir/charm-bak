package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/ui/styles"
)

// action is one action-bar button. label's first occurrence of the
// hotkey letter is highlighted, e.g. "Add" shows as [A]dd.
type action struct {
	label string
	key   key.Binding
	// needs names the action in plain words ("Backing up now") when it
	// needs its machine's daemon; "" when it works with the machine
	// offline (offline.go).
	needs string
}

// actions lists the focused column's keys, then Info when there's
// something to show.
func (a *App) actions() []action {
	acts := a.columnActions()
	if len(a.infoFields()) > 0 {
		acts = append(acts, action{label: "Info", key: keyInfo})
	}
	return acts
}

func (a *App) columnActions() []action {
	switch {
	case a.col == colSources && a.onHeader():
		// Add browses and adds on that host, while it's answering; Edit
		// is the machine's defaults (and a remote's address); Remove
		// takes a remote away altogether (hostremove.go).
		h, _ := a.selHost()
		acts := []action{{"Add", keyAdd, "Adding a source"}, {label: "Edit", key: keyEdit}}
		if !h.Local {
			acts = append(acts, action{label: "Remove", key: keyRemove})
		}
		// A key charm-bak made can be replaced now (it's also replaced
		// every 90 days); one the user gave it is theirs.
		if h.KeyMade != nil {
			needs := ""
			if !h.Local {
				needs = "Replacing its key"
			}
			acts = append(acts, action{"New key", keyNewKey, needs})
		} else if a.adoptable(h) {
			acts = append(acts, action{label: "Adopt key", key: keyAdopt})
		}
		return append(acts, action{label: "Add Remote", key: keyRemote})
	case a.col == colSources:
		// Add Remote on every row: pairing another machine isn't about
		// the selected one.
		return []action{{"Add", keyAdd, "Adding a source"}, {"Edit", keyEdit, "Editing a source"}, {"Remove", keyRemove, "Removing a source"},
			{"Backup now", keyBackup, "Backing up now"}, {label: "Add Remote", key: keyRemote}}
	case a.col == colSnapshots:
		// Changes, not Diff: on a snapshot the useful question is what
		// that backup picked up, not how its top folder differs from the
		// disk (Alok, 2026-11-24). It reads the repository, so it works
		// with the machine offline.
		return a.withFinder([]action{{label: "Restore", key: keyRestore}, {label: "Changes", key: keyChanges},
			{"Edit", keyEdit, "Editing a note"}, {"Pin", keyPin, "Pinning"}})
	default:
		return a.withFinder([]action{{label: "Restore", key: keyRestore}, {"Diff", keyDiff, "Comparing with its disk"},
			{"Exclude", keyExclude, "Excluding"}})
	}
}

// buttonSpan is a hero stop's place on screen, in columns.
type buttonSpan struct{ x, w int }

// renderButton draws "[A]dd": the hotkey letter bracketed and colored;
// all of it muted when dim (an action its machine is offline for).
func renderButton(act action, dim bool) string {
	k := act.key.Keys()[0]
	if dim {
		if i := strings.Index(strings.ToLower(act.label), k); i >= 0 {
			return styles.MutedStyle().Render(act.label[:i] + "[" + act.label[i:i+1] + "]" + act.label[i+1:])
		}
		return styles.MutedStyle().Render("[" + strings.ToUpper(k) + "] " + act.label)
	}
	i := strings.Index(strings.ToLower(act.label), k)
	if i < 0 {
		k = strings.ToUpper(k) // "[X] Remove", capital like "[A]dd"
	}
	if i < 0 {
		return styles.HotkeyStyle().Render("["+k+"]") + styles.FieldValueStyle().Render(" "+act.label)
	}
	return styles.FieldValueStyle().Render(act.label[:i]) +
		styles.HotkeyStyle().Render("["+act.label[i:i+1]+"]") +
		styles.FieldValueStyle().Render(act.label[i+1:])
}

// renderActionBar right-aligns the buttons (the app's name is in the
// hero's top edge).
func (a *App) renderActionBar() string {
	var buttons []string
	for _, act := range a.actions() {
		buttons = append(buttons, renderButton(act, a.blocked(act)))
	}
	right := strings.Join(buttons, "  ") + " "
	// The breadcrumb, when columns have slid off, keeps its end: the
	// folder nearest the columns on screen matters most.
	left := ""
	if crumb := a.breadcrumb(); crumb != "" {
		if avail := a.width - lipgloss.Width(right) - 4; avail > 3 {
			if lipgloss.Width(crumb) > avail {
				crumb = "…" + ansi.TruncateLeft(crumb, lipgloss.Width(crumb)-avail+1, "")
			}
			left = " " + styles.MutedStyle().Render(crumb)
		}
	}
	return lipgloss.NewStyle().MaxWidth(a.width).Render(fitRow(left, right, a.width))
}

// renderFooter puts status left and the help hint right. Status only,
// never key hints: those are the action bar's and help's job.
func renderFooter(width int, left string) string {
	return fitRow(" "+styles.MutedStyle().Render(left), styles.MutedStyle().Render("^N notifications · ? help "), width)
}
