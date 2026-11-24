package backup

import (
	"path"
	"slices"
	"strings"
)

// Selection is what a source backs up, as ticks on a folder tree: each
// path is included, excluded, or follows its parent. It compiles to what
// restic takes (paths, plus exclude and "!" re-include patterns) and
// back, so the picker and a source's stored form are the same thing.
type Selection struct {
	// choices maps an absolute path to whether it's included. Only
	// exceptions are stored: a choice equal to what the path would get
	// from its parent is dropped.
	choices map[string]bool
}

// Tick is how a path shows in the picker.
type Tick int

const (
	Unticked Tick = iota
	Ticked
	// Mixed: a folder whose contents are partly in and partly out.
	Mixed
)

func NewSelection() *Selection { return &Selection{choices: map[string]bool{}} }

func (s *Selection) Clone() *Selection {
	c := NewSelection()
	for k, v := range s.choices {
		c.choices[k] = v
	}
	return c
}

// under reports whether p is strictly inside dir.
func under(p, dir string) bool {
	if dir == "/" {
		return p != "/" && strings.HasPrefix(p, "/")
	}
	return strings.HasPrefix(p, dir+"/")
}

// parentIncluded is whether p's nearest chosen ancestor (not p itself)
// is included; false when no ancestor is chosen.
func (s *Selection) parentIncluded(p string) bool {
	for d := p; d != "/"; {
		d = path.Dir(d)
		if v, ok := s.choices[d]; ok {
			return v
		}
	}
	return false
}

// Included is whether p gets backed up.
func (s *Selection) Included(p string) bool {
	if v, ok := s.choices[p]; ok {
		return v
	}
	return s.parentIncluded(p)
}

// Tick is how p shows: ticked or not by what it gets, Mixed when
// something inside it differs.
func (s *Selection) Tick(p string) Tick {
	in := s.Included(p)
	for c, v := range s.choices {
		if v != in && under(c, p) {
			return Mixed
		}
	}
	if in {
		return Ticked
	}
	return Unticked
}

// Toggle flips p and everything inside it: a Mixed or unticked folder
// becomes wholly included, a ticked one wholly excluded.
func (s *Selection) Toggle(p string) {
	want := s.Tick(p) != Ticked
	for c := range s.choices {
		if under(c, p) {
			delete(s.choices, c)
		}
	}
	if s.parentIncluded(p) == want {
		delete(s.choices, p)
	} else {
		s.choices[p] = want
	}
}

// Exclude takes p and everything inside it out, whatever was chosen
// inside before (unlike Toggle, which turns a Mixed folder wholly in).
func (s *Selection) Exclude(p string) {
	for c := range s.choices {
		if under(c, p) {
			delete(s.choices, c)
		}
	}
	if s.parentIncluded(p) {
		s.choices[p] = false
	} else {
		delete(s.choices, p)
	}
}

// Exceptions counts the items excluded or re-included inside an
// included folder.
func (s *Selection) Exceptions() int {
	n := 0
	for c := range s.choices {
		if s.chosenAncestor(c) != "" {
			n++
		}
	}
	return n
}

// Empty reports whether nothing is included.
func (s *Selection) Empty() bool {
	for _, v := range s.choices {
		if v {
			return false
		}
	}
	return true
}

// Compile is the selection as restic takes it: the top included folders
// as paths, then exclude and "!" re-include patterns in path order
// (restic applies them in order, the last match winning).
//
// restic doesn't look inside an excluded folder, so a re-include needs
// the folders above it kept open: an excluded folder with something
// re-included inside is excluded as "dir/*" (its contents) instead, and
// each folder between it and the re-include is re-included and then has
// its other contents excluded the same way. Tested on restic 0.19.1
// (docs/notes.md).
func (s *Selection) Compile() (paths, excludes []string) {
	keys := make([]string, 0, len(s.choices))
	for k := range s.choices {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	emitted := map[string]bool{}
	emit := func(pattern string) {
		if !emitted[pattern] {
			emitted[pattern] = true
			excludes = append(excludes, pattern)
		}
	}
	contents := func(dir string) string {
		if dir == "/" {
			return "/*"
		}
		return escapePattern(dir) + "/*"
	}
	for _, p := range keys {
		above := s.chosenAncestor(p)
		switch {
		case s.choices[p] && above == "":
			paths = append(paths, p)
		case s.choices[p]:
			// Re-include: open each folder from the excluded one down.
			rel := strings.Split(strings.TrimPrefix(strings.TrimPrefix(p, above), "/"), "/")
			d := above
			for i, name := range rel {
				d = path.Join(d, name)
				emit("!" + escapePattern(d))
				if i < len(rel)-1 {
					emit(contents(d))
				}
			}
		case s.includedUnder(p):
			emit(contents(p))
		default:
			emit(escapePattern(p))
		}
	}
	return paths, excludes
}

// chosenAncestor is p's nearest ancestor with a choice, or "".
func (s *Selection) chosenAncestor(p string) string {
	for d := p; d != "/"; {
		d = path.Dir(d)
		if _, ok := s.choices[d]; ok {
			return d
		}
	}
	return ""
}

// includedUnder reports a re-include somewhere inside p.
func (s *Selection) includedUnder(p string) bool {
	for c, v := range s.choices {
		if v && under(c, p) {
			return true
		}
	}
	return false
}

// FromSource reads a stored source back into ticks. Excludes that name
// one absolute path become ticks; anything else (node_modules, *.tmp) is
// a pattern the picker can't show as a tick, returned as is.
func FromSource(paths, excludes []string) (sel *Selection, patterns []string) {
	sel = NewSelection()
	for _, p := range paths {
		sel.choices[path.Clean(p)] = true
	}
	// tick is e as a tick's path, or "" when it's a pattern.
	tick := func(e string) (p string, include bool) {
		include = strings.HasPrefix(e, "!")
		p = strings.TrimPrefix(e, "!")
		if !include && strings.HasSuffix(p, "/*") {
			p = strings.TrimSuffix(p, "/*") // a folder's contents: Compile's form of excluding it
			if p == "" {
				p = "/"
			}
		}
		if !strings.HasPrefix(p, "/") || hasMeta(p) {
			return "", include
		}
		return path.Clean(unescapePattern(p)), include
	}
	var excluded []string // the paths ticked out
	for _, e := range excludes {
		if p, include := tick(e); p != "" && !include {
			excluded = append(excluded, p)
		}
	}
	for _, e := range excludes {
		p, include := tick(e)
		// A re-include is a tick only inside a folder that's ticked out
		// (what Compile writes). One with nothing ticked out above it
		// undoes a pattern instead ("node_modules", then
		// "!/src/proj/node_modules"): as a tick it would only repeat its
		// parent and be dropped, and the folder with it.
		if p == "" || include && !slices.ContainsFunc(excluded, func(x string) bool { return under(p, x) }) {
			patterns = append(patterns, e)
			continue
		}
		sel.choices[p] = include
	}
	sel.normalize()
	return sel, patterns
}

// normalize drops choices that only repeat what the parent gives (the
// folders Compile re-includes on the way to a deeper re-include).
func (s *Selection) normalize() {
	keys := make([]string, 0, len(s.choices))
	for k := range s.choices {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if s.parentIncluded(k) == s.choices[k] {
			delete(s.choices, k)
		}
	}
}

// restic's patterns use filepath.Match syntax, where * ? [ and \ are
// special; a backslash makes them literal.
const patternMeta = `*?[\`

func escapePattern(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(patternMeta, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func unescapePattern(p string) string {
	var b strings.Builder
	esc := false
	for _, r := range p {
		if !esc && r == '\\' {
			esc = true
			continue
		}
		esc = false
		b.WriteRune(r)
	}
	return b.String()
}

// hasMeta reports an unescaped * ? or [ : a real pattern, not a path.
func hasMeta(p string) bool {
	esc := false
	for _, r := range p {
		switch {
		case esc:
			esc = false
		case r == '\\':
			esc = true
		case strings.ContainsRune("*?[", r):
			return true
		}
	}
	return false
}
