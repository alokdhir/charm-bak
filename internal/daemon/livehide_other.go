//go:build !windows

package daemon

import "io/fs"

// hiddenSystem is Windows' hidden-and-system entries; elsewhere none.
func hiddenSystem(fs.FileInfo) bool { return false }
