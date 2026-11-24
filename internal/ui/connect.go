package ui

import (
	"context"
	"errors"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// Connector reaches the menu bar app. main.go supplies the real one
// (internal/client and `open`); tests can supply their own.
type Connector struct {
	Dial   func(ctx context.Context) (backup.Repo, error)
	Launch func() error
}

// connState is where the connect screen is.
type connState int

const (
	connDialing   connState = iota // first attempt, nothing shown yet
	connDown                       // app not running: offer to start it
	connLaunching                  // started it, retrying until it answers
	connFatal                      // it answers but can't be used (protocol mismatch)
)

// How long to keep retrying after starting the app.
const (
	launchRetryEvery = 500 * time.Millisecond
	launchRetryFor   = 15 * time.Second
)

type dialMsg struct {
	repo backup.Repo
	err  error
}

type retryMsg struct{}

func (c *Connector) dial() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		repo, err := c.Dial(ctx)
		return dialMsg{repo, err}
	}
}

// NewConnectRoot starts on the connect step and swaps in the App once the
// menu bar app answers.
func NewConnectRoot(c Connector, now func() time.Time) *Root {
	return &Root{connector: &c, now: now}
}

// updateConnect handles messages until the App exists.
func (r *Root) updateConnect(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case dialMsg:
		if msg.err == nil {
			r.app = NewApp(msg.repo, r.now)
			cmds := []tea.Cmd{r.app.Init()}
			if r.width > 0 {
				size := tea.WindowSizeMsg{Width: r.width, Height: r.height}
				cmds = append(cmds, func() tea.Msg { return size })
			}
			return r, tea.Batch(cmds...)
		}
		r.connErr = msg.err
		var perr *protocol.Error
		if errors.As(msg.err, &perr) && perr.Code == protocol.ErrUnsupportedProtocol {
			r.conn = connFatal
			return r, nil
		}
		if r.conn == connLaunching && r.now().Sub(r.launchedAt) < launchRetryFor {
			return r, tea.Tick(launchRetryEvery, func(time.Time) tea.Msg { return retryMsg{} })
		}
		r.conn = connDown
		return r, nil

	case retryMsg:
		return r, r.connector.dial()

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return r, tea.Quit
		case "enter":
			if r.conn != connDown {
				return r, nil
			}
			if err := r.connector.Launch(); err != nil {
				r.connErr = err
				return r, nil
			}
			r.conn, r.launchedAt = connLaunching, r.now()
			return r, r.connector.dial()
		}
	}
	return r, nil
}

// renderConnect is the whole screen while there's no App.
func (r *Root) renderConnect() string {
	var b strings.Builder
	switch r.conn {
	case connDialing:
		return ""
	case connLaunching:
		b.WriteString(styles.FieldValueStyle().Render("Starting charm-bak…"))
	case connFatal:
		b.WriteString(styles.WarnTitleStyle().Render("The menu bar app and this TUI don't match"))
		b.WriteString("\n\n")
		b.WriteString(r.connErr.Error())
		b.WriteString("\n\n")
		b.WriteString(styles.FieldLabelStyle().Render("q to quit"))
	default:
		b.WriteString(styles.WarnTitleStyle().Render("charm-bak's menu bar app isn't running"))
		b.WriteString("\n\n")
		b.WriteString(styles.FieldValueStyle().Render("It runs the backups and holds the repository; this TUI shows what it has."))
		b.WriteString("\n\n")
		b.WriteString(styles.HotkeyStyle().Render("enter") + styles.FieldLabelStyle().Render(" start it    ") +
			styles.HotkeyStyle().Render("q") + styles.FieldLabelStyle().Render(" quit"))
		if r.connErr != nil && !strings.Contains(r.connErr.Error(), "isn't running") {
			b.WriteString("\n\n")
			b.WriteString(styles.HealthStyle(styles.Fail).Render(r.connErr.Error()))
		}
	}
	w, h := max(r.width, 1), max(r.height, 1)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center,
		lipgloss.NewStyle().MaxWidth(w).Render(b.String()))
}
