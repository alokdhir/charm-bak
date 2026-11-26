package ui

import (
	"context"
	"fmt"
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
// that reinstalls the remote's daemon (addRemote's repair screen), and
// the Save and Cancel buttons. Enter on the address goes to Save.
type hostForm struct {
	host    string
	addr    textinput.Model
	err     error
	sending bool
	stop    int // hfAddress, hfRepair or hfButtons
	buttons formButtons
}

const (
	hfAddress = iota
	hfRepair
	hfButtons
)

func newHostForm(host, addr string) *hostForm {
	return &hostForm{host: host, addr: newTextField(addr, "pantry.local or 192.168.1.10"), buttons: newFormButtons("Save")}
}

// onAddress reports whether the address field has the cursor.
func (f *hostForm) onAddress() bool { return f.stop == hfAddress }

type hostSetMsg struct {
	name string // the machine asked about
	host protocol.Host
	err  error
}

func (f *hostForm) typing() bool { return f.onAddress() }

// moveTo puts the cursor on stop, focusing the address when it's there.
func (f *hostForm) moveTo(stop int) tea.Cmd {
	if stop == hfButtons && f.stop != hfButtons {
		f.buttons.pick = 0
	}
	f.stop = stop
	if stop == hfAddress {
		return f.addr.Focus()
	}
	f.addr.Blur()
	return nil
}

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
		case msg.String() == "down":
			return f.moveTo(min(f.stop+1, hfButtons))
		case msg.String() == "up":
			return f.moveTo(max(f.stop-1, hfAddress))
		case f.stop == hfRepair:
			if key.Matches(msg, keyEnter) {
				return a.startRepair(f.host)
			}
			return nil
		case f.stop == hfButtons:
			switch f.buttons.key(msg) {
			case 0:
				return f.save(a)
			case 1:
				a.modal = nil
			}
			return nil
		case key.Matches(msg, keyEnter):
			return f.moveTo(hfButtons)
		}
		var cmd tea.Cmd
		f.addr, cmd = f.addr.Update(msg)
		f.err = nil
		return cmd
	}
	return nil
}

func (f *hostForm) save(a *App) tea.Cmd {
	addr := strings.TrimSpace(f.addr.Value())
	if addr == "" {
		f.err = fmt.Errorf("the address can't be empty")
		return nil
	}
	f.sending, f.err = true, nil
	repo, name := a.repo, f.host
	return func() tea.Msg {
		h, err := repo.SetHostAddress(context.Background(), protocol.HostSetParams{Name: name, Address: addr})
		return hostSetMsg{name: name, host: h, err: err}
	}
}

// body is the tab's fields, above the status line and hints.
func (f *hostForm) body(a *App, inner int) string {
	var b strings.Builder
	f.addr.SetWidth(inner)
	b.WriteString(styles.FieldLabelStyle().Render("Address") + "\n")
	b.WriteString(f.addr.View() + "\n")
	b.WriteString(styles.MutedStyle().Width(inner).Render("Where this Mac reaches "+f.host+"'s daemon: a name or IP, with :port if not 7457. The pairing stays, so it only connects if the same machine answers.") + "\n\n")
	label := "Repair " + f.host + "…"
	if f.stop == hfRepair {
		b.WriteString(styles.SelectedStyle().Render("▸ "+label) + "\n")
	} else {
		b.WriteString(styles.FieldValueStyle().Render("  "+label) + "\n")
	}
	b.WriteString(styles.MutedStyle().Width(inner).Render("A command to run on "+f.host+" that reinstalls its daemon, for when it won't start. Keeps its sources, backups and pairing.") + "\n\n")
	b.WriteString(f.buttons.view(f.stop == hfButtons) + "\n")
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
	switch f.stop {
	case hfRepair:
		b.WriteString(hints("tab", "defaults", "↑↓", "move", "enter", "repair", "esc", "cancel"))
	case hfButtons:
		b.WriteString(hints("tab", "defaults", "↑", "repair", "←→", "choose", "enter", "press", "esc", "cancel"))
	default:
		b.WriteString(hints("tab", "defaults", "↓", "repair", "enter", "next", "esc", "cancel"))
	}
	return b.String()
}
