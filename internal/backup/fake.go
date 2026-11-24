package backup

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"charm-bak/internal/protocol"
)

// Fake is fixture data shaped like the app's replies, so the UI can be
// built and tested without the app. Everything derives from Now, so
// output is deterministic.
type Fake struct {
	Now time.Time
	// Name is the fixture machine's host name.
	Name string

	mu     sync.Mutex // guards everything below against the changes in fakewrite.go
	events []chan protocol.Event
	nextID int
	// repositoryErr: what Repository answers instead (FailRepository).
	repositoryErr error

	status    protocol.StatusResult
	sources   []protocol.Source
	snapshots map[string][]protocol.Snapshot // by source id
	dirs      map[string][]protocol.Entry    // by snapshot id + "\x00" + path
	files     map[string]bool                // file paths that exist
	defaults  protocol.Defaults
	remotes   map[string]*Fake  // AddRemote; ForHost hands them out
	down      map[string]bool   // remotes that don't answer
	addrs     map[string]string // SetHostAddress
	// offline: this Fake stands for a remote that isn't answering. What
	// the controller does itself still works (its saved copy of the
	// sources and repository, and reading snapshots straight from the
	// remote's repository); everything that needs the remote fails
	// host_unreachable. unsaved: the controller has nothing saved for it
	// either, so nothing answers.
	offline, unsaved bool

	// Stores are the fixture's repository locations and their passwords,
	// shared with its remotes, for ProbeRepository and SetupRepository.
	Stores map[string]string

	// AccessOpened counts OpenAccessSettings calls.
	AccessOpened int
	// AWS is what aws_status answers; AWSCalls records aws_keys calls.
	AWS      protocol.AWSStatusResult
	AWSCalls []protocol.AWSKeysParams

	// Refused counts calls that failed because this machine is offline.
	Refused int
	// Restores is every restore asked for, oldest first.
	Restores []protocol.RestoreParams
	// ChangesAsked records each Changes call: {snapshot, base}.
	ChangesAsked [][2]string
	// Rewrites is every source_rewrite (not dry runs), oldest first.
	Rewrites []protocol.SourceRewriteParams
	// Cancelled is every job cancel was asked for.
	Cancelled []string
	runs      []protocol.Run // History beyond the sources' last runs
}

// fakeNode describes a fixture tree: a file when children is nil.
type fakeNode struct {
	name     string
	size     int64
	children []fakeNode
}

func dir(name string, children ...fakeNode) fakeNode {
	return fakeNode{name: name, children: append([]fakeNode{}, children...)}
}

func file(name string, size int64) fakeNode { return fakeNode{name: name, size: size} }

// NewFake builds the fixture repository.
func NewFake(now time.Time) *Fake {
	f := &Fake{Now: now, Name: "studio", snapshots: map[string][]protocol.Snapshot{}, dirs: map[string][]protocol.Entry{}, files: map[string]bool{}, Stores: map[string]string{}, AWS: protocol.AWSStatusResult{CLI: true}}
	home, _ := os.UserHomeDir()
	keep := protocol.Retention{Last: 10, Hourly: 48, Daily: 7, Weekly: 4, Monthly: 12, Yearly: 3}
	f.defaults = protocol.Defaults{Schedule: &protocol.Schedule{Every: 3600}, Retention: keep, Excludes: []string{"*.log"}}

	trees := []struct {
		id       string
		path     string
		every    time.Duration
		lastAgo  time.Duration
		status   string
		count    int
		children []fakeNode
	}{
		{"a1b2c3d4", home + "/src/charm-bak", time.Hour, 20 * time.Minute, protocol.RunOK, 9, []fakeNode{
			dir("internal",
				dir("backup", file("backup.go", 3200), file("fake.go", 6100)),
				dir("ui", file("app.go", 18400), file("column.go", 4100), file("hero.go", 3900),
					dir("styles", file("styles.go", 7200)))),
			dir("docs", file("research.md", 14800),
				file("a-very-long-file-name-for-seeing-how-charm-bak-copes-with-names-that-fit-nowhere-final-v2.md", 2300)),
			file("AGENTS.md", 9600), file("go.mod", 410), file("main.go", 900)}},
		{"b2c3d4e5", home + "/src/charm-cli", time.Hour, 45 * time.Minute, protocol.RunOK, 6, []fakeNode{
			dir("internal", dir("draft", file("startup.go", 9800)), dir("ui", file("app.go", 52100))),
			file("AGENTS.md", 21000), file("main.go", 7400)}},
		{"c3d4e5f6", home + "/Documents/taxes", 24 * time.Hour, 26 * time.Hour, protocol.RunOK, 5, []fakeNode{
			dir("2025", file("1040.pdf", 812000), file("w2.pdf", 120000)),
			dir("2026", file("estimates.xlsx", 44000))}},
		{"d4e5f6a7", home + "/Scans", 24 * time.Hour, 9 * 24 * time.Hour, protocol.RunIncomplete, 4, []fakeNode{
			file("passport.pdf", 2100000), file("deed.pdf", 5400000)}},
	}

	var logical int64
	for _, t := range trees {
		last := now.Add(-t.lastAgo)
		id := t.id
		run := &protocol.Run{Job: "j-" + t.id[:4], Kind: protocol.KindBackup, Source: &id, Started: last.Add(-10 * time.Second),
			Ended: last, Status: t.status, FilesNew: 2, FilesChanged: 5, BytesAdded: 52311}
		if t.status != protocol.RunOK {
			run.Error = "3 files could not be read"
		}
		next := last.Add(t.every)
		health := protocol.HealthOK
		switch {
		case t.status != protocol.RunOK:
			health = protocol.HealthFailed
		case now.Sub(next) > t.every:
			health = protocol.HealthOverdue
		}
		f.sources = append(f.sources, protocol.Source{
			ID: t.id, Name: t.path[strings.LastIndex(t.path, "/")+1:], Host: f.Name, Paths: []string{t.path}, Excludes: []string{"node_modules"},
			Schedule: &protocol.Schedule{Every: int64(t.every / time.Second)}, Retention: keep, Inherited: []string{"retention"},
			Health: health, LastRun: run, NextRun: &next,
			Files: int64(1200 + 7300*len(t.path)), Bytes: int64(len(t.path)) << 27,
		})

		var snaps []protocol.Snapshot
		for i := range t.count {
			start := last.Add(-time.Duration(i) * t.every * 3)
			end := start.Add(time.Duration(4+i) * time.Second)
			snapID := fmt.Sprintf("%016x%048x", start.Unix()*7919+int64(len(t.path)), i)
			size, files := f.addTree(snapID, t.path, t.children)
			s := protocol.Snapshot{
				ID: snapID, ShortID: snapID[:8], Time: start, End: &end, Paths: []string{t.path}, Hostname: "studio.local",
				Tags: []string{"src:" + t.id}, Files: files, Bytes: size, BytesAdded: size / int64(3+i),
			}
			switch i {
			case 0:
				s.KeptBy = []string{"last snapshot", "hourly snapshot", "daily snapshot", "weekly snapshot", "monthly snapshot", "yearly snapshot"}
			case 1:
				s.KeptBy = []string{"last snapshot", "hourly snapshot"}
			case 2:
				s.KeptBy = []string{"pinned", "last snapshot"}
				s.Pinned = true
				s.Note = "before refactor"
			default:
				s.KeptBy = []string{"last snapshot"}
			}
			snaps = append(snaps, s)
			if i == 0 {
				logical += size
			}
		}
		f.snapshots[t.id] = snaps
	}
	f.status = protocol.StatusResult{
		Repository: &protocol.Repository{Location: "s3:s3.amazonaws.com/charm-bak/studio", StoredBytes: logical * 4 / 10, LogicalBytes: logical},
		State:      protocol.StateIdle,
	}
	return f
}

// addTree registers dir's children under snapshot snap and returns the
// tree's total size and file count.
func (f *Fake) addTree(snap, dirPath string, children []fakeNode) (size, files int64) {
	mtime := f.Now.Add(-time.Duration(len(dirPath)) * time.Hour)
	var entries []protocol.Entry
	for _, c := range children {
		p := Join(dirPath, c.name)
		if c.children == nil {
			entries = append(entries, protocol.Entry{Name: c.name, Type: protocol.TypeFile, Size: c.size, Mode: "-rw-r--r--", MTime: mtime})
			f.files[snap+"\x00"+p] = true
			size += c.size
			files++
			continue
		}
		s, n := f.addTree(snap, p, c.children)
		size += s
		files += n
		entries = append(entries, protocol.Entry{Name: c.name, Type: protocol.TypeDir, Mode: "drwxr-xr-x", MTime: mtime})
	}
	slices.SortFunc(entries, func(a, b protocol.Entry) int {
		if (a.Type == protocol.TypeDir) != (b.Type == protocol.TypeDir) {
			if a.Type == protocol.TypeDir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	f.dirs[snap+"\x00"+dirPath] = entries
	return size, files
}

// goneLocked is the error for a call that needs this machine's daemon,
// nil while it answers.
func (f *Fake) goneLocked() error {
	if f.offline {
		f.Refused++
		return &protocol.Error{Code: protocol.ErrHostUnreachable, Message: f.Name + " isn't answering."}
	}
	return nil
}

// unsavedLocked is the error for a call the controller answers itself for
// a remote that's down, nil when it can (or the machine answers).
func (f *Fake) unsavedLocked() error {
	if f.offline && f.unsaved {
		return f.goneLocked()
	}
	return nil
}

func (f *Fake) Status(context.Context) (protocol.StatusResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return protocol.StatusResult{}, err
	}
	return f.status, nil
}

func (f *Fake) Sources(context.Context) ([]protocol.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.unsavedLocked(); err != nil {
		return nil, err
	}
	out := slices.Clone(f.sources)
	slices.SortStableFunc(out, func(a, b protocol.Source) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (f *Fake) Snapshots(_ context.Context, source string) ([]protocol.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.unsavedLocked(); err != nil {
		return nil, err
	}
	return slices.Clone(f.snapshots[source]), nil
}

func (f *Fake) List(_ context.Context, snapshot, dir string) ([]protocol.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The live disk needs the machine; a snapshot only its repository.
	if err := f.readErrLocked(snapshot); err != nil {
		return nil, err
	}
	entries, ok := f.dirs[snapshot+"\x00"+dir]
	if !ok {
		return nil, &protocol.Error{Code: protocol.ErrNotFound, Message: dir + " isn't in this snapshot."}
	}
	return slices.Clone(entries), nil
}

// Read returns made-up text for a fixture file: a header naming it, then
// numbered filler lines, enough to scroll a preview. PDFs and spreadsheets
// come back as binary so the preview's binary path gets exercised. Live
// reads follow liveState.
func (f *Fake) Read(_ context.Context, snapshot, file string, max int) (protocol.DumpResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readErrLocked(snapshot); err != nil {
		return protocol.DumpResult{}, err
	}
	live := snapshot == protocol.LiveSnapshot
	if live && liveState(path.Base(file)) == protocol.ChangeRemoved || !live && !f.files[snapshot+"\x00"+file] {
		return protocol.DumpResult{}, &protocol.Error{Code: protocol.ErrNotFound, Message: file + " isn't there."}
	}
	if strings.HasSuffix(file, ".pdf") || strings.HasSuffix(file, ".xlsx") {
		return protocol.DumpResult{Size: 4096, Binary: true}, nil
	}
	out := fakeContents(file, live && liveState(path.Base(file)) == protocol.ChangeChanged)
	size := int64(len(out))
	if len(out) > max {
		out = out[:max]
	}
	return protocol.DumpResult{Data: out, Size: size, Truncated: int64(len(out)) < size}, nil
}

// readErrLocked is the error for reading snapshot ("live": the disk).
func (f *Fake) readErrLocked(snapshot string) error {
	if snapshot == protocol.LiveSnapshot {
		return f.goneLocked()
	}
	return f.unsavedLocked()
}

// liveState is a fixture file's made-up state on disk, fixed by its name:
// unchanged, edited or deleted.
func liveState(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	switch h.Sum32() % 4 {
	case 0:
		return protocol.ChangeChanged
	case 1:
		return protocol.ChangeRemoved
	default:
		return protocol.ChangeSame
	}
}

// fakeContents is the made-up text of a fixture file, as saved or (live)
// as edited since.
func fakeContents(file string, live bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "// %s (fixture contents)\n\n", file)
	for i := 1; i <= 80; i++ {
		switch {
		case live && i == 12:
			fmt.Fprintf(&b, "line %d, edited on disk since the snapshot\n", i)
		case live && (i == 40 || i == 41):
			continue
		default:
			fmt.Fprintf(&b, "line %d of a pretend file, long enough to show how wide lines get cut at the pane edge\n", i)
		}
		if live && i == 60 {
			b.WriteString("a line added on disk\n")
		}
	}
	return []byte(b.String())
}

// Diff marks each saved entry by liveState and adds one file that's only
// on disk.
// Changes answers the same small difference for any two snapshots: a
// file changed, a folder added and a file removed, under the snapshot's
// first path. It reads the repository, so it answers for an offline
// remote too.
func (f *Fake) Changes(_ context.Context, snapshot, base string) (protocol.ChangesResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	root := "/"
	for _, snaps := range f.snapshots {
		for _, s := range snaps {
			if s.ID == snapshot && len(s.Paths) > 0 {
				root = s.Paths[0]
			}
		}
	}
	f.ChangesAsked = append(f.ChangesAsked, [2]string{snapshot, base})
	return protocol.ChangesResult{Changes: []protocol.PathChange{
		{Path: Join(root, "app.go"), Change: protocol.ChangeChanged},
		{Path: Join(root, "notes") + "/", Change: protocol.ChangeAdded},
		{Path: Join(root, "old.txt"), Change: protocol.ChangeRemoved},
		{Path: Join(root, "touched.txt"), Change: protocol.ChangeTouched},
	}, Added: 1, Removed: 1, Changed: 1, Touched: 1, Bytes: 52311}, nil
}

func (f *Fake) Diff(_ context.Context, snapshot, dir string) ([]protocol.DiffEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.goneLocked(); err != nil {
		return nil, err
	}
	entries, ok := f.dirs[snapshot+"\x00"+dir]
	if !ok {
		return nil, &protocol.Error{Code: protocol.ErrNotFound, Message: dir + " isn't in this snapshot."}
	}
	var out []protocol.DiffEntry
	for _, e := range entries {
		ch := protocol.ChangeSame
		if e.Type == protocol.TypeFile {
			ch = liveState(e.Name)
		}
		out = append(out, protocol.DiffEntry{Name: e.Name, Type: e.Type, Change: ch})
	}
	return append(out, protocol.DiffEntry{Name: "new-on-disk.txt", Type: protocol.TypeFile, Change: protocol.ChangeAdded}), nil
}

// AddRemote pairs another fixture machine, with its own sources, for
// tests of the multi-host TUI; down makes it a remote that isn't answering.
func (f *Fake) AddRemote(name string, down bool) *Fake {
	r := NewFake(f.Now)
	r.Name = name
	for i := range r.sources {
		r.sources[i].Host = name
		old := r.sources[i].ID
		r.sources[i].ID = name[:1] + old // ids differ across machines, as in life
		r.snapshots[r.sources[i].ID] = r.snapshots[old]
	}
	r.sources = r.sources[:1]
	// Its live disk, for the add-source picker.
	live := func(dir string, entries ...protocol.Entry) { r.dirs[protocol.LiveSnapshot+"\x00"+dir] = entries }
	live("/", protocol.Entry{Name: "data", Type: protocol.TypeDir}, protocol.Entry{Name: "etc", Type: protocol.TypeDir})
	live("/data", protocol.Entry{Name: "movies", Type: protocol.TypeDir}, protocol.Entry{Name: "pix", Type: protocol.TypeDir}, protocol.Entry{Name: "notes.txt", Type: protocol.TypeFile, Size: 12})
	f.mu.Lock()
	defer f.mu.Unlock()
	r.Stores = f.Stores
	if f.remotes == nil {
		f.remotes, f.down = map[string]*Fake{}, map[string]bool{}
	}
	f.remotes[name], f.down[name] = r, down
	// A remote that never answered: the controller has nothing saved.
	r.offline, r.unsaved = down, down
	return r
}

// SetOffline makes a paired remote stop (or start) answering. saved is
// whether the controller holds its saved copy and repository: with it,
// the remote's sources, snapshots and files still read; without, nothing
// does.
func (f *Fake) SetOffline(name string, offline, saved bool) {
	f.mu.Lock()
	r := f.remotes[name]
	f.down[name] = offline
	f.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.offline, r.unsaved = offline, offline && !saved
	r.mu.Unlock()
}

func (f *Fake) Hosts(_ context.Context) ([]protocol.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	hosts := []protocol.Host{{Name: f.Name, Local: true, State: protocol.HostOK}}
	names := slices.Sorted(maps.Keys(f.remotes))
	for _, n := range names {
		h := protocol.Host{Name: n, Address: n + ".lan:7457", State: protocol.HostOK}
		if a := f.addrs[n]; a != "" {
			h.Address = a
		}
		if f.down[n] {
			h.State, h.Error = protocol.HostUnreachable, "connection refused"
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}

// ForHost is the remote's own Fake; this one for its own name or "".
func (f *Fake) ForHost(host string) Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.remotes[host]; r != nil {
		return r
	}
	return f
}
