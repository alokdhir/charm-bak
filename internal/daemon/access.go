package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"charm-bak/internal/protocol"
)

// tccDB is readable only with Full Disk Access, root included, so
// opening it is the check. Nothing is read, and a denied open raises no
// prompt.
const tccDB = "/Library/Application Support/com.apple.TCC/TCC.db"

// fullDiskAccess is, on a Mac, whether this process has Full Disk
// Access; nil elsewhere.
func (c *Core) fullDiskAccess() *bool {
	if c.accessCheck != nil {
		return c.accessCheck()
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	f, err := os.Open(tccDB)
	ok := err == nil
	if ok {
		f.Close()
	}
	return &ok
}

// fullDiskAccessURL opens System Settings at Privacy & Security › Full
// Disk Access (the app's AccessProbe.swift uses the same).
const fullDiskAccessURL = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"

// openAccessSettings handles access_settings: on the Mac this daemon
// runs on, in the session of whoever is at its screen, show charm-bak's
// app bundle in Finder (to drag into the list) and open System Settings
// at Full Disk Access. macOS has no way to ask for this grant: a person
// adds the app there.
func (c *Core) openAccessSettings() (protocol.Empty, error) {
	if runtime.GOOS != "darwin" {
		return protocol.Empty{}, perr(protocol.ErrBadRequest, "Full Disk Access is a macOS setting.")
	}
	open := func(args ...string) error { return exec.Command("/usr/bin/open", args...).Run() }
	if os.Geteuid() == 0 {
		// A launchd daemon has no GUI session: run open as the person
		// logged in at the screen, in theirs.
		id, ok := consoleUID()
		if !ok {
			return protocol.Empty{}, perr(protocol.ErrBadRequest, "No one is logged in at %s's screen. Log in there, then try again.", c.Name)
		}
		uid := strconv.FormatUint(uint64(id), 10)
		open = func(args ...string) error {
			return exec.Command("/bin/launchctl", append([]string{"asuser", uid, "/usr/bin/sudo", "-u", "#" + uid, "/usr/bin/open"}, args...)...).Run()
		}
	}
	if b := appBundle(); b != "" {
		open("-R", b)
	}
	if err := open(fullDiskAccessURL); err != nil {
		return protocol.Empty{}, perr(protocol.ErrBadRequest, "Couldn't open System Settings on %s: %v", c.Name, err)
	}
	logf("opened Full Disk Access settings")
	return protocol.Empty{}, nil
}

// appBundle is the .app this executable is in, "" if none.
func appBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if i := strings.Index(exe, ".app/Contents/MacOS/"); i >= 0 {
		return exe[:i+len(".app")]
	}
	return ""
}
