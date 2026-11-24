package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// A controller that carries a different build for the remote's platform
// sends it in pieces; the remote verifies it, puts it in place of its
// executable and restarts. The same build isn't sent again.
func TestSelfUpdate(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "pantry-exe")
	os.WriteFile(exe, []byte("old daemon"), 0o755)
	resources := filepath.Join(dir, "Resources")
	newExe := filepath.Join(resources, "linux-"+runtime.GOARCH, "charm-bak-daemon")
	os.MkdirAll(filepath.Dir(newExe), 0o755)
	data := make([]byte, 2*updatePiece+12345) // three pieces
	rand.Read(data)
	os.WriteFile(newExe, data, 0o755)
	want, _ := FileSHA256(newExe)
	restarted := make(chan struct{}, 4)

	p := newRemotePair(t, func(mac, pantry *Core) {
		mac.SetRemoteDaemons(resources)
		pantry.goos = "linux"
		pantry.Build, _ = FileSHA256(exe)
		pantry.UpdatePath = exe
		pantry.Restart = func() { restarted <- struct{}{} }
	})
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("pantry was never updated")
	}
	got, err := os.ReadFile(exe)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("pantry's executable: %d bytes, err %v", len(got), err)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".charm-bak-daemon-update-*")); len(left) != 0 {
		t.Errorf("staging files left: %v", left)
	}

	// Reconnecting with the old build reported (a restart that didn't
	// take) doesn't send it again.
	r := p.mac.remotes.byID["pantry"]
	p.mac.updateRemote(context.Background(), r)
	select {
	case <-restarted:
		t.Error("sent the same build twice")
	case <-time.After(300 * time.Millisecond):
	}
	if r.sentBuild != want {
		t.Errorf("sent %q, want %q", r.sentBuild, want)
	}
}

// The receiving side refuses pieces out of order and a file that doesn't
// match its hash, leaving the executable alone; the Mac's daemon doesn't
// update itself at all.
func TestSelfUpdateRefuses(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "exe")
	os.WriteFile(exe, []byte("old"), 0o755)
	core := NewCore("test", memStore(t), ResticEngines, nil)
	c, _ := serve(t, core)
	if code := callErr(c, protocol.MethodSelfUpdate, protocol.SelfUpdateParams{SHA256: string(bytes.Repeat([]byte("a"), 64)), Size: 3, Data: []byte("new")}); code != protocol.ErrBadRequest {
		t.Errorf("no UpdatePath: %q", code)
	}
	core.UpdatePath, core.Restart = exe, func() { t.Error("restarted") }
	sha := string(bytes.Repeat([]byte("a"), 64)) // not new's hash
	if code := callErr(c, protocol.MethodSelfUpdate, protocol.SelfUpdateParams{SHA256: sha, Size: 3, Offset: 1, Data: []byte("ew")}); code != protocol.ErrBadRequest {
		t.Errorf("out of order: %q", code)
	}
	if code := callErr(c, protocol.MethodSelfUpdate, protocol.SelfUpdateParams{SHA256: sha, Size: 3, Data: []byte("new")}); code != protocol.ErrBadRequest {
		t.Errorf("wrong hash: %q", code)
	}
	if got, _ := os.ReadFile(exe); string(got) != "old" {
		t.Errorf("executable changed: %q", got)
	}
	time.Sleep(700 * time.Millisecond) // no Restart scheduled
}

// A Mac remote running from its app bundle takes the whole bundle: the
// new icon and Info.plist arrive with the executable, and its build then
// matches what the controller carries.
func TestSelfUpdateBundle(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	app := filepath.Join(dir, "install", "charm-bak.app")
	exe := filepath.Join(app, "Contents", "MacOS", "charm-bak-daemon")
	write(exe, "old daemon")
	write(filepath.Join(app, "Contents", "Info.plist"), "old plist")
	resources := filepath.Join(dir, "Resources")
	newApp := filepath.Join(resources, "darwin-"+runtime.GOARCH, "charm-bak.app")
	newExe := filepath.Join(newApp, "Contents", "MacOS", "charm-bak-daemon")
	write(newExe, "new daemon")
	write(filepath.Join(newApp, "Contents", "Info.plist"), "new plist")
	write(filepath.Join(newApp, "Contents", "Resources", "AppIcon.icns"), "icon")
	write(filepath.Join(newApp, "Contents", "_CodeSignature", "CodeResources"), "seal")
	restarted := make(chan struct{}, 4)

	newRemotePair(t, func(mac, pantry *Core) {
		mac.SetRemoteDaemons(resources)
		pantry.goos = "darwin"
		pantry.Build, _ = BuildID(exe)
		pantry.UpdatePath, pantry.UpdateBundle = exe, BundleOf(exe)
		pantry.Restart = func() { restarted <- struct{}{} }
	})
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("pantry was never updated")
	}
	for rel, want := range map[string]string{"MacOS/charm-bak-daemon": "new daemon", "Info.plist": "new plist", "Resources/AppIcon.icns": "icon"} {
		if got, _ := os.ReadFile(filepath.Join(app, "Contents", rel)); string(got) != want {
			t.Errorf("%s: %q", rel, got)
		}
	}
	if fi, _ := os.Stat(exe); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("executable %v", fi)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "install", ".charm-bak-*")); len(left) != 0 {
		t.Errorf("staging left: %v", left)
	}
	got, _ := BuildID(exe)
	if want, _ := BuildID(newExe); got != want {
		t.Errorf("build after update %s, carried %s", got, want)
	}
}

// A bundle with anything outside its own folder is refused and the
// installed one is left alone.
func TestInstallBundleRefuses(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "charm-bak.app")
	os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o755)
	os.WriteFile(filepath.Join(app, "Contents", "MacOS", "d"), []byte("old"), 0o755)
	for _, name := range []string{"charm-bak.app/../evil", "other.app/Contents/MacOS/d", "/charm-bak.app/x"} {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: 3, Typeflag: tar.TypeReg})
		tw.Write([]byte("new"))
		tw.Close()
		gz.Close()
		tgz := filepath.Join(dir, "u.tgz")
		os.WriteFile(tgz, b.Bytes(), 0o600)
		if err := installBundle(tgz, app, "d"); err == nil {
			t.Errorf("%s: installed", name)
		}
		if got, _ := os.ReadFile(filepath.Join(app, "Contents", "MacOS", "d")); string(got) != "old" {
			t.Errorf("%s: executable now %q", name, got)
		}
	}
}

// A Mac remote that can't take bundles isn't sent a bare executable: in
// its old bundle, launchd would refuse to start it.
func TestSelfUpdateSkipsOldMacRemote(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "charm-bak.app", "Contents", "MacOS", "charm-bak-daemon")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte("old daemon"), 0o755)
	resources := filepath.Join(dir, "Resources")
	newExe := filepath.Join(resources, "darwin-"+runtime.GOARCH, "charm-bak.app", "Contents", "MacOS", "charm-bak-daemon")
	os.MkdirAll(filepath.Dir(newExe), 0o755)
	os.WriteFile(newExe, []byte("new daemon"), 0o755)
	newRemotePair(t, func(mac, pantry *Core) {
		mac.SetRemoteDaemons(resources)
		pantry.goos = "darwin"
		pantry.Build, _ = FileSHA256(exe)
		pantry.UpdatePath = exe // no UpdateBundle: an old daemon
		pantry.Restart = func() { t.Error("updated an old Mac remote") }
	})
	time.Sleep(time.Second)
	if got, _ := os.ReadFile(exe); string(got) != "old daemon" {
		t.Errorf("executable now %q", got)
	}
}
