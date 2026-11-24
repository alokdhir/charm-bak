package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// machinePopup is Edit on a machine's header: the defaults its sources
// inherit unless they set their own (schedule, retention, skip
// patterns), and, for a remote, an Address tab: where it's reached, and
// Repair. Its repository's details are the header's hero and info popup
// (Alok, 2026-11-24: a popup of read-only details on its own key was a
// dead end). Tab switches tabs.
type machinePopup struct {
	tab     int // mpDefaults or mpAddress
	loading bool
	loadErr error // the defaults couldn't be read: the tab says why, nothing to edit
	err     error

	host   string
	remote bool
	repo   backup.Repo // the host's daemon

	// Defaults tab.
	field    int // mpSchedule, mpKeep, mpCount0+i (custom), mpSkip
	schedule int // index into defaultScheduleChoices
	keep     *keepEditor
	skip     *patternList // patterns every source skips
	saving   bool

	// Address tab (remotes).
	addr *hostForm
}

const (
	mpDefaults = iota
	mpAddress
)

// defaultScheduleChoices are scheduleChoices without "the default".
func defaultScheduleChoices() []int64 { return scheduleChoices[1:] }

// Both carry the machine they're about: Esc is allowed while one is on
// its way, so it can arrive with another machine's popup open.
type repoLoadedMsg struct {
	host string
	res  protocol.RepositoryResult
	err  error
}

type defaultsSavedMsg struct {
	host string
	d    protocol.Defaults
	err  error
}

// startMachine opens host's popup at tab.
func (a *App) startMachine(host string, tab int) tea.Cmd {
	h := a.findHost(host)
	p := &machinePopup{loading: true, host: host, remote: !h.Local && a.isRemote(host), repo: a.repo.ForHost(host), skip: newPatternList(nil, "")}
	if p.remote {
		p.addr = &hostForm{host: host, addr: newTextField(h.Address, "pantry.local or 192.168.1.10")}
		if tab == mpAddress {
			p.tab = mpAddress
		}
	}
	a.modal = p
	repo := p.repo
	return tea.Batch(p.focus(), func() tea.Msg {
		res, err := repo.Repository(context.Background())
		return repoLoadedMsg{host, res, err}
	})
}

// startMachineEdit is Edit on the selected header.
func (a *App) startMachineEdit() tea.Cmd {
	h, ok := a.selHost()
	if !ok {
		return nil
	}
	return a.startMachine(h.Name, mpDefaults)
}

// startHostEdit opens the selected remote's Address tab.
func (a *App) startHostEdit() tea.Cmd {
	r, ok := a.selRow()
	if !ok || !a.isRemote(r.host) {
		return nil
	}
	return a.startMachine(r.host, mpAddress)
}

func (p *machinePopup) setDefaults(d protocol.Defaults) {
	every := int64(0)
	if d.Schedule != nil {
		every = d.Schedule.Every
	}
	choices := defaultScheduleChoices()
	p.schedule = slices.Index(choices, every)
	if p.schedule < 0 { // an interval the list doesn't have: keep it
		scheduleChoices = append(scheduleChoices, every)
		p.schedule = len(defaultScheduleChoices()) - 1
	}
	p.keep = newKeepEditor(d.Retention, protocol.Retention{}, false, false)
	p.skip = newPatternList(d.Excludes, "add a pattern, e.g. *.tmp")
}

// The Defaults tab's fields; the six counts only while Keep is custom.
const (
	mpSchedule = 0
	mpKeep     = 1
	mpCount0   = 2 // ... mpCount0+5
	mpSkip     = mpCount0 + 6
)

func (p *machinePopup) fields() []int {
	fs := []int{mpSchedule, mpKeep}
	if p.keep != nil && p.keep.custom() {
		for i := range 6 {
			fs = append(fs, mpCount0+i)
		}
	}
	return append(fs, mpSkip)
}

// count is the focused custom count's input, nil elsewhere.
func (p *machinePopup) count() *textinput.Model {
	if p.tab == mpDefaults && p.keep != nil && p.field >= mpCount0 && p.field < mpSkip {
		return &p.keep.counts[p.field-mpCount0]
	}
	return nil
}

func (p *machinePopup) typing() bool {
	switch {
	case p.tab == mpAddress:
		return p.addr.typing()
	case p.field == mpSkip:
		return p.skip.typing()
	}
	return p.count() != nil
}

func (p *machinePopup) focus() tea.Cmd {
	if p.keep != nil {
		for i := range p.keep.counts {
			p.keep.counts[i].Blur()
		}
	}
	if p.addr != nil {
		p.addr.addr.Blur()
	}
	if p.field != mpSkip || p.tab != mpDefaults {
		p.skip.blur()
	}
	switch {
	case p.tab == mpAddress && !p.addr.onRepair:
		return p.addr.addr.Focus()
	case p.count() != nil:
		in := p.count()
		in.CursorEnd()
		return in.Focus()
	}
	return nil
}

// move goes delta fields along, entering the skip list from the side
// it's coming from.
func (p *machinePopup) move(delta int) tea.Cmd {
	fs := p.fields()
	i := slices.Index(fs, p.field)
	p.field = fs[min(max(i+delta, 0), len(fs)-1)]
	cmd := p.focus()
	if p.field == mpSkip {
		return p.skip.focus(delta > 0)
	}
	return cmd
}

func (p *machinePopup) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case repoLoadedMsg:
		if msg.host != p.host || !p.loading {
			return nil // another machine's, asked for by a popup since closed
		}
		p.loading, p.loadErr = false, msg.err
		if msg.err == nil {
			p.setDefaults(msg.res.Defaults)
		}
		return p.focus()
	case defaultsSavedMsg:
		if msg.host != p.host || !p.saving {
			return nil
		}
		p.saving = false
		if msg.err != nil {
			p.err = msg.err
			return nil
		}
		a.modal = nil
		a.statusMsg = "Defaults saved"
		return a.loadSources()
	case hostSetMsg:
		if p.addr == nil || msg.name != p.host {
			return nil
		}
		return p.addr.update(a, msg)
	case tea.KeyPressMsg:
		if key.Matches(msg, keyNext, keyPrev) && p.remote && !p.saving && !p.addr.sending {
			p.tab = 1 - p.tab
			p.err = nil
			return p.focus()
		}
		if p.tab == mpAddress {
			return p.addr.update(a, msg)
		}
		if p.saving || p.loading || p.keep == nil {
			// Nothing to edit: still asking, saving, or the defaults
			// couldn't be read (the tab says why).
			if key.Matches(msg, keyEsc) {
				a.modal = nil
			}
			return nil
		}
		return p.key(a, msg)
	}
	return nil
}

func (p *machinePopup) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	if p.field == mpSkip {
		if cmd, used := p.skip.key(msg); used {
			p.err = nil
			return cmd
		}
	}
	switch {
	case key.Matches(msg, keyEsc):
		a.modal = nil
		return nil
	case msg.String() == "down":
		return p.move(1)
	case msg.String() == "up":
		return p.move(-1)
	case key.Matches(msg, keyEnter):
		return p.save(a)
	}
	if p.field == mpKeep {
		switch msg.String() {
		case "left", "h":
			p.keep.cycle(-1)
		case "right", "l", "space":
			p.keep.cycle(1)
		}
		return nil
	}
	if p.field == mpSchedule {
		n := len(defaultScheduleChoices())
		switch msg.String() {
		case "left", "h":
			p.schedule = (p.schedule + n - 1) % n
		case "right", "l", "space":
			p.schedule = (p.schedule + 1) % n
		}
		return nil
	}
	if p.field == mpSkip {
		return nil
	}
	if len(msg.Text) > 0 && strings.Trim(msg.Text, "0123456789") != "" {
		return nil // counts are digits only
	}
	in := p.count()
	if in == nil {
		return nil
	}
	var cmd tea.Cmd
	*in, cmd = in.Update(msg)
	p.err = nil
	return cmd
}

func (p *machinePopup) save(a *App) tea.Cmd {
	if !a.hostUp(p.host, "Changing its defaults") {
		return nil
	}
	r, _, err := p.keep.retention()
	if err != nil {
		p.err = err
		return nil
	}
	every := defaultScheduleChoices()[p.schedule]
	skip := p.skip.values()
	params := protocol.DefaultsSetParams{Schedule: &protocol.Schedule{Every: every}, Retention: &r, Excludes: &skip}
	p.saving, p.err = true, nil
	repo, host := p.repo, p.host
	return func() tea.Msg {
		d, err := repo.SetDefaults(context.Background(), params)
		return defaultsSavedMsg{host, d, err}
	}
}

// followers counts the host's sources that inherit field.
func (a *App) followers(host, field string) int {
	n := 0
	for _, s := range a.sources {
		if (s.Host == host || s.Host == "") && slices.Contains(s.Inherited, field) {
			n++
		}
	}
	return n
}

// view keeps the same height on both tabs, so tabbing between them never
// reshapes or moves the popup: the shorter tab is padded above its status
// line and hints. Heights are measured wrapped at the popup's width.
func (p *machinePopup) view(a *App) string {
	inner := popupInnerWidth(a.width)
	height := func(tab int) int {
		return lipgloss.Height(lipgloss.NewStyle().Width(inner).Render(p.body(a, tab) + p.footer(a, tab)))
	}
	body := p.body(a, p.tab)
	if p.remote {
		if pad := height(1-p.tab) - height(p.tab); pad > 0 {
			body += strings.Repeat("\n", pad)
		}
	}
	return boxed(body+p.footer(a, p.tab), inner)
}

// footer is the status line and key hints under a tab's body.
func (p *machinePopup) footer(a *App, tab int) string {
	if tab == mpAddress {
		return p.addr.footer()
	}
	var b strings.Builder
	b.WriteString("\n")
	switch {
	case p.saving:
		b.WriteString(styles.MutedStyle().Render("saving…") + "\n")
	case p.err != nil:
		b.WriteString(errorLine(p.err) + "\n")
	case p.keep == nil:
		b.WriteString("\n") // loading, or the load failed: the body says so
	default:
		b.WriteString(helpBlock(p.fieldHelp(a), popupInnerWidth(a.width)))
	}
	switch {
	case p.keep == nil && p.remote:
		b.WriteString(hints("tab", "address", "esc", "close"))
	case p.keep == nil:
		b.WriteString(hints("esc", "close"))
	case p.remote:
		b.WriteString(hints("tab", "address", "↑↓", "field", "←→", "change", "enter", "save", "esc", "cancel"))
	default:
		b.WriteString(hints("↑↓", "field", "←→", "change", "enter", "save", "esc", "cancel"))
	}
	return b.String()
}

// fieldHelp explains the focused Defaults field.
func (p *machinePopup) fieldHelp(a *App) string {
	switch {
	case p.field == mpSchedule:
		return fmt.Sprintf("How often sources back up unless they set their own; %s follow it.", pluralize(a.followers(p.host, "schedule"), "source"))
	case p.field == mpSkip:
		return p.skip.help()
	case p.field >= mpCount0:
		return "How many " + keepLabels[p.field-mpCount0] + " back to keep one from; 0 turns the line off."
	}
	return p.keep.help() + fmt.Sprintf(" %s follow it.", pluralize(a.followers(p.host, "retention"), "source"))
}

// body is a tab's heading and fields, everything above the status line and
// key hints.
func (p *machinePopup) body(a *App, tab int) string {
	inner := popupInnerWidth(a.width)
	vw := inner - formLabelWidth
	var b formBuilder
	head := popupTitle(p.host)
	if p.remote {
		tabs := []string{"Defaults", "Address"}
		for i, t := range tabs {
			if i == tab {
				tabs[i] = styles.StopStyle(true).Render(" " + t + " ")
			} else {
				tabs[i] = styles.MutedStyle().Render(" " + t + " ")
			}
		}
		head += "  " + strings.Join(tabs, " ")
	}
	b.WriteString(head + "\n\n")
	if tab == mpAddress {
		b.WriteString(p.addr.body(a, inner))
		return b.String()
	}

	row := func(focused bool, label, value string) {
		lab := styles.FieldLabelStyle()
		if focused {
			lab = styles.HotkeyStyle()
		}
		b.Row(lab.Render(label), value)
	}

	if p.loading {
		b.WriteString(styles.MutedStyle().Render("asking the daemon…") + "\n")
		return b.String()
	}
	if p.keep == nil {
		// The machine didn't answer with its defaults. For a remote the
		// Address tab still works: a changed address is the usual reason.
		why := "its daemon didn't answer"
		if p.loadErr != nil {
			why = strings.TrimSpace(p.loadErr.Error())
		}
		b.WriteString(styles.HealthStyle(styles.Fail).Width(inner).Render("Couldn't read "+p.host+"'s defaults: "+why) + "\n")
		if p.remote {
			b.WriteString(styles.MutedStyle().Width(inner).Render("If its address changed, tab goes to where it's set.") + "\n")
		}
		return b.String()
	}
	b.WriteString(styles.MutedStyle().Render("What "+p.host+"'s sources do unless they set their own.") + "\n\n")
	choice := func(focused bool, text string) string {
		if focused {
			return styles.StopStyle(true).Render("‹ " + text + " ›")
		}
		return styles.FieldValueStyle().Render(text)
	}
	row(p.field == mpSchedule, "Back up", choice(p.field == mpSchedule, a.scheduleText(defaultScheduleChoices()[p.schedule])))
	row(p.field == mpKeep, "Keep", choice(p.field == mpKeep, p.keep.text()))
	if p.keep.custom() {
		for i := range p.keep.counts {
			row(p.field == mpCount0+i, "  "+keepLabels[i], p.keep.countRow(i))
		}
	}
	// The skip patterns get the rows the popup has left: the heading and
	// its blank, the line above and its blank, Back up, Keep (and its
	// counts), then the footer's blank, help and hints.
	fixed := 2 + 2 + 2 + 1 + helpLines + 1
	if p.keep.custom() {
		fixed += len(p.keep.counts)
	}
	for i, v := range p.skip.rows(p.field == mpSkip, vw-1, max(a.modalHeight()-fixed, 1)) {
		label := ""
		if i == 0 {
			label = "Skip"
		}
		row(p.field == mpSkip, label, v)
	}
	return b.String()
}
