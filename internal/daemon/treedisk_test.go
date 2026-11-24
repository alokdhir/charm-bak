package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm-bak/internal/protocol"
)

// A loaded tree is kept in the cache database, a row per folder: a new
// cache over the same file (the daemon restarted) answers from it without
// loading again, an unknown folder is "not in this snapshot", and a file
// that isn't a database is replaced.
func TestTreeCacheSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache", "trees.db")
	id := strings.Repeat("ab", 32)
	dirs := map[string][]protocol.Entry{"/": {{Name: "a", Type: protocol.TypeDir}}, "/a": {{Name: "f.txt", Type: protocol.TypeFile, Size: 3}}, "/a/empty": nil}

	db, err := openTreeDB(path)
	if err != nil {
		t.Fatal(err)
	}
	first := treeCache{db: db}
	if !first.start(id) || first.start(id) {
		t.Fatal("start: want one claim")
	}
	if _, loaded, _ := first.lookup(id, "/"); loaded {
		t.Error("loaded while only claimed")
	}
	first.finish(id, dirs, nil)
	if first.start(id) {
		t.Error("start claimed a stored snapshot again")
	}
	if len(first.trees) != 0 {
		t.Errorf("stored trees also held in memory: %d", len(first.trees))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want owner only", info.Mode().Perm())
	}
	db.db.Close()

	db, err = openTreeDB(path)
	if err != nil {
		t.Fatal(err)
	}
	second := treeCache{db: db}
	entries, loaded, found := second.lookup(id, "a/")
	if !second.has(id) || !loaded || !found || len(entries) != 1 || entries[0].Name != "f.txt" {
		t.Fatalf("after restart: loaded %v found %v %v", loaded, found, entries)
	}
	if entries, _, found := second.lookup(id, "/a/empty"); !found || entries == nil || len(entries) != 0 {
		t.Errorf("empty folder: found %v, %v", found, entries)
	}
	if _, loaded, found := second.lookup(id, "/nope"); !loaded || found {
		t.Errorf("unknown folder: loaded %v found %v", loaded, found)
	}
	db.db.Close()

	os.WriteFile(path, []byte("this is not a database"), 0o600)
	db, err = openTreeDB(path)
	if err != nil {
		t.Fatalf("a damaged cache wasn't replaced: %v", err)
	}
	if db.has(id) {
		t.Error("the replaced cache still has the snapshot")
	}
}

// The cache drops the snapshots used longest ago once it's over its size.
func TestTreeCacheTrims(t *testing.T) {
	db, err := openTreeDB(filepath.Join(t.TempDir(), "trees.db"))
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string][]protocol.Entry{"/": {{Name: "x"}}}
	for _, id := range []string{"old", "mid", "new"} {
		if err := db.store(id, dirs); err != nil {
			t.Fatal(err)
		}
	}
	db.db.Exec("UPDATE snapshots SET used = 1, bytes = ? WHERE id = 'old'", int64(treeDiskMax))
	db.db.Exec("UPDATE snapshots SET used = 2 WHERE id = 'mid'")
	db.trim("new")
	if db.has("old") || !db.has("mid") || !db.has("new") {
		t.Errorf("after trim: old %v mid %v new %v", db.has("old"), db.has("mid"), db.has("new"))
	}
	var n int
	db.db.QueryRow("SELECT COUNT(*) FROM folders WHERE snapshot = 'old'").Scan(&n)
	if n != 0 {
		t.Errorf("old's folders left behind: %d", n)
	}
}
