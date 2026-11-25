package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// keyEngine opens the repository only with the key it was given.
type keyEngine struct {
	Engine
	cfg  RepositoryConfig
	good func(id string) bool
}

func (e *keyEngine) Probe(context.Context) (ProbeResult, error) {
	if e.good(e.cfg.SecretEnv["AWS_ACCESS_KEY_ID"]) {
		return ProbeResult{State: protocol.ProbeOpens}, nil
	}
	return ProbeResult{State: protocol.ProbeFailed, Message: "403 Forbidden"}, nil
}

// rotationCore is a Mac with a key manager and a repository whose key
// charm-bak made; reject says which keys its repository refuses.
func rotationCore(t *testing.T, reject func(string) bool) (*Core, *fakeAWS, *fakeKeychain, string) {
	t.Helper()
	c, f, k := awsCore(t)
	c.makeEngine = func(cfg RepositoryConfig) Engine {
		return &keyEngine{cfg: cfg, good: func(id string) bool { return id != "" && !reject(id) }}
	}
	res, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: c.Name, SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.store.SetRepository(RepositoryConfig{Location: "s3:s3.amazonaws.com/" + res.Bucket + "/" + c.Name}); err != nil {
		t.Fatal(err)
	}
	k.add(repoKeychainService, c.Name, "pw")
	k.add(envKeychainService, c.Name, encodeEnv(map[string]string{"AWS_ACCESS_KEY_ID": res.Env["AWS_ACCESS_KEY_ID"], "AWS_SECRET_ACCESS_KEY": res.Env["AWS_SECRET_ACCESS_KEY"], "AWS_DEFAULT_REGION": "us-east-1"}))
	return c, f, k, res.Env["AWS_ACCESS_KEY_ID"]
}

// A rotation leaves the machine on a new key that works, this Mac's
// copy updated, the old key deleted, and the date reset.
func TestRotateKey(t *testing.T) {
	c, f, k, old := rotationCore(t, func(string) bool { return false })
	if err := c.rotateKey(context.Background(), c.Name); err != nil {
		t.Fatal(err)
	}
	u := f.users["charm-bak-"+c.Name]
	if len(u.keys) != 1 || u.keys[0] == old {
		t.Fatalf("keys after rotating: %v (old %s)", u.keys, old)
	}
	env, _ := decodeEnv(k.items[envKeychainService+"/"+c.Name])
	if env["AWS_ACCESS_KEY_ID"] != u.keys[0] || env["AWS_DEFAULT_REGION"] != "us-east-1" {
		t.Errorf("this Mac's copy: %v", env)
	}
	if m := c.madeKeys()[c.Name]; m.Key != u.keys[0] || time.Since(m.Made) > time.Minute {
		t.Errorf("made key %+v", m)
	}
}

// A new key the repository refuses is deleted again; the old one stays,
// in use and in the copy.
func TestRotateKeyThatFails(t *testing.T) {
	var old string
	c, f, k, made := rotationCore(t, func(id string) bool { return old != "" && id != old })
	old = made
	if err := c.rotateKey(context.Background(), c.Name); err == nil {
		t.Fatal("rotated onto a key the repository refuses")
	}
	if u := f.users["charm-bak-"+c.Name]; len(u.keys) != 1 || u.keys[0] != old {
		t.Errorf("keys after a failed rotation: %v, want only %s", u.keys, old)
	}
	if env, _ := decodeEnv(k.items[envKeychainService+"/"+c.Name]); env["AWS_ACCESS_KEY_ID"] != old {
		t.Errorf("copy moved to %s", env["AWS_ACCESS_KEY_ID"])
	}
}

// A key typed in, or one replaced by hand since, isn't charm-bak's to
// rotate.
func TestRotateOnlyMadeKeys(t *testing.T) {
	c, _, k, _ := rotationCore(t, func(string) bool { return false })
	if _, err := c.requestRotate(protocol.KeyRotateParams{Host: "pantry"}); err == nil || !strings.Contains(err.Error(), "didn't make") {
		t.Errorf("a key charm-bak didn't make: %v", err)
	}
	k.add(envKeychainService, c.Name, encodeEnv(map[string]string{"AWS_ACCESS_KEY_ID": "AKIABYHAND", "AWS_SECRET_ACCESS_KEY": "x"}))
	if err := c.rotateKey(context.Background(), c.Name); err == nil || !strings.Contains(err.Error(), "replaced by hand") {
		t.Errorf("a key replaced by hand: %v", err)
	}
	if _, ok := c.madeKeys()[c.Name]; ok {
		t.Error("still counted as charm-bak's")
	}
}

// Keys 90 days old are queued on their own; younger ones and ones tried
// in the last hour aren't.
func TestRotationsDue(t *testing.T) {
	now := time.Now()
	c, _, _, _ := rotationCore(t, func(string) bool { return false })
	c.now = func() time.Time { return now }
	c.mu.Lock()
	c.held = true // keep jobs queued, not run, for the count
	c.mu.Unlock()
	queued := func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		n := 0
		for _, p := range c.queue {
			if p.job.Kind == protocol.KindRotate {
				n++
			}
		}
		return n
	}
	c.rotationsDue(now)
	if queued() != 0 {
		t.Fatal("a new key queued for rotation")
	}
	now = now.Add(keyRotateEvery + time.Hour)
	c.rotationsDue(now)
	c.rotationsDue(now)
	if queued() != 1 {
		t.Fatalf("%d rotations queued, want 1", queued())
	}
}

// A remote's key: made by this Mac's key manager, handed over the paired
// connection (repository_set, which checks the repository still opens),
// this Mac's copy following, the old key deleted.
func TestRotateRemoteKey(t *testing.T) {
	needRestic(t)
	p := newRemotePair(t)
	p.pantry.store.db.Exec("DELETE FROM settings WHERE key = 'repository'")
	keys := &fakeKeychain{items: map[string]string{}}
	p.mac.secrets = keys
	f := newFakeAWS()
	p.mac.awsRun, p.mac.awsWait = f.run, time.Millisecond
	dir := filepath.Join(t.TempDir(), "state")
	p.pantry.SecretDir = dir
	p.pantry.makeEngine = ResticEngines
	waitConnected(t, p)

	made, err := p.mac.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "pantry", SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	old := made.Env["AWS_ACCESS_KEY_ID"]
	loc := filepath.Join(t.TempDir(), "pantry-repo")
	call[protocol.RepositorySetupResult](t, p.tui, protocol.MethodRepoSetup, protocol.RepositorySetupParams{Host: "pantry", Location: loc, Env: made.Env, Init: true})

	job := call[protocol.JobResult](t, p.tui, protocol.MethodKeyRotate, protocol.KeyRotateParams{Host: "pantry"}).Job
	if job.Kind != protocol.KindRotate {
		t.Fatalf("job %+v", job)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if m := p.mac.madeKeys()["pantry"]; m.Key != old {
			break
		}
		if time.Now().After(deadline) {
			hist := call[protocol.HistoryResult](t, p.tui, protocol.MethodHistory, protocol.HistoryParams{})
			t.Fatalf("not rotated; runs %+v", hist.Runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	newID := p.mac.madeKeys()["pantry"].Key
	cfg, _ := p.pantry.store.Repository()
	got, err := Secrets(*cfg)
	if err != nil || got["AWS_ACCESS_KEY_ID"] != newID {
		t.Errorf("pantry uses %v (%v), want %s", got["AWS_ACCESS_KEY_ID"], err, newID)
	}
	if env, _ := decodeEnv(keys.items[envKeychainService+"/pantry"]); env["AWS_ACCESS_KEY_ID"] != newID {
		t.Errorf("this Mac's copy has %s", env["AWS_ACCESS_KEY_ID"])
	}
	if u := f.users["charm-bak-pantry"]; len(u.keys) != 1 || u.keys[0] != newID {
		t.Errorf("pantry's user's keys %v, want only %s", u.keys, newID)
	}
}
