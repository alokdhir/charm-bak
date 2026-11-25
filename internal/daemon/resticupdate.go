package daemon

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// On Windows, restic.exe is a copy beside the daemon (the service runs
// as SYSTEM, without the PATH of whoever installed restic), so no package
// manager ever updates it. Once a week, while nothing's running, restic's
// own self-update replaces it with the latest release (downloaded from
// GitHub, its GPG signature checked by restic). Elsewhere restic is the
// system's, updated the way everything else there is.

const (
	resticUpdateEvery   = 7 * 24 * time.Hour
	resticUpdateTimeout = 5 * time.Minute
	resticUpdateSetting = "restic_self_update"
)

var resticUpdating atomic.Bool

// resticSelfUpdateIfDue starts the weekly update in the background when
// it's due; the caller has checked that the queue is empty.
func (c *Core) resticSelfUpdateIfDue(now time.Time) {
	// Windows' copy beside the daemon, or a Mac or Linux machine's own
	// (resticmanaged.go); a restic installed some other way is updated
	// that way.
	bin := FindRestic()
	if resticUpdating.Load() || !(isWindows || (bin != "" && bin == managedRestic())) {
		return
	}
	var last time.Time
	if ok, err := c.store.Setting(resticUpdateSetting, &last); err != nil || (ok && now.Sub(last) < resticUpdateEvery) {
		return
	}
	// Recorded before trying: a failure waits a week too, rather than
	// hitting GitHub every minute.
	if err := c.store.SetSetting(resticUpdateSetting, now); err != nil {
		return
	}
	resticUpdating.Store(true)
	go func() {
		defer resticUpdating.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), resticUpdateTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, bin, "self-update").CombinedOutput()
		msg := lastLine(string(out))
		if err != nil {
			logf("restic self-update failed: %v: %s", err, msg)
			return
		}
		if strings.Contains(msg, "already up to date") || msg == "" {
			logf("restic self-update: %s", strings.TrimSpace(msg+" (up to date)"))
			return
		}
		logf("restic self-update: %s", msg)
	}()
}
