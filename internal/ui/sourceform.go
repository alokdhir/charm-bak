package ui

import (
	"charm-bak/internal/backup"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"charm-bak/internal/protocol"
	"charm-bak/internal/ui/styles"
)

// sourceForm adds a source or edits one: name, folders, excludes,
// schedule, retention, and (adding) whether to back up right away.
// Schedule and retention either follow the defaults or are set here.
//
// Keys: ↑/↓ or tab move between fields; ←/→ change a choice; space
// toggles; Enter on Folders re-opens the folder picker, on other fields
// moves to the next one, and on the buttons at the bottom (Add or Save,
// Cancel) presses one; Esc cancels.
type sourceForm struct {
	editing *protocol.Source // nil when adding
	host    string           // whose source: editing.Host, or where it's added

	name     textinput.Model
	sel      *backup.Selection // what's ticked: folders, files and exceptions
	excludes *patternList      // patterns that aren't one path (node_modules)
	schedule int               // index into scheduleChoices; 0 = the default
	keep     *keepEditor       // default, a preset, or custom counts
	now      bool              // back up right after adding
	buttons  formButtons

	field  int
	saving bool
	err    error
}

// Form fields, in order. The six keep counts exist only while custom.
const (
	ffName = iota
	ffFolders
	ffExcludes
	ffSchedule
	ffRetention
	ffKeep0     // ... ffKeep0+5
	ffBackUpNow = ffKeep0 + 6
	ffButtons   = ffBackUpNow + 1
)

// scheduleChoices are the schedules the form offers; 0 means "manual"
// and the first entry (-1) means "the default".
var scheduleChoices = []int64{-1, 0, 900, 1800, 3600, 2 * 3600, 6 * 3600, 12 * 3600, 24 * 3600}

// builtinDefaults mirror the daemon's BuiltinDefaults, for showing what
// "default" means before any source inherits it.
var builtinDefaults = struct {
	every int64
	keep  protocol.Retention
}{3600, protocol.Retention{Last: 10, Hourly: 24, Daily: 30, Weekly: 12, Monthly: 12}}

// defaultPolicy is the schedule and retention host's sources inherit:
// its defaults as loaded with the sources, else read off one of its
// sources that inherits them, else the built-in ones.
func (a *App) defaultPolicy(host string) (every int64, keep protocol.Retention) {
	if res, ok := a.repos[host]; ok {
		if res.Defaults.Schedule != nil {
			every = res.Defaults.Schedule.Every
		}
		return every, res.Defaults.Retention
	}
	every, keep = builtinDefaults.every, builtinDefaults.keep
	for _, s := range a.sources {
		if s.Host != host {
			continue
		}
		if slices.Contains(s.Inherited, "schedule") {
			every = 0
			if s.Schedule != nil {
				every = s.Schedule.Every
			}
			break
		}
	}
	for _, s := range a.sources {
		if s.Host == host && slices.Contains(s.Inherited, "retention") {
			keep = s.Retention
			break
		}
	}
	return every, keep
}

func newTextField(value, placeholder string) textinput.Model {
	t := textinput.New()
	t.Prompt = ""
	t.Placeholder = placeholder
	t.SetValue(value)
	return t
}

func retentionValues(r protocol.Retention) [6]int {
	return [6]int{r.Last, r.Hourly, r.Daily, r.Weekly, r.Monthly, r.Yearly}
}

// newSourceForm opens the form for adding sel, or for editing src;
// host is where a new source goes: its defaults are the ones shown.
func (a *App) newSourceForm(host string, src *protocol.Source, sel *backup.Selection) *sourceForm {
	f := &sourceForm{editing: src, sel: sel, now: true, host: host, buttons: newFormButtons("Add")}
	if src != nil {
		f.buttons = newFormButtons("Save")
	}
	if src != nil {
		f.host = src.Host
	}
	_, defKeep := a.defaultPolicy(f.host)
	keep := defKeep
	name := ""
	var excludes []string
	if src != nil {
		name, keep = src.Name, src.Retention
		f.sel, excludes = backup.FromSource(src.Paths, src.Excludes)
		if !slices.Contains(src.Inherited, "schedule") {
			every := int64(0)
			if src.Schedule != nil {
				every = src.Schedule.Every
			}
			f.schedule = max(slices.Index(scheduleChoices, every), 0)
			if f.schedule == 0 && every != 0 {
				// An interval the form doesn't list: keep it as a choice.
				scheduleChoices = append(scheduleChoices, every)
				f.schedule = len(scheduleChoices) - 1
			}
		}
	} else if paths, _ := sel.Compile(); len(paths) > 0 {
		name = defaultSourceName(a.homeFor(f.host), paths)
	}
	f.name = newTextField(name, "a short name")
	f.excludes = newPatternList(excludes, "add a pattern, e.g. node_modules")
	f.keep = newKeepEditor(keep, defKeep, true, src == nil || slices.Contains(src.Inherited, "retention"))
	f.focus()
	return f
}

// defaultSourceName names a new source after its folder: "home" for ~,
// the folder's name otherwise, the common parent's for several.
func defaultSourceName(home string, paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	p := paths[0]
	if len(paths) > 1 {
		p = commonParent(paths)
	}
	if home != "" && p == home {
		return "home"
	}
	if p == "/" {
		return "system"
	}
	return filepath.Base(p)
}

func commonParent(paths []string) string {
	parent := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		for parent != "/" && !strings.HasPrefix(p, parent+"/") {
			parent = filepath.Dir(parent)
		}
	}
	return parent
}

// fields is the focusable fields in order, given the current choices.
func (f *sourceForm) fields() []int {
	fs := []int{ffName, ffFolders, ffExcludes, ffSchedule, ffRetention}
	if f.keep.custom() {
		for i := range 6 {
			fs = append(fs, ffKeep0+i)
		}
	}
	if f.editing == nil {
		fs = append(fs, ffBackUpNow)
	}
	return append(fs, ffButtons)
}

func (f *sourceForm) input(field int) *textinput.Model {
	switch {
	case field == ffName:
		return &f.name
	case field >= ffKeep0 && field < ffKeep0+6:
		return &f.keep.counts[field-ffKeep0]
	}
	return nil
}

// focus puts the text cursor in the focused field, if it's a text field.
func (f *sourceForm) focus() tea.Cmd {
	f.name.Blur()
	if f.field != ffExcludes {
		f.excludes.blur()
	}
	for i := range f.keep.counts {
		f.keep.counts[i].Blur()
	}
	if in := f.input(f.field); in != nil {
		in.CursorEnd()
		return in.Focus()
	}
	return nil
}

func (f *sourceForm) typing() bool {
	return f.input(f.field) != nil || f.field == ffExcludes && f.excludes.typing()
}

func (f *sourceForm) move(delta int) tea.Cmd {
	fs := f.fields()
	i := slices.Index(fs, f.field)
	f.field = fs[min(max(i+delta, 0), len(fs)-1)]
	if f.field == ffButtons && delta > 0 {
		f.buttons.pick = 0
	}
	cmd := f.focus()
	if f.field == ffExcludes {
		return f.excludes.focus(delta > 0)
	}
	return cmd
}

func (f *sourceForm) update(a *App, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case sourceSavedMsg:
		f.saving = false
		if msg.err != nil {
			f.err = msg.err
			return nil
		}
		a.modal = nil
		a.wantSource = sourceKey(msg.source)
		if f.editing == nil {
			a.statusMsg = "Added " + msg.source.Name
			if f.now {
				a.statusMsg += "; backing it up now"
			}
		} else {
			a.statusMsg = "Saved " + msg.source.Name
		}
		return tea.Batch(a.loadSources(), a.loadSnapshotsFor(sourceKey(msg.source)))
	case tea.KeyPressMsg:
		if f.saving {
			return nil
		}
		return f.key(a, msg)
	}
	return nil
}

// loadSnapshotsFor reloads snapshots if key is the selected source (a
// save can change what's kept).
func (a *App) loadSnapshotsFor(key string) tea.Cmd {
	if a.selectedSourceID() != key {
		return nil
	}
	return a.loadSnapshots()
}

func (f *sourceForm) key(a *App, msg tea.KeyPressMsg) tea.Cmd {
	// The skip list uses ↑↓ and Enter inside itself first.
	if f.field == ffExcludes && !key.Matches(msg, keyNext, keyPrev) {
		if cmd, used := f.excludes.key(msg); used {
			f.err = nil
			return cmd
		}
	}
	switch {
	case key.Matches(msg, keyEsc):
		a.modal = nil
		return nil
	case key.Matches(msg, keyNext), msg.String() == "down":
		return f.move(1)
	case key.Matches(msg, keyPrev), msg.String() == "up":
		return f.move(-1)
	case f.field == ffButtons:
		switch f.buttons.key(msg) {
		case 0:
			return f.save(a)
		case 1:
			a.modal = nil
		}
		return nil
	case key.Matches(msg, keyEnter):
		if f.field == ffFolders {
			return f.pickFolders(a)
		}
		return f.move(1)
	}
	switch f.field {
	case ffSchedule:
		switch msg.String() {
		case "left", "h":
			f.schedule = (f.schedule + len(scheduleChoices) - 1) % len(scheduleChoices)
		case "right", "l", "space":
			f.schedule = (f.schedule + 1) % len(scheduleChoices)
		}
		return nil
	case ffRetention:
		switch msg.String() {
		case "left", "h":
			f.keep.cycle(-1)
		case "right", "l", "space":
			f.keep.cycle(1)
		}
		return nil
	case ffBackUpNow:
		switch msg.String() {
		case "left", "h", "right", "l", "space":
			f.now = !f.now
		}
		return nil
	case ffFolders:
		if msg.String() == "space" || msg.String() == "right" {
			return f.pickFolders(a)
		}
		return nil
	}
	if in := f.input(f.field); in != nil {
		if f.field >= ffKeep0 && len(msg.Text) > 0 && strings.Trim(msg.Text, "0123456789") != "" {
			return nil // counts are digits only
		}
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		f.err = nil
		return cmd
	}
	return nil
}

// pickFolders swaps the form for the picker, and back. The picker opens
// where the source is: its folder, or its folders' common parent.
func (f *sourceForm) pickFolders(a *App) tea.Cmd {
	start := a.homeFor(f.host)
	if start == "" {
		start = "/"
	}
	if paths, _ := f.sel.Compile(); len(paths) > 0 {
		start = backup.Root(paths)
		if len(paths) == 1 {
			if a.isRemote(f.host) {
				// Can't stat a remote's disk from here: open the folder
				// holding it, which shows it either way.
				start = filepath.Dir(start)
			} else if info, err := os.Stat(start); err != nil || !info.IsDir() {
				start = filepath.Dir(start)
			}
		}
	}
	p := a.newFolderPicker(f.host, start, f.sel, func(a *App, sel *backup.Selection) tea.Cmd {
		f.sel = sel
		a.modal = f
		return nil
	}, func(a *App) tea.Cmd {
		a.modal = f
		return nil
	})
	a.modal = p
	return p.load()
}

type sourceSavedMsg struct {
	source protocol.Source
	err    error
}

func (f *sourceForm) save(a *App) tea.Cmd {
	name := strings.TrimSpace(f.name.Value())
	if name == "" {
		f.err = fmt.Errorf("the source needs a name")
		return nil
	}
	paths, tickExcludes := f.sel.Compile()
	if len(paths) == 0 {
		f.err = fmt.Errorf("choose at least one folder")
		return nil
	}
	var keep *protocol.Retention
	r, inherit, err := f.keep.retention()
	if err != nil {
		f.err = err
		return nil
	}
	if !inherit {
		keep = &r
	}
	var sched *protocol.Schedule
	if every := scheduleChoices[f.schedule]; every >= 0 {
		sched = &protocol.Schedule{Every: every}
	}
	excludes := append(tickExcludes, f.excludes.values()...)
	// A source lives on its host: the one the picker browsed.
	repo := a.repo.ForHost(f.host)
	f.saving, f.err = true, nil

	if f.editing == nil {
		p := protocol.SourceAddParams{Name: name, Paths: paths, Excludes: excludes, Schedule: sched, Retention: keep, BackUpNow: f.now}
		return func() tea.Msg {
			s, err := repo.AddSource(context.Background(), p)
			return sourceSavedMsg{s, err}
		}
	}

	// Editing: send only what changed, and "inherit" for fields put back
	// on the default.
	old := f.editing
	p := protocol.SourceSetParams{ID: old.ID}
	if name != old.Name {
		p.Name = &name
	}
	if !slices.Equal(paths, old.Paths) {
		p.Paths = paths
	}
	if !slices.Equal(excludes, old.Excludes) {
		if excludes == nil {
			excludes = []string{}
		}
		p.Excludes = &excludes
	}
	if sched == nil {
		if !slices.Contains(old.Inherited, "schedule") {
			p.Inherit = append(p.Inherit, "schedule")
		}
	} else {
		p.Schedule = sched
	}
	if keep == nil {
		if !slices.Contains(old.Inherited, "retention") {
			p.Inherit = append(p.Inherit, "retention")
		}
	} else {
		p.Retention = keep
	}
	return func() tea.Msg {
		s, err := repo.SetSource(context.Background(), p)
		return sourceSavedMsg{s, err}
	}
}

// scheduleText describes a schedule choice: "hourly", "manual only",
// "default (hourly)".
func (a *App) scheduleText(every int64) string {
	host := a.localHost()
	if r, ok := a.selRow(); ok && r.host != "" {
		host = r.host
	}
	return a.scheduleTextFor(host, every)
}

// scheduleTextFor is scheduleText where "default" is host's.
func (a *App) scheduleTextFor(host string, every int64) string {
	if every < 0 {
		def, _ := a.defaultPolicy(host)
		return "default (" + a.scheduleTextFor(host, def) + ")"
	}
	if every == 0 {
		return "manual only"
	}
	return every2text(every)
}

func every2text(secs int64) string { return every(time.Duration(secs) * time.Second) }

const formLabelWidth = 12

// formPathRows is the most rows the form gives a source's folders.
const formPathRows = 4

func (f *sourceForm) view(a *App) string {
	inner := popupInnerWidth(a.width)
	home := a.homeFor(f.host)
	vw := inner - formLabelWidth
	var b formBuilder
	title := " Add a source "
	if a.grouped() {
		title = " Add a source on " + f.host + " "
	}
	if f.editing != nil {
		title = " Edit " + f.editing.Name + " "
	}
	b.WriteString(popupTitle(title) + "\n\n")

	row := func(field int, label, value string) {
		lab := styles.FieldLabelStyle()
		if f.field == field {
			lab = styles.HotkeyStyle()
		}
		b.Row(lab.Render(label), value)
	}
	choice := func(field int, text string) string {
		if f.field == field {
			return styles.StopStyle(true).Render("‹ " + text + " ›")
		}
		return styles.FieldValueStyle().Render(text)
	}
	text := func(in *textinput.Model, w int) string {
		in.SetWidth(w)
		return in.View()
	}

	row(ffName, "Name", text(&f.name, vw))
	// Long lists get a window, not the whole popup: at most formPathRows
	// folders, and the skip patterns in what the popup has left.
	paths, _ := f.sel.Compile()
	shown := paths
	if len(shown) > formPathRows {
		shown = shown[:formPathRows-1]
	}
	for i, p := range shown {
		label := ""
		if i == 0 {
			label = "Backs up"
		}
		v := styles.FieldValueStyle().Render(midTrunc(shortPath(home, p), vw))
		if i == 0 && f.field == ffFolders {
			v = styles.StopStyle(true).Render(midTrunc(shortPath(home, p), vw-14)) + styles.MutedStyle().Render("  enter: change")
		}
		row(ffFolders, label, v)
	}
	pathRows := len(shown)
	if more := len(paths) - len(shown); more > 0 {
		row(ffFolders, "", styles.MutedStyle().Render(fmt.Sprintf("and %d more (enter shows them)", more)))
		pathRows++
	}
	exceptions := f.sel.Exceptions()
	if exceptions > 0 {
		row(-1, "", styles.MutedStyle().Render("with "+pluralize(exceptions, "exception")+" ticked inside"))
		pathRows++
	}
	// Everything but the skip rows: title and blank, name, the rows
	// above, Back up, Keep (and its counts), Back up now, then a blank,
	// the buttons and a blank, the help and the hints.
	fixed := 2 + 1 + pathRows + 2 + 1 + 2 + helpLines + 1
	if f.keep.custom() {
		fixed += len(f.keep.counts)
	}
	if f.editing == nil {
		fixed++
	}
	for i, v := range f.excludes.rows(f.field == ffExcludes, vw, max(a.modalHeight()-fixed, 1)) {
		label := ""
		if i == 0 {
			label = "Also skip"
		}
		row(ffExcludes, label, v)
	}
	row(ffSchedule, "Back up", choice(ffSchedule, a.scheduleTextFor(f.host, scheduleChoices[f.schedule])))
	row(ffRetention, "Keep", choice(ffRetention, f.keep.text()))
	if f.keep.custom() {
		for i := range f.keep.counts {
			row(ffKeep0+i, "  "+keepLabels[i], f.keep.countRow(i))
		}
	}
	if f.editing == nil {
		box := "[ ]"
		if f.now {
			box = "[x]"
		}
		row(ffBackUpNow, "Back up now", choice(ffBackUpNow, box))
	}
	b.WriteString("\n" + f.buttons.view(f.field == ffButtons) + "\n\n")
	switch {
	case f.saving:
		b.WriteString(styles.MutedStyle().Render("saving…") + "\n")
	case f.err != nil:
		b.WriteString(errorLine(f.err) + "\n")
	default:
		b.WriteString(helpBlock(f.fieldHelp(), inner))
	}
	b.WriteString(f.hints())
	return boxed(b.String(), inner)
}

func (f *sourceForm) hints() string {
	switch f.field {
	case ffButtons:
		return hints("↑", "fields", "←→", "choose", "enter", "press", "esc", "cancel")
	case ffFolders:
		return hints("↑↓", "field", "enter", "choose folders", "esc", "cancel")
	}
	return hints("↑↓", "field", "←→", "change", "enter", "next", "esc", "cancel")
}

// fieldHelp explains the focused field in one line.
func (f *sourceForm) fieldHelp() string {
	switch {
	case f.field == ffName:
		return "What the source is called in the list."
	case f.field == ffFolders:
		return "What gets backed up, ticked in the picker. Enter to change it."
	case f.field == ffExcludes:
		return f.excludes.help()
	case f.field == ffSchedule:
		return "How often it backs up. Missed runs catch up when the Mac wakes."
	case f.field == ffRetention:
		return f.keep.help()
	case f.field >= ffKeep0 && f.field < ffKeep0+6:
		return "How many " + keepLabels[f.field-ffKeep0] + " back to keep one from; 0 turns the line off."
	case f.field == ffBackUpNow:
		return "Start the first backup as soon as it's added."
	case f.field == ffButtons && f.editing == nil:
		return f.buttons.help("Adds the source.", "Closes without adding it.")
	case f.field == ffButtons:
		return f.buttons.help("Saves the changes.", "Closes without saving.")
	}
	return ""
}
