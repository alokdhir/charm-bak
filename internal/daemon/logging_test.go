package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The log rolls over to .1 past its limit, keeping one old file, and
// appends across reopenings.
func TestRotatingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	r, err := openRotating(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for range 2 {
		r.Write([]byte(line))
	}
	r.f.Close()
	r, _ = openRotating(path, 100) // reopened: appends
	r.Write([]byte(line))          // 120 > 100: rolls first (80 → .1)
	r.Write([]byte(line))
	r.Write([]byte(line))
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if len(old) != 80 || len(cur) != 40 { // the fifth line rolled again
		t.Errorf("old %d bytes, current %d", len(old), len(cur))
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}
