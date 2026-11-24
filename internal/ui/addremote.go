package ui

import (
	"context"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// addRemote (Add Remote, r) pairs a new machine: its name, then the one
// command to run on it, then a live wait that turns into the setup flow
// for it as soon as it answers. The command is click (or c) to copy. A
// Mac without Full Disk Access gets a step first that opens the setting
// on that Mac and waits, live, for the grant.
//
// Every screen is the same height (view), so the popup never jumps.
type addRemote struct {
	name   textinput.Model
	host   string // set once pair_start answered
	res    protocol.PairStartResult
	err    error
	asking bool
	paired bool // in hosts, not answering yet
	copied bool

	access    bool  // the Full Disk Access step
	opened    bool  // access_settings ran there
	accessErr error // access_settings failed

	// repair: the same command screen for a paired remote whose daemon
	// is broken (host_repair): it reinstalls the daemon there, keeping
	// its state. Done once the remote has gone and come back.
	repair  bool
	sawDown bool // the remote stopped answering since the command was shown
}

type accessOpenedMsg struct{ err error }

type accessPolledMsg struct {
	host string
	st   protocol.StatusResult
	err  error
}

// accessPoll is how often the access step asks whether the grant landed.
const accessPoll = 2 * time.Second

// expired reports whether the pairing code has run out.
func (m *addRemote) expired(now time.Time) bool {
	return m.host != "" && !m.res.Expires.IsZero() && now.After(m.res.Expires)
}

type pairStartedMsg struct {
	host string
	res  protocol.PairStartResult
	err  error
}

func (a *App) startAddRemote() tea.Cmd {
	m := &addRemote{name: newTextField("", "tower")}
	a.modal = m
	return m.name.Focus()
}

// startRepair opens the repair command for remote host.
func (a *App) startRepair(host string) tea.Cmd {
	m := &addRemote{name: newTextField(host, ""), host: host, repair: true}
	m.sawDown = a.findHost(host).State != protocol.HostOK
	a.modal = m
	return m.start(a)
}

func (m *addRemote) typing() bool { return m.host == "" }

// waiting reports whether the spinner should tick for it.
func (m *addRemote) waiting(now time.Time) bool {
	return m.asking || m.access || (m.host != "" && !m.expired(now))
}

func (m *addRemote) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case pairStartedMsg:
		m.asking = false
		if msg.err != nil {
			m.err = msg.err
			if m.repair {
				return nil
			}
			return m.name.Focus()
		}
		m.host, m.res, m.err = msg.host, msg.res, nil
		m.name.Blur()
		return nil
	case accessOpenedMsg:
		m.opened, m.accessErr = msg.err == nil, msg.err
		return nil
	case accessPolledMsg:
		if a.modal != m || msg.host != m.host {
			return nil // closed or moved on: stop polling
		}
		if msg.err == nil {
			if a.hostStatus == nil {
				a.hostStatus = map[string]protocol.StatusResult{}
			}
			a.hostStatus[m.host] = msg.st
			if fda := msg.st.FullDiskAccess; fda == nil || *fda {
				return m.finish(a, m.host+" is paired, with Full Disk Access")
			}
		}
		return m.pollAccess(a)
	case tea.KeyPressMsg:
		if m.asking {
			return nil
		}
		if m.access {
			switch {
			case key.Matches(msg, keyEsc):
				a.modal = nil
			case key.Matches(msg, keyEnter):
				repo := a.repo.ForHost(m.host)
				m.accessErr = nil
				return func() tea.Msg { return accessOpenedMsg{repo.OpenAccessSettings(context.Background())} }
			case msg.String() == "s":
				return m.finish(a, m.host+" is paired; it has no Full Disk Access yet")
			}
			return nil
		}
		switch {
		case key.Matches(msg, keyEsc):
			a.modal = nil
			return nil
		case m.host == "" && key.Matches(msg, keyEnter):
			return m.start(a)
		case m.host == "":
			var cmd tea.Cmd
			m.name, cmd = m.name.Update(msg)
			m.err = nil
			return cmd
		case msg.String() == "c":
			return m.copy(a)
		case (m.expired(a.now()) || (m.repair && m.err != nil)) && key.Matches(msg, keyEnter):
			return m.start(a)
		}
	}
	return nil
}

func (m *addRemote) start(a *App) tea.Cmd {
	name := strings.ToLower(strings.TrimSpace(m.name.Value()))
	if name == "" {
		return nil
	}
	m.asking, m.err, m.copied = true, nil, false
	repo, repair := a.repo, m.repair
	return func() tea.Msg {
		if repair {
			res, err := repo.HostRepair(context.Background(), name)
			return pairStartedMsg{name, res, err}
		}
		res, err := repo.PairStart(context.Background(), protocol.PairStartParams{Name: name})
		return pairStartedMsg{name, res, err}
	}
}

func (m *addRemote) copy(a *App) tea.Cmd {
	if m.host == "" || m.res.Command == "" || m.expired(a.now()) {
		return nil
	}
	m.copied = true
	return tea.Batch(copyToClipboardCmd(m.res.Command), audibleCueCmd())
}

// hostsChanged follows the new machine as hosts reload (host_changed
// events): paired, then answering, which hands over to the setup flow,
// or straight to its sources when it already has a repository.
func (m *addRemote) hostsChanged(a *App) tea.Cmd {
	if m.host == "" {
		return nil
	}
	h := a.findHost(m.host)
	if m.repair {
		if h.State != protocol.HostOK {
			m.sawDown = true
			return nil
		}
		if !m.sawDown || m.res.Command == "" {
			return nil
		}
		a.modal = nil
		a.statusMsg = m.host + "'s daemon is reinstalled and answering"
		return nil
	}
	if h.Address == "" && h.State == "" {
		return nil // not paired yet
	}
	m.paired = true
	if h.State != protocol.HostOK || m.access {
		return nil
	}
	if st, ok := a.hostStatus[m.host]; ok && st.FullDiskAccess != nil && !*st.FullDiskAccess {
		m.access = true
		return m.pollAccess(a)
	}
	return m.finish(a, m.host+" is paired")
}

// What a machine still needs, as its header says it.
const (
	todoSetup  = "setup"       // no repository: the setup flow
	todoAccess = "disk access" // a Mac without Full Disk Access
)

// hostTodo is what host still needs before it backs up everything, ""
// when nothing (or not known).
func (a *App) hostTodo(host string) string {
	if a.noRepository(host) {
		return todoSetup
	}
	st, ok := a.statusFor(host)
	if ok && st.FullDiskAccess != nil && !*st.FullDiskAccess {
		return todoAccess
	}
	return ""
}

// statusFor is host's last status: this machine's own, or a remote's
// fetched with its sources.
func (a *App) statusFor(host string) (protocol.StatusResult, bool) {
	if host == a.localHost() {
		return a.repoStatus, a.statusKnown
	}
	st, ok := a.hostStatus[host]
	return st, ok && a.findHost(host).State == protocol.HostOK
}

// startAccess opens the Full Disk Access step for an already paired Mac.
func (a *App) startAccess(host string) tea.Cmd {
	m := &addRemote{host: host, paired: true, access: true}
	a.modal = m
	return m.pollAccess(a)
}

// finish closes the popup and goes on to the setup flow, unless the new
// machine already has a repository.
func (m *addRemote) finish(a *App, status string) tea.Cmd {
	a.modal = nil
	a.statusMsg = status
	if st, ok := a.hostStatus[m.host]; ok && st.Repository != nil {
		return nil
	}
	return a.startSetup(m.host)
}

// pollAccess asks the new machine for its status after accessPoll.
func (m *addRemote) pollAccess(a *App) tea.Cmd {
	repo, host := a.repo.ForHost(m.host), m.host
	return tea.Tick(accessPoll, func(time.Time) tea.Msg {
		st, err := repo.Status(context.Background())
		return accessPolledMsg{host, st, err}
	})
}

// commandAt reports whether screen cell (x, y) is on the command.
func (m *addRemote) commandAt(a *App, x, y int) bool {
	if m.host == "" || m.access {
		return false
	}
	popup := m.view(a)
	pw, ph := lipgloss.Width(popup), lipgloss.Height(popup)
	top := heroHeight + actionHeight + max((a.bodyHeight()-ph)/2, 0)
	left := max((a.width-pw)/2, 0)
	lines := strings.Split(popup, "\n")
	i := y - top
	if x < left || x >= left+pw || i < 0 || i >= len(lines) {
		return false
	}
	line := strings.TrimSpace(strings.Trim(ansi.Strip(lines[i]), "│"))
	return line != "" && strings.Contains(m.res.Command, line)
}

// view pads the current screen to the tallest of the three (name,
// command, Full Disk Access), measured with a command of the usual
// length, so the popup doesn't jump from one to the next.
func (m *addRemote) view(a *App) string {
	tallest := 0
	for _, screen := range []func(*addRemote){
		func(p *addRemote) { p.host, p.access = "", false },
		func(p *addRemote) { p.host, p.access = "x", false },
		func(p *addRemote) { p.host, p.access = "x", true },
	} {
		probe := *m
		screen(&probe)
		if m.repair && (probe.host == "" || probe.access) {
			continue // repair has only the command screen
		}
		if probe.host != "" && (m.host == "" || m.res.Command == "") {
			// pairing.go's: a 44-character pin, a 16-character code.
			probe.res.Command = "curl -fsS --pinnedpubkey 'sha256//" + strings.Repeat("x", 44) + "' -k https://192.168.1.100:7458/i/" + strings.Repeat("x", 16) + " | sudo sh"
		}
		tallest = max(tallest, lipgloss.Height(probe.render(a)))
	}
	return m.renderPadded(a, tallest-lipgloss.Height(m.render(a)))
}

func (m *addRemote) render(a *App) string { return m.renderPadded(a, 0) }

func (m *addRemote) renderPadded(a *App, pad int) string {
	inner := popupInnerWidth(a.width)
	text := func(s string) string { return styles.FieldValueStyle().Width(inner).Render(s) }
	muted := func(s string) string { return styles.MutedStyle().Width(inner).Render(s) }
	var b formBuilder
	b.WriteString(popupTitle("Add a remote") + "\n\n")
	if m.host == "" {
		b.WriteString(text("Another machine on this network. It runs its own backups on its own schedule; this Mac sets it up and shows how it's doing.") + "\n\n")
		m.name.SetWidth(inner - formLabelWidth - 1)
		b.Row(styles.HotkeyStyle().Render("Name"), m.name.View())
		b.WriteString("\n" + muted("How charm-bak shows it: lowercase letters, digits and dashes. Linux and macOS for now; Windows is coming.") + "\n\n")
		switch {
		case m.asking:
			b.WriteString(styles.HealthStyle(styles.Warn).Render(a.spinFrame()) + " " + styles.MutedStyle().Render("starting…") + "\n")
		case m.err != nil:
			b.WriteString(errorLine(m.err) + "\n")
		default:
			b.WriteString("\n")
		}
		b.WriteString(strings.Repeat("\n", max(pad, 0)) + hints("enter", "next", "esc", "cancel"))
		return boxed(b.String(), inner)
	}

	if m.access {
		b.WriteString(styles.HealthStyle(styles.OK).Render("✓ ") + styles.FieldValueStyle().Render(m.host+" is paired.") + "\n\n")
		b.WriteString(text("charm-bak there needs Full Disk Access, or its backups skip protected folders (Mail, Messages, Safari, other people's files). Only someone at "+m.host+" can grant it:") + "\n\n")
		screen, at := m.host+"'s screen (not this Mac's)", "At "+m.host+", drag"
		if m.host == a.localHost() {
			screen, at = "this Mac", "Drag"
		}
		b.WriteString(text("1. Enter opens System Settings › Full Disk Access on "+screen+", with charm-bak shown in Finder beside it.") + "\n")
		b.WriteString(text("2. "+at+" charm-bak into the list (or click + and pick it) and switch it on. This popup moves on by itself.") + "\n\n")
		switch {
		case m.accessErr != nil:
			b.WriteString(errorLine(m.accessErr) + "\n")
		default:
			what := "waiting for Full Disk Access on " + m.host + "…"
			if m.opened {
				what = "opened on " + m.host + "'s screen; waiting for the switch…"
			}
			b.WriteString(styles.HealthStyle(styles.Warn).Render(a.spinFrame()) + " " + styles.MutedStyle().Render(what) + "\n")
		}
		b.WriteString(strings.Repeat("\n", max(pad, 0)) + hints("enter", "open it on "+m.host+"'s screen", "s", "skip for now", "esc", "close"))
		return boxed(b.String(), inner)
	}

	if m.repair {
		return m.renderRepair(a, pad)
	}
	b.WriteString(text("Run this on "+m.host+". It installs charm-bak as a service and pairs it with this Mac (it uses sudo).") + "\n\n")
	b.WriteString(styles.FileTitleStyle().Width(inner).Render(m.res.Command) + "\n\n")
	b.WriteString(muted("If macOS asks whether charm-bak-daemon may accept incoming connections, allow it, or "+m.host+" can't reach this Mac.") + "\n\n")
	expired := m.expired(a.now())
	switch {
	case expired:
		b.WriteString(styles.HealthStyle(styles.Fail).Render("The code expired. Enter makes a new one.") + "\n")
	case m.paired:
		b.WriteString(styles.HealthStyle(styles.OK).Render("✓ ") + styles.FieldValueStyle().Render("paired; ") +
			styles.HealthStyle(styles.Warn).Render(a.spinFrame()) + " " + styles.MutedStyle().Render("connecting to "+m.host+"…") + "\n")
	default:
		until := m.res.Expires.Local().Format("15:04")
		b.WriteString(styles.HealthStyle(styles.Warn).Render(a.spinFrame()) + " " + styles.MutedStyle().Render("waiting for "+m.host+"… (the code works once, until "+until+")") + "\n")
	}
	copyHint := "copy"
	if m.copied {
		copyHint = "copied"
	}
	b.WriteString(strings.Repeat("\n", max(pad, 0)))
	if expired {
		b.WriteString(hints("enter", "new code", "esc", "close"))
	} else {
		b.WriteString(hints("c or click", copyHint, "esc", "close"))
	}
	return boxed(b.String(), inner)
}

// renderRepair is the repair command screen: the command, then a live
// wait for the remote to go and come back.
func (m *addRemote) renderRepair(a *App, pad int) string {
	inner := popupInnerWidth(a.width)
	text := func(s string) string { return styles.FieldValueStyle().Width(inner).Render(s) }
	muted := func(s string) string { return styles.MutedStyle().Width(inner).Render(s) }
	var b strings.Builder
	b.WriteString(popupTitle("Repair "+m.host) + "\n\n")
	b.WriteString(text("Run this on "+m.host+" (it uses sudo). It reinstalls charm-bak's daemon there and its service, as Add Remote did, and keeps everything else: sources, settings, run history, the repository and its keys, and the pairing.") + "\n\n")
	cmd := m.res.Command
	if cmd == "" {
		cmd = " "
	}
	b.WriteString(styles.FileTitleStyle().Width(inner).Render(cmd) + "\n\n")
	b.WriteString(muted("For when "+m.host+"'s daemon won't start or an update went wrong. Nothing to do on this Mac.") + "\n\n")
	expired := m.expired(a.now())
	spin := styles.HealthStyle(styles.Warn).Render(a.spinFrame()) + " "
	switch {
	case m.err != nil:
		b.WriteString(errorLine(m.err) + "\n")
	case m.asking:
		b.WriteString(spin + styles.MutedStyle().Render("starting…") + "\n")
	case expired:
		b.WriteString(styles.HealthStyle(styles.Fail).Render("The code expired. Enter makes a new one.") + "\n")
	case m.sawDown:
		b.WriteString(spin + styles.MutedStyle().Render("waiting for "+m.host+" to answer… (the code works until "+m.res.Expires.Local().Format("15:04")+")") + "\n")
	default:
		b.WriteString(spin + styles.MutedStyle().Render("waiting for the command on "+m.host+"… (until "+m.res.Expires.Local().Format("15:04")+")") + "\n")
	}
	copyHint := "copy"
	if m.copied {
		copyHint = "copied"
	}
	b.WriteString(strings.Repeat("\n", max(pad, 0)))
	if expired || m.err != nil {
		b.WriteString(hints("enter", "new code", "esc", "close"))
	} else {
		b.WriteString(hints("c or click", copyHint, "esc", "close"))
	}
	return boxed(b.String(), inner)
}
