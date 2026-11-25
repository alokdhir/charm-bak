package daemon

import (
	"io/fs"
	"syscall"
)

// hiddenSystem reports whether Windows marks an entry both hidden and
// system, as Explorer hides it: the compatibility links in a profile
// ("My Documents", "Application Data", which refuse to open),
// $Recycle.Bin, System Volume Information, pagefile.sys. Nothing in them
// is anyone's to back up, so the picker leaves them out.
func hiddenSystem(info fs.FileInfo) bool {
	d, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	const both = syscall.FILE_ATTRIBUTE_HIDDEN | syscall.FILE_ATTRIBUTE_SYSTEM
	return d.FileAttributes&both == both
}
