package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"charm-bak/internal/protocol"
)

// Core answers protocol requests from the store and the engine, runs one
// job at a time, schedules backups and maintenance, and publishes events.
// It never builds a restic command; that's the Engine's job.
type Core struct {
	Version string
	// Name is this machine's host name, as remotes and the TUI know it.
	Name string
	// SecretDir is where repository_set keeps secrets in owner-only
	// files; empty where they live in the Keychain (the Mac).
	SecretDir string
	// Build is the SHA-256 of this daemon's executable (hello).
	Build string
	// UpdatePath is the executable self_update replaces, and Restart
	// shuts the daemon down so main can start the new one; both empty
	// where the daemon doesn't update itself (the Mac's ships in the app).
	UpdatePath string
	// UpdateBundle is the app bundle UpdatePath runs from on a Mac
	// remote: self_update then takes the whole bundle.
	UpdateBundle string
	Restart      func()
	// Uninstall shuts the daemon down for good and removes what the
	// pairing script installed (uninstall); nil where it wasn't (the Mac).
	Uninstall func()
	// accessCheck stands in for fullDiskAccess in tests.
	accessCheck func() *bool
	// awsRun runs the aws command for aws_keys; awsWait is how long it
	// waits between tries of a new key. Tests replace both.
	awsRun  awsRunFunc
	awsWait time.Duration
	// StateCopy is where the daily state backup copies the database
	// before backing the copy up (statebackup.go); empty: no state backup.
	StateCopy  string
	update     selfUpdate
	goos       string      // tests: the OS hello reports
	secrets    secretStore // tests; nil = the Keychain
	store      *Store
	makeEngine func(RepositoryConfig) Engine
	now        func() time.Time

	mu       sync.Mutex
	paused   bool
	stopping bool // Shutdown is cancelling the running job
	held     bool // an update is installed: nothing starts before the restart
	// lastTry is when a source's backup last ended without its run
	// being recorded (the database refused it): the scheduler waits a
	// full interval from then, as it would from a recorded run.
	lastTry   map[string]time.Time
	queue     []*pendingJob
	current   *pendingJob
	keepCache map[string]map[string][]string // by source tag
	snapLists map[string]snapList            // by source tag; cache.go
	snapGen   int                            // bumped when snapLists is dropped
	recalcing sync.Map                       // source id → recalcAdded running (rewrite.go)
	trees     treeCache
	stats     *RepoStats // nil until first read; cache.go
	statsBusy bool
	statsMore bool // asked again while busy: read once more after
	subs      map[chan protocol.Event]struct{}
	identity  *Identity // set when remotes are in play; peers.go
	remotes   remotes
	views     map[string]remoteView // how to open each remote's repository; remoteread.go

	// Pairing (pairing.go).
	pair             pairing
	remoteDaemons    string
	pairPortOverride int // tests
	idle             *sync.Cond
}

type pendingJob struct {
	job    protocol.Job
	work   func(ctx context.Context, job protocol.Job) outcome
	cancel context.CancelFunc
}

// outcome is what a job reports when it ends.
type outcome struct {
	status                 string
	filesNew, filesChanged int64
	bytesAdded             int64
	files, bytes           int64
	snapshot               string
	err                    string
}

// Maintenance cadence and how much data the check reads back.
const (
	maintenanceEvery = 7 * 24 * time.Hour
	// After a cleanup that failed or was cancelled, the next try.
	maintenanceRetry = time.Hour
	// Cleanup locks out every read while it runs, so it waits for the
	// night (local time), unless it's this long overdue: a machine
	// that's off at night still gets cleaned.
	maintenanceFrom, maintenanceUntil = 1, 5 // 01:00 to 05:00
	maintenanceOverdue                = 24 * time.Hour
	checkReadPercent                  = 5
)

// NewCore wires a core to its store. makeEngine builds an engine for the
// configured repository; now is injectable for tests.
func NewCore(version string, store *Store, makeEngine func(RepositoryConfig) Engine, now func() time.Time) *Core {
	if now == nil {
		now = time.Now
	}
	c := &Core{Version: version, Name: HostName(), store: store, makeEngine: makeEngine, now: now,
		keepCache: map[string]map[string][]string{}, snapLists: map[string]snapList{}, subs: map[chan protocol.Event]struct{}{}}
	c.idle = sync.NewCond(&c.mu)
	// Paused stays paused across restarts (a self-update is one).
	if store != nil {
		store.Setting("paused", &c.paused)
	}
	return c
}

// ResticEngines builds ResticEngines with Keychain secrets.
func ResticEngines(r RepositoryConfig) Engine {
	return &ResticEngine{Binary: FindRestic(), Repo: r, Secrets: Secrets}
}

// Events.

// Subscribe returns a channel of events until unsubscribe is called.
// Slow subscribers miss events rather than stall the daemon.
func (c *Core) Subscribe() (events <-chan protocol.Event, unsubscribe func()) {
	ch := make(chan protocol.Event, 256)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if _, ok := c.subs[ch]; ok {
			delete(c.subs, ch)
			close(ch)
		}
		c.mu.Unlock()
	}
}

// publish sends an event to every subscriber. Call with c.mu held.
func (c *Core) publish(e protocol.Event) {
	for ch := range c.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// State.

func (c *Core) stateLocked() string {
	if c.current != nil {
		if c.current.job.Kind == protocol.KindPrune {
			return protocol.StatePruning
		}
		return protocol.StateRunning
	}
	if c.paused {
		return protocol.StatePaused
	}
	return protocol.StateIdle
}

func (c *Core) currentJobLocked() *protocol.Job {
	if c.current == nil {
		return nil
	}
	j := c.current.job
	return &j
}

func (c *Core) SetPaused(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused == on {
		return
	}
	c.paused = on
	if err := c.store.SetSetting("paused", on); err != nil {
		logf("saving pause: %v", err)
	}
	logf("paused: %v", on)
	c.publish(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: c.stateLocked(), Job: c.currentJobLocked()}})
}

// Jobs.

func newJobID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "j-" + hex.EncodeToString(b)
}

func (c *Core) enqueue(kind string, source *string, work func(context.Context, protocol.Job) outcome) protocol.Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	j := protocol.Job{ID: newJobID(), Kind: kind, Source: source, Started: c.now()}
	c.queue = append(c.queue, &pendingJob{job: j, work: work})
	c.runNextLocked()
	return j
}

func (c *Core) runNextLocked() {
	if c.current != nil || len(c.queue) == 0 || c.held {
		return
	}
	p := c.queue[0]
	c.queue = c.queue[1:]
	p.job.Started = c.now()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	c.current = p
	job := p.job
	logf("job %s started: %s", job.ID, c.jobWhat(job))
	c.publish(protocol.Event{Name: protocol.EventJobStarted, JobStarted: &protocol.JobStartedEvent{Job: job}})
	c.publish(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: c.stateLocked(), Job: &job}})
	go func() {
		o := p.work(ctx, job)
		if ctx.Err() != nil && o.status == protocol.RunFailed {
			// Cancelled; what it had already made (a backup cancelled
			// during retention has its snapshot) stays on the record.
			o.status, o.err = protocol.RunCancelled, "Cancelled."
			if o.snapshot != "" {
				o.err = "Cancelled after the snapshot was made, before old ones were cleaned up."
			}
		}
		cancel()
		c.finish(job, o)
	}()
}

func (c *Core) finish(job protocol.Job, o outcome) {
	run := protocol.Run{Job: job.ID, Kind: job.Kind, Source: job.Source, Started: job.Started, Ended: c.now(),
		Status: o.status, FilesNew: o.filesNew, FilesChanged: o.filesChanged, BytesAdded: o.bytesAdded, Files: o.files, Bytes: o.bytes,
		Snapshot: o.snapshot, Error: o.err}
	c.mu.Lock()
	// A backup cut short by the daemon stopping (the app quitting, an
	// update) leaves no run, like the queued jobs dropped with it: it's
	// due again at the next start, not a whole interval later.
	dropped := c.stopping && job.Kind == protocol.KindBackup && o.status == protocol.RunCancelled && o.snapshot == ""
	c.mu.Unlock()
	if !dropped {
		if err := c.store.AddRun(run); err != nil {
			// Unrecorded, the source would look due again every minute.
			logf("job %s: recording the run: %v", job.ID, err)
			if job.Source != nil {
				c.mu.Lock()
				if c.lastTry == nil {
					c.lastTry = map[string]time.Time{}
				}
				c.lastTry[*job.Source] = run.Ended
				c.mu.Unlock()
			}
		} else if job.Source != nil {
			c.mu.Lock()
			delete(c.lastTry, *job.Source)
			c.mu.Unlock()
		}
	}
	line := fmt.Sprintf("job %s %s: %s, took %s", job.ID, run.Status, c.jobWhat(job), run.Ended.Sub(run.Started).Round(time.Second))
	if job.Kind == protocol.KindBackup && run.Status != protocol.RunFailed {
		line += fmt.Sprintf(", %d new and %d changed files, %d bytes added, snapshot %s", run.FilesNew, run.FilesChanged, run.BytesAdded, run.Snapshot)
	}
	if run.Error != "" {
		line += ": " + run.Error
	}
	logf("%s", line)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = nil
	c.dropSnapshotListsLocked()
	c.publish(protocol.Event{Name: protocol.EventJobFinished, JobFinished: &protocol.JobFinishedEvent{Run: run}})
	if job.Source != nil {
		if src, ok, _ := c.store.Source(*job.Source); ok {
			if view, err := c.sourceViewLocked(src); err == nil {
				c.publish(protocol.Event{Name: protocol.EventSourceChanged, SourceChanged: &protocol.SourceChangedEvent{Source: view}})
			}
		}
	}
	c.runNextLocked()
	if c.current == nil {
		c.publish(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: c.stateLocked()}})
		c.idle.Broadcast()
		if job.Kind != protocol.KindRestore { // restores don't change the repository
			c.refreshStatsLocked()
		}
	}
}

// WaitIdle blocks until no job runs or waits (tests, clean shutdown).
func (c *Core) WaitIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.current != nil || len(c.queue) > 0 {
		c.idle.Wait()
	}
}

// Shutdown cancels the running job and drops the queue.
func (c *Core) Shutdown() {
	c.mu.Lock()
	c.queue = nil
	c.stopping = true
	if c.current != nil {
		c.current.cancel()
	}
	c.mu.Unlock()
	c.WaitIdle()
	c.mu.Lock()
	c.stopping = false
	c.mu.Unlock()
}

func (c *Core) cancelJob(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i := slices.IndexFunc(c.queue, func(p *pendingJob) bool { return p.job.ID == id }); i >= 0 {
		j := c.queue[i].job
		c.queue = slices.Delete(c.queue, i, i+1)
		run := protocol.Run{Job: j.ID, Kind: j.Kind, Source: j.Source, Started: j.Started, Ended: c.now(),
			Status: protocol.RunCancelled, Error: "Cancelled before it started."}
		if err := c.store.AddRun(run); err != nil {
			logf("job %s: recording the run: %v", j.ID, err)
		}
		c.publish(protocol.Event{Name: protocol.EventJobFinished, JobFinished: &protocol.JobFinishedEvent{Run: run}})
		return nil
	}
	if c.current != nil && c.current.job.ID == id {
		c.current.cancel()
		return nil
	}
	return perr(protocol.ErrNotFound, "No job %s is running or waiting.", id)
}

func (c *Core) busyLocked(source string) bool {
	if c.current != nil && c.current.job.Source != nil && *c.current.job.Source == source {
		return true
	}
	return slices.ContainsFunc(c.queue, func(p *pendingJob) bool { return p.job.Source != nil && *p.job.Source == source })
}

// Tick queues whatever is due: called every minute and on wake. Backups
// run when a source's next run has passed (never-run ones at once);
// maintenance (prune, then a partial check) once a week when nothing else
// waits. Pausing stops all of it; "back up now" still works.
func (c *Core) Tick() {
	c.expireUpdate(c.now())
	engine, err := c.engine()
	if err != nil {
		return
	}
	c.mu.Lock()
	if c.paused {
		c.mu.Unlock()
		return
	}
	sources, _ := c.store.Sources()
	now := c.now()
	var due []StoredSource
	for _, s := range sources {
		if s.Paused || c.busyLocked(s.ID) {
			continue
		}
		if view, err := c.sourceViewLocked(s); err == nil && view.NextRun != nil && !view.NextRun.After(now) {
			due = append(due, s)
		}
	}
	c.mu.Unlock()
	for _, s := range due {
		c.enqueueBackup(s)
	}
	c.stateBackupIfDue(engine, now)

	c.mu.Lock()
	quiet := c.current == nil && len(c.queue) == 0
	c.mu.Unlock()
	if !quiet {
		return
	}
	if !c.maintenanceDue(now) {
		return
	}
	c.enqueue(protocol.KindPrune, nil, func(ctx context.Context, _ protocol.Job) outcome {
		return result(engine.Maintain(ctx))
	})
	c.enqueue(protocol.KindCheck, nil, func(ctx context.Context, _ protocol.Job) outcome {
		return result(engine.Check(ctx, checkReadPercent))
	})
}

// maintenanceDue reports whether the weekly cleanup should start now: a
// week after the last one that succeeded (a failed or cancelled one is
// tried again after maintenanceRetry, not a week later), and then at
// night, or at any hour once it's a day overdue. A repository never
// cleaned has nothing to wait for.
func (c *Core) maintenanceDue(now time.Time) bool {
	last, err := c.store.LastRepoRun(protocol.KindPrune)
	if err != nil {
		return false
	}
	if last == nil {
		return true
	}
	if last.Status != protocol.RunOK && now.Sub(last.Ended) < maintenanceRetry {
		return false
	}
	// Due a week after the last good one; with none, since the first try.
	due := last.Ended
	if ok, err := c.store.LastOKRepoRun(protocol.KindPrune); err != nil {
		return false
	} else if ok != nil {
		due = ok.Ended.Add(maintenanceEvery)
	}
	if now.Before(due) {
		return false
	}
	if h := now.Local().Hour(); h >= maintenanceFrom && h < maintenanceUntil {
		return true
	}
	return now.Sub(due) > maintenanceOverdue
}

func result(err error) outcome {
	if err != nil {
		var ee *EngineError
		if errors.As(err, &ee) && ee.Kind == ErrCancelled {
			return outcome{status: protocol.RunCancelled, err: "Cancelled."}
		}
		return outcome{status: protocol.RunFailed, err: err.Error()}
	}
	return outcome{status: protocol.RunOK}
}

// The engine.

func (c *Core) engine() (Engine, error) {
	r, err := c.store.Repository()
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, perr(protocol.ErrNotConfigured, "No repository is set up yet.")
	}
	return c.makeEngine(*r), nil
}

// readable is the engine for reads, which can't run while a prune holds
// the repository's exclusive lock.
func (c *Core) readable() (Engine, error) {
	c.mu.Lock()
	cur := c.currentJobLocked()
	c.mu.Unlock()
	if cur != nil && (cur.Kind == protocol.KindPrune || cur.Kind == protocol.KindRewrite) {
		return nil, perrData(protocol.ErrBusy, protocol.BusyData{Job: *cur},
			"The repository is being cleaned up (started %s); browsing works again when it finishes.", cur.Started.Local().Format("15:04"))
	}
	return c.engine()
}

// enqueueBackup backs up a source, applies its retention and refreshes
// its keep reasons.
func (c *Core) enqueueBackup(s StoredSource) protocol.Job {
	id := s.ID
	return c.enqueue(protocol.KindBackup, &id, func(ctx context.Context, job protocol.Job) outcome {
		engine, err := c.engine()
		if err != nil {
			return outcome{status: protocol.RunFailed, err: err.Error()}
		}
		eff, err := c.store.Effective(s)
		if err != nil {
			return outcome{status: protocol.RunFailed, err: err.Error()}
		}
		d, err := c.store.Defaults()
		if err != nil {
			return outcome{status: protocol.RunFailed, err: err.Error()}
		}
		excludes := append(slices.Clone(d.Excludes), s.Excludes...)
		res, err := engine.Backup(ctx, BackupSpec{Tag: s.Tag(), Paths: s.Paths, Excludes: excludes}, func(p Progress) {
			c.mu.Lock()
			c.publish(protocol.Event{Name: protocol.EventProgress, Progress: &protocol.Progress{JobID: job.ID, Percent: p.Percent,
				FilesDone: p.FilesDone, TotalFiles: p.TotalFiles, BytesDone: p.BytesDone, TotalBytes: p.TotalBytes, Current: p.Current}})
			c.mu.Unlock()
		})
		if err != nil {
			return result(err)
		}
		pins, pinErr := c.store.Pinned(s.ID)
		o := outcome{status: protocol.RunOK, filesNew: res.FilesNew, filesChanged: res.FilesChanged, bytesAdded: res.BytesAdded, files: res.Files, bytes: res.Bytes, snapshot: res.Snapshot}
		if res.Incomplete {
			o.status, o.err = protocol.RunIncomplete, "Some files couldn't be read."
		}
		if pinErr != nil {
			// Without the pins, retention could forget a pinned snapshot.
			o.status, o.err = protocol.RunFailed, "Backed up, but old snapshots weren't cleaned up: couldn't read the pins: "+pinErr.Error()
			return o
		}
		if err := engine.ApplyRetention(ctx, s.Tag(), eff.Retention, pins); err != nil {
			o.status, o.err = protocol.RunFailed, "Backed up, but applying retention failed: "+err.Error()
			return o
		}
		if reasons, err := engine.KeepReasons(ctx, s.Tag(), eff.Retention, pins); err == nil {
			c.mu.Lock()
			c.keepCache[s.Tag()] = reasons
			c.mu.Unlock()
		}
		return o
	})
}

// Views.

// sourceViewLocked is a stored source as the protocol shows it.
func (c *Core) sourceViewLocked(s StoredSource) (protocol.Source, error) {
	eff, err := c.store.Effective(s)
	if err != nil {
		return protocol.Source{}, err
	}
	last, err := c.store.LastRun(s.ID, protocol.KindBackup)
	if err != nil {
		return protocol.Source{}, err
	}
	now := c.now()
	var next *time.Time
	if eff.Schedule != nil && eff.Schedule.Every > 0 && !s.Paused {
		// Never run: due since it was added. (Not "now": the scheduler
		// compares with its own earlier reading of the clock, so a fresh
		// now would always look a moment in the future and never be due.)
		n := s.Created
		if n.IsZero() || n.After(now) {
			n = now
		}
		every := time.Duration(eff.Schedule.Every) * time.Second
		if last != nil {
			n = last.Ended.Add(every)
		}
		// A run the database wouldn't record still counts as a try.
		if try, ok := c.lastTry[s.ID]; ok && try.Add(every).After(n) {
			n = try.Add(every)
		}
		next = &n
	}
	health := protocol.HealthOK
	switch {
	case last == nil:
		health = protocol.HealthNever
	case last.Status == protocol.RunFailed || last.Status == protocol.RunIncomplete:
		health = protocol.HealthFailed
	case next != nil && now.Sub(*next) > time.Duration(eff.Schedule.Every)*time.Second:
		health = protocol.HealthOverdue
	}
	view := protocol.Source{ID: s.ID, Name: s.Name, Host: c.Name, Paths: s.Paths, Excludes: s.Excludes, Schedule: eff.Schedule,
		Retention: eff.Retention, Inherited: eff.Inherited, Paused: s.Paused, Health: health, LastRun: last, NextRun: next}
	// Size comes from the last run that made a snapshot, so a failed run
	// since then doesn't blank it.
	sized, err := c.store.LastSnapshotRun(s.ID)
	if err != nil {
		return protocol.Source{}, err
	}
	if sized != nil {
		view.Files, view.Bytes = sized.Files, sized.Bytes
	}
	return view, nil
}

func (c *Core) snapshotView(e EngineSnapshot, source string) (protocol.Snapshot, error) {
	pinned := false
	var keptBy []string
	if source != "" {
		pins, err := c.store.Pinned(source)
		if err != nil {
			return protocol.Snapshot{}, err
		}
		pinned = pins[e.ID]
		c.mu.Lock()
		keptBy = c.keepCache["src:"+source][e.ID]
		c.mu.Unlock()
	}
	if keptBy == nil {
		keptBy = []string{}
	}
	note, err := c.store.Note(e.ID)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	added := e.BytesAdded
	if e.Original != "" {
		if n, ok, err := c.store.Added(e.ID); err == nil && ok {
			added = n
		}
	}
	return protocol.Snapshot{ID: e.ID, ShortID: e.ShortID, Time: e.Time, End: e.End, Paths: e.Paths, Hostname: e.Hostname,
		Tags: e.Tags, Files: e.Files, Bytes: e.Bytes, BytesAdded: added,
		KeptBy: keptBy, Pinned: pinned, Note: note}, nil
}

// lsPage is one page of a listing, as ls answers.
func lsPage(all []protocol.Entry, p protocol.LsParams) protocol.LsResult {
	offset := max(p.Offset, 0)
	limit := p.Limit
	if limit <= 0 {
		limit = 1000
	}
	limit = min(limit, 10000)
	page := all[min(offset, len(all)):min(offset+limit, len(all))]
	res := protocol.LsResult{Entries: page, Total: len(all)}
	if offset+len(page) < len(all) {
		next := offset + len(page)
		res.NextOffset = &next
	}
	return res
}

// dumpLimit is how much of a file dump reads. No max (or 0) is the cap:
// the engine reads 0 as "all of it", which would hold a whole multi-GB
// file in memory.
func dumpLimit(max int) int {
	limit := min(max, 1<<20)
	if limit <= 0 {
		limit = 1 << 20
	}
	return limit
}

// dumpResult is dump's answer for what was read of a file of size.
func dumpResult(data []byte, size int64) protocol.DumpResult {
	binary := slices.Contains(data[:min(len(data), 8192)], 0)
	res := protocol.DumpResult{Data: data, Size: size, Truncated: int64(len(data)) < size, Binary: binary}
	if binary {
		res.Data = []byte{}
	}
	return res
}

// sourceOf is the charm-bak source a snapshot belongs to, from its tags.
// SetTreeCache keeps loaded snapshot listings in a cache database at
// path (treedb.go). If it can't be opened they stay in memory only.
func (c *Core) SetTreeCache(path string) error {
	db, err := openTreeDB(path)
	if err != nil {
		return err
	}
	c.trees.db = db
	return nil
}

// newestSnapshot is the id of the latest of snaps (not empty).
func newestSnapshot(snaps []EngineSnapshot) string {
	newest := snaps[0]
	for _, e := range snaps[1:] {
		if e.Time.After(newest.Time) {
			newest = e
		}
	}
	return newest.ID
}

func sourceOf(e EngineSnapshot) string {
	for _, t := range e.Tags {
		if id, ok := strings.CutPrefix(t, "src:"); ok {
			return id
		}
	}
	return ""
}

// Errors.

func perr(code, format string, args ...any) *protocol.Error {
	return &protocol.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func perrData(code string, data any, format string, args ...any) *protocol.Error {
	e := perr(code, format, args...)
	e.Data, _ = json.Marshal(data)
	return e
}

// protocolError turns any error from a handler into a protocol error.
func (c *Core) protocolError(err error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	var ee *EngineError
	if !errors.As(err, &ee) {
		return perr(protocol.ErrInternal, "%v", err)
	}
	switch ee.Kind {
	case ErrLocked:
		c.mu.Lock()
		cur := c.currentJobLocked()
		c.mu.Unlock()
		if cur != nil {
			return perrData(protocol.ErrBusy, protocol.BusyData{Job: *cur}, "%s", ee.Error())
		}
		return perr(protocol.ErrBusy, "%s", ee.Error())
	case ErrNoRepository:
		return perr(protocol.ErrNotConfigured, "There's no repository at the configured location. %s", ee.Message)
	case ErrNotFound:
		return perr(protocol.ErrNotFound, "%s", ee.Message)
	}
	return perrData(protocol.ErrResticFailed, protocol.ResticFailedData{Exit: ee.Code, Stderr: ee.Message}, "%s", ee.Error())
}

// Handle answers one request. Subscribe is handled by the server.
func (c *Core) Handle(ctx context.Context, req protocol.Request) (any, error) {
	params := func(v any) error {
		if len(req.Params) == 0 {
			return perr(protocol.ErrBadRequest, "%s needs params.", req.Method)
		}
		if err := json.Unmarshal(req.Params, v); err != nil {
			return perr(protocol.ErrBadRequest, "Bad params for %s: %v", req.Method, err)
		}
		return nil
	}

	if req.Host != "" && req.Host != c.Name && req.Method != protocol.MethodHello {
		// A remote's backups are read here, from its repository; the
		// rest is the remote's to do (remoteread.go).
		if out, handled, err := c.remoteRead(ctx, req); handled {
			return out, err
		}
		return c.relay(ctx, req)
	}

	switch req.Method {
	case protocol.MethodHosts:
		return c.hosts(), nil

	case protocol.MethodHostSet:
		var p protocol.HostSetParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.setHostAddress(p)

	case protocol.MethodRepoSet:
		var p protocol.RepositorySetParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.setRepository(ctx, p)

	case protocol.MethodSelfUpdate:
		var p protocol.SelfUpdateParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.selfUpdate(p)

	case protocol.MethodRepoProbe:
		var p protocol.RepositoryProbeParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.probeRepository(ctx, p)

	case protocol.MethodRepoSetup:
		var p protocol.RepositorySetupParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.setupRepository(ctx, p)

	case protocol.MethodAWSStatus:
		return c.awsStatus()

	case protocol.MethodAWSKeys:
		var p protocol.AWSKeysParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.awsKeys(ctx, p)

	case protocol.MethodPairStart:
		var p protocol.PairStartParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.startPairing(p)

	case protocol.MethodHostRepair:
		var p protocol.HostRepairParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.startRepair(p)

	case protocol.MethodHello:
		var p protocol.HelloParams
		if err := params(&p); err != nil {
			return nil, err
		}
		if p.Protocol != protocol.Version {
			return nil, perrData(protocol.ErrUnsupportedProtocol, protocol.UnsupportedProtocolData{Protocol: protocol.Version},
				"This daemon speaks protocol %d; the client speaks %d. Update whichever is older.", protocol.Version, p.Protocol)
		}
		goos := runtime.GOOS
		if c.goos != "" {
			goos = c.goos
		}
		return protocol.HelloResult{Protocol: protocol.Version, App: c.Version, Build: c.Build, OS: goos, Arch: runtime.GOARCH, Bundle: c.UpdateBundle != ""}, nil

	case protocol.MethodStatus:
		r, err := c.store.Repository()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		res := protocol.StatusResult{State: c.stateLocked(), Job: c.currentJobLocked(), Paused: c.paused, FullDiskAccess: c.fullDiskAccess()}
		if r != nil {
			res.Repository = &protocol.Repository{Location: r.Location}
			if c.stats != nil {
				res.Repository.StoredBytes, res.Repository.LogicalBytes = c.stats.Stored, c.stats.Uncompressed
			} else {
				c.refreshStatsLocked()
			}
		}
		return res, nil

	case protocol.MethodRepository:
		return c.repositoryInfo()

	case protocol.MethodDefaultsSet:
		var p protocol.DefaultsSetParams
		if err := params(&p); err != nil {
			return nil, err
		}
		d, err := c.store.Defaults()
		if err != nil {
			return nil, err
		}
		if p.Schedule != nil {
			d.Schedule = p.Schedule
			if p.Schedule.Every <= 0 {
				d.Schedule = nil
			}
		}
		if p.Retention != nil {
			d.Retention = *p.Retention
		}
		if p.Excludes != nil {
			d.Excludes = *p.Excludes
		}
		logf("defaults changed: schedule %v, retention %+v, skip %q", d.Schedule, d.Retention, d.Excludes)
		if err := c.store.SetDefaults(d); err != nil {
			return nil, err
		}
		// Keep reasons were worked out for the old retention.
		c.mu.Lock()
		clear(c.keepCache)
		c.mu.Unlock()
		// Sources that inherit now show (and run on) the new values.
		sources, _ := c.store.Sources()
		c.mu.Lock()
		for _, s := range sources {
			if view, err := c.sourceViewLocked(s); err == nil {
				c.publish(protocol.Event{Name: protocol.EventSourceChanged, SourceChanged: &protocol.SourceChangedEvent{Source: view}})
			}
		}
		c.mu.Unlock()
		return protocol.DefaultsResult{Defaults: d.view()}, nil

	case protocol.MethodPause, protocol.MethodResume:
		c.SetPaused(req.Method == protocol.MethodPause)
		return protocol.Empty{}, nil

	case protocol.MethodSources:
		stored, err := c.store.Sources()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		res := protocol.SourcesResult{Sources: []protocol.Source{}}
		for _, s := range stored {
			v, err := c.sourceViewLocked(s)
			if err != nil {
				return nil, err
			}
			res.Sources = append(res.Sources, v)
		}
		return res, nil

	case protocol.MethodSnapshots:
		var p protocol.SnapshotsParams
		if err := params(&p); err != nil {
			return nil, err
		}
		engine, err := c.readable()
		if err != nil {
			return nil, err
		}
		res := protocol.SnapshotsResult{Snapshots: []protocol.Snapshot{}}
		if p.Source == protocol.UntaggedSource {
			all, err := c.snapshots(ctx, engine, "")
			if err != nil {
				return nil, err
			}
			// Also the snapshots of sources that are gone (removed with
			// their snapshots kept, or a database started over): nothing
			// else would list them, and they must never be hidden.
			stored, err := c.store.Sources()
			if err != nil {
				return nil, err
			}
			known := map[string]bool{}
			for _, s := range stored {
				known[s.ID] = true
			}
			for _, e := range all {
				if !known[sourceOf(e)] && !slices.Contains(e.Tags, stateTag) {
					v, err := c.snapshotView(e, "")
					if err != nil {
						return nil, err
					}
					res.Snapshots = append(res.Snapshots, v)
				}
			}
			return res, nil
		}
		s, ok, err := c.store.Source(p.Source)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, perr(protocol.ErrNotFound, "No source with id %s.", p.Source)
		}
		snaps, err := c.snapshots(ctx, engine, s.Tag())
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		_, cached := c.keepCache[s.Tag()]
		c.mu.Unlock()
		if !cached {
			eff, effErr := c.store.Effective(s)
			pins, pinErr := c.store.Pinned(s.ID)
			if effErr != nil || pinErr != nil {
				// Shown without keep reasons this time.
			} else if reasons, err := engine.KeepReasons(ctx, s.Tag(), eff.Retention, pins); err == nil {
				c.mu.Lock()
				c.keepCache[s.Tag()] = reasons
				c.mu.Unlock()
			}
		}
		c.recalcAddedLater(engine, s, snaps)
		if len(snaps) > 0 {
			c.warmTree(engine, newestSnapshot(snaps))
		}
		for _, e := range snaps {
			v, err := c.snapshotView(e, s.ID)
			if err != nil {
				return nil, err
			}
			res.Snapshots = append(res.Snapshots, v)
		}
		return res, nil

	case protocol.MethodLs:
		var p protocol.LsParams
		if err := params(&p); err != nil {
			return nil, err
		}
		var all []protocol.Entry
		var err error
		if p.Snapshot == protocol.LiveSnapshot {
			all, err = listLive(p.Path)
		} else {
			all, err = c.list(ctx, p.Snapshot, p.Path)
		}
		if err != nil {
			return nil, err
		}
		return lsPage(all, p), nil

	case protocol.MethodDump:
		var p protocol.DumpParams
		if err := params(&p); err != nil {
			return nil, err
		}
		limit := dumpLimit(p.Max)
		var data []byte
		var size int64
		var err error
		if p.Snapshot == protocol.LiveSnapshot {
			data, size, err = readLive(p.Path, limit)
		} else {
			var engine Engine
			if engine, err = c.readable(); err != nil {
				return nil, err
			}
			data, size, err = engine.Read(ctx, p.Snapshot, p.Path, limit)
		}
		if err != nil {
			return nil, err
		}
		return dumpResult(data, size), nil

	case protocol.MethodChanges:
		var p protocol.ChangesParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.changes(ctx, p, c.readable)

	case protocol.MethodDiff:
		var p protocol.DiffParams
		if err := params(&p); err != nil {
			return nil, err
		}
		saved, err := c.list(ctx, p.Snapshot, p.Path)
		if err != nil {
			return nil, err
		}
		return protocol.DiffResult{Entries: diffLive(saved, p.Path)}, nil

	case protocol.MethodRestore:
		var p protocol.RestoreParams
		if err := params(&p); err != nil {
			return nil, err
		}
		if p.Target != protocol.RestoreOriginal && !filepath.IsAbs(p.Target) {
			return nil, perr(protocol.ErrBadRequest, "Restore target must be \"original\" or an absolute path.")
		}
		if p.Snapshot == "" {
			return nil, perr(protocol.ErrBadRequest, "Restore needs a snapshot id.")
		}
		for _, rp := range p.Paths {
			if !filepath.IsAbs(rp) {
				return nil, perr(protocol.ErrBadRequest, "Restore paths must be absolute: %s", rp)
			}
		}
		if p.From != "" && p.From != c.Name {
			return c.restoreFrom(p)
		}
		if _, err := c.engine(); err != nil {
			return nil, err
		}
		target := p.Target
		if target == protocol.RestoreOriginal {
			target = ""
		}
		job := c.enqueue(protocol.KindRestore, nil, func(ctx context.Context, job protocol.Job) outcome {
			engine, err := c.engine()
			if err != nil {
				return outcome{status: protocol.RunFailed, err: err.Error()}
			}
			o := result(engine.Restore(ctx, p.Snapshot, p.Paths, target, p.Overwrite, func(pr Progress) {
				c.mu.Lock()
				c.publish(protocol.Event{Name: protocol.EventProgress, Progress: &protocol.Progress{JobID: job.ID, Percent: pr.Percent,
					FilesDone: pr.FilesDone, TotalFiles: pr.TotalFiles, BytesDone: pr.BytesDone, TotalBytes: pr.TotalBytes, Current: pr.Current}})
				c.mu.Unlock()
			}))
			o.snapshot = p.Snapshot
			return o
		})
		return protocol.JobResult{Job: job}, nil

	case protocol.MethodBackupNow:
		var p protocol.BackupNowParams
		if err := params(&p); err != nil {
			return nil, err
		}
		s, ok, err := c.store.Source(p.Source)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, perr(protocol.ErrNotFound, "No source with id %s.", p.Source)
		}
		if _, err := c.engine(); err != nil {
			return nil, err
		}
		return protocol.JobResult{Job: c.enqueueBackup(s)}, nil

	case protocol.MethodCancel:
		var p protocol.CancelParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return protocol.Empty{}, c.cancelJob(p.Job)

	case protocol.MethodSourceAdd:
		var p protocol.SourceAddParams
		if err := params(&p); err != nil {
			return nil, err
		}
		if len(p.Paths) == 0 || slices.ContainsFunc(p.Paths, func(s string) bool { return !filepath.IsAbs(s) }) {
			return nil, perr(protocol.ErrBadRequest, "A source needs at least one absolute path.")
		}
		src := StoredSource{Name: p.Name, Paths: p.Paths, Excludes: p.Excludes, Retention: p.Retention, ScheduleSet: p.Schedule != nil}
		if p.Schedule != nil && p.Schedule.Every > 0 {
			src.Schedule = p.Schedule
		}
		src, err := c.store.AddSource(src, c.now())
		if err != nil {
			return nil, err
		}
		logf("source %s (%s) added: %s", src.Name, src.ID, strings.Join(src.Paths, ", "))
		if p.BackUpNow {
			if _, err := c.engine(); err == nil {
				c.enqueueBackup(src)
			}
		}
		return c.sourceChanged(src)

	case protocol.MethodSourceSet:
		var p protocol.SourceSetParams
		if err := params(&p); err != nil {
			return nil, err
		}
		s, ok, err := c.store.Source(p.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, perr(protocol.ErrNotFound, "No source with id %s.", p.ID)
		}
		if p.Name != nil {
			s.Name = *p.Name
		}
		if p.Paths != nil {
			s.Paths = p.Paths
		}
		if p.Excludes != nil {
			s.Excludes = *p.Excludes
		}
		if p.Schedule != nil {
			s.Schedule, s.ScheduleSet = p.Schedule, true
			if p.Schedule.Every <= 0 {
				s.Schedule = nil
			}
		}
		if p.Retention != nil {
			s.Retention = p.Retention
		}
		if p.Paused != nil {
			s.Paused = *p.Paused
		}
		for _, f := range p.Inherit {
			switch f {
			case "schedule":
				s.Schedule, s.ScheduleSet = nil, false
			case "retention":
				s.Retention = nil
			default:
				return nil, perr(protocol.ErrBadRequest, "Can't inherit %s; only schedule and retention.", f)
			}
		}
		logf("source %s (%s) changed", s.Name, s.ID)
		if err := c.store.UpdateSource(s); err != nil {
			return nil, err
		}
		c.mu.Lock()
		delete(c.keepCache, s.Tag())
		c.mu.Unlock()
		return c.sourceChanged(s)

	case protocol.MethodAccessSettings:
		return c.openAccessSettings()
	case protocol.MethodHostRemove:
		var p protocol.HostRemoveParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.removeHost(ctx, p)
	case protocol.MethodUninstall:
		var p protocol.UninstallParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.uninstall(p)

	case protocol.MethodSourceRewrite:
		var p protocol.SourceRewriteParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.sourceRewrite(ctx, p)

	case protocol.MethodSourceRemove:
		var p protocol.SourceRemoveParams
		if err := params(&p); err != nil {
			return nil, err
		}
		s, ok, err := c.store.Source(p.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, perr(protocol.ErrNotFound, "No source with id %s.", p.ID)
		}
		if !p.KeepSnapshots {
			engine, err := c.engine()
			if err != nil {
				return nil, err
			}
			c.enqueue(protocol.KindForget, nil, func(ctx context.Context, _ protocol.Job) outcome {
				snaps, err := engine.Snapshots(ctx, s.Tag())
				if err != nil {
					return result(err)
				}
				ids := make([]string, len(snaps))
				for i, e := range snaps {
					ids[i] = e.ID
				}
				return result(engine.Forget(ctx, ids))
			})
		}
		logf("source %s (%s) removed, keep snapshots %v", s.Name, s.ID, p.KeepSnapshots)
		if err := c.store.RemoveSource(s.ID); err != nil {
			return nil, err
		}
		c.mu.Lock()
		delete(c.keepCache, s.Tag())
		c.mu.Unlock()
		return protocol.Empty{}, nil

	case protocol.MethodSnapshotMeta:
		var p protocol.SnapshotMetaParams
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.snapshotMeta(p)

	case protocol.MethodSnapshotSet:
		var p protocol.SnapshotSetParams
		if err := params(&p); err != nil {
			return nil, err
		}
		engine, err := c.readable()
		if err != nil {
			return nil, err
		}
		all, err := c.snapshots(ctx, engine, "")
		if err != nil {
			return nil, err
		}
		i := slices.IndexFunc(all, func(e EngineSnapshot) bool { return e.ID == p.ID || e.ShortID == p.ID })
		if i < 0 {
			return nil, perr(protocol.ErrNotFound, "No snapshot %s.", p.ID)
		}
		snap := all[i]
		source := sourceOf(snap)
		if p.Note != nil {
			if err := c.store.SetNote(snap.ID, *p.Note); err != nil {
				return nil, err
			}
		}
		if p.Pinned != nil {
			if source == "" {
				return nil, perr(protocol.ErrBadRequest, "Only snapshots that belong to a source can be pinned.")
			}
			if err := c.store.SetPinned(snap.ID, source, *p.Pinned); err != nil {
				return nil, err
			}
			c.mu.Lock()
			delete(c.keepCache, "src:"+source)
			c.mu.Unlock()
		}
		v, err := c.snapshotView(snap, source)
		if err != nil {
			return nil, err
		}
		return protocol.SnapshotResult{Snapshot: v}, nil

	case protocol.MethodHistory:
		var p protocol.HistoryParams
		if err := params(&p); err != nil {
			return nil, err
		}
		runs, err := c.store.Runs(p.Since)
		if err != nil {
			return nil, err
		}
		return protocol.HistoryResult{Runs: runs}, nil
	}
	return nil, perr(protocol.ErrUnknownMethod, "Unknown method %s.", req.Method)
}

// sourceChanged publishes a source's new view and returns it as a result.
func (c *Core) sourceChanged(s StoredSource) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, err := c.sourceViewLocked(s)
	if err != nil {
		return nil, err
	}
	c.publish(protocol.Event{Name: protocol.EventSourceChanged, SourceChanged: &protocol.SourceChangedEvent{Source: v}})
	return protocol.SourceResult{Source: v}, nil
}

// diffLive compares one snapshot directory with the same directory on disk.
func diffLive(saved []protocol.Entry, dir string) []protocol.DiffEntry {
	type liveEntry struct {
		typ   string
		size  int64
		mtime time.Time
	}
	live := map[string]liveEntry{}
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		info, err := os.Lstat(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		t := protocol.TypeOther
		switch {
		case info.Mode().IsDir():
			t = protocol.TypeDir
		case info.Mode().IsRegular():
			t = protocol.TypeFile
		case info.Mode()&os.ModeSymlink != 0:
			t = protocol.TypeSymlink
		}
		live[de.Name()] = liveEntry{t, info.Size(), info.ModTime()}
	}
	out := []protocol.DiffEntry{}
	for _, e := range saved {
		l, ok := live[e.Name]
		if !ok {
			out = append(out, protocol.DiffEntry{Name: e.Name, Type: e.Type, Change: protocol.ChangeRemoved})
			continue
		}
		delete(live, e.Name)
		changed := l.typ != e.Type || (e.Type == protocol.TypeFile && (l.size != e.Size || absDur(l.mtime.Sub(e.MTime)) > time.Second))
		ch := protocol.ChangeSame
		if changed {
			ch = protocol.ChangeChanged
		}
		out = append(out, protocol.DiffEntry{Name: e.Name, Type: e.Type, Change: ch})
	}
	names := make([]string, 0, len(live))
	for n := range live {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		out = append(out, protocol.DiffEntry{Name: n, Type: live[n].typ, Change: protocol.ChangeAdded})
	}
	return out
}

// readLive reads the first limit bytes of a regular file on disk.
// listLive is ls on the disk as it is now (snapshot "live"): what the
// add-source picker browses, on whichever machine the source will be.
// Entries that vanish or can't be read mid-listing are skipped.
func listLive(dir string) ([]protocol.Entry, error) {
	if !filepath.IsAbs(dir) {
		return nil, perr(protocol.ErrBadRequest, "Path must be absolute.")
	}
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, perr(protocol.ErrNotFound, "%s isn't on disk.", dir)
	}
	if err != nil && len(des) == 0 {
		return nil, err
	}
	out := make([]protocol.Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := protocol.Entry{Name: de.Name(), Size: info.Size(), Mode: info.Mode().String(), MTime: info.ModTime()}
		switch {
		case info.IsDir():
			e.Type, e.Size = protocol.TypeDir, 0
		case info.Mode()&fs.ModeSymlink != 0:
			e.Type = protocol.TypeSymlink
		case info.Mode().IsRegular():
			e.Type = protocol.TypeFile
		default:
			e.Type = protocol.TypeOther
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}

func readLive(p string, limit int) ([]byte, int64, error) {
	if !filepath.IsAbs(p) {
		return nil, 0, perr(protocol.ErrBadRequest, "Path must be absolute.")
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, perr(protocol.ErrNotFound, "%s isn't on disk.", p)
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, perr(protocol.ErrBadRequest, "%s isn't a regular file.", p)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	return data, info.Size(), err
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// repositoryInfo is the Repository popup's data: where the repository is,
// where its credentials come from (never the credentials), its size, and
// maintenance.
func (c *Core) repositoryInfo() (protocol.RepositoryResult, error) {
	d, err := c.store.Defaults()
	if err != nil {
		return protocol.RepositoryResult{}, err
	}
	res := protocol.RepositoryResult{Defaults: d.view()}
	r, err := c.store.Repository()
	if err != nil || r == nil {
		return res, err
	}
	info := &protocol.RepositoryInfo{Location: r.Location, PasswordFrom: describePasswordCommand(r.PasswordCommand)}
	if r.PasswordFile != "" {
		info.PasswordFrom = "file " + r.PasswordFile
	}
	switch service, account, ok := strings.Cut(r.EnvKeychain+r.AWSKeychain, "/"); {
	case ok:
		info.AWSFrom = "Keychain " + service + " / " + account
	case r.EnvFile != "":
		info.AWSFrom = "file " + r.EnvFile
	case r.AWSFile != "":
		info.AWSFrom = "file " + r.AWSFile
	}
	info.LastPrune, _ = c.store.LastRepoRun(protocol.KindPrune)
	info.LastCheck, _ = c.store.LastRepoRun(protocol.KindCheck)
	next := c.now()
	if info.LastPrune != nil {
		next = info.LastPrune.Ended.Add(maintenanceEvery)
	}
	info.NextMaintenance = &next
	c.mu.Lock()
	if c.stats != nil {
		info.StoredBytes, info.LogicalBytes = c.stats.Stored, c.stats.Uncompressed
	} else {
		c.refreshStatsLocked()
	}
	c.mu.Unlock()
	res.Repository = info
	return res, nil
}

// describePasswordCommand names where the password comes from: a
// Keychain item when the command is the usual security lookup. Any other
// command isn't shown: it could have the password in it.
func describePasswordCommand(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) >= 2 && f[0] == "security" && f[1] == "find-generic-password" {
		var service, account string
		for i := 2; i+1 < len(f); i++ {
			switch f[i] {
			case "-s":
				service = f[i+1]
			case "-a":
				account = f[i+1]
			}
		}
		if service != "" {
			if account != "" {
				return "Keychain " + service + " / " + account
			}
			return "Keychain " + service
		}
	}
	if cmd == "" {
		return "RESTIC_PASSWORD in the environment"
	}
	return "a password command (see -env)"
}

// jobWhat names a job for the log: its kind and, for a source's job, the
// source's name.
func (c *Core) jobWhat(job protocol.Job) string {
	if job.Source == nil {
		return job.Kind
	}
	if s, ok, _ := c.store.Source(*job.Source); ok {
		return job.Kind + " of " + s.Name
	}
	return job.Kind + " of " + *job.Source
}
