package ui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// hostForm is a remote's Address tab (machinePopup): where it's reached,
// for when its address changed. The pairing (its key) stays, so this can
// only point at the same machine. Below the address, Repair: the command
// that reinstalls the remote's daemon (addRemote's repair screen).
type hostForm struct {
	host     string
	addr     textinput.Model
	err      error
	sending  bool
	onRepair bool // the cursor is on Repair, not the address
}

type hostSetMsg struct {
	name string // the machine asked about
	host protocol.Host
	err  error
}

func (f *hostForm) typing() bool { return !f.onRepair }

func (f *hostForm) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case hostSetMsg:
		f.sending = false
		if msg.err != nil {
			f.err = msg.err
			return nil
		}
		a.modal = nil
		a.statusMsg = f.host + " is now at " + msg.host.Address + "; reconnecting"
		return a.loadSources()
	case tea.KeyPressMsg:
		if f.sending {
			return nil
		}
		switch {
		case key.Matches(msg, keyEsc):
			a.modal = nil
			return nil
		case !f.onRepair && msg.String() == "down":
			f.onRepair = true
			f.addr.Blur()
			return nil
		case f.onRepair && msg.String() == "up":
			f.onRepair = false
			return f.addr.Focus()
		case f.onRepair:
			if key.Matches(msg, keyEnter) {
				return a.startRepair(f.host)
			}
			return nil
		case key.Matches(msg, keyEnter):
			addr := strings.TrimSpace(f.addr.Value())
			if addr == "" {
				return nil
			}
			f.sending, f.err = true, nil
			repo, name := a.repo, f.host
			return func() tea.Msg {
				h, err := repo.SetHostAddress(context.Background(), protocol.HostSetParams{Name: name, Address: addr})
				return hostSetMsg{name: name, host: h, err: err}
			}
		}
		var cmd tea.Cmd
		f.addr, cmd = f.addr.Update(msg)
		f.err = nil
		return cmd
	}
	return nil
}

// body is the tab's fields, above the status line and hints.
func (f *hostForm) body(a *App, inner int) string {
	var b strings.Builder
	f.addr.SetWidth(inner)
	b.WriteString(styles.FieldLabelStyle().Render("Address") + "\n")
	b.WriteString(f.addr.View() + "\n")
	b.WriteString(styles.MutedStyle().Width(inner).Render("Where this Mac reaches "+f.host+"'s daemon: a name or IP, with :port if not 7457. The pairing stays, so it only connects if the same machine answers.") + "\n\n")
	label := "Repair " + f.host + "…"
	if f.onRepair {
		b.WriteString(styles.SelectedStyle().Render("▸ "+label) + "\n")
	} else {
		b.WriteString(styles.FieldValueStyle().Render("  "+label) + "\n")
	}
	b.WriteString(styles.MutedStyle().Width(inner).Render("A command to run on "+f.host+" that reinstalls its daemon, for when it won't start. Keeps its sources, backups and pairing.") + "\n")
	return b.String()
}

// footer is the status line and key hints.
func (f *hostForm) footer() string {
	var b strings.Builder
	b.WriteString("\n")
	switch {
	case f.sending:
		b.WriteString(styles.MutedStyle().Render("asking the daemon…") + "\n")
	case f.err != nil:
		b.WriteString(errorLine(f.err) + "\n")
	default:
		b.WriteString("\n")
	}
	switch {
	case f.onRepair:
		b.WriteString(hints("tab", "defaults", "enter", "repair", "↑", "address", "esc", "cancel"))
	default:
		b.WriteString(hints("tab", "defaults", "enter", "save", "↓", "repair", "esc", "cancel"))
	}
	return b.String()
}
