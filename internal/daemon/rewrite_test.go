package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// rewriteEngine is a source whose snapshot old1 rewrite replaces by new1.
type rewriteEngine struct {
	Engine
	mu        sync.Mutex
	rewrites  [][]string // excludes per call, "dry:" first for dry runs
	rewritten bool
	pruned    bool
}

func (e *rewriteEngine) Rewrite(_ context.Context, tag string, excludes []string, dry bool) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	call := slices.Clone(excludes)
	if dry {
		call = append([]string{"dry:" + tag}, call...)
	} else {
		e.rewritten = true
	}
	e.rewrites = append(e.rewrites, call)
	return 1, nil
}

func (e *rewriteEngine) Snapshots(context.Context, string) ([]EngineSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	if e.rewritten {
		return []EngineSnapshot{{ID: "new1", Original: "old1", Time: t0.Add(time.Hour)}, {ID: "keep2", Time: t0}}, nil
	}
	return []EngineSnapshot{{ID: "old1", Time: t0.Add(time.Hour)}, {ID: "keep2", Time: t0}}, nil
}

func (e *rewriteEngine) Added(_ context.Context, from, to string) (int64, error) {
	if from != "keep2" || to != "new1" {
		return 0, fmt.Errorf("diff %s %s", from, to)
	}
	return 777, nil
}

func (e *rewriteEngine) KeepReasons(context.Context, string, protocol.Retention, map[string]bool) (map[string][]string, error) {
	return nil, nil
}

func (e *rewriteEngine) Stats(context.Context) (RepoStats, error) { return RepoStats{}, nil }

func (e *rewriteEngine) Maintain(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruned = true
	return nil
}

// source_rewrite: a dry run counts; the real one is a job that rewrites,
// moves the replaced snapshot's note and pin, then prunes.
func TestSourceRewrite(t *testing.T) {
	store := memStore(t)
	store.SetRepository(RepositoryConfig{Location: "fake"})
	src, _ := store.AddSource(StoredSource{Name: "settings", Paths: []string{"/Users/a/Library"}}, time.Now())
	store.SetNote("old1", "before the move")
	store.SetPinned("old1", src.ID, true)
	eng := &rewriteEngine{}
	core := NewCore("test", store, func(RepositoryConfig) Engine { return eng }, nil)
	c, _ := serve(t, core)

	if code := callErr(c, protocol.MethodSourceRewrite, protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{"Keychains"}}); code != protocol.ErrBadRequest {
		t.Errorf("relative path: %s", code)
	}
	dry := call[protocol.SourceRewriteResult](t, c, protocol.MethodSourceRewrite, protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{"/Users/a/Library/Keychains/X"}, DryRun: true})
	if dry.Snapshots != 1 || dry.Job != nil || eng.rewritten {
		t.Fatalf("dry run %+v, rewritten %v", dry, eng.rewritten)
	}
	res := call[protocol.SourceRewriteResult](t, c, protocol.MethodSourceRewrite, protocol.SourceRewriteParams{ID: src.ID, Excludes: []string{"/Users/a/Library/Keychains/X"}})
	if res.Job == nil || res.Job.Kind != protocol.KindRewrite || res.Job.Source == nil || *res.Job.Source != src.ID {
		t.Fatalf("job %+v", res.Job)
	}
	core.WaitIdle()
	if !eng.rewritten || !eng.pruned {
		t.Errorf("rewritten %v, pruned %v", eng.rewritten, eng.pruned)
	}
	if n, _ := store.Note("new1"); n != "before the move" {
		t.Errorf("note on new1: %q", n)
	}
	if pins, _ := store.Pinned(src.ID); !pins["new1"] || pins["old1"] {
		t.Errorf("pins %v", pins)
	}
	// What new1 adds is recalculated against the snapshot before it, and
	// the snapshot list shows that instead of the copied backup figure.
	if n, ok, _ := store.Added("new1"); !ok || n != 777 {
		t.Errorf("added %d %v", n, ok)
	}
	snaps := call[protocol.SnapshotsResult](t, c, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: src.ID})
	for _, s := range snaps.Snapshots {
		if s.ID == "new1" && s.BytesAdded != 777 {
			t.Errorf("new1 shows %d added", s.BytesAdded)
		}
	}
}

// Tree is the warm-up that follows a snapshot listing.
func (*rewriteEngine) Tree(context.Context, string) (map[string][]protocol.Entry, error) {
	return map[string][]protocol.Entry{}, nil
}
