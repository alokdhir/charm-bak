package ui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// Background jobs and notifications, after charm-aws: the footer's left
// end says what's running (^T lists it), and ^N lists past notifications.
// The notifications themselves are the Mac's (the menu bar app posts
// them); the list here is rebuilt from each machine's run history, so it
// covers what happened while the TUI was closed.

// task is a job running on some machine.
type task struct {
	host     string
	job      protocol.Job
	progress *protocol.Progress
}

// tasks is every running job: this machine's, then the remotes' by name.
func (a *App) tasks() []task {
	var ts []task
	if a.job != nil {
		ts = append(ts, task{host: a.localHost(), job: *a.job, progress: a.progress})
	}
	hosts := make([]string, 0, len(a.remoteJobs))
	for h := range a.remoteJobs {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	for _, h := range hosts {
		rj := a.remoteJobs[h]
		ts = append(ts, task{host: h, job: rj.job, progress: rj.progress})
	}
	return ts
}

// hostSource is host's source id, if loaded.
func (a *App) hostSource(host, id string) (protocol.Source, bool) {
	for _, s := range a.sources {
		if s.ID == id && (s.Host == host || s.Host == "") {
			return s, true
		}
	}
	return protocol.Source{}, false
}

// taskTitle is what a job does: "backing up home", with "on pantry" for
// a remote's.
func (a *App) taskTitle(t task) string {
	text := jobKindText(t.job.Kind)
	if t.job.Source != nil {
		if s, ok := a.hostSource(t.host, *t.job.Source); ok {
			text += " " + s.Name
		}
	}
	if len(a.hosts) > 1 {
		text += " on " + t.host
	}
	return text
}

// taskProgress is a job's progress in words, "" when there's none.
func taskProgress(p *protocol.Progress) string {
	if p == nil || p.Percent <= 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d%%", int(p.Percent*100))}
	if p.TotalFiles > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d files", p.FilesDone, p.TotalFiles))
	}
	if p.TotalBytes > 0 {
		parts = append(parts, humanBytes(p.BytesDone)+" of "+humanBytes(p.TotalBytes))
	}
	return strings.Join(parts, " · ")
}

// taskIndicator is the footer's note on running jobs: the one job with
// its percent, or how many; "" when none.
func (a *App) taskIndicator() string {
	ts := a.tasks()
	switch len(ts) {
	case 0:
		return ""
	case 1:
		text := a.taskTitle(ts[0])
		if p := ts[0].progress; p != nil && p.Percent > 0 {
			text += fmt.Sprintf(" %d%%", int(p.Percent*100))
		}
		return a.spinFrame() + " " + text + " · ^T"
	}
	return a.spinFrame() + " " + pluralize(len(ts), "job") + " running · ^T"
}

// taskList is ^T: the running jobs, live (it reads a.tasks() each frame).
// Enter goes to a job's source; c cancels it, after asking.
type taskList struct{ cur int }

func (m *taskList) typing() bool { return false }

func (a *App) openTasks() tea.Cmd {
	if len(a.tasks()) == 0 {
		a.statusMsg = "Nothing is running"
		return nil
	}
	a.modal = &taskList{}
	return nil
}

func (m *taskList) update(a *App, msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	ts := a.tasks()
	m.cur = min(m.cur, max(len(ts)-1, 0))
	switch {
	case key.Matches(k, keyEsc), key.Matches(k, keyTasks):
		a.modal = nil
	case key.Matches(k, keyUp):
		m.cur = max(m.cur-1, 0)
	case key.Matches(k, keyDown):
		m.cur = min(m.cur+1, max(len(ts)-1, 0))
	case key.Matches(k, keyEnter) && m.cur < len(ts):
		t := ts[m.cur]
		if t.job.Source != nil {
			a.modal = nil
			return a.goToSource(t.host, *t.job.Source)
		}
	case k.String() == "c" && m.cur < len(ts):
		t := ts[m.cur]
		repo, title := a.repo.ForHost(t.host), a.taskTitle(t)
		if t.host == a.localHost() {
			repo = a.repo
		}
		a.modal = &confirm{
			title:   "Cancel",
			body:    "Stop " + title + "? What it had done so far is kept where restic keeps it (a backup's partial upload is reused next time).",
			answers: []string{"Stop it", "Keep going"},
			pick:    1,
			danger:  map[int]bool{0: true},
			act: func(a *App, answer int) tea.Cmd {
				if answer != 0 {
					a.modal = m
					return nil
				}
				id := t.job.ID
				return func() tea.Msg {
					if err := repo.Cancel(context.Background(), id); err != nil {
						return finderMsg{"Couldn't cancel: " + err.Error()}
					}
					return finderMsg{"Cancelling " + title}
				}
			},
		}
	}
	return nil
}

func (m *taskList) view(a *App) string {
	inner := popupInnerWidth(a.width)
	ts := a.tasks()
	var b strings.Builder
	b.WriteString(fitRow(popupTitle("Running"), styles.MutedStyle().Render(pluralize(len(ts), "job")), inner) + "\n\n")
	if len(ts) == 0 {
		b.WriteString(styles.MutedStyle().Render("Nothing is running now.") + "\n\n")
	}
	m.cur = min(m.cur, max(len(ts)-1, 0))
	for i, t := range ts {
		title := a.spinFrame() + " " + a.taskTitle(t)
		if i == m.cur {
			title = styles.SelectedStyle().Render(title)
		} else {
			title = styles.FieldValueStyle().Render(title)
		}
		b.WriteString(fitRow(title, styles.MutedStyle().Render("started "+agoText(a.now(), t.job.Started)), inner) + "\n")
		b.WriteString(styles.MutedStyle().Width(inner).MaxHeight(1).Render(taskProgress(t.progress)) + "\n\n")
	}
	keys := []string{"↑/↓", "move"}
	if m.cur < len(ts) && ts[m.cur].job.Source != nil {
		keys = append(keys, "enter", "go to its source")
	}
	if len(ts) > 0 {
		keys = append(keys, "c", "cancel")
	}
	b.WriteString(hints(append(keys, "esc", "close")...))
	return boxed(b.String(), inner)
}

// goToSource selects host's source id in the Sources column.
func (a *App) goToSource(host, id string) tea.Cmd {
	for _, s := range a.sources {
		if s.ID == id && s.Host == host {
			k := sourceKey(s)
			delete(a.collapsed, host)
			a.col, a.area = colSources, areaColumns
			if i := a.rowIndex(k); i >= 0 {
				a.srcCur.pos = i
				a.srcCur.move(0, len(a.srcRows()), a.listHeight())
				if a.snapsFor == k {
					return nil // already showing its snapshots
				}
				// As moving the cursor there would: the snapshots and
				// files on screen belong to the source that was selected.
				a.snaps, a.snapsFor, a.levels = nil, "", nil
				a.snapCur = cursor{}
				return a.loadSnapshots()
			}
		}
	}
	return nil
}

// A notification as the Mac showed it: a failed run, or a finished
// rewrite (Notifier.swift's rules), rebuilt from history.
type notice struct {
	host string
	run  protocol.Run
}

// noticeDays is how far back ^N looks.
const noticeDays = 30

// noticeLimit is the most ^N lists.
const noticeLimit = 100

// noticeList is ^N: past notifications, newest first.
type noticeList struct {
	loading bool
	list    []notice
	errs    []string // machines whose history didn't load
	cur     int
	top     int
}

type noticesMsg struct {
	list []notice
	errs []string
}

func (m *noticeList) typing() bool { return false }

func (a *App) openNotices() tea.Cmd {
	m := &noticeList{loading: true}
	a.modal = m
	since := a.now().Add(-noticeDays * 24 * time.Hour)
	type src struct {
		host string
		repo interface {
			History(context.Context, time.Time) ([]protocol.Run, error)
		}
	}
	var srcs []src
	for _, h := range a.hosts {
		if h.Local {
			srcs = append(srcs, src{h.Name, a.repo})
		} else if h.State == protocol.HostOK {
			srcs = append(srcs, src{h.Name, a.repo.ForHost(h.Name)})
		}
	}
	if len(srcs) == 0 {
		srcs = append(srcs, src{a.localHost(), a.repo})
	}
	return func() tea.Msg {
		var (
			mu   sync.Mutex
			wg   sync.WaitGroup
			out  noticesMsg
			ctx  = context.Background()
			keep = func(r protocol.Run) bool {
				return r.Status == protocol.RunFailed || (r.Kind == protocol.KindRewrite && r.Status == protocol.RunOK)
			}
		)
		for _, s := range srcs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runs, err := s.repo.History(ctx, since)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					out.errs = append(out.errs, s.host)
					return
				}
				for _, r := range runs {
					if keep(r) && r.Kind != protocol.KindRestore {
						out.list = append(out.list, notice{s.host, r})
					}
				}
			}()
		}
		wg.Wait()
		slices.SortFunc(out.list, func(x, y notice) int { return y.run.Ended.Compare(x.run.Ended) })
		if len(out.list) > noticeLimit {
			out.list = out.list[:noticeLimit]
		}
		slices.Sort(out.errs)
		return out
	}
}

// noticeTitle is the notification's title, as Notifier.swift words it.
func (a *App) noticeTitle(n notice) string {
	on := ""
	if len(a.hosts) > 1 {
		on = " on " + n.host
	}
	name := ""
	if n.run.Source != nil {
		name = *n.run.Source
		if s, ok := a.hostSource(n.host, *n.run.Source); ok {
			name = s.Name
		}
	}
	if n.run.Kind == protocol.KindRewrite && n.run.Status == protocol.RunOK {
		return "Past snapshots of " + name + on + " cleaned up"
	}
	switch {
	case n.run.Kind == protocol.KindBackup && name != "":
		return "Backup of " + name + on + " failed"
	case n.run.Kind == protocol.KindRewrite:
		return "Removing from past snapshots of " + name + on + " failed"
	case n.run.Kind == protocol.KindPrune:
		return "Repository cleanup" + on + " failed"
	case n.run.Kind == protocol.KindCheck:
		return "Repository check" + on + " found a problem"
	case n.run.Kind == protocol.KindState:
		return "Backup of charm-bak's own settings" + on + " failed"
	case name != "":
		return strings.ToUpper(n.run.Kind[:1]) + n.run.Kind[1:] + " of " + name + on + " failed"
	}
	return strings.ToUpper(n.run.Kind[:1]) + n.run.Kind[1:] + on + " failed"
}

func (m *noticeList) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case noticesMsg:
		m.loading, m.list, m.errs = false, msg.list, msg.errs
		return nil
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, keyEsc), key.Matches(msg, keyNotices):
			a.modal = nil
		case key.Matches(msg, keyUp):
			m.cur = max(m.cur-1, 0)
		case key.Matches(msg, keyDown):
			m.cur = min(m.cur+1, max(len(m.list)-1, 0))
		case key.Matches(msg, keyEnter) && m.cur < len(m.list):
			n := m.list[m.cur]
			if n.run.Source != nil {
				a.modal = nil
				return a.goToSource(n.host, *n.run.Source)
			}
		}
	}
	return nil
}

// noticeRows is how many rows one notification takes: title, two lines
// of detail, a blank.
const noticeRows = 4

func (m *noticeList) view(a *App) string {
	inner := popupInnerWidth(a.width)
	// Room for the title (2), the hints (1), the border (2), and the
	// note on machines that didn't answer (2).
	fits := max((a.modalHeight()-7)/noticeRows, 1)
	m.cur = min(m.cur, max(len(m.list)-1, 0))
	if m.cur < m.top {
		m.top = m.cur
	}
	if m.cur >= m.top+fits {
		m.top = m.cur - fits + 1
	}
	var b strings.Builder
	count := ""
	if !m.loading {
		count = pluralize(len(m.list), "notification") + fmt.Sprintf(", last %d days", noticeDays)
	}
	b.WriteString(fitRow(popupTitle("Notifications"), styles.MutedStyle().Render(count), inner) + "\n\n")
	body := 0
	switch {
	case m.loading:
		b.WriteString(styles.MutedStyle().Render(a.spinFrame()+" reading each machine's history…") + "\n\n")
		body = 2
	case len(m.list) == 0:
		b.WriteString(styles.MutedStyle().Width(inner).Render(fmt.Sprintf("No failures in the last %d days.", noticeDays)) + "\n\n")
		body = 2
	}
	for i := m.top; i < len(m.list) && i < m.top+fits; i++ {
		n := m.list[i]
		mark := styles.HealthStyle(styles.Fail).Render("✗ ")
		if n.run.Status == protocol.RunOK {
			mark = styles.HealthStyle(styles.OK).Render("✓ ")
		}
		title := a.noticeTitle(n)
		if i == m.cur {
			title = styles.SelectedStyle().Render(title)
		} else {
			title = styles.FieldValueStyle().Render(title)
		}
		b.WriteString(fitRow(mark+title, styles.MutedStyle().Render(agoText(a.now(), n.run.Ended)), inner) + "\n")
		detail := n.run.Error
		if n.run.Status == protocol.RunOK {
			detail = "Removed from them, and the space freed."
		}
		d := styles.MutedStyle().Width(inner - 2).Render(detail)
		lines := strings.Split(d, "\n")
		for len(lines) < 2 {
			lines = append(lines, "")
		}
		b.WriteString("  " + lines[0] + "\n  " + lines[1] + "\n\n")
		body += noticeRows
	}
	if len(m.errs) > 0 {
		b.WriteString(styles.HealthStyle(styles.Warn).Width(inner).MaxHeight(1).Render("Not answering, so not listed: "+strings.Join(m.errs, ", ")) + "\n\n")
	}
	keys := []string{"↑/↓", "move"}
	if m.cur < len(m.list) && m.list[m.cur].run.Source != nil {
		keys = append(keys, "enter", "go to its source")
	}
	b.WriteString(hints(append(keys, "esc", "close")...))
	return boxed(b.String(), inner)
}
