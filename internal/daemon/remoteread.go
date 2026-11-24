package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm-bak/internal/protocol"
)

// The controller reads every remote's repository itself (Alok,
// 2026-11-24: a machine that's off or dead is exactly when its backups
// are needed, so browsing never goes through it). Snapshot lists, folder
// listings and file contents for a remote's sources come from restic run
// here against that remote's repository, with the copies of its secrets
// this machine keeps from setting it up. The relay is for what has to
// happen there: sources, schedules, keys, back up now, its live disk.
//
// What this needs per remote is saved at each contact (remote_cache):
// its repository (for the location), its sources, and what its database
// knows about its snapshots that the repository doesn't (pins, notes).
// A remote this machine holds no secrets for is relayed as before.

// remote_cache keys.
const (
	cacheRepository = "repository" // protocol.RepositoryResult
	cacheSources    = "sources"    // protocol.SourcesResult
	cacheMeta       = "meta:"      // + source id: protocol.SnapshotMetaResult
)

// viewMissFor is how long "no secrets here for that remote" is believed
// before looking again: a Keychain lookup runs a command.
const viewMissFor = time.Minute

// remoteMetaWait bounds asking a reachable remote for its pins and
// notes; past it the saved copy is used.
const remoteMetaWait = 5 * time.Second

// remoteView is how this machine opens a remote's repository, worked out
// once and kept: ok is false when it can't (no location saved yet, or no
// password for it here).
type remoteView struct {
	cfg RepositoryConfig
	ok  bool
	at  time.Time
}

// remoteKey is a cache key for a remote's source tag, so two machines'
// lists never mix.
func remoteKey(host, tag string) string { return host + "\x00" + tag }

// remoteEngine is the engine for host's repository, when this machine
// holds what it takes: the location saved from the remote and its
// password in this machine's secret store.
func (c *Core) remoteEngine(host string) (Engine, bool) {
	loc := c.remoteLocation(host)
	if loc == "" {
		return nil, false
	}
	c.mu.Lock()
	v, cached := c.views[host]
	c.mu.Unlock()
	if cached && v.cfg.Location == loc && (v.ok || c.now().Sub(v.at) < viewMissFor) {
		if !v.ok {
			return nil, false
		}
		return c.makeEngine(v.cfg), true
	}
	v = remoteView{cfg: RepositoryConfig{Location: loc}, at: c.now()}
	pw, err := c.readSecret(repoKeychainService, host)
	env, envErr := c.readSecret(envKeychainService, host)
	aws, awsErr := c.readSecret(awsKeychainService, host)
	if err == nil && pw != "" {
		v.ok = true
		if c.secrets == nil {
			// The Keychain: restic and each run read the items themselves,
			// as for this machine's own repository; nothing is held here.
			v.cfg.PasswordCommand = KeychainCommand(repoKeychainService, host)
			switch {
			case envErr == nil && env != "":
				v.cfg.EnvKeychain = envKeychainService + "/" + host
			case awsErr == nil && aws != "":
				v.cfg.AWSKeychain = awsKeychainService + "/" + host
			}
		} else {
			v.cfg.Password = pw
			switch {
			case envErr == nil && env != "":
				if v.cfg.SecretEnv, err = decodeEnv(env); err != nil {
					v.ok = false
				}
			case awsErr == nil && aws != "":
				if id, secret, ok := strings.Cut(aws, ":"); ok {
					v.cfg.SecretEnv = map[string]string{"AWS_ACCESS_KEY_ID": id, "AWS_SECRET_ACCESS_KEY": secret}
				}
			}
		}
	}
	c.mu.Lock()
	if c.views == nil {
		c.views = map[string]remoteView{}
	}
	c.views[host] = v
	c.mu.Unlock()
	if !v.ok {
		return nil, false
	}
	return c.makeEngine(v.cfg), true
}

// remoteLocation is where host's repository is, as last saved.
func (c *Core) remoteLocation(host string) string {
	raw, ok, err := c.store.RemoteCache(host, cacheRepository)
	if err != nil || !ok {
		return ""
	}
	var res protocol.RepositoryResult
	if json.Unmarshal(raw, &res) != nil || res.Repository == nil {
		return ""
	}
	return res.Repository.Location
}

// forgetRemoteView drops what's worked out about opening host's
// repository and its cached lists: its repository or secrets changed, or
// it's unpaired.
func (c *Core) forgetRemoteView(host string) {
	c.mu.Lock()
	delete(c.views, host)
	c.dropRemoteListsLocked(host)
	c.mu.Unlock()
}

// dropRemoteListsLocked forgets host's cached snapshot lists and keep
// reasons: a job finished there, or its pins or policy changed.
func (c *Core) dropRemoteListsLocked(host string) {
	prefix := remoteKey(host, "")
	for k := range c.snapLists {
		if strings.HasPrefix(k, prefix) {
			delete(c.snapLists, k)
		}
	}
	for k := range c.keepCache {
		if strings.HasPrefix(k, prefix) {
			delete(c.keepCache, k)
		}
	}
	c.snapGen++
}

// saveRemote keeps a remote's reply for when it isn't answering.
func (c *Core) saveRemote(host, key string, raw json.RawMessage) {
	old, had, _ := c.store.RemoteCache(host, key)
	if had && string(old) == string(raw) {
		return
	}
	if err := c.store.SetRemoteCache(host, key, raw); err != nil {
		logf("saving %s's %s: %v", host, key, err)
		return
	}
	if key == cacheRepository {
		c.forgetRemoteView(host) // maybe a new location
	}
}

// remoteRead answers req, which names a remote, without the remote where
// it can; handled is false for everything that's the remote's to do.
func (c *Core) remoteRead(ctx context.Context, req protocol.Request) (out any, handled bool, err error) {
	host := req.Host
	c.remotes.mu.Lock()
	r := c.remotes.byID[host]
	c.remotes.mu.Unlock()
	if r == nil {
		return nil, false, nil // relay says there's no such host
	}
	params := func(v any) error {
		if len(req.Params) == 0 {
			return perr(protocol.ErrBadRequest, "%s needs params.", req.Method)
		}
		if err := json.Unmarshal(req.Params, v); err != nil {
			return perr(protocol.ErrBadRequest, "Bad params for %s: %v", req.Method, err)
		}
		return nil
	}
	switch req.Method {
	case protocol.MethodSources, protocol.MethodRepository:
		key := cacheSources
		if req.Method == protocol.MethodRepository {
			key = cacheRepository
		}
		// A remote already known to be off answers from the saved copy
		// at once: dialing it first cost every TUI start a second per
		// request (Alok, 2026-11-24). The follower keeps trying it and
		// saves fresh copies when it's back (syncRemote).
		c.remotes.mu.Lock()
		down := r.state == protocol.HostUnreachable && r.err != "" && r.err != "connecting"
		c.remotes.mu.Unlock()
		if down {
			if saved, ok, _ := c.store.RemoteCache(host, key); ok {
				return saved, true, nil
			}
		}
		raw, err := c.relay(ctx, req)
		if err == nil {
			c.saveRemote(host, key, raw)
			return raw, true, nil
		}
		var pe *protocol.Error
		if errors.As(err, &pe) && pe.Code == protocol.ErrHostUnreachable {
			if saved, ok, _ := c.store.RemoteCache(host, key); ok {
				return saved, true, nil
			}
		}
		return nil, true, err

	case protocol.MethodSnapshots:
		var p protocol.SnapshotsParams
		if err := params(&p); err != nil {
			return nil, true, err
		}
		engine, ok := c.remoteEngine(host)
		if !ok {
			return nil, false, nil
		}
		res, err := c.remoteSnapshots(ctx, r, engine, p.Source)
		return res, true, err

	case protocol.MethodLs:
		var p protocol.LsParams
		if err := params(&p); err != nil {
			return nil, true, err
		}
		if p.Snapshot == protocol.LiveSnapshot {
			return nil, false, nil // its disk
		}
		engine, ok := c.remoteEngine(host)
		if !ok {
			return nil, false, nil
		}
		all, err := c.listVia(ctx, p.Snapshot, p.Path, func() (Engine, error) { return engine, c.remoteBusy(r) })
		if err != nil {
			return nil, true, err
		}
		return lsPage(all, p), true, nil

	case protocol.MethodChanges:
		var p protocol.ChangesParams
		if err := params(&p); err != nil {
			return nil, true, err
		}
		engine, ok := c.remoteEngine(host)
		if !ok {
			return nil, false, nil
		}
		res, err := c.changes(ctx, p, func() (Engine, error) { return engine, c.remoteBusy(r) })
		return res, true, err

	case protocol.MethodDump:
		var p protocol.DumpParams
		if err := params(&p); err != nil {
			return nil, true, err
		}
		if p.Snapshot == protocol.LiveSnapshot {
			return nil, false, nil
		}
		engine, ok := c.remoteEngine(host)
		if !ok {
			return nil, false, nil
		}
		if err := c.remoteBusy(r); err != nil {
			return nil, true, err
		}
		data, size, err := engine.Read(ctx, p.Snapshot, p.Path, dumpLimit(p.Max))
		if err != nil {
			return nil, true, err
		}
		return dumpResult(data, size), true, nil
	}
	return nil, false, nil
}

// remoteBusy is the busy error while r is known to be cleaning up its
// repository: restic's exclusive lock would fail every read.
func (c *Core) remoteBusy(r *remote) error {
	c.remotes.mu.Lock()
	job := r.job
	c.remotes.mu.Unlock()
	if job != nil && (job.Kind == protocol.KindPrune || job.Kind == protocol.KindRewrite) {
		return perrData(protocol.ErrBusy, protocol.BusyData{Job: *job},
			"%s's repository is being cleaned up (started %s); browsing works again when it finishes.", r.peer.Name, job.Started.Local().Format("15:04"))
	}
	return nil
}

// remoteSources is host's sources as last saved.
func (c *Core) remoteSources(host string) []protocol.Source {
	raw, ok, err := c.store.RemoteCache(host, cacheSources)
	if err != nil || !ok {
		return nil
	}
	var res protocol.SourcesResult
	if json.Unmarshal(raw, &res) != nil {
		return nil
	}
	return res.Sources
}

// remoteMeta is what r's database knows about source's snapshots: asked
// of r when it's answering (and saved), else the saved copy.
func (c *Core) remoteMeta(ctx context.Context, r *remote, source string) protocol.SnapshotMetaResult {
	host := r.peer.Name
	var meta protocol.SnapshotMetaResult
	c.remotes.mu.Lock()
	up := r.state == protocol.HostOK
	c.remotes.mu.Unlock()
	if up {
		mctx, cancel := context.WithTimeout(ctx, remoteMetaWait)
		params, _ := json.Marshal(protocol.SnapshotMetaParams{Source: source})
		raw, err := c.relay(mctx, protocol.Request{Host: host, Method: protocol.MethodSnapshotMeta, Params: params})
		cancel()
		if err == nil && json.Unmarshal(raw, &meta) == nil {
			old, had, _ := c.store.RemoteCache(host, cacheMeta+source)
			if !had || string(old) != string(raw) {
				if err := c.store.SetRemoteCache(host, cacheMeta+source, raw); err != nil {
					logf("saving %s's snapshot notes: %v", host, err)
				}
				// The pins may have changed what's kept and why.
				c.mu.Lock()
				delete(c.keepCache, remoteKey(host, "src:"+source))
				c.mu.Unlock()
			}
			return meta
		}
	}
	if raw, ok, _ := c.store.RemoteCache(host, cacheMeta+source); ok {
		_ = json.Unmarshal(raw, &meta)
	}
	return meta
}

// remoteSnapshots is the snapshots method for one of r's sources, from
// r's repository read here.
func (c *Core) remoteSnapshots(ctx context.Context, r *remote, engine Engine, source string) (protocol.SnapshotsResult, error) {
	host := r.peer.Name
	res := protocol.SnapshotsResult{Snapshots: []protocol.Snapshot{}}
	if err := c.remoteBusy(r); err != nil {
		return res, err
	}
	sources := c.remoteSources(host)
	meta := c.remoteMeta(ctx, r, source)
	pins := map[string]bool{}
	for _, id := range meta.Pinned {
		pins[id] = true
	}
	view := func(e EngineSnapshot, keptBy []string) protocol.Snapshot {
		if keptBy == nil {
			keptBy = []string{}
		}
		added := e.BytesAdded
		if n, ok := meta.Added[e.ID]; ok && e.Original != "" {
			added = n
		}
		return protocol.Snapshot{ID: e.ID, ShortID: e.ShortID, Time: e.Time, End: e.End, Paths: e.Paths, Hostname: e.Hostname,
			Tags: e.Tags, Files: e.Files, Bytes: e.Bytes, BytesAdded: added,
			KeptBy: keptBy, Pinned: pins[e.ID], Note: meta.Notes[e.ID]}
	}
	if source == protocol.UntaggedSource {
		all, err := c.snapshotsKeyed(ctx, engine, remoteKey(host, ""), "")
		if err != nil {
			return res, err
		}
		known := map[string]bool{}
		for _, s := range sources {
			known[s.ID] = true
		}
		for _, e := range all {
			if !known[sourceOf(e)] && !slices.Contains(e.Tags, stateTag) {
				res.Snapshots = append(res.Snapshots, view(e, nil))
			}
		}
		return res, nil
	}
	tag := "src:" + source
	key := remoteKey(host, tag)
	snaps, err := c.snapshotsKeyed(ctx, engine, key, tag)
	if err != nil {
		return res, err
	}
	if len(snaps) > 0 {
		c.warmTree(engine, newestSnapshot(snaps))
	}
	i := slices.IndexFunc(sources, func(s protocol.Source) bool { return s.ID == source })
	if i < 0 && len(snaps) == 0 {
		return res, perr(protocol.ErrNotFound, "No source with id %s on %s.", source, host)
	}
	c.mu.Lock()
	reasons, cached := c.keepCache[key]
	c.mu.Unlock()
	if !cached && i >= 0 {
		// Why each is kept, by the source's policy as last seen; shown
		// without when that isn't known or restic can't say.
		if got, err := engine.KeepReasons(ctx, tag, sources[i].Retention, pins); err == nil {
			reasons = got
			c.mu.Lock()
			c.keepCache[key] = got
			c.mu.Unlock()
		}
	}
	for _, e := range snaps {
		res.Snapshots = append(res.Snapshots, view(e, reasons[e.ID]))
	}
	return res, nil
}

// snapshotMeta is snapshot_meta: this daemon's own pins for a source,
// and its notes and recalculated sizes (few, so all of them).
func (c *Core) snapshotMeta(p protocol.SnapshotMetaParams) (protocol.SnapshotMetaResult, error) {
	res := protocol.SnapshotMetaResult{Pinned: []string{}, Notes: map[string]string{}, Added: map[string]int64{}}
	if p.Source != protocol.UntaggedSource {
		pins, err := c.store.Pinned(p.Source)
		if err != nil {
			return res, err
		}
		for id := range pins {
			res.Pinned = append(res.Pinned, id)
		}
		slices.Sort(res.Pinned)
	}
	var err error
	if res.Notes, err = c.store.Notes(); err != nil {
		return res, err
	}
	if res.Added, err = c.store.AllAdded(); err != nil {
		return res, err
	}
	// A controller lists this source's snapshots itself now, so nothing
	// else notices rewritten ones still missing their figure.
	if s, ok, err := c.store.Source(p.Source); err == nil && ok {
		go func() {
			engine, err := c.readable()
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), statsTimeout)
			defer cancel()
			if snaps, err := c.snapshots(ctx, engine, s.Tag()); err == nil {
				c.recalcAddedLater(engine, s, snaps)
			}
		}()
	}
	return res, nil
}

// restoreFrom is restore with from: a remote's snapshot restored onto
// this machine, as one of this machine's jobs.
func (c *Core) restoreFrom(p protocol.RestoreParams) (protocol.JobResult, error) {
	c.remotes.mu.Lock()
	r := c.remotes.byID[p.From]
	c.remotes.mu.Unlock()
	if r == nil {
		return protocol.JobResult{}, perr(protocol.ErrNotFound, "No paired host named %s.", p.From)
	}
	if p.Target == protocol.RestoreOriginal || !filepath.IsAbs(p.Target) {
		return protocol.JobResult{}, perr(protocol.ErrBadRequest, "Restoring %s's files here needs a folder on %s: an absolute path.", p.From, c.Name)
	}
	if _, ok := c.remoteEngine(p.From); !ok {
		return protocol.JobResult{}, perr(protocol.ErrNotConfigured, "%s doesn't hold what opens %s's repository (its location and password).", c.Name, p.From)
	}
	job := c.enqueue(protocol.KindRestore, nil, func(ctx context.Context, job protocol.Job) outcome {
		engine, ok := c.remoteEngine(p.From)
		if !ok {
			return outcome{status: protocol.RunFailed, err: "can't open " + p.From + "'s repository from here"}
		}
		o := result(engine.Restore(ctx, p.Snapshot, p.Paths, p.Target, p.Overwrite, func(pr Progress) {
			c.mu.Lock()
			c.publish(protocol.Event{Name: protocol.EventProgress, Progress: &protocol.Progress{JobID: job.ID, Percent: pr.Percent,
				FilesDone: pr.FilesDone, TotalFiles: pr.TotalFiles, BytesDone: pr.BytesDone, TotalBytes: pr.TotalBytes, Current: pr.Current}})
			c.mu.Unlock()
		}))
		o.snapshot = p.Snapshot
		return o
	})
	return protocol.JobResult{Job: job}, nil
}

// syncRemote saves what reading r's repository takes while r answers:
// its sources always, and with full (a new connection) its repository
// and what it's running. One at a time per remote; a request during one
// runs another after.
func (c *Core) syncRemote(ctx context.Context, r *remote, full bool) {
	c.remotes.mu.Lock()
	if r.syncing {
		r.syncMore = true
		r.syncFull = r.syncFull || full
		c.remotes.mu.Unlock()
		return
	}
	r.syncing = true
	c.remotes.mu.Unlock()
	host := r.peer.Name
	for {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if raw, err := c.relay(sctx, protocol.Request{Host: host, Method: protocol.MethodSources}); err == nil {
			c.saveRemote(host, cacheSources, raw)
		}
		if full {
			if raw, err := c.relay(sctx, protocol.Request{Host: host, Method: protocol.MethodRepository}); err == nil {
				c.saveRemote(host, cacheRepository, raw)
			}
			if raw, err := c.relay(sctx, protocol.Request{Host: host, Method: protocol.MethodStatus}); err == nil {
				var st protocol.StatusResult
				if json.Unmarshal(raw, &st) == nil {
					c.remotes.mu.Lock()
					r.job = st.Job
					c.remotes.mu.Unlock()
				}
			}
		}
		cancel()
		c.remotes.mu.Lock()
		if !r.syncMore || ctx.Err() != nil {
			r.syncing, r.syncMore, r.syncFull = false, false, false
			c.remotes.mu.Unlock()
			return
		}
		full, r.syncMore, r.syncFull = r.syncFull, false, false
		c.remotes.mu.Unlock()
	}
}

// remoteEvent keeps what's known about r current from its own events.
func (c *Core) remoteEvent(ctx context.Context, r *remote, e protocol.Event) {
	switch {
	case e.JobStarted != nil:
		job := e.JobStarted.Job
		c.remotes.mu.Lock()
		r.job = &job
		c.remotes.mu.Unlock()
	case e.JobFinished != nil:
		c.remotes.mu.Lock()
		r.job = nil
		c.remotes.mu.Unlock()
		// Any job can add or drop snapshots.
		c.mu.Lock()
		c.dropRemoteListsLocked(r.peer.Name)
		c.mu.Unlock()
	case e.StateChanged != nil:
		c.remotes.mu.Lock()
		r.job = e.StateChanged.Job
		c.remotes.mu.Unlock()
	case e.SourceChanged != nil:
		// Its policy may have changed (what's kept and why), and the
		// saved list is out of date.
		c.mu.Lock()
		c.dropRemoteListsLocked(r.peer.Name)
		c.mu.Unlock()
		go c.syncRemote(ctx, r, false)
	}
}

// remoteWritten is called after a relayed request that changes r's
// sources, pins or repository went through.
func (c *Core) remoteWritten(r *remote, method string) {
	switch method {
	case protocol.MethodSourceAdd, protocol.MethodSourceSet, protocol.MethodSourceRemove, protocol.MethodDefaultsSet:
		c.mu.Lock()
		c.dropRemoteListsLocked(r.peer.Name)
		c.mu.Unlock()
		c.remotes.mu.Lock()
		ctx := c.remotes.ctx
		c.remotes.mu.Unlock()
		if ctx != nil {
			go c.syncRemote(ctx, r, false)
		}
	case protocol.MethodSnapshotSet:
		c.mu.Lock()
		c.dropRemoteListsLocked(r.peer.Name)
		c.mu.Unlock()
	case protocol.MethodRepoSet:
		c.forgetRemoteView(r.peer.Name)
	}
}
