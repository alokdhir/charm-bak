package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// The daemon backs up a copy of its own database daily, under its own
// tag, kept out of the untagged pseudo-source; the copy is a working
// database with the sources in it.
func TestStateBackup(t *testing.T) {
	fx := newResticFixture(t)
	fx.engine.Init(context.Background())
	store := memStore(t)
	store.SetRepository(fx.engine.Repo)
	clock := &testClock{t: time.Unix(1_900_000_000, 0), tick: time.Millisecond}
	core := NewCore("test", store, func(r RepositoryConfig) Engine {
		return &ResticEngine{Binary: fx.engine.Binary, Repo: r}
	}, clock.now)
	core.StateCopy = filepath.Join(t.TempDir(), "charm-bak.db.bak")
	store.AddSource(StoredSource{Name: "kept", Paths: []string{fx.data}}, clock.now())

	states := func() int {
		runs, _ := store.Runs(time.Unix(0, 0))
		n := 0
		for _, r := range runs {
			if r.Kind == protocol.KindState {
				if r.Status != protocol.RunOK {
					t.Fatalf("state run %s: %s", r.Status, r.Error)
				}
				n++
			}
		}
		return n
	}
	step := func() { core.Tick(); core.WaitIdle() }
	step()
	if n := states(); n != 1 {
		t.Fatalf("%d state runs after the first tick", n)
	}
	step()
	clock.advance(time.Hour)
	step()
	if n := states(); n != 1 {
		t.Fatalf("%d state runs within a day", n)
	}
	clock.advance(stateBackupEvery)
	step()
	if n := states(); n != 2 {
		t.Fatalf("%d state runs after a day", n)
	}

	snaps, err := fx.engine.Snapshots(t.Context(), stateTag)
	if err != nil || len(snaps) != 2 || !slices.Equal(snaps[0].Paths, []string{core.StateCopy}) {
		t.Fatalf("state snapshots %+v, %v", snaps, err)
	}
	db, err := sql.Open("sqlite", core.StateCopy)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name string
	if err := db.QueryRow("SELECT name FROM sources").Scan(&name); err != nil || name != "kept" {
		t.Fatalf("copy's source %q, %v", name, err)
	}
	c, _ := serve(t, core)
	if u := call[protocol.SnapshotsResult](t, c, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: protocol.UntaggedSource}); len(u.Snapshots) != 0 {
		t.Errorf("state snapshots under untagged: %d", len(u.Snapshots))
	}
}

// Pause survives a restart (a self-update restarts the daemon).
func TestPauseSurvivesRestart(t *testing.T) {
	store := memStore(t)
	NewCore("test", store, nil, nil).SetPaused(true)
	if c := NewCore("test", store, nil, nil); !c.paused {
		t.Fatal("pause lost on restart")
	}
	c := NewCore("test", store, nil, nil)
	c.SetPaused(false)
	if NewCore("test", store, nil, nil).paused {
		t.Fatal("resume lost on restart")
	}
}
