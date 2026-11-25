//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// consoleUID is who is logged in at this Mac's screen (the owner of
// /dev/console); false when no one is, or only root.
func consoleUID() (uint32, bool) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return 0, false
	}
	return st.Uid, true
}
