//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"syscall"
)

// asRoot reports whether this daemon runs as root (a remote's service).
func asRoot() bool { return os.Geteuid() == 0 }

// rootOnly reports whether only root can change the file at p: it and
// every folder above it (symlinks followed) are owned by root and not
// writable by group or others.
func rootOnly(p string) bool {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		fi, err := os.Stat(dir)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			// The sticky /tmp-style exception isn't wanted here: a
			// shared folder above restic is reason enough to refuse.
			return false
		}
		if parent := filepath.Dir(dir); parent == dir {
			return true
		}
	}
}
