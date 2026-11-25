package daemon

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// The embedded key is the one restic's installation docs name.
func TestResticKeyFingerprint(t *testing.T) {
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(resticKey))
	if err != nil || len(keyring) != 1 {
		t.Fatalf("keyring: %v (%d keys)", err, len(keyring))
	}
	if got := strings.ToUpper(keyring[0].PrimaryKey.KeyIdString()); !strings.HasSuffix(resticKeyFingerprint, got) {
		t.Errorf("key id %s isn't restic's %s", got, resticKeyFingerprint)
	}
	fp := strings.ToUpper(bytesHex(keyring[0].PrimaryKey.Fingerprint))
	if fp != resticKeyFingerprint {
		t.Errorf("fingerprint %s, want %s", fp, resticKeyFingerprint)
	}
}

func bytesHex(b []byte) string {
	const digits = "0123456789abcdef"
	var s strings.Builder
	for _, c := range b {
		s.WriteByte(digits[c>>4])
		s.WriteByte(digits[c&15])
	}
	return s.String()
}

func TestParseResticVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"restic 0.19.1 compiled with go1.25.1 on darwin/arm64\n": {0, 19, 1},
		"restic 0.18.0-dev (compiled manually) compiled with go": {0, 18, 0},
		"restic 1.0 compiled": {1, 0, 0},
	} {
		if got, ok := parseResticVersion(in); !ok || got != want {
			t.Errorf("%q: %v %v", in, got, ok)
		}
	}
	if _, ok := parseResticVersion("rustic 0.9"); ok {
		t.Error("parsed something that isn't restic")
	}
	if versionAtLeast([3]int{0, 17, 9}, resticMin) || !versionAtLeast([3]int{0, 18, 0}, resticMin) || !versionAtLeast([3]int{1, 0, 0}, resticMin) {
		t.Error("versionAtLeast")
	}
}

// A well-formed signature by another key is refused: only restic's
// release key counts.
func TestResticSignatureRefused(t *testing.T) {
	other, err := openpgp.NewEntity("not restic", "", "nobody@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("abc  restic_0.20.0_darwin_arm64.bz2\n")
	var sig bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sig, other, bytes.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	// The signature itself is good: it verifies against its own key.
	if _, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList{other}, bytes.NewReader(data), bytes.NewReader(sig.Bytes()), nil); err != nil {
		t.Fatalf("the test's own signature doesn't verify: %v", err)
	}
	if err := verifyResticSignature(data, sig.Bytes()); err == nil {
		t.Error("a signature by another key passed")
	}
}

// A restic that can't say its version is asked once, not on every call.
func TestResticVersionFailureCached(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "restic")
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\necho x >> " + count + "\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, ok := resticVersion(bin); ok {
			t.Fatal("a failing restic had a version")
		}
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 1 {
		t.Errorf("asked %d times, want once", strings.Count(string(b), "x"))
	}
}

// Retries after a failed download back off from a minute to an hour.
func TestResticRetryBackoff(t *testing.T) {
	resticRetry.Lock()
	resticRetry.wait, resticRetry.next = 0, time.Time{}
	resticRetry.Unlock()
	now := time.Now()
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		scheduleResticRetry(now)
		if resticRetry.wait != want || !resticRetry.next.Equal(now.Add(want)) {
			t.Fatalf("wait %v next %v, want %v", resticRetry.wait, resticRetry.next.Sub(now), want)
		}
	}
	for range 10 {
		scheduleResticRetry(now)
	}
	if resticRetry.wait != time.Hour {
		t.Errorf("capped at %v, want an hour", resticRetry.wait)
	}
}

// Jobs wait for the first download attempt; tests (no managed folder)
// don't wait at all.
func TestWaitRestic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	waitRestic(ctx) // managedResticDir is "": returns at once
	if time.Since(start) > 100*time.Millisecond {
		t.Error("waited without a managed folder")
	}
}

// CHARM_BAK_LIVE=1: download restic's real latest release, checked
// against its signed checksums, and run it.
func TestDownloadResticLive(t *testing.T) {
	if os.Getenv("CHARM_BAK_LIVE") == "" {
		t.Skip("CHARM_BAK_LIVE=1 downloads restic from GitHub")
	}
	dir := t.TempDir()
	ver, err := downloadRestic(context.Background(), http.DefaultClient, "https://api.github.com", "https://github.com", dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(dir, "restic"), "version").Output()
	if err != nil || !strings.Contains(string(out), "restic "+ver) {
		t.Fatalf("restic version: %q %v", out, err)
	}
	t.Logf("downloaded and verified restic %s: %s", ver, strings.TrimSpace(string(out)))
}
