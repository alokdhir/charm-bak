package ui

import (
	"strings"
	"testing"

	"charm-bak/internal/protocol"
)

// evil is a name an attacker controls (a file in a cloned repository, a
// compromised remote's source): OSC 52 sets the clipboard, OSC 8 makes a
// link, and a bare ESC could start anything else.
const evil = "x\x1b]52;c;Y3VybCBldmlsfHNo\x07\x1b]8;;https://evil.example\x1b\\y\x1b[2J"

// foreign reports escape sequences in a frame that the TUI itself never
// writes: clipboard writes, links, screen clears.
func foreign(frame string) string {
	for _, bad := range []string{"\x1b]52", "\x1b]8;", "\x1b[2J"} {
		if strings.Contains(frame, bad) {
			return bad
		}
	}
	return ""
}

// Names that come from outside (files, sources, notes, errors) are
// drawn without their control characters, everywhere they show.
func TestForeignEscapesNeverReachTheTerminal(t *testing.T) {
	a := newTestApp(t, 160, 40)
	frame := func(where string) {
		t.Helper()
		if bad := foreign(a.View().Content); bad != "" {
			t.Errorf("%s: frame carries %q", where, bad)
		}
	}
	for i := range a.sources {
		a.sources[i].Name = evil
	}
	a.statusMsg = evil
	frame("sources")
	press(a, "i")
	frame("source info")
	press(a, "esc", "right")
	for i := range a.snaps {
		a.snaps[i].Note = evil
		a.snaps[i].Paths = []string{"/" + evil}
	}
	frame("snapshots")
	press(a, "i")
	frame("snapshot info")
	press(a, "esc", "right")
	if len(a.levels) > 0 {
		for i := range a.levels[0].entries {
			a.levels[0].entries[i].Name = evil + "-" + a.levels[0].entries[i].Name
		}
	}
	frame("files")
	press(a, "i")
	frame("file info")
	press(a, "esc")
	a.err = &protocol.Error{Code: protocol.ErrInternal, Message: evil}
	frame("error")
}

// Colours survive; everything else that isn't text goes, and what's left
// reads the same.
func TestScrubFrame(t *testing.T) {
	for in, want := range map[string]string{
		"\x1b[1;38;2;255;42;42mred\x1b[m plain\n":     "\x1b[1;38;2;255;42;42mred\x1b[m plain\n",
		"a\x1b]52;c;ZXZpbA==\x07b":                    "ab",
		"a\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\b": "alinkb",
		"a\x1b[2Jb\x1b[Hc":                            "abc",
		"bell\x07 tab\t cr\r":                         "bell tab cr",
		"c1\u009b31mx":                                "c131mx",
		"unterminated \x1b]52;c;abc":                  "unterminated ",
	} {
		if got := scrubFrame(in); got != want {
			t.Errorf("scrubFrame(%q) = %q, want %q", in, got, want)
		}
	}
}
