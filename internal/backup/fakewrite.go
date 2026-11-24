package backup

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"charm-bak/internal/protocol"
)

// The Fake's changes: enough like the app that the TUI's flows can be
// exercised offline. A backup "runs" for a couple of seconds, sending the
// same events the app does.

func (f *Fake) publishLocked(e protocol.Event) {
	for _, ch := range f.events {
		select {
		case ch <- e:
		default: // a slow listener misses events, as with the app
		}
	}
}

func (f *Fake) Events(ctx context.Context) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 64)
	f.mu.Lock()
	f.events = append(f.events, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.events = slices.DeleteFunc(f.events, func(c chan protocol.Event) bool { return c == ch })
		close(ch)
	}()
	return ch, nil
}

func (f *Fake) sourceIndexLocked(id string) int {
	return slices.IndexFunc(f.sources, func(s protocol.Source) bool { return s.ID == id })
}

func (f *Fake) AddSource(ctx context.Context, p protocol.SourceAddParams) (protocol.Source, error) {
	if len(p.Paths) == 0 {
		return protocol.Source{}, &protocol.Error{Code: protocol.ErrBadRequest, Message: "A source needs at least one folder."}
	}
	f.mu.Lock()
	if err := f.goneLocked(); err != nil {
		f.mu.Unlock()
		return protocol.Source{}, err
	}
	f.nextID++
	s := protocol.Source{
		ID: fmt.Sprintf("f%07x", f.nextID), Name: p.Name, Host: f.Name, Paths: p.Paths, Excludes: p.Excludes,
		Schedule: &protocol.Schedule{Every: 3600}, Retention: protocol.Retention{Last: 10, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 12, Yearly: 3},
		Inherited: []string{"schedule", "retention"}, Health: protocol.HealthNever,
	}
	if s.Name == "" {
		s.Name = path.Base(p.Paths[0])
	}
	if p.Schedule != nil {
		s.Schedule, s.Inherited = p.Schedule, slices.DeleteFunc(s.Inherited, func(f string) bool { return f == "schedule" })
	}
	if p.Retention != nil {
		s.Retention, s.Inherited = *p.Retention, slices.DeleteFunc(s.Inherited, func(f string) bool { return f == "retention" })
	}
	f.sources = append(f.sources, s)
	f.publishLocked(protocol.Event{Name: protocol.EventSourceChanged, SourceChanged: &protocol.SourceChangedEvent{Source: s}})
	f.mu.Unlock()
	if p.BackUpNow {
		f.BackupNow(ctx, s.ID)
	}
	return s, nil
}

// RewriteSource: a dry run counts the source's snapshots; the real one
// is a job that finishes at once. Rewrites records what was asked.
func (f *Fake) RewriteSource(_ context.Context, p protocol.SourceRewriteParams) (protocol.SourceRewriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.SourceRewriteResult{}, err
	}
	if f.sourceIndexLocked(p.ID) < 0 {
		return protocol.SourceRewriteResult{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No source with id " + p.ID + "."}
	}
	if p.DryRun {
		return protocol.SourceRewriteResult{Snapshots: len(f.snapshots[p.ID])}, nil
	}
	f.Rewrites = append(f.Rewrites, p)
	f.nextID++
	src := p.ID
	job := protocol.Job{ID: fmt.Sprintf("j-%x", f.nextID), Kind: protocol.KindRewrite, Source: &src, Started: time.Now()}
	f.publishLocked(protocol.Event{Name: protocol.EventJobStarted, JobStarted: &protocol.JobStartedEvent{Job: job}})
	run := protocol.Run{Job: job.ID, Kind: protocol.KindRewrite, Source: &src, Started: job.Started, Ended: time.Now(), Status: protocol.RunOK}
	f.runs = append(f.runs, run)
	f.publishLocked(protocol.Event{Name: protocol.EventJobFinished, JobFinished: &protocol.JobFinishedEvent{Run: run}})
	return protocol.SourceRewriteResult{Job: &job}, nil
}

func (f *Fake) SetSource(_ context.Context, p protocol.SourceSetParams) (protocol.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.Source{}, err
	}
	i := f.sourceIndexLocked(p.ID)
	if i < 0 {
		return protocol.Source{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No source with id " + p.ID + "."}
	}
	s := &f.sources[i]
	drop := func(field string) {
		s.Inherited = slices.DeleteFunc(s.Inherited, func(f string) bool { return f == field })
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
		s.Schedule = p.Schedule
		if p.Schedule.Every == 0 {
			s.Schedule = nil
		}
		drop("schedule")
	}
	if p.Retention != nil {
		s.Retention = *p.Retention
		drop("retention")
	}
	if p.Paused != nil {
		s.Paused = *p.Paused
	}
	for _, field := range p.Inherit {
		if !slices.Contains(s.Inherited, field) {
			s.Inherited = append(s.Inherited, field)
		}
	}
	f.publishLocked(protocol.Event{Name: protocol.EventSourceChanged, SourceChanged: &protocol.SourceChangedEvent{Source: *s}})
	return *s, nil
}

func (f *Fake) RemoveSource(_ context.Context, id string, keepSnapshots bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return err
	}
	i := f.sourceIndexLocked(id)
	if i < 0 {
		return &protocol.Error{Code: protocol.ErrNotFound, Message: "No source with id " + id + "."}
	}
	f.sources = slices.Delete(f.sources, i, i+1)
	if !keepSnapshots {
		delete(f.snapshots, id)
	}
	return nil
}

func (f *Fake) BackupNow(_ context.Context, source string) (protocol.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.Job{}, err
	}
	if f.sourceIndexLocked(source) < 0 {
		return protocol.Job{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No source with id " + source + "."}
	}
	f.nextID++
	src := source
	job := protocol.Job{ID: fmt.Sprintf("j-%x", f.nextID), Kind: protocol.KindBackup, Source: &src, Started: time.Now()}
	f.status.State, f.status.Job = protocol.StateRunning, &job
	f.publishLocked(protocol.Event{Name: protocol.EventJobStarted, JobStarted: &protocol.JobStartedEvent{Job: job}})
	go f.runBackup(job)
	return job, nil
}

// runBackup pretends: ten progress steps, then a finished run.
func (f *Fake) runBackup(job protocol.Job) {
	for i := 1; i <= 10; i++ {
		time.Sleep(200 * time.Millisecond)
		f.mu.Lock()
		f.publishLocked(protocol.Event{Name: protocol.EventProgress, Progress: &protocol.Progress{
			JobID: job.ID, Percent: float64(i) / 10, FilesDone: int64(i * 120), TotalFiles: 1200, BytesDone: int64(i) << 26, TotalBytes: 10 << 26}})
		f.mu.Unlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	run := protocol.Run{Job: job.ID, Kind: protocol.KindBackup, Source: job.Source, Started: job.Started, Ended: now,
		Status: protocol.RunOK, FilesNew: 12, Files: 1200, Bytes: 10 << 26, BytesAdded: 3 << 20}
	if i := f.sourceIndexLocked(*job.Source); i >= 0 {
		s := &f.sources[i]
		s.LastRun, s.Health, s.Files, s.Bytes = &run, protocol.HealthOK, run.Files, run.Bytes
		if s.Schedule != nil {
			next := now.Add(time.Duration(s.Schedule.Every) * time.Second)
			s.NextRun = &next
		}
		f.publishLocked(protocol.Event{Name: protocol.EventSourceChanged, SourceChanged: &protocol.SourceChangedEvent{Source: *s}})
	}
	f.status.State, f.status.Job = protocol.StateIdle, nil
	f.publishLocked(protocol.Event{Name: protocol.EventJobFinished, JobFinished: &protocol.JobFinishedEvent{Run: run}})
	f.publishLocked(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: protocol.StateIdle}})
}

// Restore pretends: a job that finishes after a few progress steps and
// writes nothing. Restores records what was asked, for tests.
func (f *Fake) Restore(_ context.Context, p protocol.RestoreParams) (protocol.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.Job{}, err
	}
	f.nextID++
	job := protocol.Job{ID: fmt.Sprintf("j-%x", f.nextID), Kind: protocol.KindRestore, Started: time.Now()}
	f.Restores = append(f.Restores, p)
	f.status.State, f.status.Job = protocol.StateRunning, &job
	f.publishLocked(protocol.Event{Name: protocol.EventJobStarted, JobStarted: &protocol.JobStartedEvent{Job: job}})
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.mu.Lock()
		defer f.mu.Unlock()
		run := protocol.Run{Job: job.ID, Kind: protocol.KindRestore, Started: job.Started, Ended: time.Now(), Status: protocol.RunOK, Snapshot: p.Snapshot}
		f.status.State, f.status.Job = protocol.StateIdle, nil
		f.publishLocked(protocol.Event{Name: protocol.EventJobFinished, JobFinished: &protocol.JobFinishedEvent{Run: run}})
		f.publishLocked(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: protocol.StateIdle}})
	}()
	return job, nil
}

// SetSnapshot changes a fixture snapshot's note or pin. As in the app,
// only snapshots of a source can be pinned.
func (f *Fake) SetSnapshot(_ context.Context, p protocol.SnapshotSetParams) (protocol.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.Snapshot{}, err
	}
	for _, snaps := range f.snapshots {
		for i := range snaps {
			s := &snaps[i]
			if s.ID != p.ID {
				continue
			}
			if p.Note != nil {
				s.Note = *p.Note
			}
			if p.Pinned != nil {
				s.Pinned = *p.Pinned
			}
			return *s, nil
		}
	}
	return protocol.Snapshot{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No snapshot " + p.ID + "."}
}

// Repository describes the fixture repository; Defaults is what
// SetDefaults changed.
// FailRepository makes Repository answer err (nil: answer again), as a
// machine that stopped answering does.
func (f *Fake) FailRepository(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repositoryErr = err
}

func (f *Fake) Repository(_ context.Context) (protocol.RepositoryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repositoryErr != nil {
		return protocol.RepositoryResult{}, f.repositoryErr
	}
	if err := f.unsavedLocked(); err != nil {
		return protocol.RepositoryResult{}, err
	}
	r := f.status.Repository
	res := protocol.RepositoryResult{Defaults: f.defaults}
	if r != nil {
		next := f.Now.Add(5 * 24 * time.Hour)
		res.Repository = &protocol.RepositoryInfo{Location: r.Location, PasswordFrom: "Keychain charm-bak-repo / " + f.Name,
			AWSFrom: "Keychain charm-bak-aws / " + f.Name, StoredBytes: r.StoredBytes, LogicalBytes: r.LogicalBytes, NextMaintenance: &next}
	}
	return res, nil
}

func (f *Fake) SetDefaults(_ context.Context, p protocol.DefaultsSetParams) (protocol.Defaults, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.Defaults{}, err
	}
	if p.Schedule != nil {
		f.defaults.Schedule = p.Schedule
		if p.Schedule.Every <= 0 {
			f.defaults.Schedule = nil
		}
	}
	if p.Retention != nil {
		f.defaults.Retention = *p.Retention
	}
	if p.Excludes != nil {
		f.defaults.Excludes = *p.Excludes
	}
	for i := range f.sources {
		s := &f.sources[i]
		if slices.Contains(s.Inherited, "schedule") {
			s.Schedule = f.defaults.Schedule
		}
		if slices.Contains(s.Inherited, "retention") {
			s.Retention = f.defaults.Retention
		}
	}
	return f.defaults, nil
}

func (f *Fake) Cancel(_ context.Context, job string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return err
	}
	f.Cancelled = append(f.Cancelled, job)
	return nil
}

// History is every source's last run plus the runs this fake finished,
// oldest first.
func (f *Fake) History(_ context.Context, since time.Time) ([]protocol.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var runs []protocol.Run
	for _, s := range f.sources {
		if s.LastRun != nil && !s.LastRun.Ended.Before(since) {
			runs = append(runs, *s.LastRun)
		}
	}
	for _, r := range f.runs {
		if !r.Ended.Before(since) {
			runs = append(runs, r)
		}
	}
	slices.SortFunc(runs, func(a, b protocol.Run) int { return a.Ended.Compare(b.Ended) })
	return runs, nil
}

// ProbeRepository answers from Stores. A key "bad" fails the way S3
// does; a location under /Volumes/Gone is an unplugged drive.
func (f *Fake) ProbeRepository(_ context.Context, p protocol.RepositoryProbeParams) (protocol.RepositoryProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(p.Location, "/Volumes/Gone"):
		return protocol.RepositoryProbeResult{State: protocol.ProbeFailed, Message: "The drive Gone isn't connected."}, nil
	case p.Env["AWS_ACCESS_KEY_ID"] == "bad":
		return protocol.RepositoryProbeResult{State: protocol.ProbeFailed, Message: "The AWS Access Key Id you provided does not exist in our records."}, nil
	}
	pw, ok := f.Stores[p.Location]
	switch {
	case !ok:
		return protocol.RepositoryProbeResult{State: protocol.ProbeEmpty}, nil
	case p.Password != pw:
		return protocol.RepositoryProbeResult{State: protocol.ProbePassword, Message: "wrong password or no key found"}, nil
	}
	return protocol.RepositoryProbeResult{State: protocol.ProbeOpens}, nil
}

// SetupRepository creates or connects p.Host's repository in Stores.
func (f *Fake) SetupRepository(ctx context.Context, p protocol.RepositorySetupParams) (protocol.RepositorySetupResult, error) {
	target := f
	if p.Host != "" && p.Host != f.Name {
		r, ok := f.ForHost(p.Host).(*Fake)
		if !ok || r == f {
			return protocol.RepositorySetupResult{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No paired host named " + p.Host + "."}
		}
		target = r
	}
	made := ""
	f.mu.Lock()
	pw, exists := f.Stores[p.Location]
	switch {
	case p.Init && exists:
		f.mu.Unlock()
		return protocol.RepositorySetupResult{}, &protocol.Error{Code: protocol.ErrResticFailed, Message: "config file already exists"}
	case p.Init:
		if p.Password == "" {
			p.Password, made = "fake-recovery-key-1234", "fake-recovery-key-1234"
		}
		f.Stores[p.Location] = p.Password
	case !exists || pw != p.Password:
		f.mu.Unlock()
		return protocol.RepositorySetupResult{}, &protocol.Error{Code: protocol.ErrBadRequest, Message: "That password doesn't open the repository."}
	}
	f.mu.Unlock()
	target.mu.Lock()
	target.status.Repository = &protocol.Repository{Location: p.Location}
	target.mu.Unlock()
	res, err := target.Repository(ctx)
	return protocol.RepositorySetupResult{Repository: res.Repository, Password: made}, err
}

// PairStart hands out a fixture command; AddRemote is the machine
// running it.
func (f *Fake) PairStart(_ context.Context, p protocol.PairStartParams) (protocol.PairStartResult, error) {
	name := strings.ToLower(strings.TrimSpace(p.Name))
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
		return protocol.PairStartResult{}, &protocol.Error{Code: protocol.ErrBadRequest, Message: "A host name is lowercase letters, digits and dashes, like pantry."}
	}
	return protocol.PairStartResult{Command: "curl -fsS --pinnedpubkey 'sha256//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=' -k https://192.168.1.2:7458/i/MFRGGZDFMZTWQ2LK | sudo sh",
		Expires: f.Now.Add(10 * time.Minute)}, nil
}

// HostRepair hands out a fixture command for a known remote.
func (f *Fake) HostRepair(_ context.Context, name string) (protocol.PairStartResult, error) {
	return protocol.PairStartResult{Command: "curl -fsS --pinnedpubkey 'sha256//47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=' -k https://192.168.1.2:7458/i/REPAIRREPAIRREPA | sudo sh",
		Expires: f.Now.Add(10 * time.Minute)}, nil
}

// SetFullDiskAccess makes this fixture machine a Mac with or without
// Full Disk Access (nil: not a Mac).
func (f *Fake) SetFullDiskAccess(ok *bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.FullDiskAccess = ok
}

// OpenAccessSettings counts as opening the settings; AccessOpened says
// how often.
func (f *Fake) OpenAccessSettings(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status.FullDiskAccess == nil {
		return &protocol.Error{Code: protocol.ErrBadRequest, Message: "Full Disk Access is a macOS setting."}
	}
	f.AccessOpened++
	return nil
}

// AWSStatus is f.AWS.
func (f *Fake) AWSStatus(context.Context) (protocol.AWSStatusResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.AWS, nil
}

// AWSKeys makes a fixture key "AKIAFAKE<HOST>"; signing in sets up the
// key manager for the bucket (charm-bak-123456789012 when none is given).
// Bucket "taken" is someone else's.
func (f *Fake) AWSKeys(_ context.Context, p protocol.AWSKeysParams) (protocol.AWSKeysResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.AWSCalls = append(f.AWSCalls, p)
	if !f.AWS.CLI {
		return protocol.AWSKeysResult{}, &protocol.Error{Code: protocol.ErrNotConfigured, Message: "The AWS CLI isn't installed. brew install awscli, then try again."}
	}
	if p.Bucket == "taken" {
		return protocol.AWSKeysResult{}, &protocol.Error{Code: protocol.ErrBadRequest, Message: "The bucket taken belongs to someone else (bucket names are global). Choose another name."}
	}
	region := p.Region
	if region == "" {
		region = "us-east-1"
	}
	res := protocol.AWSKeysResult{Bucket: p.Bucket, Region: region}
	if p.SignIn {
		if res.Bucket == "" {
			res.Bucket = "charm-bak-123456789012"
		}
		res.BucketCreated = res.Bucket == "charm-bak-123456789012"
		f.AWS.Manager = &protocol.AWSManager{Account: "123456789012", Bucket: res.Bucket, Region: region}
	} else {
		m := f.AWS.Manager
		if m == nil {
			return protocol.AWSKeysResult{}, &protocol.Error{Code: protocol.ErrNotConfigured, Message: "There's no key manager here yet; sign in to AWS to set one up."}
		}
		res.Bucket, res.Region = m.Bucket, m.Region
	}
	res.Env = map[string]string{"AWS_ACCESS_KEY_ID": "AKIAFAKE" + strings.ToUpper(p.Host), "AWS_SECRET_ACCESS_KEY": "fake-secret-" + p.Host}
	return res, nil
}

// RemoveHost drops a fixture remote; one that's down is unpaired with
// a message, unless its backups were to be deleted.
func (f *Fake) RemoveHost(_ context.Context, p protocol.HostRemoveParams) (protocol.HostRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.remotes[p.Name] == nil {
		return protocol.HostRemoveResult{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No paired remote named " + p.Name + "."}
	}
	res := protocol.HostRemoveResult{Uninstalled: true}
	if f.down[p.Name] {
		if p.DeleteBackups {
			return res, &protocol.Error{Code: protocol.ErrHostUnreachable, Message: p.Name + " isn't answering, and only it can delete its backups."}
		}
		res = protocol.HostRemoveResult{Message: p.Name + " didn't answer, so it's still installed there. On " + p.Name + ": sudo systemctl disable --now charm-bak"}
	}
	delete(f.remotes, p.Name)
	delete(f.down, p.Name)
	return res, nil
}

// NoRepository makes this fixture machine one without a repository yet.
func (f *Fake) NoRepository() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Repository = nil
}

// SetHostAddress moves a fixture remote; a bare host gets port 7457.
func (f *Fake) SetHostAddress(_ context.Context, p protocol.HostSetParams) (protocol.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.remotes[p.Name]; !ok {
		return protocol.Host{}, &protocol.Error{Code: protocol.ErrNotFound, Message: "No paired remote named " + p.Name + "."}
	}
	addr := strings.TrimSpace(p.Address)
	if !strings.Contains(addr, ":") {
		addr += ":7457"
	}
	if f.addrs == nil {
		f.addrs = map[string]string{}
	}
	f.addrs[p.Name] = addr
	return protocol.Host{Name: p.Name, Address: addr, State: protocol.HostOK}, nil
}

// AddRun adds a finished run to History, for tests.
func (f *Fake) AddRun(r protocol.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, r)
}
