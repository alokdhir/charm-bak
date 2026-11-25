// Package ui is the Bubble Tea application: hero, action bar, and the
// Sources | Snapshots | tree columns (AGENTS.md, Decisions).
package ui

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// area is which band of the screen has keyboard focus. ↑ from the top row
// of a column moves to the hero. The action bar is a key guide, never
// focused (Alok, 2026-11-24: its buttons move with the selection, so
// walking to one is slower than pressing its letter).
type area int

const (
	areaColumns area = iota
	areaHero
)

// Fixed column indexes; tree levels follow from colTree on.
const (
	colSources = iota
	colSnapshots
	colTree
)

// cursor is a list position plus its scroll offset.
type cursor struct{ pos, off int }

// move steps the cursor by delta within n rows and keeps it inside a
// window of height rows.
func (c *cursor) move(delta, n, height int) {
	c.pos = min(max(c.pos+delta, 0), max(n-1, 0))
	c.keep(height)
}

// keep scrolls just enough for the cursor to be visible.
func (c *cursor) keep(height int) {
	height = max(height, 1)
	if c.pos < c.off {
		c.off = c.pos
	}
	if c.pos >= c.off+height {
		c.off = c.pos - height + 1
	}
	c.off = max(c.off, 0)
}

// node is a tree item: a directory entry plus where it lives, a snapshot
// and an absolute path (restic addresses everything that way).
type node struct {
	protocol.Entry
	snap string
	path string
}

func (n node) IsDir() bool { return n.Type == protocol.TypeDir }

// key identifies a node across loads, so stale replies can be spotted.
func (n node) key() string { return n.snap + "\x00" + n.path }

// dirLevel is one tree column: the listing of one directory.
type dirLevel struct {
	snap    string
	path    string
	name    string
	entries []protocol.Entry
	cur     cursor
}

func (lv dirLevel) key() string { return lv.snap + "\x00" + lv.path }

// child is the node for entry i of the level.
func (lv dirLevel) child(i int) node {
	e := lv.entries[i]
	return node{Entry: e, snap: lv.snap, path: backup.Join(lv.path, e.Name)}
}

// App is the main view.
type App struct {
	repo backup.Repo
	now  func() time.Time

	width, height int

	repoStatus   protocol.StatusResult
	hosts        []protocol.Host // this machine first, then remotes
	sources      []protocol.Source
	srcCur       cursor                               // indexes srcRows(), headers included
	collapsed    map[string]bool                      // folded host headers, this session only
	remoteJobs   map[string]*remoteJob                // by host: what each remote is running
	hostStatus   map[string]protocol.StatusResult     // by host: each remote's status
	repos        map[string]protocol.RepositoryResult // by host: its repository and defaults, for its header
	spin         spinner.Model                        // on sources that are backing up
	setupOffered bool                                 // the setup flow opened by itself this session
	statusKnown  bool                                 // a status reply has arrived
	sourcesKnown bool                                 // a sources reply has arrived
	spinning     bool                                 // its tick is scheduled
	snaps        []protocol.Snapshot
	snapsFor     string // sourceKey of the source snaps belongs to
	snapCur      cursor
	levels       []dirLevel
	file         filePreview

	col     int // focused column: colSources, colSnapshots, or colTree+level
	area    area
	heroIdx int

	job *protocol.Job // the app's running job, from events
	// restoring describes restores this TUI asked for, by job id, so the
	// footer can say where things went when each finishes.
	restoring map[string]string
	progress  *protocol.Progress // its latest progress
	modal     modal              // an open popup that owns the keyboard
	// wantSource is a source to select once the list next loads (one
	// just added).
	wantSource string

	showHelp bool
	showInfo bool
	// eventsLost: the event stream dropped, so whatever it would have
	// said is unknown until everything is read again.
	eventsLost bool
	// waiting: popups asked for by a daemon's answer while another popup
	// was open (modal.go's ask); each opens when the one before closes.
	waiting []modal
	// fileRoots: snapshots (by id) whose root isn't a folder, so their
	// tree opens at the folder holding it.
	fileRoots map[string]bool
	infoAnim  infoAnim
	focusedAt time.Time // last terminal focus report; see focusClickGrace
	statusMsg string
	err       error
}

// NewApp builds the main view over repo. now is injectable for tests.
func NewApp(repo backup.Repo, now func() time.Time) *App {
	return &App{repo: repo, now: now, restoring: map[string]string{},
		spin: spinner.New(spinner.WithSpinner(spinner.MiniDot))}
}

// Messages from the async loads. Each carries what it was loaded for, so
// a reply that arrives after the cursor has moved on is dropped instead of
// overwriting newer state.
type (
	statusMsg struct {
		st  protocol.StatusResult
		err error
	}
	sourcesMsg struct {
		hosts      []protocol.Host
		sources    []protocol.Source
		remoteJobs map[string]*protocol.Job // by host; nil job = idle
		hostStatus map[string]protocol.StatusResult
		repos      map[string]protocol.RepositoryResult
		err        error
	}
	snapshotsMsg struct {
		source string
		snaps  []protocol.Snapshot
		err    error
	}
	dirMsg struct {
		level   int
		dir     node
		entries []protocol.Entry
		err     error
	}
	fileMsg struct {
		key  string
		dump protocol.DumpResult
		err  error
	}
)

// filePreview is the file shown in the preview pane.
type filePreview struct {
	key     string
	dump    protocol.DumpResult
	err     error
	loading bool
}

func (a *App) Init() tea.Cmd {
	return tea.Batch(a.loadStatus(), a.loadSources(), a.subscribe())
}

func (a *App) loadSnapshots() tea.Cmd {
	src, ok := a.selSource()
	if !ok {
		return nil
	}
	repo := a.srcRepo()
	return func() tea.Msg {
		s, err := repo.Snapshots(context.Background(), src.ID)
		return snapshotsMsg{source: sourceKey(src), snaps: s, err: err}
	}
}

func (a *App) loadDir(level int, n node) tea.Cmd {
	repo := a.srcRepo()
	return func() tea.Msg {
		entries, err := repo.List(context.Background(), n.snap, n.path)
		return dirMsg{level: level, dir: n, entries: entries, err: err}
	}
}

// previewBytes caps how much of a file the preview reads.
const previewBytes = 64 << 10

func (a *App) loadFile(n node) tea.Cmd {
	repo := a.srcRepo()
	return func() tea.Msg {
		d, err := repo.Read(context.Background(), n.snap, n.path, previewBytes)
		return fileMsg{key: n.key(), dump: d, err: err}
	}
}

// snapshotRoot is the node a snapshot's tree opens at: the source's own
// folder, not restic's "/".
func snapshotRoot(s protocol.Snapshot) node {
	root := backup.Root(s.Paths)
	name := path.Base(root)
	return node{Entry: protocol.Entry{Name: name, Type: protocol.TypeDir, MTime: s.Time}, snap: s.ID, path: root}
}

// browseRoot is where a snapshot's tree columns open: its root, or the
// folder holding it when the root turned out not to be a folder (a source
// that is one file: there's nothing to list inside it, and in the
// snapshot its parent holds just that file).
func (a *App) browseRoot(s protocol.Snapshot) node {
	n := snapshotRoot(s)
	if a.fileRoots[s.ID] {
		n.path = path.Dir(n.path)
		n.Name = path.Base(n.path)
	}
	return n
}

// rootIsFile notes that a snapshot's root listed as nothing (a file lists
// as empty, or as not found once the daemon has the snapshot's tree in
// memory) and reports whether the tree should be opened one folder up
// instead. Only for single-path snapshots, and only once each.
func (a *App) rootIsFile(msg dirMsg) bool {
	if msg.level != 0 || a.snapCur.pos >= len(a.snaps) {
		return false
	}
	s := a.snaps[a.snapCur.pos]
	var pe *protocol.Error
	empty := msg.err == nil && len(msg.entries) == 0
	missing := errors.As(msg.err, &pe) && pe.Code == protocol.ErrNotFound
	if !(empty || missing) || len(s.Paths) != 1 || a.fileRoots[s.ID] || msg.dir.path == "/" {
		return false
	}
	if a.fileRoots == nil {
		a.fileRoots = map[string]bool{}
	}
	a.fileRoots[s.ID] = true
	return true
}

// wantEntry is what slot level of the tree shows given the current
// cursors: the selected snapshot's root for level 0, the selection in the
// level above otherwise. A directory there is listed as a tree column; a
// file is shown in the preview pane instead.
func (a *App) wantEntry(level int) (node, bool) {
	if level == 0 {
		if len(a.snaps) == 0 {
			return node{}, false
		}
		return a.browseRoot(a.snaps[a.snapCur.pos]), true
	}
	if level-1 >= len(a.levels) {
		return node{}, false
	}
	parent := a.levels[level-1]
	if len(parent.entries) == 0 {
		return node{}, false
	}
	return parent.child(parent.cur.pos), true
}

// nextLevel is the one tree slot right of the focused column: the pane
// that previews the focused selection. Nothing deeper is ever loaded, so
// the layout is always "the columns you've walked, plus one".
func (a *App) nextLevel() int {
	return max(a.col-colTree+1, 0)
}

// refreshFrom drops tree levels from level down and, if level is the
// preview slot, loads what it should show: a directory listing or a file.
// Called whenever a cursor above it moves.
func (a *App) refreshFrom(level int) tea.Cmd {
	if level < len(a.levels) {
		a.levels = a.levels[:level]
	}
	if level != a.nextLevel() {
		return nil
	}
	n, ok := a.wantEntry(level)
	if !ok {
		return nil
	}
	if n.IsDir() {
		return a.loadDir(level, n)
	}
	if a.file.key == n.key() {
		return nil // already loaded
	}
	a.file = filePreview{key: n.key(), loading: true}
	return a.loadFile(n)
}

// fitLevels trims or fills the tree after focus moves: keep the levels
// up to and including the preview slot, and load the slot if it's
// missing.
func (a *App) fitLevels() tea.Cmd {
	n := a.nextLevel()
	if len(a.levels) > n+1 {
		a.levels = a.levels[:n+1]
	}
	if len(a.levels) == n+1 {
		return nil
	}
	return a.refreshFrom(len(a.levels))
}

// selectedEntry is the cursor's item in the focused tree column.
func (a *App) selectedEntry() (node, bool) {
	lv := a.col - colTree
	if lv < 0 || lv >= len(a.levels) || len(a.levels[lv].entries) == 0 {
		return node{}, false
	}
	return a.levels[lv].child(a.levels[lv].cur.pos), true
}

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if tick, ok := msg.(spinner.TickMsg); ok {
		if !a.anyRunning() {
			a.spinning = false // stop: no next tick
			return a, nil
		}
		var cmd tea.Cmd
		a.spin, cmd = a.spin.Update(tick)
		return a, cmd
	}
	if tick, ok := msg.(infoTickMsg); ok {
		return a, a.infoStep(tick)
	}
	if wheel, ok := msg.(tea.MouseWheelMsg); ok && a.showInfo && a.modal == nil {
		// The wheel scrolls an info taller than the screen.
		switch wheel.Mouse().Button {
		case tea.MouseWheelUp:
			a.infoScroll(-1)
		case tea.MouseWheelDown:
			a.infoScroll(1)
		}
		return a, nil
	}
	wasInfo := a.showInfo
	m, cmd := a.update(msg)
	a.nextWaiting()
	// The info popup springs open, then to each selection's height.
	switch {
	case a.showInfo && !wasInfo:
		cmd = tea.Batch(cmd, a.infoOpened())
	case a.showInfo:
		cmd = tea.Batch(cmd, a.infoRetarget())
	case wasInfo:
		a.infoClosed()
	}
	// Start the spinner when something starts running anywhere.
	if a.anyRunning() && !a.spinning {
		a.spinning = true
		cmd = tea.Batch(cmd, a.spin.Tick)
	}
	return m, cmd
}

// offerSetup opens the setup flow by itself, once a session, when this
// machine has no repository. It needs the status and the machine's name
// (from hosts), which arrive separately.
func (a *App) offerSetup() tea.Cmd {
	if a.statusKnown && a.repoStatus.Repository == nil && a.localHost() != "" && !a.setupOffered && a.modal == nil {
		a.setupOffered = true
		return a.startSetup(a.localHost())
	}
	return nil
}

// anyRunning reports whether a backup or other job runs on any host.
func (a *App) anyRunning() bool {
	if f, ok := a.modal.(*setupFlow); ok && f.busy != "" {
		return true
	}
	if m, ok := a.modal.(*addRemote); ok && m.waiting(a.now()) {
		return true
	}
	return a.job != nil || len(a.remoteJobs) > 0 || slices.ContainsFunc(a.hosts, func(h protocol.Host) bool {
		return h.State != protocol.HostOK && h.Error == hostConnecting
	})
}

// spinFrame is the spinner's current frame, unstyled, for callers that
// color it themselves.
func (a *App) spinFrame() string { return ansi.Strip(a.spin.View()) }

func (a *App) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case excludedMsg:
		return a, a.excluded(msg)
	case rewriteStartedMsg:
		return a, a.rewriteStarted(msg)
	case finderMsg:
		a.statusMsg = msg.status
		return a, nil
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		a.keepCursors()
		return a, nil

	case statusMsg:
		a.repoStatus, a.err = msg.st, msg.err
		if msg.err == nil {
			a.statusKnown = true
			offer := a.offerSetup()
			a.job = msg.st.Job
			if a.job == nil {
				a.progress = nil
			}
			return a, offer
		}
		return a, nil

	case eventsReadyMsg:
		if a.eventsLost {
			// Events sent while the stream was down are gone (the daemon
			// restarted, say, and a job finished meanwhile): read it all
			// again instead of waiting for the next event.
			a.eventsLost = false
			return a, tea.Batch(waitEvent(msg.ch), a.loadStatus(), a.loadSources(), a.loadSnapshots())
		}
		return a, waitEvent(msg.ch)
	case eventMsg:
		return a, tea.Batch(a.handleEvent(msg.e), waitEvent(msg.ch))
	case eventsClosedMsg:
		a.eventsLost = true
		return a, tea.Tick(eventsRetry, func(time.Time) tea.Msg { return resubscribeMsg{} })
	case resubscribeMsg:
		return a, a.subscribe()

	case sourcesMsg:
		if msg.err != nil {
			a.err = msg.err
			return a, nil
		}
		// The first time the machines are known: a notification clicked to
		// start this TUI is waiting.
		var take tea.Cmd
		if !a.sourcesKnown {
			take = a.takeNotice()
			a.kittyHint()
		}
		a.sourcesKnown = true
		// Keep the cursor on the same source when the list is reloaded
		// (an event, an add or a remove), and only reload its snapshots
		// when the selection really changed.
		was, wasRow := a.selectedSourceID(), a.selectedRowKey()
		a.hosts, a.sources, a.err = msg.hosts, msg.sources, nil
		for host, j := range msg.remoteJobs {
			a.setRemoteJob(host, j)
		}
		// A remote that stopped answering or was removed isn't in the
		// reply: what it was running is unknown, not still running.
		for host := range a.remoteJobs {
			if _, ok := msg.remoteJobs[host]; !ok && msg.remoteJobs != nil {
				delete(a.remoteJobs, host)
			}
		}
		if msg.hostStatus != nil {
			a.hostStatus = msg.hostStatus
		}
		if msg.repos != nil {
			a.repos = msg.repos
		}
		paired := a.offerSetup()
		if m, ok := a.modal.(*addRemote); ok {
			paired = m.hostsChanged(a)
		}
		if a.wantSource != "" {
			// A source just added: unfold its host so it shows.
			host, _, _ := strings.Cut(a.wantSource, "/")
			delete(a.collapsed, host)
		}
		if i := a.rowIndex(a.wantSource); i >= 0 {
			a.srcCur.pos, a.wantSource = i, ""
		} else if i := a.rowIndex(wasRow); i >= 0 {
			a.srcCur.pos = i
		}
		if wasRow == "" && a.onHeader() && len(a.srcRows()) > 1 && !a.srcRows()[1].header() {
			a.srcCur.pos = 1 // start on the first source, not its machine
		}
		a.srcCur.move(0, len(a.srcRows()), a.listHeight())
		if now := a.selectedSourceID(); now != was || a.snapsFor != now {
			if a.col > colSources {
				a.col = colSources
			}
			a.snaps, a.snapsFor, a.levels = nil, "", nil
			return a, tea.Batch(a.loadSnapshots(), paired, take)
		}
		return a, tea.Batch(paired, take)

	case noticeTakenMsg:
		if msg.n != nil {
			return a, a.showNotice(*msg.n)
		}
		return a, nil

	case snapshotsMsg:
		if a.selectedSourceID() != msg.source {
			return a, nil // the cursor moved on
		}
		if msg.err != nil {
			a.err = msg.err
			return a, nil
		}
		// A reload of the same source keeps the cursor on the same
		// snapshot and leaves the tree alone if that snapshot is still
		// selected.
		prev := ""
		if a.snapsFor == msg.source && a.snapCur.pos < len(a.snaps) {
			prev = a.snaps[a.snapCur.pos].ID
		}
		a.snaps, a.snapsFor = msg.snaps, msg.source
		for i, s := range a.snaps {
			if s.ID == prev {
				a.snapCur.pos = i
				a.snapCur.keep(a.listHeight())
				return a, nil
			}
		}
		// The snapshot being browsed is gone (forgotten, or rewritten
		// under a new id): its tree columns go with it, and so does
		// focus, which would otherwise sit in a column that isn't there.
		a.snapCur = cursor{}
		a.col = min(a.col, colSnapshots)
		return a, a.refreshFrom(0)

	case dirMsg:
		want, ok := a.wantEntry(msg.level)
		if !ok || want.key() != msg.dir.key() || msg.level > len(a.levels) || msg.level > a.nextLevel() {
			return a, nil
		}
		if msg.level < len(a.levels) && a.levels[msg.level].key() == msg.dir.key() {
			// A second answer for a listing already shown (two loads of
			// the same folder were in flight): replacing it would reset
			// its cursor and drop the columns walked since.
			return a, nil
		}
		if a.rootIsFile(msg) {
			return a, a.refreshFrom(0)
		}
		if msg.err != nil {
			a.err = msg.err
			return a, nil
		}
		a.levels = append(a.levels[:msg.level], dirLevel{snap: msg.dir.snap, path: msg.dir.path, name: msg.dir.Name, entries: msg.entries})
		return a, nil

	case fileMsg:
		if msg.key != a.file.key {
			return a, nil // the cursor moved on
		}
		a.file = filePreview{key: msg.key, dump: msg.dump, err: msg.err}
		return a, nil

	case tea.FocusMsg:
		a.focusedAt = time.Now()
		return a, nil

	case tea.MouseClickMsg:
		return a, a.handleClick(msg)

	case hostRemovedMsg:
		return a, a.hostRemoved(msg)

	case changedMsg:
		if msg.err != nil {
			a.statusMsg = "error: " + msg.err.Error()
		} else {
			a.statusMsg = msg.note
		}
		return a, tea.Batch(a.loadSources(), a.loadStatus())

	case tea.KeyPressMsg:
		return a.handleKey(msg)
	case snapshotSetMsg:
		// A pin's answer is the App's whatever popup is open; a note's
		// goes to the form that asked.
		if f, ok := a.modal.(*noteForm); ok && msg.note && f.sending {
			return a, f.update(a, msg)
		}
		a.applySnapshot(msg)
		return a, nil
	}
	if a.modal != nil {
		return a, a.modal.update(a, msg)
	}
	return a, nil
}

// resubscribeMsg retries the event subscription.
type resubscribeMsg struct{}

func (a *App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// q quits from the main view; inside a popup only ctrl+c does, so a
	// stray q can't throw away a half-filled form.
	if key.Matches(msg, keyQuit) && (a.modal == nil || msg.String() == "ctrl+c") {
		return a, tea.Quit
	}
	if a.tooSmall() {
		// The whole screen says "resize, or q to quit": nothing else may
		// act on a selection that can't be seen.
		return a, nil
	}
	if a.showHelp {
		if key.Matches(msg, keyHelp, keyEsc) {
			a.showHelp = false
		}
		return a, nil
	}
	// A popup opened over info (Changes) has the keys until it closes;
	// info is still there underneath.
	if a.showInfo && a.modal == nil {
		// Like Quick Look: moving the column's cursor keeps the popup open
		// on the new selection (never up into the hero).
		h := a.listHeight()
		var d int
		switch {
		case key.Matches(msg, keyInfo, keyEsc):
			a.showInfo = false
			return a, nil
		case key.Matches(msg, keyChanges) && a.col == colSnapshots:
			// What this snapshot changed, in front of its info.
			return a, a.startChanges()
		case key.Matches(msg, keyUp):
			d = -1
		case key.Matches(msg, keyDown):
			d = 1
		case key.Matches(msg, keyPageUp):
			d = -h
		case key.Matches(msg, keyPageDown):
			d = h
		case key.Matches(msg, keyTop):
			d = -a.focusedLen()
		case key.Matches(msg, keyBottom):
			d = a.focusedLen()
		case key.Matches(msg, keyLeft, keyRight):
			// Across columns too: info follows to the selection there.
			a.area = areaColumns
			a.infoAnim.scroll = 0
			return a, a.handleColumnKey(msg)
		case msg.String() == "shift+up":
			a.infoScroll(-1)
			return a, nil
		case msg.String() == "shift+down":
			a.infoScroll(1)
			return a, nil
		default:
			return a, nil
		}
		// The cursor being walked is the column's, wherever info was
		// opened from (Enter on the hero's name leaves focus up there).
		a.area = areaColumns
		a.infoAnim.scroll = 0
		return a, a.moveCursor(d)
	}
	if a.modal != nil {
		return a, a.modal.update(a, msg)
	}
	if key.Matches(msg, keyHelp) {
		a.showHelp = true
		return a, nil
	}
	if key.Matches(msg, keyTasks) {
		return a, a.openTasks()
	}
	if key.Matches(msg, keyNotices) {
		return a, a.openNotices()
	}
	if key.Matches(msg, keyInfo) {
		a.showInfo = len(a.infoFields()) > 0
		return a, nil
	}

	// Tab jumps to the hero's first stop, and back to the columns: the
	// hero is otherwise only reachable by walking up to the top row.
	if key.Matches(msg, keyNext) {
		if a.area == areaHero {
			a.area = areaColumns
		} else if len(a.heroStops()) > 0 {
			a.area, a.heroIdx = areaHero, 0
		}
		return a, nil
	}

	// Action-bar letters work whatever has focus (charm-aws rule).
	for _, act := range a.actions() {
		if key.Matches(msg, act.key) {
			return a, a.runAction(act)
		}
	}

	switch a.area {
	case areaHero:
		return a, a.handleHeroKey(msg)
	}
	return a, a.handleColumnKey(msg)
}

// Arrow keys in the hero move to the nearest stop in that direction,
// judged by screen position, and do nothing when there isn't one. The
// hero's two lines are rows; Left and Right stay on a row, Up and Down
// change rows, and Down from the bottom line goes back to the columns.

// nearestSpan returns the index of the span whose center is closest to
// center, or -1 if there are none.
func nearestSpan(spans []buttonSpan, center int) int {
	best, bestD := -1, 0
	for i, s := range spans {
		d := s.x + s.w/2 - center
		if d < 0 {
			d = -d
		}
		if best < 0 || d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// heroLineSpans returns the stops on one hero line as screen spans, with
// each one's index into heroStops.
func heroLineSpans(stops []heroStop, line int) (spans []buttonSpan, idx []int) {
	for i, s := range stops {
		if s.line == line {
			spans = append(spans, buttonSpan{heroContentX + s.x, s.w})
			idx = append(idx, i)
		}
	}
	return spans, idx
}

func (a *App) handleHeroKey(msg tea.KeyPressMsg) tea.Cmd {
	stops := a.heroStops()
	if len(stops) == 0 {
		a.area = areaColumns
		return nil
	}
	a.heroIdx = min(a.heroIdx, len(stops)-1)
	cur := stops[a.heroIdx]
	center := heroContentX + cur.x + cur.w/2
	switch {
	case key.Matches(msg, keyEsc):
		a.area = areaColumns
	case key.Matches(msg, keyLeft):
		if a.heroIdx > 0 && stops[a.heroIdx-1].line == cur.line {
			a.heroIdx--
		}
	case key.Matches(msg, keyRight):
		if a.heroIdx+1 < len(stops) && stops[a.heroIdx+1].line == cur.line {
			a.heroIdx++
		}
	case key.Matches(msg, keyUp):
		for line := cur.line - 1; line >= 0; line-- {
			if spans, idx := heroLineSpans(stops, line); len(spans) > 0 {
				a.heroIdx = idx[nearestSpan(spans, center)]
				break
			}
		}
	case key.Matches(msg, keyDown):
		last := stops[len(stops)-1].line
		for line := cur.line + 1; line <= last; line++ {
			if spans, idx := heroLineSpans(stops, line); len(spans) > 0 {
				a.heroIdx = idx[nearestSpan(spans, center)]
				return nil
			}
		}
		a.area = areaColumns
	case key.Matches(msg, keyEnter):
		switch cur.label {
		case "Info":
			a.showInfo = true
		case "Schedule":
			if a.hostUp(a.selectionHost(), "Changing its schedule") {
				return a.startEdit(ffSchedule)
			}
		case "Retention":
			if a.hostUp(a.selectionHost(), "Changing what it keeps") {
				return a.startEdit(ffRetention)
			}
		case "Description":
			if a.hostUp(a.selectionHost(), "Editing a note") {
				return a.startNote()
			}
		case "Pin":
			if a.hostUp(a.selectionHost(), "Pinning") {
				return a.togglePin()
			}
		case "Address":
			return a.startHostEdit()
		default:
			a.statusMsg = notBuilt(cur.label)
		}
	}
	return nil
}

// notBuilt is the footer's answer to Enter on something that has no
// screen yet, worded so it can't be read as the thing's own state.
func notBuilt(what string) string {
	return "The " + strings.ToLower(what) + " screen isn't built yet"
}

func (a *App) handleColumnKey(msg tea.KeyPressMsg) tea.Cmd {
	h := a.listHeight()
	switch {
	case key.Matches(msg, keyUp):
		if a.focusedCursor() != nil && a.focusedCursor().pos == 0 {
			// Into the hero's bottom line, at its first stop.
			stops := a.heroStops()
			if len(stops) > 0 {
				last := stops[len(stops)-1].line
				a.area, a.heroIdx = areaHero, slices.IndexFunc(stops, func(s heroStop) bool { return s.line == last })
			}
			return nil
		}
		return a.moveCursor(-1)
	case key.Matches(msg, keyDown):
		return a.moveCursor(1)
	case key.Matches(msg, keyPageUp):
		return a.moveCursor(-h)
	case key.Matches(msg, keyPageDown):
		return a.moveCursor(h)
	case key.Matches(msg, keyTop):
		return a.moveCursor(-a.focusedLen())
	case key.Matches(msg, keyBottom):
		return a.moveCursor(a.focusedLen())
	case a.col == colSources && key.Matches(msg, keyEnter, keyRight, keyLeft) && a.onHeader():
		// A host header folds: Enter toggles, → opens, ← closes. On a
		// machine that still needs something (its header says "setup" or
		// "disk access"), Enter goes back to that step instead.
		h, _ := a.selHost()
		switch {
		case key.Matches(msg, keyEnter) && a.hostTodo(h.Name) == todoSetup:
			return a.startSetup(h.Name)
		case key.Matches(msg, keyEnter) && a.hostTodo(h.Name) == todoAccess:
			return a.startAccess(h.Name)
		case key.Matches(msg, keyEnter):
			return a.foldHost(!a.collapsed[h.Name])
		case key.Matches(msg, keyRight):
			return a.foldHost(false)
		default:
			return a.foldHost(true)
		}
	case key.Matches(msg, keyLeft):
		if a.col > colSources {
			a.col--
			return a.fitLevels()
		}
	case key.Matches(msg, keyRight, keyEnter):
		// Only into a column that exists and has something in it: a file
		// has no column to enter, and focus in an empty one (a source
		// with no snapshots, an empty folder) looks like a lost cursor.
		if a.col+1 < colTree+len(a.levels) && a.focusedLen() > 0 && a.colLen(a.col+1) > 0 {
			a.col++
			return a.fitLevels()
		}
	}
	return nil
}

// focusedCursor and focusedLen describe the focused column's list.
func (a *App) focusedCursor() *cursor {
	switch {
	case a.col == colSources:
		return &a.srcCur
	case a.col == colSnapshots:
		return &a.snapCur
	case a.col-colTree < len(a.levels):
		return &a.levels[a.col-colTree].cur
	}
	return nil
}

func (a *App) focusedLen() int { return a.colLen(a.col) }

// colLen is how many rows column col lists.
func (a *App) colLen(col int) int {
	switch {
	case col == colSources:
		return len(a.srcRows())
	case col == colSnapshots:
		return len(a.snaps)
	case col-colTree < len(a.levels):
		return len(a.levels[col-colTree].entries)
	}
	return 0
}

// moveCursor moves the focused column's cursor and reloads everything to
// its right.
func (a *App) moveCursor(delta int) tea.Cmd {
	c := a.focusedCursor()
	if c == nil {
		return nil
	}
	before := c.pos
	c.move(delta, a.focusedLen(), a.listHeight())
	if c.pos == before {
		return nil
	}
	a.statusMsg = ""
	switch {
	case a.col == colSources:
		a.snaps, a.snapsFor, a.levels = nil, "", nil
		return a.loadSnapshots()
	case a.col == colSnapshots:
		return a.refreshFrom(0)
	default:
		return a.refreshFrom(a.col - colTree + 1)
	}
}

// keepCursors re-clamps scroll offsets after a resize.
func (a *App) keepCursors() {
	h := a.listHeight()
	a.srcCur.keep(h)
	a.snapCur.keep(h)
	for i := range a.levels {
		a.levels[i].cur.keep(h)
	}
}

// Layout heights: hero box (2 lines plus border), action bar, column
// title row, footer.
const (
	heroHeight   = 4
	actionHeight = 1
	footerHeight = 1
)

// bodyHeight is the height of the columns, title row included.
func (a *App) bodyHeight() int {
	return max(a.height-heroHeight-actionHeight-footerHeight, 2)
}

// listHeight is the number of rows a column can show under its title.
func (a *App) listHeight() int { return a.bodyHeight() - 1 }

func (a *App) View() tea.View {
	if a.width == 0 {
		return tea.NewView("")
	}
	if a.tooSmall() {
		return tea.NewView(renderTooSmall(a.width, a.height))
	}

	hero := a.renderHero()
	bar := a.renderActionBar()
	body := lipgloss.NewStyle().Height(a.bodyHeight()).MaxHeight(a.bodyHeight()).Render(a.renderColumns())
	footer := a.renderFooter()

	switch {
	case a.showHelp:
		body = compositeOverlay(body, renderPopup(renderHelp(a.width), a.width), a.width, a.bodyHeight())
	case a.showInfo && a.modal != nil:
		// A popup opened from info draws in front of it.
		body = compositeOverlay(body, a.renderInfo(), a.width, a.bodyHeight())
		body = compositeOverlay(body, a.modal.view(a), a.width, a.bodyHeight())
	case a.showInfo:
		body = compositeOverlay(body, a.renderInfo(), a.width, a.bodyHeight())
	case a.modal != nil:
		body = compositeOverlay(body, a.modal.view(a), a.width, a.bodyHeight())
	}
	return tea.NewView(lipgloss.JoinVertical(lipgloss.Left, hero, bar, body, footer))
}

// handleClick copies what was clicked: a row of the info popup, or the
// selection's name in the hero (its full live path). Anything else is
// ignored, including the click that merely focused the window.
func (a *App) handleClick(msg tea.MouseClickMsg) tea.Cmd {
	if !a.focusedAt.IsZero() && time.Since(a.focusedAt) < focusClickGrace {
		a.focusedAt = time.Time{}
		return nil
	}
	m := msg.Mouse()
	if m.Button != tea.MouseLeft {
		return nil
	}
	var value string
	switch {
	case a.showInfo && a.modal == nil:
		f, ok := a.infoFieldAt(m.X, m.Y)
		if !ok {
			return nil
		}
		value = f.value
	case a.modal != nil:
		if am, ok := a.modal.(*addRemote); ok && am.commandAt(a, m.X, m.Y) {
			return am.copy(a)
		}
		f, ok := a.modal.(*setupFlow)
		if !ok || !f.keyAt(a, m.X, m.Y) {
			return nil
		}
		f.copied = true
		return tea.Batch(copyToClipboardCmd(f.key), audibleCueCmd())
	case !a.showHelp && a.heroNameAt(m.X, m.Y):
		value, _ = a.selectionPath()
	}
	if value == "" {
		return nil
	}
	a.statusMsg = "copied " + midTrunc(value, 60)
	return tea.Batch(copyToClipboardCmd(value), audibleCueCmd())
}

func (a *App) renderFooter() string {
	// What's running comes first (^T lists it), then the latest news.
	var parts []string
	if t := a.taskIndicator(); t != "" {
		parts = append(parts, t)
	}
	if a.err != nil {
		parts = append(parts, "error: "+a.err.Error())
	} else if a.statusMsg != "" {
		parts = append(parts, a.statusMsg)
	}
	return renderFooter(a.width, strings.Join(parts, " │ "))
}
