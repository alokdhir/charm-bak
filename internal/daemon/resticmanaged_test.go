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

// A signature that isn't restic's key's is refused.
func TestResticSignatureRefused(t *testing.T) {
	sig := "-----BEGIN PGP SIGNATURE-----\n\nwsBcBAABCAAQBQJnAAAACRCRpoaL0/epBwAAx\n-----END PGP SIGNATURE-----\n"
	if err := verifyResticSignature([]byte("abc  restic_0.20.0_darwin_arm64.bz2\n"), []byte(sig)); err == nil {
		t.Error("a forged signature passed")
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
