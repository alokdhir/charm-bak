package daemon

// On Windows the installer puts restic beside the daemon in Program
// Files, which only administrators can change; systemRestic isn't used.
func asRoot() bool { return false }

func rootOnly(string) bool { return true }
