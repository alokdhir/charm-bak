package ui

import (
	"strings"

	"charm-bak/internal/ui/styles"
)

// renderHelp lists every binding from helpBindings as a table: keys in
// one column, descriptions wrapping under themselves in the other.
func renderHelp(availWidth int) string {
	var b strings.Builder
	b.WriteString(popupTitle("Help"))
	b.WriteString("\n\n")
	var rows [][2]string
	for _, kb := range helpBindings {
		h := kb.Help()
		rows = append(rows, [2]string{styles.HotkeyStyle().Render(h.Key), h.Desc})
	}
	rows = append(rows, [2]string{"", ""}, [2]string{styles.FieldLabelStyle().Render("click"),
		"the name in the top box copies its full path; info rows copy their value. shift+drag selects text."})
	b.WriteString(indentLines(renderRows(rows, popupInnerWidth(availWidth)-2), "  "))
	b.WriteString("\n\n  " + styles.MutedStyle().Render("esc or ? to close"))
	return b.String()
}

// indentLines puts prefix before every line of s.
func indentLines(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
