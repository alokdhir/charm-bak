package daemon

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"charm-bak/internal/protocol"
)

// treeDB keeps snapshot listings in a SQLite database of its own (Alok,
// 2026-11-24: the cache is SQLite, like charm-aws's), one row per folder:
// opening a folder reads that row, and nothing holds a whole tree in
// memory. It is a cache: separate from the state database so the daily
// state backup doesn't carry it, safe to delete, and trimmed to
// treeDiskMax by dropping the snapshots used longest ago. It holds file
// names, so the file is owner-only like the state database.
type treeDB struct {
	db *sql.DB

	mu      sync.Mutex
	touched map[string]time.Time // last time a snapshot's use was written
}

// openTreeDB opens (creating) the cache database at path. A file that
// can't be used is replaced: nothing in it is needed.
func openTreeDB(path string) (*treeDB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	t, err := openTreeDBOnce(path)
	if err != nil {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			os.Remove(path + suffix)
		}
		t, err = openTreeDBOnce(path)
	}
	return t, err
}

func openTreeDBOnce(path string) (*treeDB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // the pragmas below are per connection
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		// A snapshot is listed only once all its folders are in (store
		// writes both in one transaction).
		`CREATE TABLE IF NOT EXISTS snapshots (id TEXT PRIMARY KEY, used REAL NOT NULL, bytes INTEGER NOT NULL) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS folders (snapshot TEXT NOT NULL, dir TEXT NOT NULL, entries BLOB NOT NULL, PRIMARY KEY (snapshot, dir)) WITHOUT ROWID`,
		// What a snapshot changed from an earlier one (changes): two
		// snapshots never change, so neither does this.
		// (changes2: answers saved before touched files were listed
		// are dropped with the old table.)
		`DROP TABLE IF EXISTS changes`,
		`CREATE TABLE IF NOT EXISTS changes2 (snapshot TEXT NOT NULL, base TEXT NOT NULL, result BLOB NOT NULL, PRIMARY KEY (snapshot, base)) WITHOUT ROWID`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	os.Chmod(path, 0o600)
	return &treeDB{db: db, touched: map[string]time.Time{}}, nil
}

func (t *treeDB) has(snapshot string) bool {
	var one int
	return t.db.QueryRow("SELECT 1 FROM snapshots WHERE id = ?", snapshot).Scan(&one) == nil
}

// lookup is dir's entries in snapshot: loaded is whether the snapshot is
// in the cache at all, found whether dir is a folder in it.
func (t *treeDB) lookup(snapshot, dir string) (entries []protocol.Entry, loaded, found bool) {
	if !t.has(snapshot) {
		return nil, false, false
	}
	t.touch(snapshot)
	var blob []byte
	err := t.db.QueryRow("SELECT entries FROM folders WHERE snapshot = ? AND dir = ?", snapshot, dir).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, false
	}
	if err == nil {
		entries, err = unpackEntries(blob)
	}
	if err != nil {
		// Unreadable: drop the snapshot so it loads again.
		t.drop(snapshot)
		return nil, false, false
	}
	return entries, true, true
}

// touch records that snapshot was used, at most once a minute.
func (t *treeDB) touch(snapshot string) {
	now := time.Now()
	t.mu.Lock()
	recent := now.Sub(t.touched[snapshot]) < time.Minute
	if !recent {
		t.touched[snapshot] = now
	}
	t.mu.Unlock()
	if !recent {
		t.db.Exec("UPDATE snapshots SET used = ? WHERE id = ?", unixSeconds(now), snapshot)
	}
}

func (t *treeDB) drop(snapshot string) {
	t.db.Exec("DELETE FROM folders WHERE snapshot = ?", snapshot)
	t.db.Exec("DELETE FROM snapshots WHERE id = ?", snapshot)
}

// store saves a whole tree in one transaction, then trims the cache.
func (t *treeDB) store(snapshot string, dirs map[string][]protocol.Entry) error {
	tx, err := t.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM folders WHERE snapshot = ?", snapshot); err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO folders (snapshot, dir, entries) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	var total int64
	for dir, entries := range dirs {
		blob, err := packEntries(entries)
		if err != nil {
			return err
		}
		total += int64(len(blob) + len(dir))
		if _, err := stmt.Exec(snapshot, dir, blob); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO snapshots (id, used, bytes) VALUES (?, ?, ?)", snapshot, unixSeconds(time.Now()), total); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	t.trim(snapshot)
	return nil
}

// trim drops the snapshots used longest ago until the cache is within
// treeDiskMax, never the one just stored.
func (t *treeDB) trim(keep string) {
	for {
		var total sql.NullInt64
		if t.db.QueryRow("SELECT SUM(bytes) FROM snapshots").Scan(&total) != nil || total.Int64 <= treeDiskMax {
			return
		}
		var oldest string
		if t.db.QueryRow("SELECT id FROM snapshots WHERE id != ? ORDER BY used LIMIT 1", keep).Scan(&oldest) != nil {
			return
		}
		t.drop(oldest)
	}
}

// changes is the saved difference between two snapshots, if any.
func (t *treeDB) changes(snapshot, base string) (res protocol.ChangesResult, ok bool) {
	var blob []byte
	if t.db.QueryRow("SELECT result FROM changes2 WHERE snapshot = ? AND base = ?", snapshot, base).Scan(&blob) != nil {
		return res, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return res, false
	}
	if json.NewDecoder(zr).Decode(&res) != nil {
		return protocol.ChangesResult{}, false
	}
	return res, true
}

func (t *treeDB) storeChanges(snapshot, base string, res protocol.ChangesResult) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if json.NewEncoder(zw).Encode(res) != nil || zw.Close() != nil {
		return
	}
	t.db.Exec("INSERT OR REPLACE INTO changes2 (snapshot, base, result) VALUES (?, ?, ?)", snapshot, base, buf.Bytes())
}

func unixSeconds(at time.Time) float64 { return float64(at.UnixNano()) / 1e9 }

// packEntries is a folder's entries as gzipped JSON.
func packEntries(entries []protocol.Entry) ([]byte, error) {
	if entries == nil {
		entries = []protocol.Entry{}
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(entries); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func unpackEntries(blob []byte) ([]protocol.Entry, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var entries []protocol.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}
