package ui

import "charm.land/bubbles/v2/key"

// Key bindings. helpBindings lists them for the help popup, so help can't
// drift from what's actually bound.
var (
	keyUp       = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up; from the top row, to the hero"))
	keyDown     = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down; from the hero, back to the columns"))
	keyLeft     = key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "previous column / previous field"))
	keyRight    = key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "open: next column / next field"))
	keyEnter    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open, or activate the focused field"))
	keyPageUp   = key.NewBinding(key.WithKeys("pgup"), key.WithHelp("pgup", "page up"))
	keyPageDown = key.NewBinding(key.WithKeys("pgdown"), key.WithHelp("pgdn", "page down"))
	keyTop      = key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("home/g", "first row"))
	keyBottom   = key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("end/G", "last row"))
	keyEsc      = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close popup / leave the hero"))
	keyInfo     = key.NewBinding(key.WithKeys("i", "space"), key.WithHelp("i/space", "info: everything about the selection, full names, click a row to copy; ↑↓ move to the next item with it open, shift+↑↓ or the wheel scroll a long one"))
	keyHelp     = key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "this help"))
	keyTasks    = key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("^T", "running jobs, on every machine (cancel one with c)"))
	keyNotices  = key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("^N", "past notifications: failures and finished clean-ups"))
	keyQuit     = key.NewBinding(key.WithKeys("ctrl+c", "q"), key.WithHelp("q", "quit"))

	// Action-bar hotkeys. They work whatever has focus, but only in the
	// columns that list them (actions()). Either case works: the bar
	// shows them capitalized ("[A]dd").
	keyAdd     = key.NewBinding(key.WithKeys("a", "A"), key.WithHelp("a", "add sources (Sources)"))
	keyEdit    = key.NewBinding(key.WithKeys("e", "E"), key.WithHelp("e", "edit settings (Sources) / a machine's defaults and address (its header) / note (Snapshots)"))
	keyRemove  = key.NewBinding(key.WithKeys("x", "X"), key.WithHelp("x", "remove a source, or a machine on its header (Sources)"))
	keyRemote  = key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("r", "add a remote: pair a Linux machine and set it up (Sources)"))
	keyBackup  = key.NewBinding(key.WithKeys("b", "B"), key.WithHelp("b", "back up now (Sources)"))
	keyRestore = key.NewBinding(key.WithKeys("r", "R"), key.WithHelp("r", "restore: a copy in a folder, or in place (Snapshots, tree, diff)"))
	keyPin     = key.NewBinding(key.WithKeys("p", "P"), key.WithHelp("p", "pin / unpin (Snapshots)"))
	keyDiff    = key.NewBinding(key.WithKeys("d", "D"), key.WithHelp("d", "compare a file or folder with what's on disk now (tree)"))
	keyChanges = key.NewBinding(key.WithKeys("c", "C"), key.WithHelp("c", "changes: what this snapshot added, removed and changed since the one before (Snapshots)"))
	keyExclude = key.NewBinding(key.WithKeys("x", "X"), key.WithHelp("x", "exclude from the source's backups from now on (tree)"))
	keyAdopt   = key.NewBinding(key.WithKeys("o", "O"), key.WithHelp("o", "adopt a machine's AWS key, one charm-bak didn't make, so it's rotated (its header)"))
	keyNewKey  = key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("n", "new AWS key: replace a machine's key now, when charm-bak made it (its header); it's also done every 90 days"))
	keyFinder  = key.NewBinding(key.WithKeys("f", "F"), key.WithHelp("f", "show it on disk now in Finder (Snapshots, tree; this Mac's sources)"))

	// Inside popups.
	keyMark   = key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "mark a folder (folder picker); toggle (forms)"))
	keyHidden = key.NewBinding(key.WithKeys("."), key.WithHelp(".", "show hidden folders (folder picker)"))
	keyNext   = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "to the hero and back; next field (forms)"))
	keyPrev   = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous field (forms)"))
)

var helpBindings = []key.Binding{
	keyUp, keyDown, keyLeft, keyRight, keyEnter, keyPageUp, keyPageDown, keyTop, keyBottom,
	keyAdd, keyEdit, keyRemove, keyRemote, keyBackup, keyRestore, keyPin, keyDiff, keyChanges, keyExclude, keyFinder, keyNewKey, keyAdopt,
	keyMark, keyHidden, keyNext, keyPrev,
	keyInfo, keyTasks, keyNotices, keyEsc, keyHelp, keyQuit,
}
