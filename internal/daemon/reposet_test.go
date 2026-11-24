package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm-bak/internal/protocol"
)

// fakeKeychain is the Mac's Keychain in tests.
type fakeKeychain struct {
	mu    sync.Mutex
	items map[string]string
}

func (k *fakeKeychain) read(service, account string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.items[service+"/"+account]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

func (k *fakeKeychain) remove(service, account string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.items, service+"/"+account)
}

func (k *fakeKeychain) add(service, account, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.items[service+"/"+account] = value
	return nil
}

// initEngine records the configs it was asked to init.
type initEngine struct {
	statsEngine
	mu    *sync.Mutex
	inits *[]RepositoryConfig
}

func (e *initEngine) Init(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.inits = append(*e.inits, RepositoryConfig{})
	return nil
}

// repository_set on a remote (what the controller sends it during setup):
// it keeps the password and the AWS key in owner-only files in its state
// directory and uses them; the Mac, whose secrets are in the Keychain,
// refuses it.
func TestRepositorySetOnRemote(t *testing.T) {
	p := newRemotePair(t)
	dir := filepath.Join(t.TempDir(), "state")
	p.pantry.SecretDir = dir
	var mu sync.Mutex
	var inits []RepositoryConfig
	p.pantry.makeEngine = func(r RepositoryConfig) Engine {
		return &initEngine{mu: &mu, inits: &inits}
	}
	const pw = "a-repository-password-of-some-length-0123456789"
	set := func(init bool) protocol.RepositoryResult {
		t.Helper()
		raw, _ := json.Marshal(protocol.RepositorySetParams{Location: "s3:s3.amazonaws.com/bucket/pantry", Password: pw, AWSKey: "AKIDPANTRY:secretpantry", Init: init})
		out, err := p.pantry.Handle(context.Background(), protocol.Request{Method: protocol.MethodRepoSet, Params: raw})
		if err != nil {
			t.Fatalf("repository_set: %v", err)
		}
		res, ok := out.(protocol.RepositoryResult)
		if !ok {
			t.Fatalf("repository_set answered %T", out)
		}
		return res
	}

	r := set(true).Repository
	if r == nil || r.Location != "s3:s3.amazonaws.com/bucket/pantry" {
		t.Fatalf("repository %+v", r)
	}
	pwFile, awsFile := filepath.Join(dir, "repo-password"), filepath.Join(dir, "aws")
	if r.PasswordFrom != "file "+pwFile || r.AWSFrom != "file "+awsFile {
		t.Errorf("from %q, %q", r.PasswordFrom, r.AWSFrom)
	}
	for file, want := range map[string]string{pwFile: pw, awsFile: "AKIDPANTRY:secretpantry"} {
		b, err := os.ReadFile(file)
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s: %q %v", file, b, err)
		}
		if fi, _ := os.Stat(file); fi == nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", file, fi.Mode())
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", fi.Mode())
	}
	if len(inits) != 1 {
		t.Errorf("%d inits, want 1", len(inits))
	}
	cfg, _ := p.pantry.store.Repository()
	if cfg.PasswordFile != pwFile || cfg.AWSFile != awsFile || cfg.PasswordCommand != "" {
		t.Errorf("pantry's config %+v", cfg)
	}
	env, err := Secrets(*cfg)
	if err != nil || env["AWS_ACCESS_KEY_ID"] != "AKIDPANTRY" || env["AWS_SECRET_ACCESS_KEY"] != "secretpantry" {
		t.Errorf("secrets %v %v", env, err)
	}
	sh, _ := ShellEnv(*cfg)
	if strings.Contains(sh, "secretpantry") || strings.Contains(sh, pw) || !strings.Contains(sh, "RESTIC_PASSWORD_FILE='"+pwFile+"'") || !strings.Contains(sh, "cat '"+awsFile+"'") {
		t.Errorf("-env:\n%s", sh)
	}

	// Again, without init: nothing is created a second time.
	set(false)
	if b, _ := os.ReadFile(pwFile); strings.TrimSpace(string(b)) != pw || len(inits) != 1 {
		t.Errorf("second set changed the password or ran init (%d)", len(inits))
	}

	// The Mac keeps its secrets in the Keychain: no repository_set.
	if code := callErr(p.tui, protocol.MethodRepoSet, protocol.RepositorySetParams{Location: "x", Password: "y"}); code != protocol.ErrBadRequest {
		t.Errorf("repository_set on the Mac: %q", code)
	}
}

// Probe: whatever is there opens, so "use one already there" goes ahead.
func (e *initEngine) Probe(context.Context) (ProbeResult, error) {
	return ProbeResult{State: protocol.ProbeOpens}, nil
}
