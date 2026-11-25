package daemon

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// A machine without a recent enough restic gets one of its own, like
// Backrest does: restic's official build for its OS and CPU, downloaded
// from restic's GitHub releases into the daemon's state folder, checked
// against the release's SHA256SUMS and that file's signature by restic's
// release key (the key restic's own self-update checks with,
// internal/selfupdate/verify.go; fingerprint in restic's installation
// docs). A restic that's already installed and recent enough is used
// instead, so Homebrew users keep managing their own. The managed copy is
// kept current by the weekly `restic self-update` (resticupdate.go).
// Windows remotes are set up by their installer instead.

//go:embed restic-key.asc
var resticKey []byte

// resticKeyFingerprint is restic's release key as its installation docs
// give it (0x91A6868BD3F7A907, Alexander Neumann).
const resticKeyFingerprint = "CF8F18F2844575973F79D4E191A6868BD3F7A907"

// resticMin is the oldest restic charm-bak runs: it uses diff --metadata,
// --exclude-cloud-files and rewrite (tested from 0.18.1 on).
var resticMin = [3]int{0, 18, 0}

// managedResticDir is where the daemon keeps its own restic; "" until
// main sets it (tests leave it unset).
var managedResticDir string

// SetManagedResticDir sets the folder for the daemon's own restic.
func SetManagedResticDir(dir string) { managedResticDir = dir }

// managedRestic is the daemon's own restic, if it has one.
func managedRestic() string {
	if managedResticDir == "" {
		return ""
	}
	p := filepath.Join(managedResticDir, "restic")
	if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
		return p
	}
	return ""
}

// resticVersions caches `restic version` per binary and its mtime, so
// FindRestic stays cheap on every call, failures included: a restic that
// can't say its version (another CPU's build, a wrapper) isn't asked again
// until the file changes.
var resticVersions sync.Map // path+mtime -> versionResult

type versionResult struct {
	v  [3]int
	ok bool
}

// resticVersion is the version a restic binary reports, or false.
func resticVersion(bin string) ([3]int, bool) {
	fi, err := os.Stat(bin)
	if err != nil {
		return [3]int{}, false
	}
	key := bin + "@" + fi.ModTime().String()
	if r, ok := resticVersions.Load(key); ok {
		return r.(versionResult).v, r.(versionResult).ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r versionResult
	if out, err := exec.CommandContext(ctx, bin, "version").Output(); err == nil {
		r.v, r.ok = parseResticVersion(string(out))
	}
	resticVersions.Store(key, r)
	return r.v, r.ok
}

// parseResticVersion reads "restic 0.19.1 compiled with go1.25 on …".
func parseResticVersion(s string) ([3]int, bool) {
	f := strings.Fields(s)
	if len(f) < 2 || f[0] != "restic" {
		return [3]int{}, false
	}
	// Up to the first character that's neither a digit nor a dot:
	// "0.18.0-dev", "0.18.1+ds" (Debian's) are 0.18.0 and 0.18.1.
	num := f[1]
	if i := strings.IndexFunc(num, func(r rune) bool { return (r < '0' || r > '9') && r != '.' }); i >= 0 {
		num = num[:i]
	}
	var v [3]int
	parts := strings.SplitN(strings.TrimSuffix(num, "."), ".", 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

func versionAtLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// resticUsable reports whether this machine has a restic charm-bak can
// use: an installed one that's recent enough, or the daemon's own.
func resticUsable() bool {
	if sys := systemRestic(); sys != "" {
		if v, ok := resticVersion(sys); ok && versionAtLeast(v, resticMin) {
			return true
		}
	}
	return managedRestic() != ""
}

// The first download attempt at start: jobs wait for it (waitRestic), so
// a backup that's overdue at start doesn't fail for want of a restic
// that's seconds away. Later attempts come from the scheduler, with
// backoff (retryRestic).
var (
	resticSettled     = make(chan struct{})
	resticSettledOnce sync.Once
	resticRetry       struct {
		sync.Mutex
		running bool
		next    time.Time
		wait    time.Duration
	}
)

// settleRestic marks the first attempt done, however it went.
func settleRestic() { resticSettledOnce.Do(func() { close(resticSettled) }) }

// waitRestic waits for the first download attempt, if one is under way.
func waitRestic(ctx context.Context) {
	if managedResticDir == "" || runtime.GOOS == "windows" {
		return
	}
	select {
	case <-resticSettled:
	case <-ctx.Done():
	}
}

// StartRestic is the first attempt, at start; jobs wait for it.
func StartRestic(ctx context.Context) {
	defer settleRestic()
	if err := EnsureRestic(ctx); err != nil {
		scheduleResticRetry(time.Now())
	}
}

// scheduleResticRetry sets the next attempt: a minute after the first
// failure, doubling to an hour.
func scheduleResticRetry(now time.Time) {
	resticRetry.Lock()
	defer resticRetry.Unlock()
	resticRetry.wait = min(max(resticRetry.wait*2, time.Minute), time.Hour)
	resticRetry.next = now.Add(resticRetry.wait)
}

// retryRestic downloads restic again, from the scheduler, when there's
// still none usable and the backoff allows.
func retryRestic(now time.Time) {
	if managedResticDir == "" || runtime.GOOS == "windows" || resticUsable() {
		return
	}
	resticRetry.Lock()
	if resticRetry.running || now.Before(resticRetry.next) {
		resticRetry.Unlock()
		return
	}
	resticRetry.running = true
	resticRetry.Unlock()
	go func() {
		err := EnsureRestic(context.Background())
		resticRetry.Lock()
		resticRetry.running = false
		resticRetry.Unlock()
		if err != nil {
			scheduleResticRetry(time.Now())
		} else {
			resticRetry.Lock()
			resticRetry.wait = 0
			resticRetry.Unlock()
		}
	}()
}

// EnsureRestic gives this machine a restic it can use: nothing to do when
// one is installed and recent enough, or the daemon already has its own;
// otherwise it downloads restic's latest release. Not on Windows, whose
// installer puts restic beside the daemon.
func EnsureRestic(ctx context.Context) error {
	if runtime.GOOS == "windows" || managedResticDir == "" {
		return nil
	}
	if resticUsable() {
		return nil
	}
	logf("restic: none installed (or older than %d.%d.%d); downloading restic's own release", resticMin[0], resticMin[1], resticMin[2])
	ver, err := downloadRestic(ctx, http.DefaultClient, "https://api.github.com", "https://github.com", managedResticDir)
	if err != nil {
		logf("restic: download failed: %v", err)
		return err
	}
	logf("restic: installed restic %s in %s", ver, managedResticDir)
	return nil
}

// downloadRestic fetches the latest restic release for this OS and CPU
// into dir/restic, after checking its checksum and the checksums'
// signature. api and web are GitHub's (tests point them elsewhere).
func downloadRestic(ctx context.Context, client *http.Client, api, web, dir string) (string, error) {
	get := func(url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "charm-bak")
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	}
	body, err := get(api + "/repos/restic/restic/releases/latest")
	if err != nil {
		return "", err
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("reading restic's latest release: %w", err)
	}
	if !strings.HasPrefix(rel.Tag, "v") {
		return "", fmt.Errorf("restic's latest release has an unexpected tag %q", rel.Tag)
	}
	ver := strings.TrimPrefix(rel.Tag, "v")
	base := web + "/restic/restic/releases/download/" + rel.Tag + "/"
	sums, err := get(base + "SHA256SUMS")
	if err != nil {
		return "", err
	}
	sig, err := get(base + "SHA256SUMS.asc")
	if err != nil {
		return "", err
	}
	if err := verifyResticSignature(sums, sig); err != nil {
		return "", fmt.Errorf("restic's SHA256SUMS isn't signed by restic's release key: %w", err)
	}
	name := fmt.Sprintf("restic_%s_%s_%s.bz2", ver, runtime.GOOS, runtime.GOARCH)
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("restic %s has no build for %s/%s", ver, runtime.GOOS, runtime.GOARCH)
	}
	packed, err := get(base + name)
	if err != nil {
		return "", err
	}
	if got := sha256.Sum256(packed); hex.EncodeToString(got[:]) != want {
		return "", errors.New(name + " doesn't match its checksum")
	}
	bin, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(packed)))
	if err != nil {
		return "", fmt.Errorf("unpacking %s: %w", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".restic-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, "restic")); err != nil {
		return "", err
	}
	return ver, nil
}

// verifyResticSignature checks data's detached, armored signature
// against restic's release key.
func verifyResticSignature(data, sig []byte) error {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(resticKey))
	if err != nil {
		return err
	}
	// Only restic's key, as its installation docs give it.
	if len(keyring) != 1 || !strings.EqualFold(hex.EncodeToString(keyring[0].PrimaryKey.Fingerprint), resticKeyFingerprint) {
		return errors.New("the embedded key isn't restic's release key")
	}
	_, err = openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(data), bytes.NewReader(sig), nil)
	return err
}
