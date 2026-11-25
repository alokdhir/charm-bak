package daemon

import (
	"os"
	"strings"
	"unicode"

	"charm-bak/internal/protocol"
)

// Paths on Windows. restic shows a snapshot's tree with drives as top
// folders, /C/Users/…, and takes that form for ls, dump, restore
// --include and rewrite --exclude; but its backup arguments, backup
// --exclude and the snapshot list's own paths are C:\Users\… (tested
// with restic 0.20.0 on Windows 11; docs/notes.md). charm-bak uses the
// tree form everywhere, in the protocol, the TUI and the database, and
// converts at the edges: what restic's backup takes, the snapshot list,
// the live disk and restore targets. Elsewhere both forms are the same
// and these return their argument.

// isWindows is runtime.GOOS == "windows", a variable so tests on any
// system can exercise the Windows conversions.
var isWindows = os.PathSeparator == '\\'

// nativePath is a tree-form path as Windows writes it: /C/Users/a is
// C:\Users\a, /C is C:\. Anything else is returned as it is.
func nativePath(p string) string {
	if !isWindows {
		return p
	}
	drive, rest, ok := splitDrive(p)
	if !ok {
		return p
	}
	return drive + `:\` + strings.ReplaceAll(strings.TrimPrefix(rest, "/"), "/", `\`)
}

// treePath is a Windows path in tree form: C:\Users\a is /C/Users/a.
func treePath(p string) string {
	if !isWindows || len(p) < 2 || p[1] != ':' || !unicode.IsLetter(rune(p[0])) {
		return p
	}
	rest := strings.Trim(strings.ReplaceAll(p[2:], `\`, "/"), "/")
	out := "/" + strings.ToUpper(p[:1])
	if rest != "" {
		out += "/" + rest
	}
	return out
}

// splitDrive splits /C/rest into "C" and "/rest" ("" for /C itself).
func splitDrive(p string) (drive, rest string, ok bool) {
	if len(p) < 2 || p[0] != '/' || !unicode.IsLetter(rune(p[1])) || (len(p) > 2 && p[2] != '/') {
		return "", "", false
	}
	return strings.ToUpper(p[1:2]), p[2:], true
}

// nativeExclude is a backup exclude (as selection.go compiles them or a
// person typed one) in the form restic's backup takes on this system.
// On Windows: anchored patterns lose their tree form (/C/… → C:\…),
// separators become backslashes, and characters escaped with a backslash
// are escaped as a class instead ([[] for [), since a backslash there is
// a separator. Leading ! (a re-include) is kept.
func nativeExclude(e string) string {
	if !isWindows {
		return e
	}
	neg := strings.HasPrefix(e, "!")
	e = strings.TrimPrefix(e, "!")
	var b strings.Builder
	for i := 0; i < len(e); i++ {
		switch c := e[i]; {
		case c == '\\' && i+1 < len(e):
			i++
			b.WriteString(classEscape(e[i]))
		case c == '/':
			b.WriteByte('\\')
		default:
			b.WriteByte(c)
		}
	}
	out := b.String()
	// \C\rest → C:\rest
	if len(out) >= 2 && out[0] == '\\' && unicode.IsLetter(rune(out[1])) && (len(out) == 2 || out[2] == '\\') {
		out = strings.ToUpper(out[1:2]) + `:\` + strings.TrimPrefix(out[2:], `\`)
	}
	if neg {
		out = "!" + out
	}
	return out
}

// classEscape is c matched literally inside a pattern on Windows.
func classEscape(c byte) string {
	switch c {
	case '[', '*', '?':
		return "[" + string(c) + "]"
	case '\\':
		return "" // can't be in a Windows name
	}
	return string(c)
}

// isTreeAbs reports whether p is an absolute path in tree form.
func isTreeAbs(p string) bool { return strings.HasPrefix(p, "/") }

// listDrives is the top of a Windows disk listing: one folder per drive
// that's there (C, D, …), as the tree shows them.
func listDrives() []protocol.Entry {
	var out []protocol.Entry
	for c := 'A'; c <= 'Z'; c++ {
		if info, err := os.Stat(string(c) + `:\`); err == nil && info.IsDir() {
			out = append(out, protocol.Entry{Name: string(c), Type: protocol.TypeDir, Mode: info.Mode().String(), MTime: info.ModTime()})
		}
	}
	return out
}
