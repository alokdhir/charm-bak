package daemon

import (
	"context"
	"fmt"
	"path"
	"slices"
	"time"

	"charm-bak/internal/protocol"
)

// sourceRewrite handles source_rewrite: paths left out of a source from
// now on, taken out of the snapshots it already has too. A dry run
// counts the snapshots that would change. The real one is a job: restic
// rewrite --forget (exclusive lock, like prune), then each replaced
// snapshot's note and pin move to its replacement, then a prune frees
// the space.
func (c *Core) sourceRewrite(ctx context.Context, p protocol.SourceRewriteParams) (protocol.SourceRewriteResult, error) {
	s, ok, err := c.store.Source(p.ID)
	if err != nil {
		return protocol.SourceRewriteResult{}, err
	}
	if !ok {
		return protocol.SourceRewriteResult{}, perr(protocol.ErrNotFound, "No source with id %s.", p.ID)
	}
	if len(p.Excludes) == 0 {
		return protocol.SourceRewriteResult{}, perr(protocol.ErrBadRequest, "Nothing to remove.")
	}
	for _, e := range p.Excludes {
		if !path.IsAbs(e) {
			return protocol.SourceRewriteResult{}, perr(protocol.ErrBadRequest, "%q isn't an absolute path.", e)
		}
	}
	if p.DryRun {
		engine, err := c.readable()
		if err != nil {
			return protocol.SourceRewriteResult{}, err
		}
		n, err := engine.Rewrite(ctx, s.Tag(), p.Excludes, true)
		if err != nil {
			return protocol.SourceRewriteResult{}, err
		}
		return protocol.SourceRewriteResult{Snapshots: n}, nil
	}
	engine, err := c.engine()
	if err != nil {
		return protocol.SourceRewriteResult{}, err
	}
	id, tag, excludes := s.ID, s.Tag(), p.Excludes
	logf("source %s (%s): removing %v from its snapshots", s.Name, s.ID, excludes)
	job := c.enqueue(protocol.KindRewrite, &id, func(ctx context.Context, _ protocol.Job) outcome {
		n, rerr := engine.Rewrite(ctx, tag, excludes, false)
		// Notes and pins follow whatever was rewritten, even when the
		// rewrite failed or was cancelled part-way: restic replaces
		// snapshots one at a time, and a pin left on a replaced ID would
		// let the next retention pass forget the pinned snapshot. So this
		// runs on its own clock, not the cancelled job's.
		mctx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		snaps, err := c.moveRewritten(mctx, engine, tag)
		done()
		// Keep reasons were worked out for the replaced IDs.
		c.mu.Lock()
		delete(c.keepCache, tag)
		c.mu.Unlock()
		if rerr != nil {
			if err != nil {
				logf("source %s: after a rewrite that didn't finish, couldn't move notes and pins: %v", id, err)
			}
			return result(rerr)
		}
		if err != nil {
			return result(fmt.Errorf("rewrote %d snapshots, but couldn't list them to move notes and pins: %w", n, err))
		}
		logf("source %s: rewrote %d snapshots; pruning", id, n)
		if err := engine.Maintain(ctx); err != nil {
			return result(err)
		}
		c.recalcAdded(ctx, engine, tag, snaps)
		return result(nil)
	})
	return protocol.SourceRewriteResult{Job: &job}, nil
}

// moveRewritten carries notes and pins from each replaced snapshot of tag
// to its replacement (restic records the one it replaced as "original").
// Moving one that was moved before does nothing.
func (c *Core) moveRewritten(ctx context.Context, engine Engine, tag string) ([]EngineSnapshot, error) {
	snaps, err := engine.Snapshots(ctx, tag)
	if err != nil {
		return nil, err
	}
	for _, e := range snaps {
		if e.Original != "" {
			if err := c.store.MoveSnapshot(e.Original, e.ID); err != nil {
				logf("moving %s's note and pin to %s: %v", e.Original, e.ID, err)
			}
		}
	}
	return snaps, nil
}

// recalcAdded works out what each rewritten snapshot adds over the one
// before it (restic diff), where that isn't known yet: restic copies the
// original backup's "added" figure into the new snapshot, which counted
// what the rewrite took out. The oldest snapshot adds everything it has.
func (c *Core) recalcAdded(ctx context.Context, engine Engine, tag string, snaps []EngineSnapshot) {
	byTime := slices.Clone(snaps)
	slices.SortFunc(byTime, func(a, b EngineSnapshot) int { return a.Time.Compare(b.Time) })
	done := 0
	for i, e := range byTime {
		if e.Original == "" {
			continue
		}
		if _, ok, err := c.store.Added(e.ID); err != nil || ok {
			continue
		}
		n := e.Bytes
		if i > 0 {
			var err error
			if n, err = engine.Added(ctx, byTime[i-1].ID, e.ID); err != nil {
				logf("recalculating what %s adds: %v", e.ShortID, err)
				return
			}
		}
		if err := c.store.SetAdded(e.ID, n); err != nil {
			return
		}
		done++
	}
	if done > 0 {
		logf("%s: recalculated what %d rewritten snapshots add", tag, done)
	}
}

// recalcAddedLater runs recalcAdded in the background for a source whose
// snapshots include rewritten ones without a figure (rewritten before
// this was built, or by hand), once at a time per source; the source's
// view is republished when it's done, so clients reload its snapshots.
func (c *Core) recalcAddedLater(engine Engine, s StoredSource, snaps []EngineSnapshot) {
	missing := slices.ContainsFunc(snaps, func(e EngineSnapshot) bool {
		if e.Original == "" {
			return false
		}
		_, ok, err := c.store.Added(e.ID)
		return err == nil && !ok
	})
	if !missing {
		return
	}
	if _, busy := c.recalcing.LoadOrStore(s.ID, true); busy {
		return
	}
	go func() {
		defer c.recalcing.Delete(s.ID)
		c.recalcAdded(context.Background(), engine, s.Tag(), snaps)
		c.sourceChanged(s)
	}()
}
