package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"charm-bak/internal/protocol"
)

// Keychain services for each host's secrets on the controller (the Mac):
// account is the host name, as for this Mac's own items.
const (
	awsKeychainService  = "charm-bak-aws"
	repoKeychainService = "charm-bak-repo"
	envKeychainService  = "charm-bak-env"
)

// setRepository is repository_set: write the secrets into owner-only
// files in SecretDir, optionally create the repository, then make it this
// daemon's repository. Daemons without a SecretDir (the Mac, whose
// secrets live in the Keychain) refuse.
func (c *Core) setRepository(ctx context.Context, p protocol.RepositorySetParams) (protocol.RepositoryResult, error) {
	if c.SecretDir == "" {
		return protocol.RepositoryResult{}, perr(protocol.ErrBadRequest, "%s keeps its secrets in the Keychain; set its repository up in the TUI.", c.Name)
	}
	if strings.TrimSpace(p.Location) == "" || p.Password == "" {
		return protocol.RepositoryResult{}, perr(protocol.ErrBadRequest, "repository_set needs a location and a password.")
	}
	c.mu.Lock()
	busy := c.current != nil
	c.mu.Unlock()
	if busy {
		return protocol.RepositoryResult{}, perr(protocol.ErrBusy, "A job is running; set the repository when it's done.")
	}
	cfg := RepositoryConfig{Location: p.Location}
	// Check (or create) with the secrets as sent, and only then replace
	// the files: a set that doesn't open anything must leave a working
	// repository's secrets alone.
	env := maps.Clone(p.Env)
	if p.AWSKey != "" {
		id, secret, ok := strings.Cut(p.AWSKey, ":")
		if !ok {
			return protocol.RepositoryResult{}, perr(protocol.ErrBadRequest, "aws_key isn't KEYID:SECRET.")
		}
		if env == nil {
			env = map[string]string{}
		}
		env["AWS_ACCESS_KEY_ID"], env["AWS_SECRET_ACCESS_KEY"] = id, secret
	}
	if err := c.openOrCreate(ctx, cfg, p.Password, env, p.Init); err != nil {
		return protocol.RepositoryResult{}, err
	}
	var err error
	if cfg.PasswordFile, err = writeSecret(c.SecretDir, passwordFileName, p.Password); err != nil {
		return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "can't store the password: %v", err)
	}
	// A file this repository doesn't use is one left from the last.
	if p.AWSKey != "" {
		if cfg.AWSFile, err = writeSecret(c.SecretDir, awsFileName, p.AWSKey); err != nil {
			return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "can't store the AWS key: %v", err)
		}
	} else {
		os.Remove(filepath.Join(c.SecretDir, awsFileName))
	}
	if len(p.Env) > 0 {
		if cfg.EnvFile, err = writeSecret(c.SecretDir, envFileName, encodeEnv(p.Env)); err != nil {
			return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "can't store the credentials: %v", err)
		}
	} else {
		os.Remove(filepath.Join(c.SecretDir, envFileName))
	}
	if err := c.store.SetRepository(cfg); err != nil {
		return protocol.RepositoryResult{}, err
	}
	logf("repository set to %s (created: %v); secrets in %s", cfg.Location, p.Init, c.SecretDir)
	c.repositoryChanged()
	return c.repositoryInfo()
}

// openOrCreate creates the repository (init) or checks the password
// opens the one already there, before anything is made to depend on it.
func (c *Core) openOrCreate(ctx context.Context, cfg RepositoryConfig, password string, env map[string]string, create bool) error {
	cfg.Password, cfg.SecretEnv = password, env
	eng := c.makeEngine(cfg)
	if create {
		return eng.Init(ctx)
	}
	res, err := eng.Probe(ctx)
	if err != nil {
		return err
	}
	switch res.State {
	case protocol.ProbeOpens:
		return nil
	case protocol.ProbeEmpty:
		return perr(protocol.ErrNotConfigured, "There's no repository at %s yet.", cfg.Location)
	case protocol.ProbePassword:
		return perr(protocol.ErrBadRequest, "That password doesn't open the repository at %s.", cfg.Location)
	}
	return perr(protocol.ErrResticFailed, "%s", res.Message)
}

// repositoryChanged drops everything cached about the old repository.
func (c *Core) repositoryChanged() {
	c.mu.Lock()
	c.keepCache = map[string]map[string][]string{}
	c.snapLists = map[string]snapList{}
	c.snapGen++
	c.stats = nil
	c.publish(protocol.Event{Name: protocol.EventStateChanged, StateChanged: &protocol.StateChangedEvent{State: c.stateLocked(), Job: c.currentJobLocked()}})
	c.mu.Unlock()
	c.trees.clear()
}

// probeRepository is repository_probe: what's at a location, with these
// credentials, on this machine.
func (c *Core) probeRepository(ctx context.Context, p protocol.RepositoryProbeParams) (protocol.RepositoryProbeResult, error) {
	if strings.TrimSpace(p.Location) == "" {
		return protocol.RepositoryProbeResult{}, perr(protocol.ErrBadRequest, "repository_probe needs a location.")
	}
	res, err := c.makeEngine(RepositoryConfig{Location: p.Location, Password: p.Password, SecretEnv: p.Env}).Probe(ctx)
	if err != nil {
		return protocol.RepositoryProbeResult{}, err
	}
	return protocol.RepositoryProbeResult{State: res.State, Message: res.Message}, nil
}

// setupRepository is repository_setup, on the controller: a machine's
// repository from the setup flow. For this machine on macOS the secrets
// go into the Keychain (charm-bak-env/<name>, charm-bak-repo/<name>);
// for a remote a copy stays here the same way and repository_set sends
// them over the paired connection. A machine that has a repository
// keeps it: changing where backups go isn't this.
func (c *Core) setupRepository(ctx context.Context, p protocol.RepositorySetupParams) (protocol.RepositorySetupResult, error) {
	host := p.Host
	if host == "" {
		host = c.Name
	}
	if strings.TrimSpace(p.Location) == "" {
		return protocol.RepositorySetupResult{}, perr(protocol.ErrBadRequest, "repository_setup needs a location.")
	}
	made := ""
	if p.Password == "" {
		if !p.Init {
			return protocol.RepositorySetupResult{}, perr(protocol.ErrBadRequest, "Connecting to a repository needs its password.")
		}
		p.Password = newPassword()
		made = p.Password
	}
	var res protocol.RepositoryResult
	var err error
	if host == c.Name {
		res, err = c.setupOwn(ctx, p)
	} else {
		res, err = c.setupRemote(ctx, host, p)
	}
	if err != nil {
		return protocol.RepositorySetupResult{}, err
	}
	return protocol.RepositorySetupResult{Repository: res.Repository, Password: made}, nil
}

func (c *Core) setupOwn(ctx context.Context, p protocol.RepositorySetupParams) (protocol.RepositoryResult, error) {
	if r, err := c.store.Repository(); err != nil || r != nil {
		if err != nil {
			return protocol.RepositoryResult{}, err
		}
		return protocol.RepositoryResult{}, perr(protocol.ErrBadRequest, "%s already has a repository (%s).", c.Name, r.Location)
	}
	if c.SecretDir != "" {
		return c.setRepository(ctx, protocol.RepositorySetParams{Location: p.Location, Password: p.Password, Env: p.Env, Init: p.Init})
	}
	c.mu.Lock()
	busy := c.current != nil
	c.mu.Unlock()
	if busy {
		return protocol.RepositoryResult{}, perr(protocol.ErrBusy, "A job is running; set up the repository when it's done.")
	}
	cfg := RepositoryConfig{Location: p.Location}
	label := "charm-bak repository password (" + c.Name + ")"
	if p.Init {
		// Creating: the password is stored before the repository exists,
		// so a Keychain that refuses can't leave a repository nobody can
		// open. If creating fails, the Keychain goes back as it was.
		undo, err := c.holdSecret(repoKeychainService, c.Name, label, p.Password)
		if err != nil {
			return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "%v", err)
		}
		if err := c.openOrCreate(ctx, cfg, p.Password, p.Env, true); err != nil {
			undo()
			return protocol.RepositoryResult{}, err
		}
	} else {
		// Connecting: check first, store after: a typo shouldn't leave
		// Keychain items.
		if err := c.openOrCreate(ctx, cfg, p.Password, p.Env, false); err != nil {
			return protocol.RepositoryResult{}, err
		}
		if err := c.addSecret(repoKeychainService, c.Name, label, p.Password); err != nil {
			return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "%v", err)
		}
	}
	cfg.PasswordCommand = KeychainCommand(repoKeychainService, c.Name)
	if len(p.Env) > 0 {
		if err := c.addSecret(envKeychainService, c.Name, "charm-bak credentials ("+c.Name+")", encodeEnv(p.Env)); err != nil {
			return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "%v", err)
		}
		cfg.EnvKeychain = envKeychainService + "/" + c.Name
	}
	if err := c.store.SetRepository(cfg); err != nil {
		return protocol.RepositoryResult{}, err
	}
	logf("repository set up at %s (created: %v); secrets in the Keychain", cfg.Location, p.Init)
	c.repositoryChanged()
	return c.repositoryInfo()
}

func (c *Core) setupRemote(ctx context.Context, host string, p protocol.RepositorySetupParams) (protocol.RepositoryResult, error) {
	c.remotes.mu.Lock()
	_, paired := c.remotes.byID[host]
	c.remotes.mu.Unlock()
	if !paired {
		return protocol.RepositoryResult{}, perr(protocol.ErrNotFound, "No paired host named %s.", host)
	}
	cl, r, err := c.remoteClient(ctx, host)
	if err != nil {
		return protocol.RepositoryResult{}, err
	}
	forward := func(method string, params any, out any) error {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		res, err := cl.Forward(ctx, protocol.Request{Method: method, Params: raw})
		var pe *protocol.Error
		if err != nil && !errors.As(err, &pe) {
			c.markRemote(r, err)
			return perr(protocol.ErrHostUnreachable, "%s isn't answering: %v", host, err)
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(res, out)
	}
	var cur protocol.RepositoryResult
	if err := forward(protocol.MethodRepository, struct{}{}, &cur); err != nil {
		return protocol.RepositoryResult{}, err
	}
	if cur.Repository != nil {
		return protocol.RepositoryResult{}, perr(protocol.ErrBadRequest, "%s already has a repository (%s).", host, cur.Repository.Location)
	}
	logf("setting up %s's repository at %s (create: %v)", host, p.Location, p.Init)
	var out protocol.RepositoryResult
	label := "charm-bak repository password (" + host + ")"
	set := protocol.RepositorySetParams{Location: p.Location, Password: p.Password, Env: p.Env, Init: p.Init}
	if p.Init {
		// Creating: this Mac's copy of the new password is stored first,
		// so the remote never holds the only one; it goes back as it was
		// if the remote can't create the repository.
		undo, err := c.holdSecret(repoKeychainService, host, label, p.Password)
		if err != nil {
			return protocol.RepositoryResult{}, perr(protocol.ErrInternal, "%v", err)
		}
		if err := forward(protocol.MethodRepoSet, set, &out); err != nil {
			undo()
			return protocol.RepositoryResult{}, err
		}
	} else {
		if err := forward(protocol.MethodRepoSet, set, &out); err != nil {
			return protocol.RepositoryResult{}, err
		}
		// The remote has it; keep a copy here, where the user can find it.
		if err := c.addSecret(repoKeychainService, host, label, p.Password); err != nil {
			logf("couldn't keep %s's password here: %v", host, err)
		}
	}
	if len(p.Env) > 0 {
		if err := c.addSecret(envKeychainService, host, "charm-bak credentials ("+host+")", encodeEnv(p.Env)); err != nil {
			logf("couldn't keep %s's credentials here: %v", host, err)
		}
	}
	// This machine reads the remote's backups itself from here on
	// (remoteread.go): where they are, and that it now has the secrets.
	// The location this Mac chose is kept apart from what the remote
	// reports later, and reads use it (remoteLocation).
	if raw, err := json.Marshal(out); err == nil {
		c.saveRemote(host, cacheRepository, raw)
	}
	if raw, err := json.Marshal(p.Location); err == nil {
		c.saveRemote(host, cacheSetupLocation, raw)
	}
	c.forgetRemoteView(host)
	return out, nil
}

// readSecret and addSecret reach this machine's secret store; tests
// replace them.
func (c *Core) readSecret(service, account string) (string, error) {
	if c.secrets != nil {
		return c.secrets.read(service, account)
	}
	return KeychainRead(service, account)
}

func (c *Core) addSecret(service, account, label, value string) error {
	if c.secrets != nil {
		return c.secrets.add(service, account, value)
	}
	return keychainAdd(service, account, label, value)
}

// holdSecret stores value and returns how to take that back: the item
// removed, or set to what it held before.
func (c *Core) holdSecret(service, account, label, value string) (undo func(), err error) {
	old, readErr := c.readSecret(service, account)
	if err := c.addSecret(service, account, label, value); err != nil {
		return nil, err
	}
	return func() {
		if readErr == nil && old != "" {
			if err := c.addSecret(service, account, label, old); err != nil {
				logf("couldn't put back the Keychain item %s/%s: %v", service, account, err)
			}
			return
		}
		c.removeSecret(service, account)
	}, nil
}

// secretStore stands in for the Keychain in tests.
type secretStore interface {
	read(service, account string) (string, error)
	add(service, account, value string) error
	remove(service, account string)
}
