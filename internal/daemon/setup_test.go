package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

func needRestic(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(FindRestic()); err != nil {
		t.Skip("restic not installed")
	}
}

// A probe tells an empty location from a repository the password opens,
// one it doesn't, and a drive that isn't connected, all with real restic.
func TestProbe(t *testing.T) {
	needRestic(t)
	core := NewCore("test", memStore(t), ResticEngines, nil)
	c, _ := serve(t, core)
	loc := filepath.Join(t.TempDir(), "repo")
	probe := func(pw string) protocol.RepositoryProbeResult {
		return call[protocol.RepositoryProbeResult](t, c, protocol.MethodRepoProbe, protocol.RepositoryProbeParams{Location: loc, Password: pw})
	}
	if r := probe(""); r.State != protocol.ProbeEmpty {
		t.Fatalf("before init: %+v", r)
	}
	if err := ResticEngines(RepositoryConfig{Location: loc, Password: "right"}).Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	for pw, want := range map[string]string{"right": protocol.ProbeOpens, "wrong": protocol.ProbePassword, "": protocol.ProbePassword} {
		if r := probe(pw); r.State != want {
			t.Errorf("password %q: %+v, want %s", pw, r, want)
		}
	}
	r := call[protocol.RepositoryProbeResult](t, c, protocol.MethodRepoProbe, protocol.RepositoryProbeParams{Location: "/Volumes/NoSuchDrive-cbk/repo"})
	if r.State != protocol.ProbeFailed || !strings.Contains(r.Message, "isn't connected") {
		t.Errorf("missing drive: %+v", r)
	}
	// A mistyped path doesn't become a tree: only the last folder is made.
	deep := filepath.Join(t.TempDir(), "Users", "sam", "x")
	r = call[protocol.RepositoryProbeResult](t, c, protocol.MethodRepoProbe, protocol.RepositoryProbeParams{Location: deep})
	if r.State != protocol.ProbeFailed || !strings.Contains(r.Message, "no folder") {
		t.Errorf("missing parent: %+v", r)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(deep))); err == nil {
		t.Error("the probe made folders")
	}
}

// repository_setup for this machine: create makes a password, keeps it
// (and the credentials) in the Keychain and returns it once; a machine
// with a repository refuses; connecting checks the password first and
// stores nothing when it's wrong.
func TestSetupOwn(t *testing.T) {
	needRestic(t)
	newMac := func() (*Core, *fakeKeychain) {
		keys := &fakeKeychain{items: map[string]string{}}
		core := NewCore("test", memStore(t), ResticEngines, nil)
		core.Name = "studio"
		core.secrets = keys
		return core, keys
	}
	mac, keys := newMac()
	c, _ := serve(t, mac)
	loc := filepath.Join(t.TempDir(), "repo")
	env := map[string]string{"RESTIC_REST_USERNAME": "u"} // ignored by a local repo, stored all the same
	res := call[protocol.RepositorySetupResult](t, c, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: loc, Env: env, Init: true})
	if res.Repository == nil || res.Repository.Location != loc || len(res.Password) < 40 {
		t.Fatalf("setup: %+v", res)
	}
	if keys.items["charm-bak-repo/studio"] != res.Password {
		t.Error("password not kept in the Keychain")
	}
	if got, err := decodeEnv(keys.items["charm-bak-env/studio"]); err != nil || got["RESTIC_REST_USERNAME"] != "u" {
		t.Errorf("credentials in the Keychain: %v %v", got, err)
	}
	cfg, _ := mac.store.Repository()
	if cfg.PasswordCommand != KeychainCommand("charm-bak-repo", "studio") || cfg.EnvKeychain != "charm-bak-env/studio" {
		t.Errorf("config %+v", cfg)
	}
	if e := callErr(c, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: loc, Init: true}); e != protocol.ErrBadRequest {
		t.Errorf("second setup: %q", e)
	}

	other, otherKeys := newMac()
	c2, _ := serve(t, other)
	if e := callErr(c2, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: loc, Password: "wrong"}); e != protocol.ErrBadRequest {
		t.Errorf("wrong password: %q", e)
	}
	if len(otherKeys.items) != 0 {
		t.Errorf("a failed connect stored %v", otherKeys.items)
	}
	if e := callErr(c2, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: loc}); e != protocol.ErrBadRequest {
		t.Errorf("connect without a password: %q", e)
	}
	res = call[protocol.RepositorySetupResult](t, c2, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Location: loc, Password: res.Password})
	if res.Repository == nil || res.Password != "" {
		t.Errorf("connect: %+v", res)
	}
}

// repository_setup for a remote: the remote stores the secrets in its
// owner-only files and checks them with restic; the Mac keeps a copy.
func TestSetupRemote(t *testing.T) {
	needRestic(t)
	p := newRemotePair(t)
	p.pantry.store.db.Exec("DELETE FROM settings WHERE key = 'repository'")
	keys := &fakeKeychain{items: map[string]string{}}
	p.mac.secrets = keys
	dir := filepath.Join(t.TempDir(), "state")
	p.pantry.SecretDir = dir
	p.pantry.makeEngine = ResticEngines
	waitConnected(t, p)

	loc := filepath.Join(t.TempDir(), "pantry-repo")
	env := map[string]string{"AWS_ACCESS_KEY_ID": "AKID", "AWS_SECRET_ACCESS_KEY": "s3cret"}
	res := call[protocol.RepositorySetupResult](t, p.tui, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Host: "pantry", Location: loc, Env: env, Init: true})
	if res.Repository == nil || res.Repository.Location != loc || len(res.Password) < 40 {
		t.Fatalf("setup: %+v", res)
	}
	if res.Repository.AWSFrom != "file "+filepath.Join(dir, "credentials") {
		t.Errorf("credentials from %q", res.Repository.AWSFrom)
	}
	cfg, _ := p.pantry.store.Repository()
	got, err := Secrets(*cfg)
	if err != nil || got["AWS_SECRET_ACCESS_KEY"] != "s3cret" {
		t.Errorf("pantry's secrets: %v %v", got, err)
	}
	if keys.items["charm-bak-repo/pantry"] != res.Password || keys.items["charm-bak-env/pantry"] == "" {
		t.Errorf("Mac's copy: %v", keys.items)
	}
	if e := callErr(p.tui, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Host: "pantry", Location: loc, Init: true}); e != protocol.ErrBadRequest {
		t.Errorf("second setup: %q", e)
	}
	// The probe runs on pantry when the request names it.
	pr := call[protocol.RepositoryProbeResult](t, p.tui.WithHost("pantry"), protocol.MethodRepoProbe, protocol.RepositoryProbeParams{Location: loc, Password: res.Password})
	if pr.State != protocol.ProbeOpens {
		t.Errorf("probe via pantry: %+v", pr)
	}
}

// -env's eval line reads stored credentials through -exports.
func TestExports(t *testing.T) {
	out, err := Exports(encodeEnv(map[string]string{"B": "it's", "A": "x"}))
	if err != nil || out != "export A='x'\nexport B='it'\\''s'\n" {
		t.Errorf("exports %q %v", out, err)
	}
	if _, err := Exports("not credentials"); err == nil {
		t.Error("garbage decoded")
	}
}

// waitConnected waits for the Mac to reach pantry.
func waitConnected(t *testing.T, p *remotePair) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := call[protocol.HostsResult](t, p.tui, protocol.MethodHosts, nil)
		if len(h.Hosts) == 2 && h.Hosts[1].State == protocol.HostOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pantry never connected: %+v", h.Hosts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// host_remove tells the remote to uninstall itself, then unpairs it;
// kept backups keep the Mac's copies of their secrets, deleted ones
// don't. A remote not installed by pairing refuses, and stays paired.
func TestHostRemove(t *testing.T) {
	p := newRemotePair(t)
	keys := &fakeKeychain{items: map[string]string{"charm-bak-repo/pantry": "pw", "charm-bak-env/pantry": "e"}}
	p.mac.secrets = keys
	waitConnected(t, p)
	if e := callErr(p.tui, protocol.MethodHostRemove, protocol.HostRemoveParams{Name: "pantry"}); e != protocol.ErrBadRequest {
		t.Fatalf("pantry without Uninstall: %q", e)
	}
	uninstalled := make(chan struct{}, 1)
	p.pantry.mu.Lock() // the server reads it
	p.pantry.Uninstall = func() { uninstalled <- struct{}{} }
	p.pantry.mu.Unlock()
	res := call[protocol.HostRemoveResult](t, p.tui, protocol.MethodHostRemove, protocol.HostRemoveParams{Name: "pantry"})
	if !res.Uninstalled {
		t.Fatalf("result %+v", res)
	}
	select {
	case <-uninstalled:
	case <-time.After(5 * time.Second):
		t.Fatal("pantry never uninstalled")
	}
	if peers, _ := p.mac.store.Peers(); len(peers) != 0 {
		t.Errorf("still paired: %+v", peers)
	}
	if hs := call[protocol.HostsResult](t, p.tui, protocol.MethodHosts, nil); len(hs.Hosts) != 1 {
		t.Errorf("hosts %+v", hs.Hosts)
	}
	if keys.items["charm-bak-repo/pantry"] != "pw" {
		t.Error("kept backups lost their password on the Mac")
	}
	if e := callErr(p.tui, protocol.MethodHostRemove, protocol.HostRemoveParams{Name: "pantry"}); e != protocol.ErrNotFound {
		t.Errorf("removing it twice: %q", e)
	}
}
