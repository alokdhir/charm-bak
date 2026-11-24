package backup

import (
	"slices"
	"testing"
)

func TestSelectionTicksAndCompile(t *testing.T) {
	s := NewSelection()
	s.Toggle("/home/a")              // include a folder
	s.Toggle("/home/a/big")          // exclude something inside it
	s.Toggle("/home/a/big/keep")     // re-include inside the excluded one
	s.Toggle("/home/b/file.txt")     // a second, unrelated root
	s.Toggle("/home/a/Shot [1].png") // a name with pattern characters

	for p, want := range map[string]Tick{
		"/home":                Mixed, // parts of it are in
		"/home/a":              Mixed,
		"/home/a/src":          Ticked,
		"/home/a/big":          Mixed,
		"/home/a/big/junk":     Unticked,
		"/home/a/big/keep":     Ticked,
		"/home/a/big/keep/x":   Ticked,
		"/home/b":              Mixed,
		"/home/b/file.txt":     Ticked,
		"/home/b/other":        Unticked,
		"/home/a/Shot [1].png": Unticked,
	} {
		if got := s.Tick(p); got != want {
			t.Errorf("Tick(%s) = %v, want %v", p, got, want)
		}
	}

	paths, excludes := s.Compile()
	if want := []string{"/home/a", "/home/b/file.txt"}; !slices.Equal(paths, want) {
		t.Errorf("paths %q, want %q", paths, want)
	}
	if want := []string{`/home/a/Shot \[1].png`, "/home/a/big/*", "!/home/a/big/keep"}; !slices.Equal(excludes, want) {
		t.Errorf("excludes %q, want %q", excludes, want)
	}

	// Back from the stored form, with a floating pattern kept aside.
	back, patterns := FromSource(paths, append(excludes, "node_modules", "/x/*.tmp"))
	if !slices.Equal(patterns, []string{"node_modules", "/x/*.tmp"}) {
		t.Errorf("patterns %q", patterns)
	}
	p2, e2 := back.Compile()
	if !slices.Equal(p2, paths) || !slices.Equal(e2, excludes) {
		t.Errorf("round trip: %q %q, want %q %q", p2, e2, paths, excludes)
	}
}

func TestToggleClearsInsideAndDropsRedundantChoices(t *testing.T) {
	s := NewSelection()
	s.Toggle("/a")
	s.Toggle("/a/b") // out
	s.Toggle("/a/b") // back in: the same as its parent, so no choice left
	if paths, excludes := s.Compile(); !slices.Equal(paths, []string{"/a"}) || len(excludes) != 0 {
		t.Errorf("got %q %q, want just /a", paths, excludes)
	}
	s.Toggle("/a/b/c") // out, so /a is mixed
	s.Toggle("/a")     // a mixed folder becomes wholly in: the exception goes
	if s.Tick("/a") != Ticked || s.Tick("/a/b/c") != Ticked {
		t.Errorf("after toggling mixed /a: %v", s.choices)
	}
	s.Toggle("/a") // and then wholly out
	if !s.Empty() || s.Tick("/a/b/c") != Unticked {
		t.Errorf("after untoggling /a: %v", s.choices)
	}
	s.Toggle("/") // everything
	if paths, _ := s.Compile(); !slices.Equal(paths, []string{"/"}) {
		t.Errorf("root: %q", paths)
	}
}

// The deep case, in the exact form tested against restic: a re-include
// two folders inside an excluded one, with an exclude inside it.
func TestCompileDeepReinclude(t *testing.T) {
	s := NewSelection()
	s.Toggle("/d/a")
	s.Toggle("/d/a/big")
	s.Toggle("/d/a/big/x/y")
	s.Toggle("/d/a/big/x/y/tmp.log")
	paths, excludes := s.Compile()
	want := []string{"/d/a/big/*", "!/d/a/big/x", "/d/a/big/x/*", "!/d/a/big/x/y", "/d/a/big/x/y/tmp.log"}
	if !slices.Equal(paths, []string{"/d/a"}) || !slices.Equal(excludes, want) {
		t.Fatalf("got %q %q, want [/d/a] %q", paths, excludes, want)
	}
	for p, tick := range map[string]Tick{"/d/a/big/x/other": Unticked, "/d/a/big/x/y/z": Ticked, "/d/a/big/junk": Unticked} {
		if got := s.Tick(p); got != tick {
			t.Errorf("Tick(%s) = %v, want %v", p, got, tick)
		}
	}
	back, _ := FromSource(paths, excludes)
	if p2, e2 := back.Compile(); !slices.Equal(p2, paths) || !slices.Equal(e2, excludes) {
		t.Errorf("round trip: %q %q", p2, e2)
	}
	if len(back.choices) != len(s.choices) {
		t.Errorf("round trip kept extra choices: %v vs %v", back.choices, s.choices)
	}
}
