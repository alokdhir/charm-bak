//go:build !unix

package daemon

// consoleUID: no Unix console here (Full Disk Access is a macOS setting).
func consoleUID() (uint32, bool) { return 0, false }
