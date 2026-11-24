package daemon

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"charm-bak/internal/protocol"

	_ "modernc.org/sqlite"
)

// Store is the daemon's state in SQLite (pure Go, no cgo): settings and
// defaults, sources, run history, snapshot notes and pins. Only the
// daemon opens it.
type Store struct {
	db *sql.DB
}

// BuiltinDefaults apply until the user sets their own. Retention is the
// TUI's "1 year" (Alok, 2026-11-24, replacing six counts with a choice):
// every backup from the last day, then one a day for a month, one a week
// for 3 months, one a month for a year.
var BuiltinDefaults = Defaults{
	Schedule:  &protocol.Schedule{Every: 3600},
	Retention: protocol.Retention{Last: 10, Hourly: 24, Daily: 30, Weekly: 12, Monthly: 12},
	// No patterns every source skips: a skip belongs to the sources it's
	// for (Alok, 2026-11-22, dropping a built-in *.log).
	Excludes: []string{},
}

// Defaults are what sources inherit.
type Defaults struct {
	Schedule  *protocol.Schedule `json:"schedule"`
	Retention protocol.Retention `json:"retention"`
	Excludes  []string           `json:"excludes"` // every source skips these, before its own
}

// view is the defaults as the protocol sends them; excludes never null.
func (d Defaults) view() protocol.Defaults {
	ex := d.Excludes
	if ex == nil {
		ex = []string{}
	}
	return protocol.Defaults{Schedule: d.Schedule, Retention: d.Retention, Excludes: ex}
}

// StoredSource is a source as stored: a nil Retention, or ScheduleSet
// false, means "inherit the default".
type StoredSource struct {
	ID          string
	Name        string
	Paths       []string
	Excludes    []string
	Schedule    *protocol.Schedule // nil with ScheduleSet = manual only
	ScheduleSet bool
	Retention   *protocol.Retention
	Paused      bool
	Created     time.Time
}

// Tag is how the source's snapshots are marked in the repository.
func (s StoredSource) Tag() string { return "src:" + s.ID }

// Effective is a source's policy with defaults filled in.
type Effective struct {
	Schedule  *protocol.Schedule
	Retention protocol.Retention
	Inherited []string
}

var migrations = []string{
	`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE sources (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		paths TEXT NOT NULL,
		excludes TEXT NOT NULL,
		schedule_set INTEGER NOT NULL DEFAULT 0,
		schedule_every INTEGER,
		retention TEXT,
		paused INTEGER NOT NULL DEFAULT 0,
		created REAL NOT NULL
	);
	CREATE TABLE runs (
		job TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		source TEXT,
		started REAL NOT NULL,
		ended REAL NOT NULL,
		status TEXT NOT NULL,
		files_new INTEGER NOT NULL DEFAULT 0,
		files_changed INTEGER NOT NULL DEFAULT 0,
		bytes_added INTEGER NOT NULL DEFAULT 0,
		snapshot TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX runs_source_ended ON runs (source, ended);
	CREATE TABLE notes (snapshot TEXT PRIMARY KEY, note TEXT NOT NULL);`,
	// Pins live in the daemon, not as restic tags: restic's tag command
	// rewrites a snapshot under a new ID, which would orphan its note.
	`CREATE TABLE pins (snapshot TEXT PRIMARY KEY, source TEXT NOT NULL);`,
	// What a backup covered in all, so a source's size needs no restic call.
	`ALTER TABLE runs ADD COLUMN files INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN bytes INTEGER NOT NULL DEFAULT 0;`,
	// Paired daemons (peers.go): remotes this one relays to (with an
	// address), and controllers allowed to connect to it (without).
	`CREATE TABLE peers (name TEXT PRIMARY KEY, address TEXT NOT NULL, pin TEXT NOT NULL UNIQUE, added REAL NOT NULL);`,
	// What a rewritten snapshot adds over the one before it, recalculated
	// (rewrite.go): restic copies the original backup's figure.
	`CREATE TABLE snapshot_added (snapshot TEXT PRIMARY KEY, bytes INTEGER NOT NULL);`,
	// The built-in defaults changed (to "1 year"): a machine already
	// backing up on the old ones keeps them, saved as its own.
	`INSERT INTO settings (key, value)
	SELECT 'defaults', '{"schedule":{"every":3600},"retention":{"last":10,"hourly":24,"daily":7,"weekly":4,"monthly":12,"yearly":3},"excludes":[]}'
	WHERE NOT EXISTS (SELECT 1 FROM settings WHERE key = 'defaults') AND EXISTS (SELECT 1 FROM sources);`,
	// What a controller saved about each remote at last contact
	// (remoteread.go): its repository, its sources and its snapshots'
	// pins and notes, so its backups can be read while it's off.
	`CREATE TABLE remote_cache (host TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (host, key));`,
}

// OpenStore opens (creating and migrating) the database at path;
// ":memory:" for tests.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; also keeps ":memory:" a single database
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Times are stored as seconds since 1970, rounded to the millisecond so a
// value read back is identical to the one written.
func toDB(t time.Time) float64 { return math.Round(float64(t.UnixMilli())) / 1000 }

func fromDB(f float64) time.Time { return time.UnixMilli(int64(math.Round(f * 1000))) }

// Settings.

// Setting decodes the JSON value stored under key into v; false if unset.
func (s *Store) Setting(key string, v any) (bool, error) {
	var raw string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (s *Store) SetSetting(key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, string(raw))
	return err
}

func (s *Store) Defaults() (Defaults, error) {
	d := BuiltinDefaults
	// Its own schedule: decoding fills the pointee, which would otherwise
	// be the built-in one every caller shares.
	if d.Schedule != nil {
		sched := *d.Schedule
		d.Schedule = &sched
	}
	d.Excludes = slices.Clone(d.Excludes)
	_, err := s.Setting("defaults", &d)
	return d, err
}

func (s *Store) SetDefaults(d Defaults) error { return s.SetSetting("defaults", d) }

// Repository is the configured repository, or nil before setup.
func (s *Store) Repository() (*RepositoryConfig, error) {
	var r RepositoryConfig
	ok, err := s.Setting("repository", &r)
	if !ok || err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) SetRepository(r RepositoryConfig) error { return s.SetSetting("repository", r) }

// Sources.

const sourceCols = "id, name, paths, excludes, schedule_set, schedule_every, retention, paused, created"

func scanSource(row interface{ Scan(...any) error }) (StoredSource, error) {
	var src StoredSource
	var paths, excludes string
	var every sql.NullInt64
	var retention sql.NullString
	var created float64
	if err := row.Scan(&src.ID, &src.Name, &paths, &excludes, &src.ScheduleSet, &every, &retention, &src.Paused, &created); err != nil {
		return src, err
	}
	json.Unmarshal([]byte(paths), &src.Paths)
	json.Unmarshal([]byte(excludes), &src.Excludes)
	if src.Excludes == nil {
		src.Excludes = []string{}
	}
	if every.Valid {
		src.Schedule = &protocol.Schedule{Every: every.Int64}
	}
	if retention.Valid {
		var r protocol.Retention
		if json.Unmarshal([]byte(retention.String), &r) == nil {
			src.Retention = &r
		}
	}
	src.Created = fromDB(created)
	return src, nil
}

// Sources lists every source, oldest first.
func (s *Store) Sources() ([]StoredSource, error) {
	// By name, ignoring case (Alok, 2026-11-20): predictable, and a new
	// source lands where you'd look for it.
	rows, err := s.db.Query("SELECT " + sourceCols + " FROM sources ORDER BY name COLLATE NOCASE, created, rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredSource
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// Source returns one source; ok is false if there's none with that id.
func (s *Store) Source(id string) (StoredSource, bool, error) {
	src, err := scanSource(s.db.QueryRow("SELECT "+sourceCols+" FROM sources WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return src, false, nil
	}
	return src, err == nil, err
}

// AddSource stores a new source under a fresh random 8-hex-digit id.
// Leave Schedule nil and ScheduleSet false (and Retention nil) to inherit.
func (s *Store) AddSource(src StoredSource, now time.Time) (StoredSource, error) {
	for {
		b := make([]byte, 4)
		rand.Read(b)
		src.ID = hex.EncodeToString(b)
		if _, exists, err := s.Source(src.ID); err != nil {
			return src, err
		} else if !exists {
			break
		}
	}
	if src.Schedule != nil {
		src.ScheduleSet = true
	}
	if src.Excludes == nil {
		src.Excludes = []string{}
	}
	src.Created = fromDB(toDB(now))
	return src, s.save(src, true)
}

func (s *Store) UpdateSource(src StoredSource) error { return s.save(src, false) }

func (s *Store) save(src StoredSource, insert bool) error {
	paths, _ := json.Marshal(src.Paths)
	excludes, _ := json.Marshal(src.Excludes)
	var every, retention any
	if src.Schedule != nil {
		every = src.Schedule.Every
	}
	if src.Retention != nil {
		b, _ := json.Marshal(src.Retention)
		retention = string(b)
	}
	if insert {
		_, err := s.db.Exec("INSERT INTO sources ("+sourceCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			src.ID, src.Name, string(paths), string(excludes), src.ScheduleSet, every, retention, src.Paused, toDB(src.Created))
		return err
	}
	_, err := s.db.Exec("UPDATE sources SET name = ?, paths = ?, excludes = ?, schedule_set = ?, schedule_every = ?, retention = ?, paused = ? WHERE id = ?",
		src.Name, string(paths), string(excludes), src.ScheduleSet, every, retention, src.Paused, src.ID)
	return err
}

func (s *Store) RemoveSource(id string) error {
	_, err := s.db.Exec("DELETE FROM sources WHERE id = ?", id)
	return err
}

// Effective is the source's policy with defaults filled in.
func (s *Store) Effective(src StoredSource) (Effective, error) {
	d, err := s.Defaults()
	if err != nil {
		return Effective{}, err
	}
	eff := Effective{Inherited: []string{}}
	if src.ScheduleSet {
		eff.Schedule = src.Schedule
	} else {
		eff.Schedule = d.Schedule
		eff.Inherited = append(eff.Inherited, "schedule")
	}
	if src.Retention != nil {
		eff.Retention = *src.Retention
	} else {
		eff.Retention = d.Retention
		eff.Inherited = append(eff.Inherited, "retention")
	}
	return eff, nil
}

// Runs.

const runCols = "job, kind, source, started, ended, status, files_new, files_changed, bytes_added, snapshot, error, files, bytes"

func (s *Store) AddRun(r protocol.Run) error {
	var source any
	if r.Source != nil {
		source = *r.Source
	}
	_, err := s.db.Exec("INSERT OR REPLACE INTO runs ("+runCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.Job, r.Kind, source, toDB(r.Started), toDB(r.Ended), r.Status, r.FilesNew, r.FilesChanged, r.BytesAdded, r.Snapshot, r.Error, r.Files, r.Bytes)
	return err
}

func (s *Store) queryRuns(q string, args ...any) ([]protocol.Run, error) {
	rows, err := s.db.Query("SELECT "+runCols+" FROM runs "+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []protocol.Run{}
	for rows.Next() {
		var r protocol.Run
		var source sql.NullString
		var started, ended float64
		if err := rows.Scan(&r.Job, &r.Kind, &source, &started, &ended, &r.Status, &r.FilesNew, &r.FilesChanged, &r.BytesAdded, &r.Snapshot, &r.Error, &r.Files, &r.Bytes); err != nil {
			return nil, err
		}
		if source.Valid {
			r.Source = &source.String
		}
		r.Started, r.Ended = fromDB(started), fromDB(ended)
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// Runs that ended at or after since, oldest first.
func (s *Store) Runs(since time.Time) ([]protocol.Run, error) {
	return s.queryRuns("WHERE ended >= ? ORDER BY ended", toDB(since))
}

// LastRun is a source's most recent run of kind, or nil.
func (s *Store) LastRun(source, kind string) (*protocol.Run, error) {
	runs, err := s.queryRuns("WHERE source = ? AND kind = ? ORDER BY ended DESC LIMIT 1", source, kind)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// LastSnapshotRun is source's most recent backup that made a snapshot
// (ok or incomplete), or nil.
func (s *Store) LastSnapshotRun(source string) (*protocol.Run, error) {
	runs, err := s.queryRuns("WHERE source = ? AND kind = ? AND snapshot != '' ORDER BY ended DESC LIMIT 1", source, protocol.KindBackup)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// LastRepoRun is the most recent repository-wide run of kind (prune,
// check), or nil.
func (s *Store) LastRepoRun(kind string) (*protocol.Run, error) {
	runs, err := s.queryRuns("WHERE source IS NULL AND kind = ? ORDER BY ended DESC LIMIT 1", kind)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// LastOKRepoRun is the most recent repository-wide run of kind that
// succeeded, or nil.
func (s *Store) LastOKRepoRun(kind string) (*protocol.Run, error) {
	runs, err := s.queryRuns("WHERE source IS NULL AND kind = ? AND status = ? ORDER BY ended DESC LIMIT 1", kind, protocol.RunOK)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// sourceIDs is every source this daemon has or has a run for (a source
// removed with its snapshots kept still has its runs).
func (s *Store) sourceIDs() (map[string]bool, error) {
	rows, err := s.db.Query("SELECT id FROM sources UNION SELECT source FROM runs WHERE source IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// Notes and pins.

func (s *Store) Note(snapshot string) (string, error) {
	var note string
	err := s.db.QueryRow("SELECT note FROM notes WHERE snapshot = ?", snapshot).Scan(&note)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return note, err
}

func (s *Store) SetNote(snapshot, note string) error {
	if note == "" {
		_, err := s.db.Exec("DELETE FROM notes WHERE snapshot = ?", snapshot)
		return err
	}
	_, err := s.db.Exec("INSERT INTO notes (snapshot, note) VALUES (?, ?) ON CONFLICT(snapshot) DO UPDATE SET note = excluded.note", snapshot, note)
	return err
}

// Pinned is the set of a source's pinned snapshot IDs.
func (s *Store) Pinned(source string) (map[string]bool, error) {
	rows, err := s.db.Query("SELECT snapshot FROM pins WHERE source = ?", source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pins := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		pins[id] = true
	}
	return pins, rows.Err()
}

// Added is a rewritten snapshot's recalculated new data, if known.
func (s *Store) Added(snapshot string) (int64, bool, error) {
	var n int64
	err := s.db.QueryRow("SELECT bytes FROM snapshot_added WHERE snapshot = ?", snapshot).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

func (s *Store) SetAdded(snapshot string, bytes int64) error {
	_, err := s.db.Exec("INSERT INTO snapshot_added (snapshot, bytes) VALUES (?, ?) ON CONFLICT(snapshot) DO UPDATE SET bytes = excluded.bytes", snapshot, bytes)
	return err
}

// MoveSnapshot carries a snapshot's note and pin over to the one that
// replaced it (rewrite).
func (s *Store) MoveSnapshot(old, new string) error {
	if _, err := s.db.Exec("UPDATE notes SET snapshot = ? WHERE snapshot = ?", new, old); err != nil {
		return err
	}
	_, err := s.db.Exec("UPDATE pins SET snapshot = ? WHERE snapshot = ?", new, old)
	return err
}

func (s *Store) SetPinned(snapshot, source string, on bool) error {
	if on {
		_, err := s.db.Exec("INSERT OR IGNORE INTO pins (snapshot, source) VALUES (?, ?)", snapshot, source)
		return err
	}
	_, err := s.db.Exec("DELETE FROM pins WHERE snapshot = ?", snapshot)
	return err
}

// RemoteCache is what was saved about a remote under key (JSON), if
// anything.
func (s *Store) RemoteCache(host, key string) (json.RawMessage, bool, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM remote_cache WHERE host = ? AND key = ?", host, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return json.RawMessage(v), true, nil
}

func (s *Store) SetRemoteCache(host, key string, value json.RawMessage) error {
	_, err := s.db.Exec("INSERT INTO remote_cache (host, key, value) VALUES (?, ?, ?) ON CONFLICT (host, key) DO UPDATE SET value = excluded.value",
		host, key, string(value))
	return err
}

// Notes is every snapshot note, by snapshot id.
func (s *Store) Notes() (map[string]string, error) {
	rows, err := s.db.Query("SELECT snapshot, note FROM notes WHERE note != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, note string
		if err := rows.Scan(&id, &note); err != nil {
			return nil, err
		}
		out[id] = note
	}
	return out, rows.Err()
}

// AllAdded is every recalculated "added" figure, by snapshot id.
func (s *Store) AllAdded() (map[string]int64, error) {
	rows, err := s.db.Query("SELECT snapshot, bytes FROM snapshot_added")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
