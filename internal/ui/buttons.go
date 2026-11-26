package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/ui/styles"
)

// formButtons is the row a form ends with: its action (Save, Restore)
// and Cancel, the form's last cursor stop (Alok, 2026-11-26: with Enter
// saving from any field, a field with its own Enter, like the folder
// picker, meant moving off it to save). ↓ from the last field reaches
// it, ←→ choose, Enter presses; Enter on a plain field moves to the next
// one, so pressing Enter through a form ends on Save.
type formButtons struct {
	labels [2]string
	pick   int // 0 = the action, 1 = Cancel
}

func newFormButtons(action string) formButtons {
	return formButtons{labels: [2]string{action, "Cancel"}}
}

// key handles ←→ (and h, l, space) on the row; enter reports which
// button was pressed. pressed is -1 when nothing was.
func (b *formButtons) key(msg tea.KeyPressMsg) (pressed int) {
	switch msg.String() {
	case "left", "h":
		b.pick = 0
	case "right", "l":
		b.pick = 1
	case "space":
		b.pick = 1 - b.pick
	case "enter":
		return b.pick
	}
	return -1
}

// view draws the row; the picked button has the cursor while focused.
func (b *formButtons) view(focused bool) string {
	out := make([]string, len(b.labels))
	for i, l := range b.labels {
		text := "[ " + l + " ]"
		if focused && i == b.pick {
			out[i] = styles.SelectedStyle().Render(text)
		} else {
			out[i] = styles.FieldValueStyle().Render(text)
		}
	}
	return strings.Join(out, "  ")
}

// help is the line a form shows while the row has the cursor.
func (b *formButtons) help(action, cancel string) string {
	if b.pick == 1 {
		return cancel
	}
	return action
}
