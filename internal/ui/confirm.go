package ui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/ui/styles"
)

// confirm is a question with a row of answers. ←/→ choose, Enter
// answers, Esc picks the last answer (always the safe one). The default
// is preselected.
type confirm struct {
	title   string
	body    string
	answers []string
	pick    int
	// danger marks answers drawn in red.
	danger map[int]bool
	act    func(a *App, answer int) tea.Cmd
}

func (c *confirm) typing() bool { return false }

func (c *confirm) update(a *App, msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case key.Matches(k, keyLeft), key.Matches(k, keyPrev):
		c.pick = max(c.pick-1, 0)
	case key.Matches(k, keyRight), key.Matches(k, keyNext):
		c.pick = min(c.pick+1, len(c.answers)-1)
	case key.Matches(k, keyEsc):
		a.modal = nil
		return c.act(a, len(c.answers)-1)
	case key.Matches(k, keyEnter):
		a.modal = nil
		return c.act(a, c.pick)
	}
	return nil
}

func (c *confirm) view(a *App) string {
	inner := min(popupInnerWidth(a.width), 60)
	var b strings.Builder
	b.WriteString(popupTitle(c.title) + "\n\n")
	b.WriteString(styles.FieldValueStyle().Width(inner).Render(c.body) + "\n\n")
	var btns []string
	for i, ans := range c.answers {
		text := " " + ans + " "
		switch {
		case i == c.pick && c.danger[i]:
			btns = append(btns, styles.DangerButtonStyle().Render(text))
		case i == c.pick:
			btns = append(btns, styles.StopStyle(true).Render(text))
		case c.danger[i]:
			btns = append(btns, styles.HealthStyle(styles.Fail).Render(text))
		default:
			btns = append(btns, styles.FieldValueStyle().Render(text))
		}
	}
	b.WriteString(strings.Join(btns, "  ") + "\n\n")
	if len(c.answers) == 1 {
		b.WriteString(hints("enter", strings.ToLower(c.answers[0])))
	} else {
		b.WriteString(hints("←→", "choose", "enter", "answer", "esc", c.answers[len(c.answers)-1]))
	}
	return boxed(b.String(), inner)
}
