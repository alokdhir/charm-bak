package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"charm-bak/internal/protocol"
)

// AWS key rotation, AWS's own procedure (Security Blog, "How to Rotate
// Access Keys for IAM Users") done by the key manager: a second key for
// the machine's user, the machine moved onto it and checked (its
// repository opens with it), then the old key deleted. Only keys charm-bak
// made (aws_made_keys): one the user typed in is theirs to rotate. Every
// 90 days on its own (the age AWS's own checks flag), or when asked
// (key_rotate). A rotation is a job in this Mac's queue, so it has a run
// in history, a failure is notified like a failed backup, and it never
// overlaps this Mac's own backups.

const (
	keyRotateEvery = 90 * 24 * time.Hour
	keyRotateRetry = time.Hour
)

// errRotateLater is a rotation that can't happen now (the machine is busy
// or offline): tried again later, not reported as a failure.
var errRotateLater = errors.New("later")

// requestRotate handles key_rotate: queue host's rotation now.
func (c *Core) requestRotate(p protocol.KeyRotateParams) (protocol.JobResult, error) {
	host := p.Host
	if host == "" {
		host = c.Name
	}
	if _, ok := c.madeKeys()[host]; !ok {
		return protocol.JobResult{}, perr(protocol.ErrBadRequest,
			"charm-bak didn't make %s's AWS key, so it can't replace it: do that in the AWS console, then give %s the new key.", host, host)
	}
	if st, err := c.awsStatus(); err != nil || st.Manager == nil {
		return protocol.JobResult{}, perr(protocol.ErrNotConfigured,
			"There's no key manager on this Mac to make %s a new key: sign in to AWS in setup to make one.", host)
	}
	return protocol.JobResult{Job: c.queueRotate(host)}, nil
}

// queueRotate adds host's rotation to the queue, once: one already
// queued or running for it is returned instead.
func (c *Core) queueRotate(host string) protocol.Job {
	c.mu.Lock()
	var pending *protocol.Job
	check := func(j protocol.Job) {
		if h, ok := c.rotating.Load(j.ID); ok && h == host && pending == nil {
			pending = &j
		}
	}
	for _, p := range c.queue {
		check(p.job)
	}
	if c.current != nil {
		check(c.current.job)
	}
	if c.rotateTried == nil {
		c.rotateTried = map[string]time.Time{}
	}
	if pending == nil {
		c.rotateTried[host] = c.now()
	}
	c.mu.Unlock()
	if pending != nil {
		return *pending
	}
	return c.enqueueNoting(protocol.KindRotate, nil, func(j protocol.Job) { c.rotating.Store(j.ID, host) },
		func(ctx context.Context, job protocol.Job) outcome {
			err := c.rotateKey(ctx, host)
			switch {
			case err == nil:
				return outcome{status: protocol.RunOK}
			case errors.Is(err, errRotateLater):
				return outcome{status: protocol.RunCancelled, err: fmt.Sprintf("Not now: %v; tried again within the hour.", err)}
			}
			return outcome{status: protocol.RunFailed, err: fmt.Sprintf("Couldn't replace %s's AWS key: %v", host, err)}
		})
}

// rotationsDue queues each made key that's 90 days old, from the
// scheduler; a machine tried in the last hour waits.
func (c *Core) rotationsDue(now time.Time) {
	made := c.madeKeys()
	if len(made) == 0 {
		return
	}
	if st, err := c.awsStatus(); err != nil || st.Manager == nil {
		return
	}
	for host, k := range made {
		if now.Sub(k.Made) < keyRotateEvery {
			continue
		}
		c.mu.Lock()
		tried := c.rotateTried[host]
		c.mu.Unlock()
		if now.Sub(tried) < keyRotateRetry {
			continue
		}
		if host != c.Name && !c.isPaired(host) {
			continue
		}
		c.queueRotate(host)
	}
}

func (c *Core) isPaired(host string) bool {
	c.remotes.mu.Lock()
	defer c.remotes.mu.Unlock()
	_, ok := c.remotes.byID[host]
	return ok
}

// rotateKey makes host a new key, moves it onto it, checks it works there
// and deletes the old one. Until the new key is in place and works, the
// old one stays and nothing on the machine changes; a new key that
// doesn't make it is deleted again.
func (c *Core) rotateKey(ctx context.Context, host string) error {
	k, ok := c.madeKeys()[host]
	if !ok {
		return fmt.Errorf("charm-bak didn't make its key")
	}
	st, err := c.awsStatus()
	if err != nil || st.Manager == nil {
		return fmt.Errorf("there's no key manager on this Mac")
	}
	mkey, err := c.readSecret(awsKeychainService, awsManagerAccount)
	if err != nil {
		return fmt.Errorf("the key manager's key isn't in the Keychain")
	}
	env, err := keyEnv(mkey, st.Manager.Region)
	if err != nil {
		return err
	}
	// What the machine uses now, from this Mac's copy: the new key goes in
	// beside the rest (region, endpoint).
	creds, err := c.readSecret(envKeychainService, host)
	if err != nil {
		return fmt.Errorf("this Mac has no copy of its credentials")
	}
	old, err := decodeEnv(creds)
	if err != nil {
		return err
	}
	if old["AWS_ACCESS_KEY_ID"] != k.Key {
		// Replaced by hand since: not charm-bak's to rotate any more.
		c.forgetMadeKey(host)
		return fmt.Errorf("its key was replaced by hand; charm-bak leaves it alone from now on")
	}
	if host != c.Name {
		// Offline: no new key is made for a machine that can't take it.
		if _, _, err := c.remoteClient(ctx, host); err != nil {
			return fmtLater("%s isn't answering", host)
		}
	}

	user := "charm-bak-" + host
	// A user holds two keys at most: one left over from a rotation that
	// stopped part way goes first.
	var keys struct {
		AccessKeyMetadata []struct{ AccessKeyId string }
	}
	if err := c.awsJSON(ctx, env, &keys, "iam", "list-access-keys", "--user-name", user); err != nil {
		return awsPerr(err)
	}
	for _, ak := range keys.AccessKeyMetadata {
		if ak.AccessKeyId != k.Key {
			if _, err := c.aws(ctx, env, "iam", "delete-access-key", "--user-name", user, "--access-key-id", ak.AccessKeyId); err != nil {
				return awsPerr(err)
			}
		}
	}
	var made struct {
		AccessKey struct{ AccessKeyId, SecretAccessKey string }
	}
	if err := c.awsJSON(ctx, env, &made, "iam", "create-access-key", "--user-name", user); err != nil {
		return awsPerr(err)
	}
	newID := made.AccessKey.AccessKeyId
	dropNew := func() {
		c.aws(context.Background(), env, "iam", "delete-access-key", "--user-name", user, "--access-key-id", newID)
	}
	next := map[string]string{}
	for k, v := range old {
		next[k] = v
	}
	next["AWS_ACCESS_KEY_ID"], next["AWS_SECRET_ACCESS_KEY"] = newID, made.AccessKey.SecretAccessKey
	region := next["AWS_DEFAULT_REGION"]
	if region == "" {
		region = st.Manager.Region
	}
	if err := c.awsKeyWorks(ctx, map[string]string{"AWS_ACCESS_KEY_ID": newID, "AWS_SECRET_ACCESS_KEY": made.AccessKey.SecretAccessKey}, region); err != nil {
		dropNew()
		return awsPerr(err)
	}
	if err := c.moveToKey(ctx, host, next); err != nil {
		dropNew()
		return err
	}
	// In place and working: this Mac's copy follows, then the old key goes.
	if err := c.addSecret(envKeychainService, host, "charm-bak credentials ("+host+")", encodeEnv(next)); err != nil {
		logf("AWS: %s has its new key, but this Mac's copy wasn't updated: %v", host, err)
	}
	if host != c.Name {
		c.forgetRemoteView(host)
	}
	c.rememberMadeKey(host, newID)
	if _, err := c.aws(ctx, env, "iam", "delete-access-key", "--user-name", user, "--access-key-id", k.Key); err != nil {
		logf("AWS: %s moved to its new key; deleting the old one failed (it's deleted at the next rotation): %v", host, err)
	}
	logf("AWS: rotated %s's key (%s replaces %s)", host, newID, k.Key)
	return nil
}

// moveToKey puts the new credentials in place where host's backups run,
// checked first: the repository must open with them. This Mac checks its
// own; a remote checks as it stores them (repository_set without init).
func (c *Core) moveToKey(ctx context.Context, host string, env map[string]string) error {
	if host == c.Name {
		c.mu.Lock()
		busy := c.current != nil && c.current.job.Kind != protocol.KindRotate
		c.mu.Unlock()
		if busy {
			return fmtLater("a job is running")
		}
		cfg, err := c.store.Repository()
		if err != nil || cfg == nil {
			return fmt.Errorf("this Mac has no repository")
		}
		pw, err := c.readSecret(repoKeychainService, c.Name)
		if err != nil {
			return fmt.Errorf("its repository password isn't in the Keychain")
		}
		return c.openOrCreate(ctx, RepositoryConfig{Location: cfg.Location}, pw, env, false)
	}
	loc := c.remoteLocation(host)
	pw, err := c.readSecret(repoKeychainService, host)
	if loc == "" || err != nil {
		return fmt.Errorf("this Mac doesn't have %s's repository and password to hand it the key with", host)
	}
	cl, _, err := c.remoteClient(ctx, host)
	if err != nil {
		return fmtLater("%s isn't answering", host)
	}
	raw, err := json.Marshal(protocol.RepositorySetParams{Location: loc, Password: pw, Env: env})
	if err != nil {
		return err
	}
	if _, err := cl.Forward(ctx, protocol.Request{Method: protocol.MethodRepoSet, Params: raw}); err != nil {
		var pe *protocol.Error
		if errors.As(err, &pe) && pe.Code == protocol.ErrBusy {
			return fmtLater("%s is busy", host)
		}
		if !errors.As(err, &pe) || pe.Code == protocol.ErrHostUnreachable {
			return fmtLater("%s isn't answering", host)
		}
		return err
	}
	return nil
}

// fmtLater is a reason to try again later (errRotateLater).
func fmtLater(format string, a ...any) error {
	return fmt.Errorf("%w", &laterError{fmt.Sprintf(format, a...)})
}

type laterError struct{ why string }

func (e *laterError) Error() string        { return e.why }
func (e *laterError) Is(target error) bool { return target == errRotateLater }
