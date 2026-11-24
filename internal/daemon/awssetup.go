package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"charm-bak/internal/protocol"
)

// aws_keys: S3 for people who don't want to make IAM users by hand.
//
// Signing in (aws login, the AWS CLI's browser sign-in) gives temporary
// credentials with the signed-in user's rights. With them, once: the
// bucket (if it isn't there: private, SSE-S3, a lifecycle transition to
// Intelligent-Tiering, nothing that expires or archives), a permissions
// boundary that allows that bucket only, and the key manager, an IAM user
// that can only make users at path /charm-bak/ with that boundary. Its key
// stays in this machine's Keychain; the sign-in is thrown away. Each
// machine then gets its own user charm-bak-<host> at /charm-bak/ whose
// policy reaches only <bucket>/<host>/, made by the key manager without
// signing in again.
//
// Everything runs through the aws command (its sign-in can't be done
// from an SDK, and it's what a person can rerun by hand).

const (
	awsManagerAccount = "manager" // Keychain charm-bak-aws/manager
	awsManagerUser    = "charm-bak-manager"
	awsManagerPath    = "/charm-bak-manager/"
	awsMachinePath    = "/charm-bak/"
	awsSettingManager = "aws_manager"
	awsSignInTimeout  = 10 * time.Minute
)

// awsRun runs the aws command with env added to a clean environment (no
// AWS_* from this process) and returns its stdout. Tests replace it.
type awsRunFunc func(ctx context.Context, env []string, args ...string) ([]byte, error)

// awsError is a failed aws command: Code is the service's error code
// ("NoSuchEntity"), or the HTTP status for head-bucket ("404").
type awsError struct {
	Code string
	Msg  string
}

func (e *awsError) Error() string { return e.Msg }

var awsCodeRe = regexp.MustCompile(`An error occurred \(([^)]+)\)`)

func awsCode(err error) string {
	var ae *awsError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func awsPath() string {
	if p, err := exec.LookPath("aws"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/aws", "/usr/local/bin/aws"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func runAWS(ctx context.Context, env []string, args ...string) ([]byte, error) {
	exe := awsPath()
	if exe == "" {
		return nil, &awsError{Code: "NoCLI", Msg: "the AWS CLI isn't installed (brew install awscli)"}
	}
	cmd := exec.CommandContext(ctx, exe, append(args, "--output", "json")...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AWS_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "AWS_PAGER=")
	cmd.Env = append(cmd.Env, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		code := ""
		if m := awsCodeRe.FindStringSubmatch(msg); m != nil {
			code = m[1]
		}
		return nil, &awsError{Code: code, Msg: "aws " + args[0] + " " + args[1] + ": " + lastLine(msg)}
	}
	return out, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (c *Core) aws(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if c.awsRun != nil {
		return c.awsRun(ctx, env, args...)
	}
	return runAWS(ctx, env, args...)
}

// awsStatus is aws_status.
func (c *Core) awsStatus() (protocol.AWSStatusResult, error) {
	res := protocol.AWSStatusResult{CLI: c.awsRun != nil || awsPath() != ""}
	var m protocol.AWSManager
	ok, err := c.store.Setting(awsSettingManager, &m)
	if err != nil {
		return res, err
	}
	if ok {
		if _, err := c.readSecret(awsKeychainService, awsManagerAccount); err == nil {
			res.Manager = &m
		}
	}
	return res, nil
}

var iamName = regexp.MustCompile(`^[\w+=,.@-]{1,54}$`)

// awsKeys is aws_keys.
func (c *Core) awsKeys(ctx context.Context, p protocol.AWSKeysParams) (protocol.AWSKeysResult, error) {
	host := p.Host
	if host == "" {
		host = c.Name
	}
	if !iamName.MatchString(host) {
		return protocol.AWSKeysResult{}, perr(protocol.ErrBadRequest, "%s can't be part of an IAM user name.", host)
	}
	if p.SignIn {
		return c.awsSignInKeys(ctx, host, p)
	}
	st, err := c.awsStatus()
	if err != nil {
		return protocol.AWSKeysResult{}, err
	}
	m := st.Manager
	if m == nil {
		return protocol.AWSKeysResult{}, perr(protocol.ErrNotConfigured, "There's no key manager here yet; sign in to AWS to set one up.")
	}
	if p.Bucket != "" && p.Bucket != m.Bucket {
		return protocol.AWSKeysResult{}, perr(protocol.ErrBadRequest, "The key manager only makes keys for %s; sign in to AWS to use %s.", m.Bucket, p.Bucket)
	}
	key, err := c.readSecret(awsKeychainService, awsManagerAccount)
	if err != nil {
		return protocol.AWSKeysResult{}, perr(protocol.ErrNotConfigured, "The key manager's key isn't in the Keychain (%s / %s).", awsKeychainService, awsManagerAccount)
	}
	env, err := keyEnv(key, m.Region)
	if err != nil {
		return protocol.AWSKeysResult{}, err
	}
	logf("making %s's AWS key with the key manager", host)
	made, err := c.awsMachineKey(ctx, env, m.Account, m.Bucket, host)
	if err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	c.awsStep(host, protocol.AWSStepUser, m.Account, m.Bucket)
	if err := c.awsKeyWorks(ctx, made, m.Region); err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	c.awsStep(host, protocol.AWSStepKeyWorks, m.Account, m.Bucket)
	return protocol.AWSKeysResult{Bucket: m.Bucket, Region: m.Region, Env: made}, nil
}

func awsPerr(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return err
	}
	if awsCode(err) == "NoCLI" {
		return perr(protocol.ErrNotConfigured, "The AWS CLI isn't installed. brew install awscli, then try again.")
	}
	return perr(protocol.ErrInternal, "%v", err)
}

// keyEnv turns a stored "KEYID:SECRET" into the aws command's environment.
func keyEnv(key, region string) ([]string, error) {
	id, secret, ok := strings.Cut(key, ":")
	if !ok {
		return nil, perr(protocol.ErrInternal, "The stored key isn't KEYID:SECRET.")
	}
	return []string{"AWS_ACCESS_KEY_ID=" + id, "AWS_SECRET_ACCESS_KEY=" + secret, "AWS_DEFAULT_REGION=" + region,
		"AWS_CONFIG_FILE=" + os.DevNull, "AWS_SHARED_CREDENTIALS_FILE=" + os.DevNull}, nil
}

func (c *Core) awsSignInKeys(ctx context.Context, host string, p protocol.AWSKeysParams) (protocol.AWSKeysResult, error) {
	region := p.Region
	if region == "" {
		region = "us-east-1"
	}
	// The sign-in lives in a folder of its own, gone when this returns:
	// the person's own ~/.aws is never touched and nothing stays signed in.
	dir, err := os.MkdirTemp("", "charm-bak-aws-")
	if err != nil {
		return protocol.AWSKeysResult{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("[default]\nregion = "+region+"\n"), 0o600); err != nil {
		return protocol.AWSKeysResult{}, err
	}
	env := []string{
		"AWS_CONFIG_FILE=" + filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(dir, "credentials"),
		"AWS_LOGIN_CACHE_DIRECTORY=" + filepath.Join(dir, "login"),
		"AWS_DEFAULT_REGION=" + region,
	}
	sctx, cancel := context.WithTimeout(ctx, awsSignInTimeout)
	defer cancel()
	logf("signing in to AWS for %s's key (browser)", host)
	if _, err := c.aws(sctx, env, "login", "--region", region); err != nil {
		if sctx.Err() != nil {
			return protocol.AWSKeysResult{}, perr(protocol.ErrInternal, "Nobody signed in within %v.", awsSignInTimeout)
		}
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	var who struct{ Account, Arn string }
	if err := c.awsJSON(ctx, env, &who, "sts", "get-caller-identity"); err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	logf("signed in to AWS as %s", who.Arn)
	bucket := p.Bucket
	if bucket == "" {
		bucket = "charm-bak-" + who.Account
	}
	c.awsStep(host, protocol.AWSStepSignedIn, who.Account, bucket)
	created, err := c.awsBucket(ctx, env, bucket, region)
	if err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	c.awsStep(host, protocol.AWSStepBucket, who.Account, bucket)
	if err := c.awsManager(ctx, env, who.Account, bucket, region); err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	c.awsStep(host, protocol.AWSStepManager, who.Account, bucket)
	made, err := c.awsMachineKey(ctx, env, who.Account, bucket, host)
	if err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	c.awsStep(host, protocol.AWSStepUser, who.Account, bucket)
	if err := c.awsKeyWorks(ctx, made, region); err != nil {
		return protocol.AWSKeysResult{}, awsPerr(err)
	}
	c.awsStep(host, protocol.AWSStepKeyWorks, who.Account, bucket)
	return protocol.AWSKeysResult{Bucket: bucket, Region: region, Env: made, BucketCreated: created}, nil
}

// awsStep tells subscribers how far making host's key has got.
func (c *Core) awsStep(host, step, account, bucket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publish(protocol.Event{Name: protocol.EventAWSStep, AWSStep: &protocol.AWSStepEvent{For: host, Step: step, Account: account, Bucket: bucket}})
}

func (c *Core) awsJSON(ctx context.Context, env []string, out any, args ...string) error {
	raw, err := c.aws(ctx, env, args...)
	if err != nil {
		return err
	}
	if out == nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// awsBucket makes the bucket with charm-bak's settings when it isn't
// there; one that is, the signed-in account's, is used as it is.
func (c *Core) awsBucket(ctx context.Context, env []string, bucket, region string) (bool, error) {
	_, err := c.aws(ctx, env, "s3api", "head-bucket", "--bucket", bucket)
	switch {
	case err == nil:
		logf("AWS: using bucket %s as it is", bucket)
		return false, nil
	case awsCode(err) == "403":
		return false, perr(protocol.ErrBadRequest, "The bucket %s belongs to someone else (bucket names are global). Choose another name.", bucket)
	case awsCode(err) != "404":
		return false, err
	}
	args := []string{"s3api", "create-bucket", "--bucket", bucket}
	if region != "us-east-1" {
		args = append(args, "--create-bucket-configuration", "LocationConstraint="+region)
	}
	if _, err := c.aws(ctx, env, args...); err != nil {
		if awsCode(err) == "BucketAlreadyExists" {
			return false, perr(protocol.ErrBadRequest, "The bucket name %s is taken (bucket names are global). Choose another.", bucket)
		}
		return false, err
	}
	steps := [][]string{
		{"s3api", "put-public-access-block", "--bucket", bucket, "--public-access-block-configuration",
			"BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"},
		{"s3api", "put-bucket-encryption", "--bucket", bucket, "--server-side-encryption-configuration",
			`{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}`},
		// A transition only: restic decides when objects go, so nothing
		// expires, and no archive tiers (restic has to read to restore
		// and prune).
		{"s3api", "put-bucket-lifecycle-configuration", "--bucket", bucket, "--lifecycle-configuration",
			`{"Rules":[{"ID":"restic-to-intelligent-tiering","Status":"Enabled","Filter":{},"Transitions":[{"Days":0,"StorageClass":"INTELLIGENT_TIERING"}]}]}`},
	}
	for _, s := range steps {
		if _, err := c.aws(ctx, env, s...); err != nil {
			return true, err
		}
	}
	logf("AWS: made bucket %s in %s", bucket, region)
	return true, nil
}

func policyJSON(statements ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": statements})
	return string(b)
}

func boundaryName(bucket string) string { return "charm-bak-boundary-" + bucket }

// boundaryPolicy is the most any machine's user can ever do: this
// bucket's objects and listing. Each machine's own policy narrows it to
// its folder.
func boundaryPolicy(bucket string) string {
	return policyJSON(
		map[string]any{"Sid": "Bucket", "Effect": "Allow", "Action": []string{"s3:ListBucket", "s3:GetBucketLocation"}, "Resource": "arn:aws:s3:::" + bucket},
		map[string]any{"Sid": "Objects", "Effect": "Allow", "Action": []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"}, "Resource": "arn:aws:s3:::" + bucket + "/*"},
	)
}

// machinePolicy is a machine's own: its folder in the bucket, nothing
// else (the same as the hand-made charm-bak-studio's).
func machinePolicy(bucket, host string) string {
	return policyJSON(
		map[string]any{"Sid": "ListOwnPrefix", "Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::" + bucket,
			"Condition": map[string]any{"StringLike": map[string]any{"s3:prefix": []string{host + "/*", host}}}},
		map[string]any{"Sid": "BucketLocation", "Effect": "Allow", "Action": "s3:GetBucketLocation", "Resource": "arn:aws:s3:::" + bucket},
		map[string]any{"Sid": "ReadWriteOwnPrefix", "Effect": "Allow", "Action": []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"},
			"Resource": "arn:aws:s3:::" + bucket + "/" + host + "/*"},
	)
}

// managerPolicy lets the key manager make, change and delete users at
// /charm-bak/ only, and only with the boundary (IAM's documented pattern
// for delegating user creation: the iam:PermissionsBoundary condition on
// CreateUser, PutUserPolicy and DeleteUserPolicy). It has no S3 access and
// can't touch policies, so it can't give anyone more than the bucket.
func managerPolicy(account, bucket string) string {
	users := "arn:aws:iam::" + account + ":user" + awsMachinePath + "*"
	return policyJSON(
		map[string]any{"Sid": "WithBoundaryOnly", "Effect": "Allow",
			"Action":    []string{"iam:CreateUser", "iam:PutUserPolicy", "iam:DeleteUserPolicy"},
			"Resource":  users,
			"Condition": map[string]any{"StringEquals": map[string]any{"iam:PermissionsBoundary": "arn:aws:iam::" + account + ":policy/" + boundaryName(bucket)}}},
		map[string]any{"Sid": "MachineUsers", "Effect": "Allow",
			"Action":   []string{"iam:GetUser", "iam:DeleteUser", "iam:ListUserPolicies", "iam:ListAccessKeys", "iam:CreateAccessKey", "iam:DeleteAccessKey"},
			"Resource": users},
	)
}

// awsManager makes (or remakes) the boundary and the key manager, keeps
// its key in the Keychain and remembers which bucket it serves.
func (c *Core) awsManager(ctx context.Context, env []string, account, bucket, region string) error {
	_, err := c.aws(ctx, env, "iam", "create-policy", "--policy-name", boundaryName(bucket),
		"--description", "charm-bak: the most a machine's key can do (bucket "+bucket+")",
		"--policy-document", boundaryPolicy(bucket))
	if err != nil && awsCode(err) != "EntityAlreadyExists" {
		return err
	}
	_, err = c.aws(ctx, env, "iam", "create-user", "--user-name", awsManagerUser, "--path", awsManagerPath)
	if err != nil && awsCode(err) != "EntityAlreadyExists" {
		return err
	}
	if _, err := c.aws(ctx, env, "iam", "put-user-policy", "--user-name", awsManagerUser,
		"--policy-name", "charm-bak-manager", "--policy-document", managerPolicy(account, bucket)); err != nil {
		return err
	}
	key, err := c.awsFreshKey(ctx, env, awsManagerUser)
	if err != nil {
		return err
	}
	if err := c.addSecret(awsKeychainService, awsManagerAccount, "charm-bak AWS key manager", key); err != nil {
		return err
	}
	logf("AWS: key manager %s set up for bucket %s", awsManagerUser, bucket)
	return c.store.SetSetting(awsSettingManager, protocol.AWSManager{Account: account, Bucket: bucket, Region: region})
}

// awsFreshKey deletes user's access keys and makes a new one, returned as
// "KEYID:SECRET". Only one machine ever uses a key, and a machine being
// set up has no repository yet, so an old key has nothing left to do.
func (c *Core) awsFreshKey(ctx context.Context, env []string, user string) (string, error) {
	var keys struct {
		AccessKeyMetadata []struct{ AccessKeyId string }
	}
	if err := c.awsJSON(ctx, env, &keys, "iam", "list-access-keys", "--user-name", user); err != nil {
		return "", err
	}
	for _, k := range keys.AccessKeyMetadata {
		if _, err := c.aws(ctx, env, "iam", "delete-access-key", "--user-name", user, "--access-key-id", k.AccessKeyId); err != nil {
			return "", err
		}
	}
	var made struct {
		AccessKey struct{ AccessKeyId, SecretAccessKey string }
	}
	if err := c.awsJSON(ctx, env, &made, "iam", "create-access-key", "--user-name", user); err != nil {
		return "", err
	}
	if made.AccessKey.AccessKeyId == "" || made.AccessKey.SecretAccessKey == "" {
		return "", fmt.Errorf("aws iam create-access-key returned no key")
	}
	return made.AccessKey.AccessKeyId + ":" + made.AccessKey.SecretAccessKey, nil
}

// awsMachineKey makes host's user (charm-bak-<host> at /charm-bak/, with
// the boundary and its own folder's policy) and a fresh key for it, as
// restic's environment.
func (c *Core) awsMachineKey(ctx context.Context, env []string, account, bucket, host string) (map[string]string, error) {
	user := "charm-bak-" + host
	_, err := c.aws(ctx, env, "iam", "create-user", "--user-name", user, "--path", awsMachinePath,
		"--permissions-boundary", "arn:aws:iam::"+account+":policy/"+boundaryName(bucket))
	if err != nil && awsCode(err) != "EntityAlreadyExists" {
		return nil, err
	}
	if err != nil {
		// Already there: one charm-bak made can be reused; one made some
		// other way (another path) isn't charm-bak's to change.
		var u struct{ User struct{ Path string } }
		if err := c.awsJSON(ctx, env, &u, "iam", "get-user", "--user-name", user); err != nil {
			if awsCode(err) == "AccessDenied" {
				return nil, perr(protocol.ErrBadRequest, "There's already an IAM user %s that charm-bak didn't make. Delete it or rename this machine.", user)
			}
			return nil, err
		}
		if u.User.Path != awsMachinePath {
			return nil, perr(protocol.ErrBadRequest, "There's already an IAM user %s that charm-bak didn't make. Delete it or rename this machine.", user)
		}
	}
	if _, err := c.aws(ctx, env, "iam", "put-user-policy", "--user-name", user,
		"--policy-name", "charm-bak-bucket", "--policy-document", machinePolicy(bucket, host)); err != nil {
		return nil, err
	}
	key, err := c.awsFreshKey(ctx, env, user)
	if err != nil {
		return nil, err
	}
	id, secret, _ := strings.Cut(key, ":")
	logf("AWS: made %s's key (user %s, %s/%s/ only)", host, user, bucket, host)
	return map[string]string{"AWS_ACCESS_KEY_ID": id, "AWS_SECRET_ACCESS_KEY": secret}, nil
}

// awsKeyWorks waits for a new key to be usable: IAM is eventually
// consistent, and a key used straight away can be refused for a few
// seconds, which would fail the setup flow's check.
func (c *Core) awsKeyWorks(ctx context.Context, made map[string]string, region string) error {
	env, _ := keyEnv(made["AWS_ACCESS_KEY_ID"]+":"+made["AWS_SECRET_ACCESS_KEY"], region)
	wait := c.awsWait
	if wait == 0 {
		wait = 3 * time.Second
	}
	var err error
	for i := 0; i < 20; i++ {
		if _, err = c.aws(ctx, env, "sts", "get-caller-identity"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return fmt.Errorf("the new key still isn't accepted: %w", err)
}
