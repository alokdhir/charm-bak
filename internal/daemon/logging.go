package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// The daemon's own log: daemon.log beside its database (the state
// directory), on every platform, so what a daemon did can be read on the
// machine without any service manager. It rolls over to daemon.log.1 at
// logMax. Lines also go to stderr (a Linux service's journal, a
// terminal), except when stderr already is this file: the Mac app sends
// the daemon's stderr there, for crashes.
//
// What's logged: start and stop, each job's start and outcome, remotes
// answering or not, pairing, repository and default changes, sources
// added, changed and removed, self-updates. Never a secret.

const logMax = 5 << 20

// rotatingFile appends to path and rolls it to path.1 past max bytes.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func openRotating(path string, max int64) (*rotatingFile, error) {
	r := &rotatingFile{path: path, max: max}
	return r, r.open()
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *rotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.f.Close()
		os.Rename(r.path, r.path+".1")
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// SetupLog sends the standard logger to path (rolling over at 5 MiB) and,
// unless stderr is that same file, to stderr too.
func SetupLog(path string) error {
	rf, err := openRotating(path, logMax)
	if err != nil {
		return err
	}
	var w io.Writer = rf
	if !sameFile(os.Stderr, rf.f) {
		w = io.MultiWriter(rf, os.Stderr)
	}
	log.SetOutput(w)
	log.SetFlags(log.Ldate | log.Ltime)
	return nil
}

func sameFile(a, b *os.File) bool {
	ai, err1 := a.Stat()
	bi, err2 := b.Stat()
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

// logf is log.Printf; a name so call sites read as the daemon's log.
func logf(format string, args ...any) { log.Output(2, fmt.Sprintf(format, args...)) }
