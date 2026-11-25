package ui

import (
	"context"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// The app's events keep the TUI live: a scheduled backup, one started
// from the menu or another TUI, a source changed elsewhere. The TUI
// subscribes once and re-subscribes if the app restarts.

type (
	eventsReadyMsg struct{ ch <-chan protocol.Event }
	eventMsg       struct {
		ch <-chan protocol.Event
		e  protocol.Event
	}
	eventsClosedMsg struct{}
)

// eventsRetry is how long to wait before re-subscribing after the app
// went away.
const eventsRetry = 3 * time.Second

func (a *App) subscribe() tea.Cmd {
	repo := a.repo
	return func() tea.Msg {
		ch, err := repo.Events(context.Background())
		if err != nil {
			return eventsClosedMsg{}
		}
		return eventsReadyMsg{ch}
	}
}

func waitEvent(ch <-chan protocol.Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{ch, e}
	}
}

func (a *App) loadStatus() tea.Cmd {
	repo := a.repo
	return func() tea.Msg { st, err := repo.Status(context.Background()); return statusMsg{st, err} }
}

// loadSources reads every host's sources: this machine's, then each
// remote's (answering or not), in the order the daemon lists hosts.
func (a *App) loadSources() tea.Cmd {
	repo := a.repo
	return func() tea.Msg {
		ctx := context.Background()
		hosts, err := repo.Hosts(ctx)
		if err != nil {
			return sourcesMsg{err: err}
		}
		var all []protocol.Source
		jobs := map[string]*protocol.Job{}
		statuses := map[string]protocol.StatusResult{}
		repos := map[string]protocol.RepositoryResult{}
		for i, h := range hosts {
			up := h.State == protocol.HostOK
			// A remote that isn't answering is asked too: the daemon keeps
			// a copy of each remote's sources and repository, so its
			// backups stay browsable (offline.go). With nothing saved it
			// says host_unreachable and only the header shows.
			s, err := repo.ForHost(h.Name).Sources(ctx)
			if err != nil {
				if i == 0 {
					return sourcesMsg{err: err}
				}
				if up {
					hosts[i].State, hosts[i].Error = protocol.HostUnreachable, err.Error()
				}
				continue
			}
			all = append(all, s...)
			// The repository and defaults, for the host's header.
			if r, err := repo.ForHost(h.Name).Repository(ctx); err == nil {
				repos[h.Name] = r
			}
			if i > 0 && up {
				// What a remote is running now: its events only say when
				// that changes, and this TUI may have opened mid-run.
				if st, err := repo.ForHost(h.Name).Status(ctx); err == nil {
					jobs[h.Name], statuses[h.Name] = st.Job, st
				}
			}
		}
		return sourcesMsg{hosts: hosts, sources: all, remoteJobs: jobs, hostStatus: statuses, repos: repos}
	}
}

// handleEvent applies one event and asks for whatever it made stale.
func (a *App) handleEvent(e protocol.Event) tea.Cmd {
	if e.Host != "" {
		return a.handleRemoteEvent(e)
	}
	switch e.Name {
	case protocol.EventProgress:
		if a.job != nil && e.Progress.JobID == a.job.ID {
			p := *e.Progress
			a.progress = &p
		}
		return nil
	case protocol.EventJobStarted:
		j := e.JobStarted.Job
		a.job, a.progress = &j, nil
		return a.loadStatus()
	case protocol.EventJobFinished:
		run := e.JobFinished.Run
		if a.job != nil && a.job.ID == run.Job {
			a.job, a.progress = nil, nil
		}
		a.statusMsg = a.runSummary(run)
		cmds := []tea.Cmd{a.loadStatus(), a.loadSources()}
		if run.Source != nil && a.selectedSourceID() == a.localHost()+"/"+*run.Source {
			cmds = append(cmds, a.loadSnapshots())
		}
		return tea.Batch(cmds...)
	case protocol.EventSourceChanged:
		return a.loadSources()
	case protocol.EventNoticeShow:
		return a.takeNotice()
	case protocol.EventAWSStep:
		if f, ok := a.modal.(*setupFlow); ok {
			f.awsStep(e.AWSStep)
		}
		return nil
	case protocol.EventStateChanged:
		a.job = e.StateChanged.Job
		if a.job == nil {
			a.progress = nil
		}
		return a.loadStatus()
	}
	return nil
}

// handleRemoteEvent applies an event a remote's daemon sent through
// ours. The hero's job and progress are this machine's, so a remote's
// jobs only refresh its sources and, when it's in view, its snapshots.
func (a *App) handleRemoteEvent(e protocol.Event) tea.Cmd {
	switch e.Name {
	case protocol.EventProgress:
		if j := a.remoteJobs[e.Host]; j != nil && j.job.ID == e.Progress.JobID {
			p := *e.Progress
			j.progress = &p
		}
		return nil
	case protocol.EventStateChanged:
		// Also sent when the remote's repository size is read: reload
		// to get its status.
		a.setRemoteJob(e.Host, e.StateChanged.Job)
		return a.loadSources()
	case protocol.EventJobStarted:
		j := e.JobStarted.Job
		a.setRemoteJob(e.Host, &j)
		return a.loadSources()
	case protocol.EventHostChanged, protocol.EventSourceChanged:
		return a.loadSources()
	case protocol.EventJobFinished:
		run := e.JobFinished.Run
		if j := a.remoteJobs[e.Host]; j != nil && j.job.ID == run.Job {
			delete(a.remoteJobs, e.Host)
		}
		if run.Status == protocol.RunFailed || run.Status == protocol.RunIncomplete {
			// A remote's failure says why in the footer too, like this
			// machine's; its red ✗ alone doesn't.
			a.statusMsg = e.Host + ": " + a.runSummary(run)
		}
		cmds := []tea.Cmd{a.loadSources()}
		if run.Source != nil && a.selectedSourceID() == e.Host+"/"+*run.Source {
			cmds = append(cmds, a.loadSnapshots())
		}
		return tea.Batch(cmds...)
	}
	return nil
}

// localHost is this machine's name, as its daemon reports it.
func (a *App) localHost() string {
	if len(a.hosts) > 0 {
		return a.hosts[0].Name
	}
	return ""
}

// sourceKey identifies a source across hosts: ids are only unique per
// machine.
func sourceKey(s protocol.Source) string { return s.Host + "/" + s.ID }

// runSummary is the footer's note on a finished job.
func (a *App) runSummary(run protocol.Run) string {
	what := run.Kind
	if run.Source != nil {
		if s, ok := a.sourceByID(*run.Source); ok {
			what += " of " + s.Name
		}
	}
	if w, ok := a.restoring[run.Job]; ok {
		delete(a.restoring, run.Job)
		what = "restoring " + w
		if run.Status == protocol.RunOK {
			return "Restored " + w
		}
	}
	switch run.Status {
	case protocol.RunOK:
		if run.Kind == protocol.KindBackup {
			return "Finished " + what + ": " + humanBytes(run.BytesAdded) + " new"
		}
		return "Finished " + what
	case protocol.RunCancelled:
		return "Cancelled " + what
	case protocol.RunIncomplete:
		return "Finished " + what + ", but some files couldn't be read"
	}
	return "Failed " + what + ": " + run.Error
}

func (a *App) sourceByID(id string) (protocol.Source, bool) {
	for _, s := range a.sources {
		if s.ID == id {
			return s, true
		}
	}
	return protocol.Source{}, false
}

func (a *App) selectedSourceID() string {
	s, ok := a.selSource()
	if !ok {
		return ""
	}
	return sourceKey(s)
}

// srcRepo is the Repo for the selected source's host: snapshots, trees,
// restores and changes to it go to the daemon that backs it up.
func (a *App) srcRepo() backup.Repo {
	r, ok := a.selRow()
	if !ok || r.host == "" {
		return a.repo
	}
	return a.repo.ForHost(r.host)
}

// jobFor is the running job's progress if it's backing up source id.
func (a *App) jobFor(s protocol.Source) (running bool, percent float64) {
	job, progress := a.job, a.progress
	if a.isRemote(s.Host) {
		job, progress = nil, nil
		if j := a.remoteJobs[s.Host]; j != nil {
			job, progress = &j.job, j.progress
		}
	}
	if job == nil || job.Source == nil || *job.Source != s.ID {
		return false, 0
	}
	if progress != nil {
		percent = progress.Percent
	}
	return true, percent
}

// remoteJob is what a remote is running, from its events.
type remoteJob struct {
	job      protocol.Job
	progress *protocol.Progress
}

// setRemoteJob records host's running job (nil: none), keeping the
// progress when it's the same job.
func (a *App) setRemoteJob(host string, j *protocol.Job) {
	if j == nil {
		delete(a.remoteJobs, host)
		return
	}
	if a.remoteJobs == nil {
		a.remoteJobs = map[string]*remoteJob{}
	}
	if cur := a.remoteJobs[host]; cur != nil && cur.job.ID == j.ID {
		return
	}
	a.remoteJobs[host] = &remoteJob{job: *j}
}

// sourceWithPath is a source that backs up exactly path, if any.
func (a *App) sourceWithPath(host, path string) (protocol.Source, bool) {
	for _, s := range a.sources {
		if (s.Host == host || s.Host == "" || host == "") && slices.Contains(s.Paths, path) {
			return s, true
		}
	}
	return protocol.Source{}, false
}
