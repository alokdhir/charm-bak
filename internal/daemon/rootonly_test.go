//go:build !windows

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// A system binary under root-owned folders counts; one in a folder this
// (non-root) test user owns doesn't, as Homebrew's doesn't.
func TestRootOnly(t *testing.T) {
	if !rootOnly("/bin/sh") {
		t.Error("/bin/sh isn't root-only")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: every temp file is root's")
	}
	p := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rootOnly(p) {
		t.Errorf("%s, owned by this user, counts as root-only", p)
	}
}
