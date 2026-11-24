package backup

import (
	"math/rand"
	"path"
	"slices"
	"strings"
	"testing"
)

// Exclude takes a folder out whatever was chosen inside it; Toggle on
// the same (Mixed) folder would bring all of it back in.
func TestExcludeMixedFolder(t *testing.T) {
	sel, _ := FromSource([]string{"/home/a"}, []string{"/home/a/docs/file.md"})
	if sel.Tick("/home/a/docs") != Mixed {
		t.Fatalf("setup: docs is %v", sel.Tick("/home/a/docs"))
	}
	sel.Exclude("/home/a/docs")
	paths, excludes := sel.Compile()
	if !slices.Equal(paths, []string{"/home/a"}) || !slices.Equal(excludes, []string{"/home/a/docs"}) {
		t.Errorf("paths %v, excludes %v", paths, excludes)
	}
	// Inside something already out it adds nothing.
	sel.Exclude("/home/a/docs/sub")
	if _, ex := sel.Compile(); !slices.Equal(ex, []string{"/home/a/docs"}) {
		t.Errorf("excludes %v", ex)
	}
	// Excluding the folder a re-include lives in drops the re-include.
	sel, _ = FromSource([]string{"/home/a"}, []string{"/home/a/big/*", "!/home/a/big/keep"})
	sel.Exclude("/home/a/big")
	if _, ex := sel.Compile(); !slices.Equal(ex, []string{"/home/a/big"}) {
		t.Errorf("excludes %v", ex)
	}
}

// A re-include that undoes a pattern (nothing above it is ticked out)
// stays a pattern: as a tick it only repeats its parent, and was dropped
// on the next save, taking the folder out of the backup.
func TestFromSourceKeepsPatternReIncludes(t *testing.T) {
	in := []string{"node_modules", "!/Users/a/src/proj/node_modules"}
	sel, patterns := FromSource([]string{"/Users/a/src"}, in)
	paths, excludes := sel.Compile()
	if !slices.Equal(paths, []string{"/Users/a/src"}) || len(excludes) != 0 || !slices.Equal(patterns, in) {
		t.Errorf("paths %v, tick excludes %v, patterns %v", paths, excludes, patterns)
	}
	// One inside a ticked-out folder is still a tick, as Compile writes it.
	sel, patterns = FromSource([]string{"/Users/a"}, []string{"/Users/a/big/*", "!/Users/a/big/keep", "*.tmp"})
	if !sel.Included("/Users/a/big/keep") || sel.Included("/Users/a/big/other") || !slices.Equal(patterns, []string{"*.tmp"}) {
		t.Errorf("keep in: %v, other in: %v, patterns %v", sel.Included("/Users/a/big/keep"), sel.Included("/Users/a/big/other"), patterns)
	}
}

// backedUp is restic's rule for the patterns Compile writes (tested
// 0.19.1): a pattern matches a path and everything under it ("dir/*"
// only what's under it), the last match wins, and nothing is looked at
// inside an excluded folder.
func backedUp(paths, excludes []string, p string) bool {
	var root string
	for _, r := range paths {
		if p == r || under(p, r) {
			root = r
		}
	}
	if root == "" {
		return false
	}
	// Walk down from the root: each step must survive the patterns.
	rel := strings.Split(strings.TrimPrefix(strings.TrimPrefix(p, root), "/"), "/")
	cur := root
	for _, name := range rel {
		if name == "" {
			continue
		}
		cur = path.Join(cur, name)
		out := false
		for _, e := range excludes {
			neg := strings.HasPrefix(e, "!")
			pat := unescapePattern(strings.TrimPrefix(e, "!"))
			hit := false
			if dir, ok := strings.CutSuffix(pat, "/*"); ok && !neg {
				if dir == "" {
					dir = "/"
				}
				hit = under(cur, dir)
			} else {
				hit = cur == pat || under(cur, pat)
			}
			if hit {
				out = !neg
			}
		}
		if out {
			return false
		}
	}
	return true
}

// Random ticks and excludes, with awkward names: what Compile writes
// backs up exactly what the ticks say, and reading it back gives the
// same ticks.
func TestSelectionSoundWithExclude(t *testing.T) {
	names := []string{"a", "b", "[2024] Taxes", "a*b", "x?y", `back\slash`, "c d"}
	var all []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		all = append(all, dir)
		if depth == 3 {
			return
		}
		for _, n := range names[:3+depth] {
			walk(path.Join(dir, n), depth+1)
		}
	}
	walk("/r", 0)
	rng := rand.New(rand.NewSource(1))
	for round := range 3000 {
		sel := NewSelection()
		for range 1 + rng.Intn(6) {
			p := all[rng.Intn(len(all))]
			if rng.Intn(3) == 0 {
				sel.Exclude(p)
			} else {
				sel.Toggle(p)
			}
		}
		paths, excludes := sel.Compile()
		for _, p := range all {
			if !sel.Included(p) && sel.includedUnder(p) {
				continue // a folder kept open as the way down to a re-include
			}
			if got, want := backedUp(paths, excludes, p), sel.Included(p); got != want {
				t.Fatalf("round %d: %s backed up %v, ticked %v\npaths %q\nexcludes %q", round, p, got, want, paths, excludes)
			}
		}
		back, patterns := FromSource(paths, excludes)
		if len(patterns) != 0 {
			t.Fatalf("round %d: Compile's own output read back as patterns %q (excludes %q)", round, patterns, excludes)
		}
		for _, p := range all {
			if back.Included(p) != sel.Included(p) {
				t.Fatalf("round %d: %s reads back %v, was %v\nexcludes %q", round, p, back.Included(p), sel.Included(p), excludes)
			}
		}
	}
}
