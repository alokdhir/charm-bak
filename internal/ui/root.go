package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"charm-bak/internal/ui/styles"
)

// Root is the model main.go runs: App's view, padded to the full terminal,
// in alt-screen mode, with the theme background set on the terminal
// itself.
type Root struct {
	app           *App
	width, height int

	// Until the menu bar app answers there's no App, only the connect
	// screen (connect.go).
	connector  *Connector
	now        func() time.Time
	conn       connState
	connErr    error
	launchedAt time.Time
}

// NewRoot runs app directly (tests, and --fake).
func NewRoot(app *App) *Root { return &Root{app: app} }

func (r *Root) Init() tea.Cmd {
	if r.app == nil {
		return r.connector.dial()
	}
	return r.app.Init()
}

func (r *Root) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		r.width, r.height = ws.Width, ws.Height
	}
	if r.app == nil {
		return r.updateConnect(msg)
	}
	m, cmd := r.app.Update(msg)
	r.app = m.(*App)
	return r, cmd
}

func (r *Root) View() tea.View {
	var v tea.View
	if r.app == nil {
		v = tea.NewView(r.renderConnect())
	} else {
		v = r.app.View()
	}
	if r.width > 0 && r.height > 0 {
		v = tea.NewView(lipgloss.NewStyle().Width(r.width).Height(r.height).MaxHeight(r.height).Render(v.Content))
	}
	// The background is a real OSC 11 change to the terminal's default
	// background, not a lipgloss Background() wrap: a wrap is undone by
	// the reset at the end of every colored span inside it.
	v.BackgroundColor = styles.Background
	// Alt-screen is a property of the view in bubbletea v2; without it
	// every frame prints inline and scrolls the terminal.
	v.AltScreen = true
	// Mouse tracking is always on, because the hero (always visible) has
	// click-to-copy. Tradeoff, deliberate: a plain drag no longer selects
	// text in the terminal; shift+drag still does.
	v.MouseMode = tea.MouseModeCellMotion
	// Focus reports let App ignore the click that only focused the window.
	v.ReportFocus = true
	return v
}
