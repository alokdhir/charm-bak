package daemon

import (
	"context"
	"os"
	"slices"
	"time"

	"charm-bak/internal/protocol"
)

// The daemon's own database (sources, run history, notes, pins, peers,
// defaults) is backed up into its repository once a day, so a lost disk
// loses no configuration: a copy made with SQLite's VACUUM INTO (a
// consistent snapshot while the daemon runs) beside the database, backed
// up under its own tag with its own retention. These snapshots are no
// source's and don't show under "untagged". To get the database back:
//
//	restic restore latest --tag charm-bak:state --target /tmp/x
//
// The identity key and secrets are not in it: a lost remote is paired
// again, and its repository password is the Mac's Keychain item.

const (
	stateTag         = "charm-bak:state"
	stateBackupEvery = 24 * time.Hour
	stateRetryEvery  = time.Hour
)

var stateRetention = protocol.Retention{Daily: 7, Weekly: 4, Monthly: 12}

// stateBackupIfDue queues the state backup when the last one is a day
// old (or there's none), unless one is already queued.
func (c *Core) stateBackupIfDue(engine Engine, now time.Time) {
	if c.StateCopy == "" {
		return
	}
	last, _ := c.store.LastRepoRun(protocol.KindState)
	every := stateBackupEvery
	if last != nil && last.Status != protocol.RunOK {
		every = stateRetryEvery // failed (say the repository was locked): sooner
	}
	if last != nil && now.Sub(last.Ended) < every {
		return
	}
	c.mu.Lock()
	queued := (c.current != nil && c.current.job.Kind == protocol.KindState) ||
		slices.ContainsFunc(c.queue, func(p *pendingJob) bool { return p.job.Kind == protocol.KindState })
	c.mu.Unlock()
	if queued {
		return
	}
	c.enqueue(protocol.KindState, nil, func(ctx context.Context, _ protocol.Job) outcome {
		return result(c.backupState(ctx, engine))
	})
}

func (c *Core) backupState(ctx context.Context, engine Engine) error {
	if err := c.store.CopyTo(c.StateCopy); err != nil {
		logf("state backup: copying the database: %v", err)
		return err
	}
	res, err := engine.Backup(ctx, BackupSpec{Tag: stateTag, Paths: []string{c.StateCopy}}, nil)
	if err != nil {
		return err
	}
	if err := engine.ApplyRetention(ctx, stateTag, stateRetention, nil); err != nil {
		return err
	}
	logf("state backup: snapshot %.8s", res.Snapshot)
	return nil
}

// CopyTo writes a consistent copy of the database to path (replacing
// it), owner-only.
func (s *Store) CopyTo(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := s.db.Exec("VACUUM INTO ?", path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
