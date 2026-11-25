package daemon

import (
	"context"
	"strings"
	"testing"

	"charm-bak/internal/protocol"
)

// A key made before charm-bak made keys (its user at IAM's default path,
// this Mac's copy as KEYID:SECRET) is adopted: the user moves under
// /charm-bak/ with the bucket's limit, this Mac's repository reads the
// key from the credentials item, and it counts as charm-bak's, so it's
// rotated.
func TestAdoptKey(t *testing.T) {
	c, f, k := awsCore(t)
	c.Name = "studio"
	f.buckets["example-backups"] = true
	f.users["charm-bak-studio"] = &fakeUser{path: "/", policy: "hand-written", keys: []string{"AKIAOLDSTUDIO"}}
	k.add(awsKeychainService, "studio", "AKIAOLDSTUDIO:secret")
	if err := c.store.SetRepository(RepositoryConfig{Location: "s3:s3.amazonaws.com/example-backups/studio", AWSKeychain: awsKeychainService + "/studio"}); err != nil {
		t.Fatal(err)
	}
	res, err := c.awsAdopt(context.Background(), protocol.AWSAdoptParams{Hosts: []string{""}})
	if err != nil || len(res.Adopted) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	u := f.users["charm-bak-studio"]
	if res.Adopted[0].User != "charm-bak-studio" || u.path != "/charm-bak/" || !strings.HasSuffix(u.boundary, "charm-bak-boundary-example-backups") || !strings.Contains(u.policy, "example-backups/studio/*") {
		t.Fatalf("after adopting: %+v user %+v", res, u)
	}
	if m := c.madeKeys()["studio"]; m.Key != "AKIAOLDSTUDIO" {
		t.Errorf("not counted as charm-bak's: %+v", m)
	}
	cfg, _ := c.store.Repository()
	if cfg.EnvKeychain != envKeychainService+"/studio" || cfg.AWSKeychain != "" {
		t.Errorf("repository reads its key from %+v", cfg)
	}
	if env, _ := decodeEnv(k.items[envKeychainService+"/studio"]); env["AWS_ACCESS_KEY_ID"] != "AKIAOLDSTUDIO" {
		t.Errorf("credentials item %v", env)
	}
	if _, err := c.awsAdopt(context.Background(), protocol.AWSAdoptParams{Hosts: []string{""}}); err == nil {
		t.Error("adopted twice")
	}
}

// A key whose user isn't charm-bak-<machine>, or a repository that isn't
// in S3, isn't adopted, and nothing in IAM changes.
func TestAdoptRefused(t *testing.T) {
	c, f, k := awsCore(t)
	c.Name = "studio"
	f.users["alice"] = &fakeUser{path: "/", keys: []string{"AKIAALICE"}}
	k.add(awsKeychainService, "studio", "AKIAALICE:secret")
	c.store.SetRepository(RepositoryConfig{Location: "s3:s3.amazonaws.com/example-backups/studio"})
	if res, _ := c.awsAdopt(context.Background(), protocol.AWSAdoptParams{Hosts: []string{""}}); len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "belongs to the IAM user alice") {
		t.Errorf("someone else's user: %+v", res)
	}
	if f.users["alice"].path != "/" {
		t.Error("alice moved")
	}
	c.store.SetRepository(RepositoryConfig{Location: "/Volumes/Backup/studio"})
	if _, err := c.awsAdopt(context.Background(), protocol.AWSAdoptParams{Hosts: []string{""}}); err == nil || !strings.Contains(err.Error(), "isn't in Amazon S3") {
		t.Errorf("a drive: %v", err)
	}
}

// Several machines take one sign-in; one that fails doesn't stop the
// others.
func TestAdoptSeveralOneSignIn(t *testing.T) {
	c, f, k := awsCore(t)
	c.Name = "studio"
	f.buckets["example-backups"] = true
	f.users["charm-bak-studio"] = &fakeUser{path: "/", keys: []string{"AKIASTUDIO"}}
	k.add(awsKeychainService, "studio", "AKIASTUDIO:s")
	c.store.SetRepository(RepositoryConfig{Location: "s3:s3.amazonaws.com/example-backups/studio"})
	res, err := c.awsAdopt(context.Background(), protocol.AWSAdoptParams{Hosts: []string{"", "nowhere"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0].Host != "studio" || len(res.Failed) != 1 || res.Failed[0].Host != "nowhere" {
		t.Fatalf("result %+v", res)
	}
	logins := 0
	for _, call := range f.calls {
		if strings.HasPrefix(call, "login") {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("%d sign-ins, want 1 (calls %v)", logins, f.calls)
	}
}
