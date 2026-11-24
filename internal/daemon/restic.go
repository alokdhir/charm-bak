package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm-bak/internal/backends"
	"charm-bak/internal/protocol"
)

// ResticEngine runs the restic CLI with --json. Every restic detail
// (flags, JSON shapes, exit codes) stays in this file.
type ResticEngine struct {
	Binary string
	Repo   RepositoryConfig
	// Secrets fetches secret environment (AWS keys) per run; nil for none.
	Secrets func(RepositoryConfig) (map[string]string, error)
}

// noLock goes on every read. The daemon never reads while its own prune
// runs (Core.readable), and each machine has its own repository, so the
// lock only costs time: about 0.4s per call on S3 (measured 2026-11-20).
// A prune run by hand meanwhile can make a read fail, never damage data.
const noLock = "--no-lock"

// defaultBackupFlags go on every backup (docs/design.md, Sources).
var defaultBackupFlags = backupFlags(runtime.GOOS)

// backupFlags are the default backup flags for an OS. restic only has
// --exclude-cloud-files where files can be cloud placeholders (iCloud,
// OneDrive): on Linux it's an unknown flag and the backup fails (seen on
// pantry, restic 0.18.1).
func backupFlags(goos string) []string {
	f := []string{"--exclude-caches", "--exclude-if-present", ".nobackup"}
	if goos == "darwin" || goos == "windows" {
		f = append(f, "--exclude-cloud-files")
	}
	return f
}

// FindRestic looks in the usual places: a daemon started by the menu bar
// app or systemd gets a minimal PATH.
func FindRestic() string {
	for _, p := range []string{"/opt/homebrew/bin/restic", "/usr/local/bin/restic", "/usr/bin/restic"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath("restic"); err == nil {
		return p
	}
	return "restic"
}

func (r *ResticEngine) env() ([]string, error) {
	var env []string
	for _, kv := range os.Environ() {
		// RESTIC_CACHE_DIR is the daemon's own (main sets it for a
		// service without HOME), not a repository's: it stays.
		if strings.HasPrefix(kv, "RESTIC_CACHE_DIR=") || !slices.ContainsFunc(backends.EnvPrefixes, func(p string) bool { return strings.HasPrefix(kv, p) }) {
			env = append(env, kv)
		}
	}
	env = append(env, "RESTIC_REPOSITORY="+r.Repo.Location, "RESTIC_PROGRESS_FPS=1")
	if r.Repo.Password != "" {
		env = append(env, "RESTIC_PASSWORD="+r.Repo.Password)
	}
	for k, v := range r.Repo.SecretEnv {
		env = append(env, k+"="+v)
	}
	if r.Repo.PasswordCommand != "" {
		env = append(env, "RESTIC_PASSWORD_COMMAND="+r.Repo.PasswordCommand)
	}
	if r.Repo.PasswordFile != "" {
		env = append(env, "RESTIC_PASSWORD_FILE="+r.Repo.PasswordFile)
	}
	for k, v := range r.Repo.Environment {
		env = append(env, k+"="+v)
	}
	if r.Secrets != nil {
		secret, err := r.Secrets(r.Repo)
		if err != nil {
			return nil, err
		}
		for k, v := range secret {
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}

// output is a finished restic process.
type output struct {
	status    int
	stdout    []byte
	stderr    []byte // the last 8 KiB
	truncated bool   // stdout hit the limit and restic was stopped
	signal    string // what killed restic, if a signal did
}

const stderrKeep = 8 << 10

// run runs restic. With onLine, stdout is streamed line by line instead
// of kept. With limit > 0, restic is stopped once that much stdout is in.
// Cancelling ctx sends SIGINT, which lets restic remove its lock.
func (r *ResticEngine) run(ctx context.Context, args []string, onLine func([]byte), limit int, ok ...int) (output, error) {
	env, err := r.env()
	if err != nil {
		return output{}, err
	}
	cmd := exec.Command(r.Binary, args...)
	cmd.Env = env
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return output{}, err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return output{}, &EngineError{Kind: ErrFailed, Code: -1, Message: "couldn't start " + r.Binary + ": " + err.Error()}
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			cmd.Process.Signal(syscall.SIGINT)
		case <-done:
		}
	}()

	var out output
	if onLine != nil {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			if len(sc.Bytes()) > 0 {
				onLine(sc.Bytes())
			}
		}
		io.Copy(io.Discard, stdout)
	} else {
		var buf bytes.Buffer
		chunk := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(chunk)
			buf.Write(chunk[:n])
			if limit > 0 && buf.Len() >= limit && !out.truncated {
				out.truncated = true
				cmd.Process.Signal(syscall.SIGTERM)
			}
			if err != nil {
				break
			}
		}
		out.stdout = buf.Bytes()
	}
	waitErr := cmd.Wait()
	close(done)
	out.stderr = stderr.Bytes()
	out.status = 0
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		out.status = exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			out.signal = ws.Signal().String()
			if ws.Signal() == syscall.SIGINT {
				out.status = 130
			}
		}
	} else if waitErr != nil {
		return out, waitErr
	}
	if out.truncated || out.status == 0 || slices.Contains(ok, out.status) {
		return out, nil
	}
	if ctx.Err() != nil {
		return out, &EngineError{Kind: ErrCancelled, Code: 130}
	}
	return out, resticError(out)
}

// resticError maps restic's exit codes (restic docs, "Exit status codes")
// and its JSON exit_error message onto EngineError.
func resticError(out output) *EngineError {
	msg := strings.TrimSpace(string(out.stderr))
	lines := bytes.Split(out.stderr, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			MessageType string `json:"message_type"`
			Message     string `json:"message"`
		}
		if json.Unmarshal(lines[i], &e) == nil && e.MessageType == "exit_error" {
			msg = e.Message
			break
		}
	}
	if msg == "" && out.signal != "" {
		// Killed from outside, e.g. the daemon restarting mid-call.
		msg = "restic was stopped (" + out.signal + "); try again."
	}
	switch out.status {
	case 10:
		return &EngineError{Kind: ErrNoRepository, Code: 10, Message: msg}
	case 11:
		return &EngineError{Kind: ErrLocked, Code: 11, Message: msg}
	case 12:
		return &EngineError{Kind: ErrWrongPassword, Code: 12, Message: msg}
	case 130:
		return &EngineError{Kind: ErrCancelled, Code: 130, Message: msg}
	}
	return &EngineError{Kind: ErrFailed, Code: out.status, Message: msg}
}

// tailBuffer keeps the last stderrKeep bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrKeep {
		t.buf = t.buf[len(t.buf)-stderrKeep:]
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.buf)
}

// The Engine methods.

func (r *ResticEngine) Init(ctx context.Context) error {
	if msg := missingVolume(r.Repo.Location); msg != "" {
		return &EngineError{Kind: ErrFailed, Message: msg}
	}
	_, err := r.run(ctx, []string{"init"}, nil, 0)
	return err
}

// probeTimeout bounds a probe that gets no answer at all.
const probeTimeout = 30 * time.Second

// retrying is how restic reports a backend error it's about to retry:
// "Stat(<config/>) returned error, retrying after 1.37s: <reason>". A
// wrong key or a missing bucket would otherwise retry for minutes.
var retrying = regexp.MustCompile(`returned error, retrying after [^:]+: (.*)`)

// Probe opens the repository's config (restic cat config): exit 0 opens,
// 10 nothing there, 12 the password doesn't fit. Without a password a
// placeholder is used, so an existing repository answers 12, not a
// prompt. The first retry restic announces ends the probe with its reason.
func (r *ResticEngine) Probe(ctx context.Context) (ProbeResult, error) {
	if msg := missingVolume(r.Repo.Location); msg != "" {
		return ProbeResult{State: protocol.ProbeFailed, Message: msg}, nil
	}
	repo := r.Repo
	if repo.Password == "" && repo.PasswordCommand == "" && repo.PasswordFile == "" {
		repo.Password = "charm-bak-probe"
	}
	e := &ResticEngine{Binary: r.Binary, Repo: repo, Secrets: r.Secrets}
	env, err := e.env()
	if err != nil {
		return ProbeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Binary, "cat", "config", noLock, "--json")
	cmd.Env = env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGINT) }
	cmd.WaitDelay = 5 * time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return ProbeResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return ProbeResult{}, &EngineError{Kind: ErrFailed, Code: -1, Message: "couldn't start " + r.Binary + ": " + err.Error()}
	}
	var reason string
	var tail tailBuffer
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		line := sc.Text()
		tail.Write([]byte(line + "\n"))
		if m := retrying.FindStringSubmatch(line); m != nil && reason == "" {
			reason = m[1]
			cancel()
		}
	}
	err = cmd.Wait()
	switch {
	case reason != "":
		return ProbeResult{State: protocol.ProbeFailed, Message: reason}, nil
	case err == nil:
		return ProbeResult{State: protocol.ProbeOpens}, nil
	case ctx.Err() != nil && ctx.Err() != context.Canceled:
		return ProbeResult{State: protocol.ProbeFailed, Message: "no answer in " + probeTimeout.String()}, nil
	case ctx.Err() != nil:
		return ProbeResult{}, &EngineError{Kind: ErrCancelled, Code: 130}
	}
	var exit *exec.ExitError
	code := -1
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	switch code {
	case 10:
		return ProbeResult{State: protocol.ProbeEmpty}, nil
	case 12:
		return ProbeResult{State: protocol.ProbePassword}, nil
	}
	return ProbeResult{State: protocol.ProbeFailed, Message: resticError(output{status: code, stderr: tail.Bytes()}).Message}, nil
}

// missingVolume explains a local location on a drive that isn't
// connected: restic would otherwise make the folder on the startup disk
// (init) or call it empty (probe). Only macOS mounts drives in one place.
//
// The folder above a local repository must exist too: restic would make
// every missing folder on the way, so a mistyped path (another machine's,
// say) would quietly become a new tree. Only the last folder is made.
func missingVolume(location string) string {
	if !strings.HasPrefix(location, "/") {
		return ""
	}
	if rest, ok := strings.CutPrefix(location, "/Volumes/"); ok {
		name, _, _ := strings.Cut(rest, "/")
		if _, err := os.Stat("/Volumes/" + name); err != nil {
			return "The drive " + name + " isn't connected."
		}
	}
	parent := filepath.Dir(filepath.Clean(location))
	if fi, err := os.Stat(parent); err != nil || !fi.IsDir() {
		return "There's no folder " + parent + " on this machine; only the last folder is made."
	}
	return ""
}

type resticSnapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Original string    `json:"original"`
	Summary  *struct {
		BackupEnd           *time.Time `json:"backup_end"`
		TotalFilesProcessed int64      `json:"total_files_processed"`
		TotalBytesProcessed int64      `json:"total_bytes_processed"`
		DataAdded           int64      `json:"data_added"`
	} `json:"summary"`
}

func (r *ResticEngine) Snapshots(ctx context.Context, tag string) ([]EngineSnapshot, error) {
	args := []string{noLock, "snapshots", "--json"}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	out, err := r.run(ctx, args, nil, 0)
	if err != nil {
		return nil, err
	}
	var raw []resticSnapshot
	if err := json.Unmarshal(out.stdout, &raw); err != nil {
		return nil, fmt.Errorf("restic snapshots: %w", err)
	}
	snaps := make([]EngineSnapshot, 0, len(raw))
	for _, s := range raw {
		e := EngineSnapshot{ID: s.ID, ShortID: s.ShortID, Time: s.Time, Paths: s.Paths, Hostname: s.Hostname, Tags: s.Tags, Original: s.Original}
		if e.Tags == nil {
			e.Tags = []string{}
		}
		if s.Summary != nil {
			e.End, e.Files, e.Bytes, e.BytesAdded = s.Summary.BackupEnd, s.Summary.TotalFilesProcessed, s.Summary.TotalBytesProcessed, s.Summary.DataAdded
		}
		snaps = append(snaps, e)
	}
	slices.SortFunc(snaps, func(a, b EngineSnapshot) int { return b.Time.Compare(a.Time) })
	return snaps, nil
}

type resticNode struct {
	MessageType string    `json:"message_type"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	Path        string    `json:"path"`
	Size        int64     `json:"size"`
	Permissions string    `json:"permissions"`
	MTime       time.Time `json:"mtime"`
}

// nodes is `restic ls` without --recursive: the node at p, plus its
// children when it's a directory.
func (r *ResticEngine) nodes(ctx context.Context, snapshot, p string) ([]resticNode, error) {
	out, err := r.run(ctx, []string{noLock, "ls", "--json", snapshot, p}, nil, 0)
	if err != nil {
		return nil, err
	}
	var nodes []resticNode
	for _, line := range bytes.Split(out.stdout, []byte("\n")) {
		var n resticNode
		if json.Unmarshal(line, &n) == nil && n.MessageType == "node" {
			nodes = append(nodes, n)
		}
	}
	return nodes, nil
}

func clean(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean(p)
}

func (r *ResticEngine) List(ctx context.Context, snapshot, dir string) ([]protocol.Entry, error) {
	want := clean(dir)
	all, err := r.nodes(ctx, snapshot, want)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, &EngineError{Kind: ErrNotFound, Message: fmt.Sprintf("%s isn't in snapshot %s.", want, snapshot)}
	}
	entries := []protocol.Entry{}
	for _, n := range all {
		if n.Path != want {
			entries = append(entries, n.entry())
		}
	}
	sortEntries(entries)
	return entries, nil
}

// Stats reads the repository's size from its index (about 1.5s on S3).
func (r *ResticEngine) Stats(ctx context.Context) (RepoStats, error) {
	out, err := r.run(ctx, []string{noLock, "stats", "--mode", "raw-data", "--json"}, nil, 0)
	if err != nil {
		return RepoStats{}, err
	}
	var s struct {
		TotalSize             int64 `json:"total_size"`
		TotalUncompressedSize int64 `json:"total_uncompressed_size"`
	}
	if err := json.Unmarshal(out.stdout, &s); err != nil {
		return RepoStats{}, fmt.Errorf("restic stats: %w", err)
	}
	if s.TotalUncompressedSize == 0 { // a repository without compression (format v1)
		s.TotalUncompressedSize = s.TotalSize
	}
	return RepoStats{Stored: s.TotalSize, Uncompressed: s.TotalUncompressedSize}, nil
}

// Tree lists a whole snapshot in one restic call (a few seconds for
// ~100k files, against about a second for each single folder).
func (r *ResticEngine) Tree(ctx context.Context, snapshot string) (map[string][]protocol.Entry, error) {
	dirs := map[string][]protocol.Entry{"/": {}}
	_, err := r.run(ctx, []string{noLock, "ls", "--json", snapshot}, func(line []byte) {
		var n resticNode
		if json.Unmarshal(line, &n) != nil || n.MessageType != "node" || n.Path == "" {
			return
		}
		parent := path.Dir(n.Path)
		dirs[parent] = append(dirs[parent], n.entry())
		if n.Type == "dir" && dirs[n.Path] == nil {
			dirs[n.Path] = []protocol.Entry{}
		}
	}, 0)
	if err != nil {
		return nil, err
	}
	for _, entries := range dirs {
		sortEntries(entries)
	}
	return dirs, nil
}

// entry is a node as the protocol shows it.
func (n resticNode) entry() protocol.Entry {
	t := protocol.TypeOther
	switch n.Type {
	case "dir":
		t = protocol.TypeDir
	case "file":
		t = protocol.TypeFile
	case "symlink":
		t = protocol.TypeSymlink
	}
	size := n.Size
	if t == protocol.TypeDir {
		size = 0
	}
	return protocol.Entry{Name: n.Name, Type: t, Size: size, Mode: n.Permissions, MTime: n.MTime}
}

// sortEntries puts directories first, then sorts by name.
func sortEntries(entries []protocol.Entry) {
	slices.SortFunc(entries, func(a, b protocol.Entry) int {
		if (a.Type == protocol.TypeDir) != (b.Type == protocol.TypeDir) {
			if a.Type == protocol.TypeDir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// Read looks the file up and dumps it at the same time: two restic
// calls, but one call's wait. A dump of something that isn't a file is
// thrown away.
func (r *ResticEngine) Read(ctx context.Context, snapshot, file string, max int) ([]byte, int64, error) {
	want := clean(file)
	var out output
	var dumpErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, dumpErr = r.run(ctx, []string{noLock, "dump", snapshot, want}, nil, max)
	}()
	all, err := r.nodes(ctx, snapshot, want)
	<-done
	if err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(all, func(n resticNode) bool { return n.Path == want })
	if i < 0 {
		return nil, 0, &EngineError{Kind: ErrNotFound, Message: fmt.Sprintf("%s isn't in snapshot %s.", want, snapshot)}
	}
	if all[i].Type != "file" {
		return nil, 0, &EngineError{Kind: ErrNotFound, Message: want + " isn't a file."}
	}
	if dumpErr != nil {
		return nil, 0, dumpErr
	}
	data := out.stdout
	if len(data) > max {
		data = data[:max]
	}
	return data, all[i].Size, nil
}

type statusLine struct {
	MessageType   string   `json:"message_type"`
	PercentDone   float64  `json:"percent_done"`
	TotalFiles    int64    `json:"total_files"`
	FilesDone     int64    `json:"files_done"`
	FilesRestored int64    `json:"files_restored"`
	TotalBytes    int64    `json:"total_bytes"`
	BytesDone     int64    `json:"bytes_done"`
	BytesRestored int64    `json:"bytes_restored"`
	CurrentFiles  []string `json:"current_files"`
	SnapshotID    string   `json:"snapshot_id"`
	FilesNew      int64    `json:"files_new"`
	FilesChanged  int64    `json:"files_changed"`
	DataAdded     int64    `json:"data_added"`
	// In the summary line only.
	TotalFilesProcessed int64 `json:"total_files_processed"`
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
}

func (s statusLine) progress() Progress {
	p := Progress{Percent: s.PercentDone, FilesDone: max(s.FilesDone, s.FilesRestored), TotalFiles: s.TotalFiles,
		BytesDone: max(s.BytesDone, s.BytesRestored), TotalBytes: s.TotalBytes}
	if len(s.CurrentFiles) > 0 {
		p.Current = s.CurrentFiles[0]
	}
	return p
}

func (r *ResticEngine) Backup(ctx context.Context, spec BackupSpec, progress func(Progress)) (BackupResult, error) {
	args := append([]string{"backup", "--json", "--tag", spec.Tag}, defaultBackupFlags...)
	for _, e := range spec.Excludes {
		args = append(args, "--exclude", e)
	}
	args = append(args, spec.Paths...)
	var summary *statusLine
	out, err := r.run(ctx, args, func(line []byte) {
		var s statusLine
		if json.Unmarshal(line, &s) != nil {
			return
		}
		switch s.MessageType {
		case "status":
			if progress != nil {
				progress(s.progress())
			}
		case "summary":
			summary = &s
		}
	}, 0, 3)
	if err != nil {
		return BackupResult{}, err
	}
	if summary == nil || summary.SnapshotID == "" {
		return BackupResult{}, &EngineError{Kind: ErrFailed, Code: out.status, Message: "restic finished without a summary"}
	}
	return BackupResult{Incomplete: out.status == 3, Snapshot: summary.SnapshotID, FilesNew: summary.FilesNew,
		FilesChanged: summary.FilesChanged, BytesAdded: summary.DataAdded,
		Files: summary.TotalFilesProcessed, Bytes: summary.TotalBytesProcessed}, nil
}

// keepFlags is forget's policy, or nil when every count is zero.
func keepFlags(r protocol.Retention) []string {
	var flags []string
	for _, p := range []struct {
		flag string
		n    int
	}{{"--keep-last", r.Last}, {"--keep-hourly", r.Hourly}, {"--keep-daily", r.Daily},
		{"--keep-weekly", r.Weekly}, {"--keep-monthly", r.Monthly}, {"--keep-yearly", r.Yearly}} {
		switch {
		case p.n == protocol.KeepUnlimited:
			flags = append(flags, p.flag, "unlimited") // restic rejects -1
		case p.n > 0:
			flags = append(flags, p.flag, fmt.Sprint(p.n))
		}
	}
	return flags
}

// plan is what the policy would keep (with restic's reasons) and remove,
// without changing anything. nil maps when every count is zero.
func (r *ResticEngine) plan(ctx context.Context, tag string, ret protocol.Retention) (keep map[string][]string, remove []string, err error) {
	flags := keepFlags(ret)
	if flags == nil {
		return nil, nil, nil
	}
	args := append([]string{noLock, "forget", "--tag", tag, "--group-by", "tags", "--dry-run", "--json"}, flags...)
	out, err := r.run(ctx, args, nil, 0)
	if err != nil {
		return nil, nil, err
	}
	var groups []struct {
		Remove []struct {
			ID string `json:"id"`
		} `json:"remove"`
		Reasons []struct {
			Snapshot struct {
				ID string `json:"id"`
			} `json:"snapshot"`
			Matches []string `json:"matches"`
		} `json:"reasons"`
	}
	if err := json.Unmarshal(out.stdout, &groups); err != nil {
		return nil, nil, fmt.Errorf("restic forget: %w", err)
	}
	keep = map[string][]string{}
	for _, g := range groups {
		for _, reason := range g.Reasons {
			keep[reason.Snapshot.ID] = reason.Matches
		}
		for _, s := range g.Remove {
			remove = append(remove, s.ID)
		}
	}
	return keep, remove, nil
}

func (r *ResticEngine) ApplyRetention(ctx context.Context, tag string, ret protocol.Retention, pinned map[string]bool) error {
	// All zeros means "keep everything": restic would otherwise forget
	// every snapshot. Pins live in the daemon, so the policy runs as a dry
	// run and only the unpinned removals are forgotten, by ID.
	_, remove, err := r.plan(ctx, tag, ret)
	if err != nil {
		return err
	}
	var ids []string
	for _, id := range remove {
		if !pinned[id] {
			ids = append(ids, id)
		}
	}
	return r.Forget(ctx, ids)
}

func (r *ResticEngine) KeepReasons(ctx context.Context, tag string, ret protocol.Retention, pinned map[string]bool) (map[string][]string, error) {
	keep, _, err := r.plan(ctx, tag, ret)
	if err != nil {
		return nil, err
	}
	if keep == nil {
		keep = map[string][]string{}
	}
	for id := range pinned {
		keep[id] = append([]string{"pinned"}, keep[id]...)
	}
	return keep, nil
}

func (r *ResticEngine) Forget(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.run(ctx, append([]string{"forget"}, ids...), nil, 0)
	return err
}

// literalPattern is p as a restic pattern that matches only p itself.
// restic reads --exclude and --include values as globs, so a path passed
// as is over-matches: "[2024] Taxes" is a character class and took out a
// sibling "2 Taxes" instead (tested with restic 0.19.1, as was this
// escaping, for \ * ? [ in rewrite --exclude and restore --include; ]
// { } $ ! # and spaces need none).
func literalPattern(p string) string {
	if !strings.ContainsAny(p, `\*?[`) {
		return p
	}
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '\\', '*', '?', '[':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// rewriteCount is the count rewrite ends with: "modified 2 snapshots",
// "would modify 1 snapshot"; "no snapshots …" is 0. restic 0.19.1;
// rewrite has no --json output.
var rewriteCount = regexp.MustCompile(`(?m)^(?:would modify|modified) (\d+) snapshots?$`)

func (r *ResticEngine) Rewrite(ctx context.Context, tag string, excludes []string, dryRun bool) (int, error) {
	if len(excludes) == 0 {
		return 0, nil
	}
	args := []string{"rewrite", "--tag", tag}
	if dryRun {
		args = append(args, "--dry-run")
	} else {
		args = append(args, "--forget")
	}
	for _, e := range excludes {
		args = append(args, "--exclude", literalPattern(e))
	}
	out, err := r.run(ctx, args, nil, 0)
	if err != nil {
		return 0, err
	}
	m := rewriteCount.FindSubmatch(out.stdout)
	if m == nil {
		return 0, nil // "no snapshots would be modified"
	}
	return strconv.Atoi(string(m[1]))
}

func (r *ResticEngine) Added(ctx context.Context, from, to string) (int64, error) {
	out, err := r.run(ctx, []string{noLock, "diff", "--json", from, to}, nil, 0)
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(out.stdout, []byte("\n")) {
		var st struct {
			MessageType string `json:"message_type"`
			Added       struct {
				Bytes int64 `json:"bytes"`
			} `json:"added"`
		}
		if json.Unmarshal(line, &st) == nil && st.MessageType == "statistics" {
			return st.Added.Bytes, nil
		}
	}
	return 0, fmt.Errorf("restic diff: no statistics in its output")
}

// Changes is restic diff --metadata: one "change" line per path with a
// modifier (+ added, - removed, U the same contents with different
// metadata: touched; M content, T type and ? unreadable count as
// changed), then the statistics. Touched folders are left out: a folder
// is touched whenever anything inside it is.
func (r *ResticEngine) Changes(ctx context.Context, from, to string) (EngineChanges, error) {
	out, err := r.run(ctx, []string{noLock, "diff", "--metadata", "--json", from, to}, nil, 0)
	if err != nil {
		return EngineChanges{}, err
	}
	var res EngineChanges
	for _, line := range bytes.Split(out.stdout, []byte("\n")) {
		var l struct {
			MessageType string `json:"message_type"`
			Path        string `json:"path"`
			Modifier    string `json:"modifier"`
			Added       struct {
				Bytes int64 `json:"bytes"`
			} `json:"added"`
		}
		if len(line) == 0 || json.Unmarshal(line, &l) != nil {
			continue
		}
		switch l.MessageType {
		case "change":
			change := protocol.ChangeChanged
			switch l.Modifier {
			case "+":
				change = protocol.ChangeAdded
			case "-":
				change = protocol.ChangeRemoved
			case "U":
				if strings.HasSuffix(l.Path, "/") {
					continue
				}
				change = protocol.ChangeTouched
			}
			res.Paths = append(res.Paths, protocol.PathChange{Path: l.Path, Change: change})
		case "statistics":
			res.BytesAdded = l.Added.Bytes
		}
	}
	slices.SortStableFunc(res.Paths, func(a, b protocol.PathChange) int { return strings.Compare(a.Path, b.Path) })
	return res, nil
}

func (r *ResticEngine) Maintain(ctx context.Context) error {
	_, err := r.run(ctx, []string{"prune"}, nil, 0)
	return err
}

func (r *ResticEngine) Check(ctx context.Context, readPercent int) error {
	readPercent = min(max(readPercent, 1), 100)
	_, err := r.run(ctx, []string{"check", fmt.Sprintf("--read-data-subset=%d%%", readPercent)}, nil, 0)
	return err
}

// restoreBase is the folder a restore is relative to: the deepest one
// holding all of paths ("/" when there are none).
func restoreBase(paths []string) string {
	if len(paths) == 0 {
		return "/"
	}
	parents := make([]string, len(paths))
	for i, p := range paths {
		parents[i] = path.Dir(path.Clean(p))
	}
	common := parents[0]
	for _, p := range parents[1:] {
		for common != "/" && p != common && !strings.HasPrefix(p, common+"/") {
			common = path.Dir(common)
		}
	}
	return common
}

func (r *ResticEngine) Restore(ctx context.Context, snapshot string, paths []string, target, overwrite string, progress func(Progress)) error {
	mode := "if-newer"
	switch overwrite {
	case protocol.OverwriteNever:
		mode = "never"
	case protocol.OverwriteAlways:
		mode = "always"
	}
	// restic's snapshot:folder form restores relative to that folder, so
	// items land directly in the target.
	base := restoreBase(paths)
	if target == "" {
		target = base
	}
	args := []string{"restore", "--json", snapshot + ":" + base, "--target", target, "--overwrite", mode}
	for _, p := range paths {
		if rel := strings.TrimPrefix(p, base); rel != "" && rel != "/" {
			args = append(args, "--include", literalPattern("/"+strings.TrimPrefix(rel, "/")))
		}
	}
	_, err := r.run(ctx, args, func(line []byte) {
		var s statusLine
		if json.Unmarshal(line, &s) == nil && s.MessageType == "status" && progress != nil {
			progress(s.progress())
		}
	}, 0)
	return err
}
