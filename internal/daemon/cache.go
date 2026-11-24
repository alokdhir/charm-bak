package daemon

import (
	"context"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"charm-bak/internal/protocol"
)

// Every restic call pays for starting up and loading the index from the
// repository (about 0.8s on S3), so browsing folder by folder is slow.
// Two caches fix the common case:
//
//   - trees: a snapshot never changes, so the first folder opened in it
//     starts one listing of the whole snapshot in the background, and
//     every folder after that is answered from memory. The most recently
//     used few stay in memory; with a cache database (treedb.go) every
//     loaded tree is kept there instead, one row per folder, so a
//     snapshot opened once is instant from then on, across daemon
//     restarts too, and a folder is read without holding its whole tree.
//     Listing a source's snapshots warms the newest one's tree. The same
//     cache serves this machine's repository and the remotes' the
//     controller reads (remoteread.go).
//   - snapshot lists, per source tag, until a job finishes (any job can
//     add or drop snapshots) or snapListTTL passes (restic run by hand).
//   - the repository's size (status), read when first asked for and again
//     whenever the queue empties after a job that can change it.

const (
	treesKept = 8
	// treeDiskMax caps the trees kept on disk; the least recently used
	// go first.
	treeDiskMax  = 512 << 20
	treeTimeout  = 5 * time.Minute
	snapListTTL  = 10 * time.Minute
	statsTimeout = 2 * time.Minute
)

type treeCache struct {
	mu    sync.Mutex
	trees map[string]*snapTree // by snapshot id as the client sent it
	order []string             // least recently used first
	// db keeps loaded trees between runs (treedb.go); nil: memory only,
	// the most recently used treesKept.
	db      *treeDB
	warming atomic.Bool // one warm-up at a time
}

// snapTree is a tree in memory, or (dirs nil, done false) the claim of
// whoever is loading it.
type snapTree struct {
	done bool
	dirs map[string][]protocol.Entry
}

// lookup returns dir's children if snapshot's tree is loaded. found is
// false when the tree is loaded but dir isn't in it.
func (t *treeCache) lookup(snapshot, dir string) (entries []protocol.Entry, loaded, found bool) {
	dir = path.Clean("/" + dir)
	if t.db != nil {
		return t.db.lookup(snapshot, dir)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.trees[snapshot]
	if tr == nil || !tr.done {
		return nil, false, false
	}
	t.touchLocked(snapshot)
	entries, found = tr.dirs[dir]
	return entries, true, found
}

// has reports whether snapshot's tree is loaded or loading.
func (t *treeCache) has(snapshot string) bool {
	t.mu.Lock()
	tr := t.trees[snapshot]
	t.mu.Unlock()
	return tr != nil || t.db != nil && t.db.has(snapshot)
}

// start claims snapshot for loading; false if it's loaded or loading.
func (t *treeCache) start(snapshot string) bool {
	if t.db != nil && t.db.has(snapshot) {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.trees == nil {
		t.trees = map[string]*snapTree{}
	}
	if t.trees[snapshot] != nil {
		return false
	}
	t.trees[snapshot] = &snapTree{}
	return true
}

// finish stores a loaded tree, or forgets the attempt so a later request
// tries again. In memory it evicts the least recently used past
// treesKept; the database trims itself by size.
func (t *treeCache) finish(snapshot string, dirs map[string][]protocol.Entry, err error) {
	if err == nil && t.db != nil {
		err = t.db.store(snapshot, dirs)
		if err == nil {
			dirs = nil // it's in the database: only the claim goes
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil || dirs == nil {
		delete(t.trees, snapshot)
		return
	}
	t.trees[snapshot] = &snapTree{done: true, dirs: dirs}
	t.touchLocked(snapshot)
	for len(t.order) > treesKept {
		delete(t.trees, t.order[0])
		t.order = t.order[1:]
	}
}

// clear forgets every tree (the repository changed).
func (t *treeCache) clear() {
	t.mu.Lock()
	t.trees, t.order = nil, nil
	t.mu.Unlock()
}

func (t *treeCache) touchLocked(snapshot string) {
	t.order = slices.DeleteFunc(t.order, func(s string) bool { return s == snapshot })
	t.order = append(t.order, snapshot)
}

// warmTree loads snapshot's tree in the background if it isn't cached,
// one at a time: listing a source's snapshots calls it for the newest, so
// the first folder opened there is already loaded. A warm-up under way
// isn't interrupted; the snapshot skipped loads when a folder is opened.
func (c *Core) warmTree(engine Engine, snapshot string) {
	if c.trees.has(snapshot) || !c.trees.warming.CompareAndSwap(false, true) {
		return
	}
	if !c.trees.start(snapshot) {
		c.trees.warming.Store(false)
		return
	}
	go func() {
		defer c.trees.warming.Store(false)
		tctx, cancel := context.WithTimeout(context.Background(), treeTimeout)
		defer cancel()
		dirs, err := engine.Tree(tctx, snapshot)
		c.trees.finish(snapshot, dirs, err)
	}()
}

// list answers a directory listing from the tree cache when it can;
// otherwise it starts the snapshot's tree loading and asks the engine for
// just this directory meanwhile. A loaded tree is used even while a prune
// blocks other reads: its snapshot's contents can't have changed.
func (c *Core) list(ctx context.Context, snapshot, dir string) ([]protocol.Entry, error) {
	return c.listVia(ctx, snapshot, dir, c.readable)
}

// listVia is list with the engine to read through: this machine's, or a
// remote's repository (remoteread.go). A snapshot id names one snapshot
// whichever repository it's in, so the tree cache is shared.
func (c *Core) listVia(ctx context.Context, snapshot, dir string, readable func() (Engine, error)) ([]protocol.Entry, error) {
	if entries, loaded, found := c.trees.lookup(snapshot, dir); loaded {
		if !found {
			return nil, perr(protocol.ErrNotFound, "%s isn't in snapshot %s.", path.Clean("/"+dir), snapshot)
		}
		return entries, nil
	}
	engine, err := readable()
	if err != nil {
		return nil, err
	}
	if c.trees.start(snapshot) {
		go func() {
			tctx, cancel := context.WithTimeout(context.Background(), treeTimeout)
			defer cancel()
			dirs, err := engine.Tree(tctx, snapshot)
			c.trees.finish(snapshot, dirs, err)
		}()
	}
	return engine.List(ctx, snapshot, dir)
}

// changesMax is how many changed paths one reply lists.
const changesMax = 5000

// changes is what snapshot has that base doesn't, from the cache database
// when it's been asked before (two snapshots never change), else from the
// engine.
func (c *Core) changes(ctx context.Context, p protocol.ChangesParams, readable func() (Engine, error)) (protocol.ChangesResult, error) {
	if p.Snapshot == "" || p.Base == "" {
		return protocol.ChangesResult{}, perr(protocol.ErrBadRequest, "changes needs a snapshot and the earlier one to compare it with.")
	}
	if db := c.trees.db; db != nil {
		if res, ok := db.changes(p.Snapshot, p.Base); ok {
			return res, nil
		}
	}
	engine, err := readable()
	if err != nil {
		return protocol.ChangesResult{}, err
	}
	diff, err := engine.Changes(ctx, p.Base, p.Snapshot)
	if err != nil {
		return protocol.ChangesResult{}, err
	}
	res := protocol.ChangesResult{Changes: []protocol.PathChange{}, Bytes: diff.BytesAdded}
	for _, ch := range diff.Paths {
		switch ch.Change {
		case protocol.ChangeAdded:
			res.Added++
		case protocol.ChangeRemoved:
			res.Removed++
		case protocol.ChangeTouched:
			res.Touched++
		default:
			res.Changed++
		}
		if len(res.Changes) < changesMax {
			res.Changes = append(res.Changes, ch)
		}
	}
	if db := c.trees.db; db != nil {
		db.storeChanges(p.Snapshot, p.Base, res)
	}
	return res, nil
}

type snapList struct {
	snaps []EngineSnapshot
	at    time.Time
}

// snapshots is engine.Snapshots through the snapshot-list cache.
func (c *Core) snapshots(ctx context.Context, engine Engine, tag string) ([]EngineSnapshot, error) {
	return c.snapshotsKeyed(ctx, engine, tag, tag)
}

// snapshotsKeyed caches under key: the tag for this machine's repository,
// host and tag for a remote's (remoteKey), so machines never mix.
func (c *Core) snapshotsKeyed(ctx context.Context, engine Engine, key, tag string) ([]EngineSnapshot, error) {
	c.mu.Lock()
	hit, ok := c.snapLists[key]
	gen := c.snapGen
	c.mu.Unlock()
	if ok && time.Since(hit.at) < snapListTTL {
		return hit.snaps, nil
	}
	snaps, err := engine.Snapshots(ctx, tag)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.snapGen == gen { // no job finished meanwhile
		c.snapLists[key] = snapList{snaps: snaps, at: time.Now()}
	}
	c.mu.Unlock()
	return snaps, nil
}

// dropSnapshotListsLocked forgets cached snapshot lists; called when a
// job finishes.
func (c *Core) dropSnapshotListsLocked() {
	clear(c.snapLists)
	c.snapGen++
}

// refreshStatsLocked reads the repository's size in the background, one
// read at a time, and tells subscribers when it changes (they re-ask for
// status). A failed read keeps the last value. Call with c.mu held.
func (c *Core) refreshStatsLocked() {
	if c.statsBusy {
		c.statsMore = true
		return
	}
	if cur := c.currentJobLocked(); cur != nil && cur.Kind == protocol.KindPrune {
		return // the next finish refreshes
	}
	c.statsBusy = true
	go func() {
		var s *RepoStats
		if engine, err := c.engine(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), statsTimeout)
			if got, err := engine.Stats(ctx); err == nil {
				s = &got
			} else {
				logf("reading the repository size: %v", err)
			}
			cancel()
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.statsBusy = false
		if s != nil && (c.stats == nil || *c.stats != *s) {
			c.stats = s
			c.publish(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: c.stateLocked(), Job: c.currentJobLocked()}})
		}
		if c.statsMore {
			c.statsMore = false
			c.refreshStatsLocked()
		}
	}()
}
