package daemon

import "testing"

// The Windows path conversions, run on any system by switching isWindows.
func TestWindowsPaths(t *testing.T) {
	was := isWindows
	isWindows = true
	t.Cleanup(func() { isWindows = was })
	for in, want := range map[string]string{
		"/C/Users/alok": `C:\Users\alok`,
		"/C":            `C:\`,
		"/c/x":          `C:\x`,
		"/Users/alok":   "/Users/alok", // not a drive: left alone
		"/CD/x":         "/CD/x",
		`C:\already`:    `C:\already`,
	} {
		if got := nativePath(in); got != want {
			t.Errorf("nativePath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		`C:\Users\alok`: "/C/Users/alok",
		`C:\`:           "/C",
		`d:\x\`:         "/D/x",
		"/C/Users":      "/C/Users",
	} {
		if got := treePath(in); got != want {
			t.Errorf("treePath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		// as selection.go compiles them
		"/C/Users/a/big/*":      `C:\Users\a\big\*`,
		"!/C/Users/a/big/keep":  `!C:\Users\a\big\keep`,
		`/C/Users/a/\[2024\] x`: `C:\Users\a\[[]2024] x`,
		`/C/Users/a/a\*b`:       `C:\Users\a\a[*]b`,
		// typed by a person
		"node_modules":   "node_modules",
		"Library/Caches": `Library\Caches`,
		"*.tmp":          "*.tmp",
	} {
		if got := nativeExclude(in); got != want {
			t.Errorf("nativeExclude(%q) = %q, want %q", in, got, want)
		}
	}
	if got := literalPattern("/C/data/[2024] x"); got != "/C/data/[[]2024] x" {
		t.Errorf("literalPattern on Windows: %q", got)
	}
}

func TestAppAlias(t *testing.T) {
	irregular := `unsupported file type "irregular"`
	for _, c := range []struct {
		item, msg string
		want      bool
	}{
		{`C:\Users\alok\AppData\Local\Microsoft\WindowsApps\BingWallpaper.exe`, irregular, true},
		{`C:\Users\alok\AppData\Local\Microsoft\WindowsApps\AppleInc.iCloud_nzyj5cx40ttqa\iCloudDrive-AppX.exe`, irregular, true},
		{`C:\Users\alok\appdata\local\microsoft\windowsapps\python.exe`, irregular, true},
		// A real file there that can't be read is still reported.
		{`C:\Users\alok\AppData\Local\Microsoft\WindowsApps\notes.txt`, "The process cannot access the file", false},
		// An irregular file elsewhere is too.
		{`C:\Users\alok\Documents\link`, irregular, false},
	} {
		if got := appAlias(c.item, c.msg); got != c.want {
			t.Errorf("appAlias(%q, %q) = %v", c.item, c.msg, got)
		}
	}
}
