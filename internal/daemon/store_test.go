package daemon

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

func memStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreSourcesInheritDefaults(t *testing.T) {
	s := memStore(t)
	now := time.Unix(1_900_000_000, 123_456_789)
	if d, _ := s.Defaults(); !reflect.DeepEqual(d, BuiltinDefaults) {
		t.Errorf("defaults %+v", d)
	}
	inheriting, err := s.AddSource(StoredSource{Name: "src", Paths: []string{"/Users/a/src"}, Excludes: []string{"node_modules"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(inheriting.ID) != 8 || inheriting.Tag() != "src:"+inheriting.ID {
		t.Errorf("id %q", inheriting.ID)
	}
	eff, _ := s.Effective(inheriting)
	if !slices.Equal(eff.Inherited, []string{"schedule", "retention"}) || eff.Retention != BuiltinDefaults.Retention {
		t.Errorf("effective %+v", eff)
	}

	custom := protocol.Retention{Last: 3, Daily: 7}
	s.SetDefaults(Defaults{Schedule: &protocol.Schedule{Every: 86400}, Retention: custom})
	eff, _ = s.Effective(inheriting)
	if eff.Retention != custom || eff.Schedule.Every != 86400 {
		t.Errorf("after new defaults %+v", eff)
	}

	// Manual: schedule explicitly nil, not inherited.
	manual, _ := s.AddSource(StoredSource{Name: "scans", Paths: []string{"/Users/a/Scans"}, ScheduleSet: true, Retention: &custom}, now)
	eff, _ = s.Effective(manual)
	if eff.Schedule != nil || len(eff.Inherited) != 0 {
		t.Errorf("manual %+v", eff)
	}

	edited := manual
	edited.Paused = true
	edited.Paths = append(edited.Paths, "/Users/a/More")
	if err := s.UpdateSource(edited); err != nil {
		t.Fatal(err)
	}
	got, ok, _ := s.Source(manual.ID)
	if !ok || !reflect.DeepEqual(got, edited) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, edited)
	}
	all, _ := s.Sources()
	// By name, case ignored: "scans" before "src", though src was added first.
	if len(all) != 2 || all[0].ID != manual.ID || all[1].ID != inheriting.ID {
		t.Errorf("order %v", all)
	}
	s.RemoveSource(inheriting.ID)
	if all, _ := s.Sources(); len(all) != 1 {
		t.Errorf("after remove %v", all)
	}
	if _, ok, _ := s.Source("nope"); ok {
		t.Error("found a missing source")
	}
}

func TestStoreRunsNotesPins(t *testing.T) {
	s := memStore(t)
	t0 := time.Unix(1_800_000_000, 0)
	src := "abcd1234"
	run := func(job string, ended time.Duration, status string) protocol.Run {
		return protocol.Run{Job: job, Kind: protocol.KindBackup, Source: &src, Started: t0, Ended: t0.Add(ended), Status: status, Snapshot: "s-" + job}
	}
	s.AddRun(run("j-1", 10*time.Second, protocol.RunOK))
	s.AddRun(run("j-2", 20*time.Second, protocol.RunFailed))
	s.AddRun(run("j-3", 30*time.Second, protocol.RunOK))
	s.AddRun(protocol.Run{Job: "j-4", Kind: protocol.KindPrune, Started: t0, Ended: t0.Add(40 * time.Second), Status: protocol.RunOK})
	runs, _ := s.Runs(t0.Add(15 * time.Second))
	var jobs []string
	for _, r := range runs {
		jobs = append(jobs, r.Job)
	}
	if !slices.Equal(jobs, []string{"j-2", "j-3", "j-4"}) {
		t.Errorf("since: %v", jobs)
	}
	if last, _ := s.LastRun(src, protocol.KindBackup); last == nil || last.Job != "j-3" {
		t.Errorf("last %+v", last)
	}
	if last, _ := s.LastRepoRun(protocol.KindPrune); last == nil || last.Job != "j-4" || last.Source != nil {
		t.Errorf("last prune %+v", last)
	}

	s.SetNote("x", "before refactor")
	if n, _ := s.Note("x"); n != "before refactor" {
		t.Errorf("note %q", n)
	}
	s.SetNote("x", "")
	if n, _ := s.Note("x"); n != "" {
		t.Errorf("cleared note %q", n)
	}
	s.SetPinned("snap1", src, true)
	s.SetPinned("snap2", "other", true)
	if p, _ := s.Pinned(src); !reflect.DeepEqual(p, map[string]bool{"snap1": true}) {
		t.Errorf("pins %v", p)
	}
	s.SetPinned("snap1", src, false)
	if p, _ := s.Pinned(src); len(p) != 0 {
		t.Errorf("unpinned %v", p)
	}
}

func TestStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := s.AddSource(StoredSource{Name: "a", Paths: []string{"/a"}}, time.Now())
	s.SetRepository(RepositoryConfig{Location: "s3:example/bucket", AWSKeychain: "charm-bak-aws/studio"})
	s.Close()
	again, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, ok, _ := again.Source(src.ID); !ok || got.Name != "a" {
		t.Errorf("source %+v", got)
	}
	if r, _ := again.Repository(); r == nil || r.Location != "s3:example/bucket" || r.AWSKeychain != "charm-bak-aws/studio" {
		t.Errorf("repo %+v", r)
	}
}

// A machine already backing up when the built-in defaults changed keeps
// the old ones, saved as its own; a new one gets the new built-ins.
func TestStoreKeepsOldDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Defaults(); !reflect.DeepEqual(d, BuiltinDefaults) {
		t.Fatalf("new store's defaults %+v", d)
	}
	if _, err := s.AddSource(StoredSource{Name: "home", Paths: []string{"/Users/a"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// As if this database predated that migration (the sixth) and the
	// ones after it.
	if _, err := s.db.Exec("DROP TABLE remote_cache; ALTER TABLE runs DROP COLUMN skipped; ALTER TABLE runs DROP COLUMN skipped_files; PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, _ := s.Defaults()
	old := protocol.Retention{Last: 10, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 12, Yearly: 3}
	if d.Retention != old || d.Schedule == nil || d.Schedule.Every != 3600 || d.Excludes == nil {
		t.Errorf("defaults after migrating %+v", d)
	}
}

func TestKeepFlagsUnlimited(t *testing.T) {
	got := keepFlags(protocol.Retention{Daily: 30, Yearly: protocol.KeepUnlimited})
	if !slices.Equal(got, []string{"--keep-daily", "30", "--keep-yearly", "unlimited"}) {
		t.Errorf("flags %q", got)
	}
}

// A run's skipped files are kept apart from history and come back with
// the run.
func TestStoreRunSkippedFiles(t *testing.T) {
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	run := protocol.Run{Job: "j-1", Kind: protocol.KindBackup, Started: now, Ended: now, Status: protocol.RunIncomplete, Skipped: 2}
	if err := s.AddRun(run); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSkippedFiles("j-1", []string{"/a (x)", "/b (y)"}); err != nil {
		t.Fatal(err)
	}
	got, files, err := s.Run("j-1")
	if err != nil || got == nil || got.Skipped != 2 || strings.Join(files, ",") != "/a (x),/b (y)" {
		t.Fatalf("%+v %v %v", got, files, err)
	}
	if got, _, err := s.Run("j-none"); got != nil || err != nil {
		t.Errorf("missing run: %+v %v", got, err)
	}
}
