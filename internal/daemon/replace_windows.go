//go:build windows

package daemon

import "os"

// replaceExecutable puts the new executable at path. Windows won't
// replace a running executable, but it will rename one: the old moves
// aside to path.old (deleted at the next start, CleanUpReplaced), then
// the new takes its name.
func replaceExecutable(fresh, path string) error {
	old := path + ".old"
	os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		return err
	}
	if err := os.Rename(fresh, path); err != nil {
		os.Rename(old, path)
		return err
	}
	return nil
}

// CleanUpReplaced deletes the executable an update moved aside.
func CleanUpReplaced(path string) { os.Remove(path + ".old") }
