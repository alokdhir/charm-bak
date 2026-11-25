package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// fakeAWS is an account as the aws command sees it: buckets, policies,
// users with their path, boundary, inline policy and keys.
type fakeAWS struct {
	mu        sync.Mutex
	calls     []string
	envs      [][]string
	buckets   map[string]bool
	foreign   map[string]bool // buckets someone else owns
	policies  map[string]string
	users     map[string]*fakeUser
	keyN      int
	rejectNew int // get-caller-identity refusals before a new key works
}

type fakeUser struct {
	path, boundary, policy string
	keys                   []string
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{buckets: map[string]bool{}, foreign: map[string]bool{}, policies: map[string]string{}, users: map[string]*fakeUser{}}
}

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (f *fakeAWS) run(_ context.Context, env []string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args[:2], " "))
	f.envs = append(f.envs, env)
	j := func(v any) ([]byte, error) { b, _ := json.Marshal(v); return b, nil }
	fail := func(code string) ([]byte, error) { return nil, &awsError{Code: code, Msg: code} }
	switch strings.Join(args[:2], " ") {
	case "login --region":
		return nil, nil
	case "sts get-caller-identity":
		for _, kv := range env {
			if strings.HasPrefix(kv, "AWS_ACCESS_KEY_ID=") && f.rejectNew > 0 {
				f.rejectNew--
				return fail("InvalidClientTokenId")
			}
		}
		return j(map[string]string{"Account": "111122223333", "Arn": "arn:aws:iam::111122223333:user/admin"})
	case "s3api head-bucket":
		b := flag(args, "--bucket")
		if f.foreign[b] {
			return fail("403")
		}
		if !f.buckets[b] {
			return fail("404")
		}
		return nil, nil
	case "s3api create-bucket":
		f.buckets[flag(args, "--bucket")] = true
		return j(map[string]string{})
	case "s3api put-public-access-block", "s3api put-bucket-encryption", "s3api put-bucket-lifecycle-configuration":
		return nil, nil
	case "iam create-policy":
		n := flag(args, "--policy-name")
		if _, ok := f.policies[n]; ok {
			return fail("EntityAlreadyExists")
		}
		f.policies[n] = flag(args, "--policy-document")
		return j(map[string]any{})
	case "iam create-user":
		n := flag(args, "--user-name")
		if _, ok := f.users[n]; ok {
			return fail("EntityAlreadyExists")
		}
		f.users[n] = &fakeUser{path: flag(args, "--path"), boundary: flag(args, "--permissions-boundary")}
		return j(map[string]any{})
	case "iam get-user":
		u := f.users[flag(args, "--user-name")]
		return j(map[string]any{"User": map[string]any{"Path": u.path, "PermissionsBoundary": map[string]string{"PermissionsBoundaryArn": u.boundary}}})
	case "iam put-user-permissions-boundary":
		f.users[flag(args, "--user-name")].boundary = flag(args, "--permissions-boundary")
		return nil, nil
	case "iam put-user-policy":
		f.users[flag(args, "--user-name")].policy = flag(args, "--policy-document")
		return nil, nil
	case "iam list-access-keys":
		var ks []map[string]string
		for _, k := range f.users[flag(args, "--user-name")].keys {
			ks = append(ks, map[string]string{"AccessKeyId": k})
		}
		return j(map[string]any{"AccessKeyMetadata": ks})
	case "iam delete-access-key":
		u := f.users[flag(args, "--user-name")]
		id := flag(args, "--access-key-id")
		for i, k := range u.keys {
			if k == id {
				u.keys = append(u.keys[:i], u.keys[i+1:]...)
				break
			}
		}
		return nil, nil
	case "iam create-access-key":
		f.keyN++
		id := "AKIA" + strings.Repeat("0", 3) + string(rune('A'+f.keyN))
		u := f.users[flag(args, "--user-name")]
		u.keys = append(u.keys, id)
		return j(map[string]any{"AccessKey": map[string]string{"AccessKeyId": id, "SecretAccessKey": "secret-" + id}})
	}
	return fail("Unexpected " + strings.Join(args, " "))
}

func awsCore(t *testing.T) (*Core, *fakeAWS, *fakeKeychain) {
	t.Helper()
	c := NewCore("studio", memStore(t), func(RepositoryConfig) Engine { return &statsEngine{} }, nil)
	f := newFakeAWS()
	k := &fakeKeychain{items: map[string]string{}}
	c.awsRun, c.awsWait, c.secrets = f.run, time.Millisecond, k
	return c, f, k
}

// Signing in on a fresh account makes the bucket, the boundary, the key
// manager (its key in the Keychain) and the machine's own user, then
// waits for the new key to work.
func TestAWSKeysSignIn(t *testing.T) {
	c, f, k := awsCore(t)
	f.rejectNew = 2
	res, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren", SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Bucket != "charm-bak-111122223333" || res.Region != "us-east-1" || !res.BucketCreated {
		t.Errorf("result %+v", res)
	}
	u := f.users["charm-bak-wren"]
	if u == nil || u.path != "/charm-bak/" || u.boundary != "arn:aws:iam::111122223333:policy/charm-bak-boundary-charm-bak-111122223333" {
		t.Fatalf("wren's user %+v", u)
	}
	if !strings.Contains(u.policy, `"arn:aws:s3:::charm-bak-111122223333/wren/*"`) || !strings.Contains(u.policy, `"wren/*"`) {
		t.Errorf("wren's policy %s", u.policy)
	}
	if len(u.keys) != 1 || res.Env["AWS_ACCESS_KEY_ID"] != u.keys[0] || res.Env["AWS_SECRET_ACCESS_KEY"] != "secret-"+u.keys[0] {
		t.Errorf("key %v, env %v", u.keys, res.Env)
	}
	m := f.users["charm-bak-manager"]
	if m == nil || m.path != "/charm-bak-manager/" || m.boundary != "" {
		t.Fatalf("manager %+v", m)
	}
	if !strings.Contains(m.policy, `"iam:PermissionsBoundary":["arn:aws:iam::111122223333:policy/charm-bak-boundary-charm-bak-111122223333"]`) ||
		!strings.Contains(m.policy, `"arn:aws:iam::111122223333:user/charm-bak/*"`) || strings.Contains(m.policy, "s3:") {
		t.Errorf("manager policy %s", m.policy)
	}
	if got := k.items["charm-bak-aws/manager"]; got != m.keys[0]+":secret-"+m.keys[0] {
		t.Errorf("manager key in Keychain %q", got)
	}
	st, _ := c.awsStatus()
	if st.Manager == nil || st.Manager.Bucket != res.Bucket || st.Manager.Account != "111122223333" {
		t.Errorf("status %+v", st)
	}
	// The sign-in never reaches the person's own ~/.aws.
	for i, call := range f.calls {
		if call == "login --region" {
			if !strings.HasPrefix(envGet(f.envs[i], "AWS_CONFIG_FILE"), "/") || strings.Contains(envGet(f.envs[i], "AWS_CONFIG_FILE"), ".aws") {
				t.Errorf("login config %q", envGet(f.envs[i], "AWS_CONFIG_FILE"))
			}
		}
	}

	// The next machine needs no sign-in: the key manager makes its key,
	// with its own credentials.
	n := len(f.calls)
	res2, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "basement"})
	if err != nil {
		t.Fatal(err)
	}
	for i, call := range f.calls[n:] {
		if call == "login --region" {
			t.Error("signed in again")
		}
		if envGet(f.envs[n+i], "AWS_ACCESS_KEY_ID") != m.keys[0] && call != "sts get-caller-identity" {
			t.Errorf("%s ran without the manager's key", call)
		}
	}
	if u := f.users["charm-bak-basement"]; u == nil || res2.Env["AWS_ACCESS_KEY_ID"] != u.keys[0] || res2.Bucket != res.Bucket {
		t.Errorf("basement %+v %+v", u, res2)
	}

	// Again for the same machine: a new key replaces the old one.
	old := f.users["charm-bak-basement"].keys[0]
	if _, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "basement"}); err != nil {
		t.Fatal(err)
	}
	if ks := f.users["charm-bak-basement"].keys; len(ks) != 1 || ks[0] == old {
		t.Errorf("keys after again %v", ks)
	}
}

func envGet(env []string, k string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, k+"="); ok {
			return v
		}
	}
	return ""
}

// A bucket that's there is used as it is; one someone else owns, or a
// user charm-bak didn't make, is refused with a reason.
func TestAWSKeysExisting(t *testing.T) {
	c, f, _ := awsCore(t)
	f.buckets["mine"] = true
	res, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren", Bucket: "mine", SignIn: true})
	if err != nil || res.BucketCreated {
		t.Fatalf("%+v %v", res, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "s3api put-") || call == "s3api create-bucket" {
			t.Errorf("changed an existing bucket: %s", call)
		}
	}

	f.foreign["taken"] = true
	if _, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren", Bucket: "taken", SignIn: true}); err == nil || !strings.Contains(err.Error(), "belongs to someone else") {
		t.Errorf("foreign bucket: %v", err)
	}

	f.users["charm-bak-pantry"] = &fakeUser{path: "/"}
	if _, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "pantry", Bucket: "mine", SignIn: true}); err == nil || !strings.Contains(err.Error(), "didn't make") {
		t.Errorf("hand-made user: %v", err)
	}

	// Without a manager, no sign-in is refused; a different bucket too.
	c2, _, _ := awsCore(t)
	if _, err := c2.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren"}); err == nil {
		t.Error("made a key with no manager")
	}
	if _, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren", Bucket: "other"}); err == nil || !strings.Contains(err.Error(), "can't make keys for other yet") {
		t.Errorf("other bucket: %v", err)
	}

	// Signing in for a second bucket adds it: the manager then makes keys
	// for either, and its policy allows both boundaries.
	f.buckets["second"] = true
	if _, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren", Bucket: "second", SignIn: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := c.awsStatus()
	if m := st.Manager; m == nil || m.Bucket != "second" || strings.Join(m.Buckets, ",") != "second,mine" {
		t.Fatalf("manager after a second sign-in: %+v", st.Manager)
	}
	if pol := f.users["charm-bak-manager"].policy; !strings.Contains(pol, "charm-bak-boundary-mine") || !strings.Contains(pol, "charm-bak-boundary-second") {
		t.Errorf("manager policy: %s", pol)
	}
	for _, b := range []string{"mine", "second"} {
		res, err := c.awsKeys(context.Background(), protocol.AWSKeysParams{Host: "wren", Bucket: b})
		if err != nil || res.Bucket != b {
			t.Errorf("key for %s: %+v %v", b, res, err)
		}
		if u := f.users["charm-bak-wren"]; u == nil || !strings.HasSuffix(u.boundary, "charm-bak-boundary-"+b) {
			t.Errorf("wren's boundary for %s: %+v", b, u)
		}
	}
}
