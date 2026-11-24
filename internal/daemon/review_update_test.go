package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// A remote that's busy when its update is offered isn't written off: the
// build goes when its job ends (it once counted as sent, and the remote
// stayed on the old daemon until the controller restarted).
func TestSelfUpdateRetriesAfterBusy(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pantry-exe")
	os.WriteFile(exe, []byte("old daemon"), 0o755)
	resources := filepath.Join(dir, "Resources")
	newExe := filepath.Join(resources, "linux-"+runtime.GOARCH, "charm-bak-daemon")
	os.MkdirAll(filepath.Dir(newExe), 0o755)
	os.WriteFile(newExe, []byte("new daemon"), 0o755)
	restarted := make(chan struct{}, 4)
	release := make(chan struct{})

	p := newRemotePair(t, func(mac, pantry *Core) {
		mac.SetRemoteDaemons(resources)
		pantry.goos = "linux"
		pantry.Build, _ = FileSHA256(exe)
		pantry.UpdatePath = exe
		pantry.Restart = func() { restarted <- struct{}{} }
		pantry.enqueue(protocol.KindCheck, nil, func(context.Context, protocol.Job) outcome {
			<-release
			return result(nil)
		})
	})
	select {
	case <-restarted:
		t.Fatal("updated while a job was running")
	case <-time.After(time.Second):
	}
	p.mac.remotes.mu.Lock()
	sent := p.mac.remotes.byID["pantry"].sentBuild
	p.mac.remotes.mu.Unlock()
	if sent != "" {
		t.Errorf("a refused build counts as sent: %q", sent)
	}
	close(release)
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("never updated after the job ended")
	}
	if got, _ := os.ReadFile(exe); string(got) != "new daemon" {
		t.Errorf("executable %q", got)
	}
}

// A job that starts while the pieces arrive isn't cancelled by the
// restart: the last piece is refused as busy and nothing is installed.
// And once an update is installed, nothing starts before the restart.
func TestSelfUpdateBusyAtLastPiece(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	core := NewCore("test", memStore(t), ResticEngines, nil)
	restarted := make(chan struct{}, 1)
	core.UpdatePath, core.Restart = exe, func() { restarted <- struct{}{} }
	c, _ := serve(t, core)
	data := []byte("new daemon")
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	piece := func(off int, b []byte) protocol.SelfUpdateParams {
		return protocol.SelfUpdateParams{SHA256: sha, Size: int64(len(data)), Offset: int64(off), Data: b}
	}

	call[protocol.SelfUpdateResult](t, c, protocol.MethodSelfUpdate, piece(0, data[:4]))
	release := make(chan struct{})
	core.enqueue(protocol.KindCheck, nil, func(context.Context, protocol.Job) outcome {
		<-release
		return result(nil)
	})
	if code := callErr(c, protocol.MethodSelfUpdate, piece(4, data[4:])); code != protocol.ErrBusy {
		t.Fatalf("last piece with a job running: %q", code)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Errorf("executable changed: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".charm-bak-daemon-update-*")); len(left) != 0 {
		t.Errorf("staging files left: %v", left)
	}
	close(release)
	core.WaitIdle()

	// Idle now: it installs, and holds the queue until the restart.
	call[protocol.SelfUpdateResult](t, c, protocol.MethodSelfUpdate, piece(0, data[:4]))
	res := call[protocol.SelfUpdateResult](t, c, protocol.MethodSelfUpdate, piece(4, data[4:]))
	if !res.Restarting {
		t.Fatalf("result %+v", res)
	}
	ran := make(chan struct{}, 1)
	core.enqueue(protocol.KindCheck, nil, func(context.Context, protocol.Job) outcome {
		ran <- struct{}{}
		return result(nil)
	})
	select {
	case <-ran:
		t.Error("a job started between the install and the restart")
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("never restarted")
	}
	core.Shutdown() // what main does next: the held job is dropped
}

// A transfer abandoned part-way is dropped, with its staged file.
func TestSelfUpdateExpiresStaleTransfer(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	clock := &testClock{t: time.Unix(1_900_000_000, 0)}
	core := NewCore("test", memStore(t), ResticEngines, clock.now)
	core.UpdatePath, core.Restart = exe, func() { t.Error("restarted") }
	c, _ := serve(t, core)
	sha := hex.EncodeToString(make([]byte, 32))
	call[protocol.SelfUpdateResult](t, c, protocol.MethodSelfUpdate, protocol.SelfUpdateParams{SHA256: sha, Size: 10, Data: []byte("half")})
	staged := func() int {
		left, _ := filepath.Glob(filepath.Join(dir, ".charm-bak-daemon-update-*"))
		return len(left)
	}
	clock.advance(5 * time.Minute)
	core.Tick()
	if staged() != 1 {
		t.Fatalf("dropped after 5 minutes: %d staged", staged())
	}
	clock.advance(6 * time.Minute)
	core.Tick()
	if staged() != 0 {
		t.Errorf("still staged after 11 minutes")
	}
}
