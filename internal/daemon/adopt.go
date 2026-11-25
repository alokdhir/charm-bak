package daemon

import (
	"context"
	"strings"
	"time"

	"charm-bak/internal/backends"
	"charm-bak/internal/protocol"
)

// Adopting a machine's AWS key (aws_adopt) brings a key charm-bak didn't
// make under its care, so it's rotated like one it did (rotate.go) and
// removing the machine deletes its user. One AWS sign-in, since the key
// manager can only touch users under /charm-bak/:
//
//   - the user the key belongs to (asked of AWS by the key's ID) must be
//     charm-bak-<machine>, the name charm-bak gives a machine's user;
//   - the key manager is set up for the machine's bucket (its limit, the
//     bucket's boundary policy, included);
//   - the user moves to /charm-bak/ (IAM changes a user's path in place),
//     gets that boundary and the same folder-only policy a made user has;
//   - this Mac's copy of the key is kept in the current form (a
//     KEYID:SECRET item from before becomes the credentials item rotation
//     reads; this Mac's own repository then uses it);
//   - the key is recorded as charm-bak's, with its real creation date, so
//     an old key is rotated soon.

func (c *Core) awsAdopt(ctx context.Context, p protocol.AWSAdoptParams) (protocol.AWSAdoptResult, error) {
	res := protocol.AWSAdoptResult{Adopted: []protocol.AdoptedKey{}, Failed: []protocol.HostError{}}
	// Everything that can be checked without AWS first, so a sign-in
	// isn't asked for nothing.
	var todo []adoptee
	for _, host := range p.Hosts {
		if host == "" {
			host = c.Name
		}
		a, err := c.adopteeFor(host)
		if err != nil {
			res.Failed = append(res.Failed, protocol.HostError{Host: host, Error: err.Error()})
			continue
		}
		todo = append(todo, a)
	}
	if len(todo) == 0 {
		if len(res.Failed) == 1 {
			return res, perr(protocol.ErrBadRequest, "%s", res.Failed[0].Error)
		}
		return res, nil
	}
	names := make([]string, len(todo))
	for i, a := range todo {
		names[i] = a.host
	}
	aenv, who, done, err := c.awsSignIn(ctx, strings.Join(names, ", "), "us-east-1")
	if err != nil {
		return res, err
	}
	defer done()
	for _, a := range todo {
		made, err := c.adoptOne(ctx, aenv, who, a)
		if err != nil {
			res.Failed = append(res.Failed, protocol.HostError{Host: a.host, Error: err.Error()})
			continue
		}
		res.Adopted = append(res.Adopted, protocol.AdoptedKey{Host: a.host, User: "charm-bak-" + a.host, KeyMade: made})
	}
	return res, nil
}

// adoptee is a machine checked for adoption: its bucket and this Mac's
// copy of its key.
type adoptee struct {
	host, bucket string
	env          map[string]string
}

func (c *Core) adopteeFor(host string) (adoptee, error) {
	if _, ok := c.madeKeys()[host]; ok {
		return adoptee{}, perr(protocol.ErrBadRequest, "%s's AWS key is charm-bak's already.", host)
	}
	if host != c.Name && !c.isPaired(host) {
		return adoptee{}, perr(protocol.ErrNotFound, "No paired remote named %s.", host)
	}
	var loc string
	if host == c.Name {
		cfg, err := c.store.Repository()
		if err != nil || cfg == nil {
			return adoptee{}, perr(protocol.ErrNotConfigured, "%s has no repository.", host)
		}
		loc = cfg.Location
	} else {
		loc = c.remoteLocation(host)
	}
	id, values, ok := backends.Parse(loc)
	if !ok || id != "s3" || values["bucket"] == "" {
		return adoptee{}, perr(protocol.ErrBadRequest, "%s's repository isn't in Amazon S3 (%s): only AWS keys are adopted.", host, loc)
	}
	env, err := c.machineKeyCopy(host)
	if err != nil {
		return adoptee{}, err
	}
	return adoptee{host: host, bucket: values["bucket"], env: env}, nil
}

// adoptOne adopts one machine's key with a signed-in session, returning
// when the key was made.
func (c *Core) adoptOne(ctx context.Context, aenv []string, who awsIdentity, a adoptee) (time.Time, error) {
	host, bucket, keyID := a.host, a.bucket, a.env["AWS_ACCESS_KEY_ID"]
	var used struct{ UserName string }
	if err := c.awsJSON(ctx, aenv, &used, "iam", "get-access-key-last-used", "--access-key-id", keyID); err != nil {
		return time.Time{}, awsPerr(err)
	}
	user := "charm-bak-" + host
	if used.UserName != user {
		return time.Time{}, perr(protocol.ErrBadRequest,
			"%s's key belongs to the IAM user %s; charm-bak looks after users named %s. Leave it as it is, or set %s up again with a key charm-bak makes.", host, used.UserName, user, host)
	}
	region := "us-east-1"
	var where struct{ LocationConstraint *string }
	if err := c.awsJSON(ctx, aenv, &where, "s3api", "get-bucket-location", "--bucket", bucket); err == nil && where.LocationConstraint != nil && *where.LocationConstraint != "" {
		region = *where.LocationConstraint
	}
	if err := c.awsManager(ctx, aenv, who.Account, bucket, region); err != nil {
		return time.Time{}, awsPerr(err)
	}
	boundary := "arn:aws:iam::" + who.Account + ":policy/" + boundaryName(bucket)
	for _, args := range [][]string{
		{"iam", "update-user", "--user-name", user, "--new-path", awsMachinePath},
		{"iam", "put-user-permissions-boundary", "--user-name", user, "--permissions-boundary", boundary},
		{"iam", "put-user-policy", "--user-name", user, "--policy-name", "charm-bak-bucket", "--policy-document", machinePolicy(bucket, host)},
	} {
		if _, err := c.aws(ctx, aenv, args...); err != nil {
			return time.Time{}, awsPerr(err)
		}
	}
	// The key's real age: an old one is due for rotation soon.
	made := c.now()
	var keys struct {
		AccessKeyMetadata []struct {
			AccessKeyId string
			CreateDate  time.Time
		}
	}
	if err := c.awsJSON(ctx, aenv, &keys, "iam", "list-access-keys", "--user-name", user); err == nil {
		for _, k := range keys.AccessKeyMetadata {
			if k.AccessKeyId == keyID && !k.CreateDate.IsZero() {
				made = k.CreateDate
			}
		}
	}
	if err := c.keepKeyCopy(host, a.env); err != nil {
		return time.Time{}, perr(protocol.ErrInternal, "%v", err)
	}
	m := c.madeKeys()
	m[host] = madeKey{Key: keyID, Made: made}
	if err := c.store.SetSetting(awsSettingMadeKeys, m); err != nil {
		return time.Time{}, err
	}
	logf("AWS: adopted %s's key (user %s, now at %s with the %s limit)", host, user, awsMachinePath, bucket)
	return made, nil
}

// machineKeyCopy is this Mac's copy of host's AWS key, as environment:
// the credentials item, or a KEYID:SECRET item from before it.
func (c *Core) machineKeyCopy(host string) (map[string]string, error) {
	if v, err := c.readSecret(envKeychainService, host); err == nil && v != "" {
		env, err := decodeEnv(v)
		if err == nil && env["AWS_ACCESS_KEY_ID"] != "" {
			return env, nil
		}
	}
	if v, err := c.readSecret(awsKeychainService, host); err == nil && v != "" {
		if id, secret, ok := strings.Cut(strings.TrimSpace(v), ":"); ok {
			return map[string]string{"AWS_ACCESS_KEY_ID": id, "AWS_SECRET_ACCESS_KEY": secret}, nil
		}
	}
	return nil, perr(protocol.ErrNotConfigured, "This Mac has no copy of %s's AWS key to adopt.", host)
}

// keepKeyCopy stores host's key as the credentials item rotation reads
// and replaces; this Mac's own repository switches to it.
func (c *Core) keepKeyCopy(host string, env map[string]string) error {
	if err := c.addSecret(envKeychainService, host, "charm-bak credentials ("+host+")", encodeEnv(env)); err != nil {
		return err
	}
	if host != c.Name {
		return nil
	}
	cfg, err := c.store.Repository()
	if err != nil || cfg == nil {
		return err
	}
	if cfg.EnvKeychain == envKeychainService+"/"+host && cfg.AWSKeychain == "" {
		return nil
	}
	cfg.EnvKeychain, cfg.AWSKeychain = envKeychainService+"/"+host, ""
	return c.store.SetRepository(*cfg)
}
