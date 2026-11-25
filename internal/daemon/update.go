package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm-bak/internal/client"
	"charm-bak/internal/protocol"
)

// Self-update: a controller keeps its remotes on the daemon it carries.
// When it connects to a remote, the remote's hello says which build it
// runs (the SHA-256 of its executable) and on what OS/arch; if the
// controller's copy for that platform differs, it sends it with
// self_update, a piece at a time over the paired connection. The remote
// checks the hash, renames the file over its own executable and restarts
// itself in place (main re-executes the new binary after a clean
// shutdown), so no service manager is involved.
//
// A Mac remote runs from a small app bundle (Full Disk Access is granted
// to the bundle), so it takes the whole bundle instead: a tar.gz that it
// unpacks beside its own and swaps in. That way the bundle's Info.plist,
// icon and signature seal follow the app too. Its build covers them.

// updatePiece is how much of the executable one self_update carries.
const updatePiece = 1 << 20

// FileSHA256 is the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BundleOf is the app bundle an executable runs from
// (X.app/Contents/MacOS/exe), or "".
func BundleOf(exe string) string {
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}

// BuildID is what hello's build says: the executable's SHA-256, and for
// one in an app bundle, of its Info.plist and resource seal too (the
// executable's signature covers them, but a bare-executable update can
// leave an old bundle around a new executable).
func BuildID(exe string) (string, error) {
	app := BundleOf(exe)
	if app == "" {
		return FileSHA256(exe)
	}
	h := sha256.New()
	for _, p := range []string{exe, filepath.Join(app, "Contents", "Info.plist"), filepath.Join(app, "Contents", "_CodeSignature", "CodeResources")} {
		f, err := os.Open(p)
		if err != nil {
			if p == exe {
				return "", err
			}
			fmt.Fprintf(h, "no %s\n", filepath.Base(p))
			continue
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// selfUpdate is an update in progress on the receiving side.
type selfUpdate struct {
	mu       sync.Mutex
	file     *os.File
	bundle   bool
	sha      string
	size     int64
	received int64
	last     time.Time // when the last piece came
}

// updateStale is how long a half-sent update waits for its next piece
// before it's thrown away.
const updateStale = 10 * time.Minute

// expireUpdate drops an update whose sender went away part-way, with its
// staged file (Tick).
func (c *Core) expireUpdate(now time.Time) {
	u := &c.update
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.file != nil && now.Sub(u.last) > updateStale {
		logf("self_update: no piece for %v; dropping the half-sent update", updateStale)
		u.abortLocked()
	}
}

func (u *selfUpdate) abortLocked() {
	if u.file != nil {
		u.file.Close()
		os.Remove(u.file.Name())
	}
	u.resetLocked()
}

// resetLocked forgets the update in progress (not the mutex, held).
func (u *selfUpdate) resetLocked() {
	u.file, u.bundle, u.sha, u.size, u.received = nil, false, "", 0, 0
}

// selfUpdate handles self_update.
func (c *Core) selfUpdate(p protocol.SelfUpdateParams) (protocol.SelfUpdateResult, error) {
	if c.UpdatePath == "" || c.Restart == nil {
		return protocol.SelfUpdateResult{}, perr(protocol.ErrBadRequest, "%s doesn't update itself; its daemon comes with the app.", c.Name)
	}
	u := &c.update
	u.mu.Lock()
	defer u.mu.Unlock()
	if p.Offset == 0 {
		u.abortLocked()
		if p.Size <= 0 || len(p.SHA256) != 64 {
			return protocol.SelfUpdateResult{}, perr(protocol.ErrBadRequest, "self_update needs a size and a SHA-256.")
		}
		c.mu.Lock()
		busy := c.current != nil
		c.mu.Unlock()
		if busy {
			return protocol.SelfUpdateResult{}, perr(protocol.ErrBusy, "A job is running; update when it's done.")
		}
		// Beside the executable (or bundle), so the final rename is atomic.
		dir := filepath.Dir(c.UpdatePath)
		if p.Bundle {
			if c.UpdateBundle == "" {
				return protocol.SelfUpdateResult{}, perr(protocol.ErrBadRequest, "%s doesn't run from an app bundle.", c.Name)
			}
			dir = filepath.Dir(c.UpdateBundle)
		}
		f, err := os.CreateTemp(dir, ".charm-bak-daemon-update-*")
		if err != nil {
			return protocol.SelfUpdateResult{}, perr(protocol.ErrInternal, "can't stage the update: %v", err)
		}
		u.file, u.bundle, u.sha, u.size = f, p.Bundle, p.SHA256, p.Size
	}
	if u.file == nil || p.SHA256 != u.sha || p.Size != u.size || p.Offset != u.received || p.Bundle != u.bundle {
		u.abortLocked()
		return protocol.SelfUpdateResult{}, perr(protocol.ErrBadRequest, "self_update piece out of order; start again from offset 0.")
	}
	if u.received+int64(len(p.Data)) > u.size {
		u.abortLocked()
		return protocol.SelfUpdateResult{}, perr(protocol.ErrBadRequest, "self_update sent more than its size.")
	}
	if _, err := u.file.Write(p.Data); err != nil {
		u.abortLocked()
		return protocol.SelfUpdateResult{}, perr(protocol.ErrInternal, "can't write the update: %v", err)
	}
	u.received += int64(len(p.Data))
	u.last = c.now()
	if u.received < u.size {
		return protocol.SelfUpdateResult{Received: u.received}, nil
	}

	// The last piece. A job that started while the pieces came would be
	// cancelled by the restart: refuse instead (the sender tries again
	// when the job ends), and hold the queue from here to the restart.
	c.mu.Lock()
	busy := c.current != nil
	if !busy {
		c.held = true
	}
	c.mu.Unlock()
	if busy {
		u.abortLocked()
		return protocol.SelfUpdateResult{}, perr(protocol.ErrBusy, "A job started meanwhile; update when it's done.")
	}
	installed := false
	defer func() {
		if !installed {
			c.mu.Lock()
			c.held = false
			c.runNextLocked()
			c.mu.Unlock()
		}
	}()

	// The last piece: verify, install, restart.
	name := u.file.Name()
	if err := u.file.Close(); err != nil {
		u.abortLocked()
		return protocol.SelfUpdateResult{}, perr(protocol.ErrInternal, "can't write the update: %v", err)
	}
	u.file = nil
	defer os.Remove(name) // a no-op once renamed
	got, err := FileSHA256(name)
	if err != nil || got != u.sha {
		u.resetLocked()
		return protocol.SelfUpdateResult{}, perr(protocol.ErrBadRequest, "The update doesn't match its SHA-256; nothing changed.")
	}
	if u.bundle {
		if err := installBundle(name, c.UpdateBundle, filepath.Base(c.UpdatePath)); err != nil {
			u.resetLocked()
			return protocol.SelfUpdateResult{}, perr(protocol.ErrInternal, "can't install the update: %v", err)
		}
	} else {
		if err := os.Chmod(name, 0o755); err != nil {
			u.resetLocked()
			return protocol.SelfUpdateResult{}, perr(protocol.ErrInternal, "can't install the update: %v", err)
		}
		if err := replaceExecutable(name, c.UpdatePath); err != nil {
			u.resetLocked()
			return protocol.SelfUpdateResult{}, perr(protocol.ErrInternal, "can't install the update: %v", err)
		}
	}
	size := u.size
	u.resetLocked()
	installed = true
	logf("self_update: installed %s (%d bytes); restarting", got[:12], size)
	// After the reply has gone out.
	time.AfterFunc(500*time.Millisecond, c.Restart)
	return protocol.SelfUpdateResult{Received: size, Restarting: true}, nil
}

// installBundle unpacks tgz (a tar.gz of one X.app) beside app and swaps
// it in for app. The new bundle must hold Contents/MacOS/<exe>. Only
// folders and regular files, inside the bundle, are unpacked.
func installBundle(tgz, app, exe string) error {
	parent, base := filepath.Dir(app), filepath.Base(app)
	stage, err := os.MkdirTemp(parent, ".charm-bak-bundle-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(strings.TrimSuffix(hdr.Name, "/"))
		if !filepath.IsLocal(name) || (name != base && !strings.HasPrefix(name, base+string(filepath.Separator))) {
			return fmt.Errorf("the bundle has %q outside %s", hdr.Name, base)
		}
		dst := filepath.Join(stage, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("the bundle has %q, which isn't a file or folder", hdr.Name)
		}
	}
	fresh := filepath.Join(stage, base)
	if fi, err := os.Stat(filepath.Join(fresh, "Contents", "MacOS", exe)); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("the bundle has no Contents/MacOS/%s", exe)
	}
	old := filepath.Join(stage, base+".old")
	if err := os.Rename(app, old); err != nil {
		return err
	}
	if err := os.Rename(fresh, app); err != nil {
		os.Rename(old, app)
		return err
	}
	return nil // the old one goes with stage
}

// updateRemote brings a remote onto the daemon this controller carries
// for its platform, if it runs something else. Called when the remote
// (re)connects; at most one at a time per remote, and a build is only
// sent once per remote while this daemon runs, so a remote that can't
// take it isn't sent it again on every reconnect.
func (c *Core) updateRemote(ctx context.Context, r *remote) {
	if c.remoteDaemons == "" {
		return
	}
	c.remotes.mu.Lock()
	if r.updating {
		c.remotes.mu.Unlock()
		return
	}
	r.updating = true
	c.remotes.mu.Unlock()
	defer func() {
		c.remotes.mu.Lock()
		r.updating = false
		c.remotes.mu.Unlock()
	}()

	cl, _, err := c.remoteClient(ctx, r.peer.Name)
	if err == nil {
		// A call first: it redials if the remote restarted, and hello
		// runs on every new connection, so Hello is this daemon's, not
		// the last.
		_, err = cl.Status(ctx)
	}
	if err != nil {
		// A remote that speaks another protocol version refuses the
		// usual hello, exactly when it needs the update: reach it in its
		// own version, for self_update only.
		cl, err = client.DialAnyVersion(ctx, remoteDialer(r.peer, c.identity), c.Version)
		if err != nil {
			return
		}
		defer cl.Close()
		if cl.Hello().Protocol == protocol.Version {
			return // same version: it failed for another reason
		}
		logf("remote %s speaks protocol %d (this daemon %d); sending it this daemon", r.peer.Name, cl.Hello().Protocol, protocol.Version)
	}
	h := cl.Hello()
	path := c.remoteDaemon(h.OS, h.Arch)
	if path == "" {
		return // no daemon for its platform travels with the app
	}
	want, err := BuildID(path)
	if err != nil || want == h.Build {
		return
	}
	// The format is part of what's sent: the same build may go once as
	// an executable and once as a bundle.
	bundle := h.Bundle && BundleOf(path) != ""
	if !bundle && BundleOf(path) != "" {
		// A Mac remote from before bundle updates: a new executable in
		// its old bundle won't start (its signature covers Info.plist,
		// and launchd's exec refuses a mismatch: basement, 2026-11-23).
		logf("not updating %s: its daemon predates bundle updates; reinstall it with Add Remote", r.peer.Name)
		return
	}
	sent := want
	if bundle {
		sent += " bundle"
	}
	// Each build is sent once; one that didn't arrive is tried again (on
	// the next connection, or when the remote's job ends), a few times.
	c.remotes.mu.Lock()
	if r.failedBuild != sent {
		r.failedBuild, r.updateFails = sent, 0
	}
	skip := r.sentBuild == sent || r.updateFails >= updateTries
	c.remotes.mu.Unlock()
	if skip {
		return
	}
	var data []byte
	if bundle {
		var b bytes.Buffer
		err = tarGz(&b, BundleOf(path))
		data = b.Bytes()
	} else {
		data, err = os.ReadFile(path)
	}
	if err == nil {
		err = sendUpdate(ctx, cl, data, bundle)
	}
	if err != nil {
		var pe *protocol.Error
		busy := errors.As(err, &pe) && pe.Code == protocol.ErrBusy
		c.remotes.mu.Lock()
		if !busy { // busy isn't a failure: its job_finished brings this back
			r.updateFails++
		}
		c.remotes.mu.Unlock()
		logf("updating %s: %v", r.peer.Name, err)
		return
	}
	c.remotes.mu.Lock()
	r.sentBuild = sent
	c.remotes.mu.Unlock()
	logf("updated %s to %s; it's restarting", r.peer.Name, want[:12])
}

// updateTries is how often one build is sent to a remote that keeps
// failing to take it (busy doesn't count).
const updateTries = 3

// remoteDaemon is the daemon this app carries for a remote's platform:
// Linux's as a bare executable, macOS's inside its bundle (sent whole to
// a remote that says it takes bundles). "" if none.
func (c *Core) remoteDaemon(goos, arch string) string {
	if c.remoteDaemons == "" || (arch != "amd64" && arch != "arm64") {
		return ""
	}
	switch goos {
	case "linux":
		return filepath.Join(c.remoteDaemons, "linux-"+arch, "charm-bak-daemon")
	case "darwin":
		return filepath.Join(c.remoteDaemons, "darwin-"+arch, "charm-bak.app", "Contents", "MacOS", "charm-bak-daemon")
	case "windows":
		return filepath.Join(c.remoteDaemons, "windows-"+arch, "charm-bak-daemon.exe")
	}
	return ""
}

// sendUpdate sends data (an executable, or a bundle's tar.gz) to cl as
// self_update pieces.
func sendUpdate(ctx context.Context, cl *client.Client, data []byte, bundle bool) error {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	for off := 0; off < len(data); off += updatePiece {
		end := min(off+updatePiece, len(data))
		raw, err := json.Marshal(protocol.SelfUpdateParams{SHA256: sha, Size: int64(len(data)), Offset: int64(off), Data: data[off:end], Bundle: bundle})
		if err != nil {
			return err
		}
		pctx, cancel := context.WithTimeout(ctx, time.Minute)
		out, err := cl.Forward(pctx, protocol.Request{Method: protocol.MethodSelfUpdate, Params: raw})
		cancel()
		if err != nil {
			return err
		}
		var res protocol.SelfUpdateResult
		if err := json.Unmarshal(out, &res); err != nil {
			return err
		}
		if end == len(data) && !res.Restarting {
			return fmt.Errorf("the remote took the last piece but isn't restarting")
		}
	}
	return nil
}
