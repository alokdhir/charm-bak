package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// folderPicker chooses what a source backs up by ticking the live disk:
// every file and folder has a box, space flips it and everything inside,
// and inside a ticked folder space excludes just that item (inside an
// excluded one, re-includes it). → opens a folder, ← goes up, Enter is
// done. The first row is the folder being shown, so it can be ticked
// too (~ itself for a whole-home source). The ticks are a
// backup.Selection, which compiles to restic's paths and patterns.
type folderPicker struct {
	dir     string
	entries []pickerEntry
	cur     cursor
	back    []cursor // cursors of the folders above, for ←
	sel     *backup.Selection
	hidden  bool // show dot-files
	// host whose disk this is; repo lists it when it's a remote (ls on
	// the live disk), nil for this machine's own.
	host   string
	repo   backup.Repo
	home   string // shown as ~; "" on a remote
	loadID int
	err    error
	// pendingSelect is a name to put the cursor on once the listing
	// arrives (going up past where the picker started).
	pendingSelect string
	// restore is the cursor to put back once the listing arrives (going
	// back up into a folder walked through).
	restore *cursor

	// done gets the ticks; cancel runs on Esc.
	done   func(a *App, sel *backup.Selection) tea.Cmd
	cancel func(a *App) tea.Cmd
}

type pickerEntry struct {
	name string
	dir  bool
	size int64
}

type pickerDirMsg struct {
	id      int
	entries []pickerEntry
	err     error
}

var pickerLoads int

func (a *App) newFolderPicker(host, start string, sel *backup.Selection, done func(*App, *backup.Selection) tea.Cmd, cancel func(*App) tea.Cmd) *folderPicker {
	p := &folderPicker{dir: start, sel: sel.Clone(), done: done, cancel: cancel, host: host, home: a.homeFor(host)}
	if a.isRemote(host) {
		p.repo = a.repo.ForHost(host)
	}
	return p
}

// load reads p.dir in the background; replies for an older read are
// dropped. Folders first, then files, each by name.
func (p *folderPicker) load() tea.Cmd {
	pickerLoads++
	p.loadID = pickerLoads
	id, dir, hidden, repo := p.loadID, p.dir, p.hidden, p.repo
	return func() tea.Msg {
		var out []pickerEntry
		var err error
		if repo != nil {
			var entries []protocol.Entry
			entries, err = repo.List(context.Background(), protocol.LiveSnapshot, dir)
			for _, e := range entries {
				if hidden || !strings.HasPrefix(e.Name, ".") {
					out = append(out, pickerEntry{name: e.Name, dir: e.Type == protocol.TypeDir, size: e.Size})
				}
			}
		} else {
			var des []os.DirEntry
			des, err = os.ReadDir(dir)
			for _, de := range des {
				if !hidden && strings.HasPrefix(de.Name(), ".") {
					continue
				}
				e := pickerEntry{name: de.Name(), dir: de.IsDir()}
				if !e.dir {
					if info, err := de.Info(); err == nil {
						e.size = info.Size()
					}
				}
				out = append(out, e)
			}
		}
		slices.SortFunc(out, func(a, b pickerEntry) int {
			if a.dir != b.dir {
				if a.dir {
					return -1
				}
				return 1
			}
			return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
		})
		return pickerDirMsg{id: id, entries: out, err: err}
	}
}

func (p *folderPicker) typing() bool { return false }

// Row 0 is the folder itself; row i is entries[i-1].
func (p *folderPicker) rowPath(i int) string {
	if i == 0 {
		return p.dir
	}
	return filepath.Join(p.dir, p.entries[i-1].name)
}

func (p *folderPicker) rowCount() int { return len(p.entries) + 1 }

// listHeight is how many rows of the list fit: title, path, blank, list,
// blank, summary, hints.
func (p *folderPicker) listHeight(a *App) int { return max(a.modalHeight()-6, 3) }

func (p *folderPicker) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case pickerDirMsg:
		if msg.id != p.loadID {
			return nil
		}
		p.entries, p.err = msg.entries, msg.err
		if p.restore != nil {
			p.cur, p.restore = *p.restore, nil
		}
		if i := slices.IndexFunc(p.entries, func(e pickerEntry) bool { return e.name == p.pendingSelect }); i >= 0 {
			p.cur.pos = i + 1
		}
		p.pendingSelect = ""
		p.cur.move(0, p.rowCount(), p.listHeight(a))
		return nil
	case tea.KeyPressMsg:
		return p.key(a, msg)
	}
	return nil
}

func (p *folderPicker) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	h := p.listHeight(a)
	p.cur.pos = min(p.cur.pos, len(p.entries)) // never past the rows there are
	switch {
	case key.Matches(msg, keyEsc):
		a.modal = nil
		if p.cancel != nil {
			return p.cancel(a)
		}
	case key.Matches(msg, keyUp):
		p.cur.move(-1, p.rowCount(), h)
	case key.Matches(msg, keyDown):
		p.cur.move(1, p.rowCount(), h)
	case key.Matches(msg, keyPageUp):
		p.cur.move(-h, p.rowCount(), h)
	case key.Matches(msg, keyPageDown):
		p.cur.move(h, p.rowCount(), h)
	case key.Matches(msg, keyTop):
		p.cur.move(-p.rowCount(), p.rowCount(), h)
	case key.Matches(msg, keyBottom):
		p.cur.move(p.rowCount(), p.rowCount(), h)
	case key.Matches(msg, keyRight):
		if p.cur.pos == 0 || !p.entries[p.cur.pos-1].dir {
			return nil
		}
		p.back = append(p.back, p.cur)
		p.dir, p.entries, p.cur = p.rowPath(p.cur.pos), nil, cursor{}
		return p.load()
	case key.Matches(msg, keyLeft):
		if p.dir == "/" {
			return nil
		}
		child := filepath.Base(p.dir)
		p.dir, p.entries, p.cur = filepath.Dir(p.dir), nil, cursor{}
		if n := len(p.back); n > 0 {
			// Put back once the listing is here: until then there are no
			// rows for the cursor to be on.
			saved := p.back[n-1]
			p.restore, p.back = &saved, p.back[:n-1]
		} else {
			p.pendingSelect = child
		}
		return p.load()
	case key.Matches(msg, keyMark):
		p.sel.Toggle(p.rowPath(p.cur.pos))
		p.cur.move(1, p.rowCount(), h)
	case key.Matches(msg, keyHidden):
		p.hidden = !p.hidden
		return p.load()
	case key.Matches(msg, keyEnter):
		if p.sel.Empty() {
			p.sel.Toggle(p.rowPath(p.cur.pos)) // nothing ticked: take the highlighted one
		}
		a.modal = nil
		return p.done(a, p.sel)
	}
	return nil
}

// tickBox draws a path's box: [x] in, [ ] out, [-] mixed.
func tickBox(t backup.Tick) string {
	switch t {
	case backup.Ticked:
		return styles.HealthStyle(styles.OK).Render("[x]")
	case backup.Mixed:
		return styles.HealthStyle(styles.Warn).Render("[-]")
	}
	return styles.MutedStyle().Render("[ ]")
}

func (p *folderPicker) view(a *App) string {
	inner := popupInnerWidth(a.width)
	var b strings.Builder
	title := " Choose what to back up "
	if a.grouped() {
		title = " Choose what to back up on " + p.host + " "
	}
	b.WriteString(popupTitle(title) + "\n")
	b.WriteString(styles.FieldValueStyle().Render(midTrunc(shortPath(p.home, p.dir), inner)) + "\n\n")

	h := p.listHeight(a)
	shown := 0
	for i := p.cur.off; i < min(p.cur.off+h, p.rowCount()); i++ {
		path := p.rowPath(i)
		var name, right string
		if i == 0 {
			name = "· this folder"
		} else {
			e := p.entries[i-1]
			name = e.name
			if e.dir {
				name += "/"
			} else {
				right = styles.MutedStyle().Render(humanBytes(e.size))
			}
		}
		if s, ok := a.sourceWithPath(p.host, path); ok {
			right = styles.MutedStyle().Render("source " + s.Name)
		}
		w := inner - 2
		left := tickBox(p.sel.Tick(path)) + " " + midTrunc(name, w-5-lipgloss.Width(right))
		line := fitRow(left, right, w)
		if i == p.cur.pos {
			line = styles.SelectedStyle().Render(ansi.Strip(line))
		}
		b.WriteString(" " + line + "\n")
		shown++
	}
	if p.err != nil {
		b.WriteString(errorLine(p.err) + "\n")
		shown++
	}
	b.WriteString(strings.Repeat("\n", max(h-shown, 0)) + "\n")
	b.WriteString(selectionSummary(p.sel, p.home, inner) + "\n")
	b.WriteString(hints("space", "tick", "→", "open", "←", "up", ".", "hidden", "enter", "done", "esc", "cancel"))
	return boxed(b.String(), inner)
}

// selectionSummary is one line on what the ticks add up to.
func selectionSummary(sel *backup.Selection, home string, width int) string {
	paths, _ := sel.Compile()
	if len(paths) == 0 {
		return styles.MutedStyle().Render("nothing ticked: Enter takes the highlighted item")
	}
	var names []string
	for _, p := range paths {
		names = append(names, shortPath(home, p))
	}
	text := strings.Join(names, ", ")
	if n := sel.Exceptions(); n > 0 {
		text += "  (" + pluralize(n, "exception") + ")"
	}
	return styles.FieldLabelStyle().Render("Backing up  ") + styles.FieldValueStyle().Render(midTrunc(text, width-12))
}

// pluralize is "1 thing", "3 things".
func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
