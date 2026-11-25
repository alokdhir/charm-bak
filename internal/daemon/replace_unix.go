//go:build !windows

package daemon

import "os"

// replaceExecutable puts the new executable at path. A running
// executable can be renamed over on Unix; the process keeps the old one.
func replaceExecutable(fresh, path string) error { return os.Rename(fresh, path) }

// CleanUpReplaced is nothing to do on Unix.
func CleanUpReplaced(string) {}
