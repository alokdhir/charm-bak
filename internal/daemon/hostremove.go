package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm-bak/internal/protocol"
)

// removeHost handles host_remove on the controller: tell the remote to
// uninstall itself, then unpair it. A remote that doesn't answer is
// unpaired anyway (it can be cleaned up by hand), unless its backups are
// to be deleted, which only it can do.
func (c *Core) removeHost(ctx context.Context, p protocol.HostRemoveParams) (protocol.HostRemoveResult, error) {
	c.remotes.mu.Lock()
	r := c.remotes.byID[p.Name]
	c.remotes.mu.Unlock()
	if r == nil {
		return protocol.HostRemoveResult{}, perr(protocol.ErrNotFound, "No paired remote named %s.", p.Name)
	}
	var res protocol.HostRemoveResult
	params, _ := json.Marshal(protocol.UninstallParams{DeleteBackups: p.DeleteBackups})
	cl, _, err := c.remoteClient(ctx, p.Name)
	if err == nil {
		_, err = cl.Forward(ctx, protocol.Request{Method: protocol.MethodUninstall, Params: params})
	}
	// A *protocol.Error is the remote's own answer (or remoteClient
	// saying it can't be reached); anything else is a connection that
	// failed or dropped: it didn't answer.
	var refused *protocol.Error
	switch {
	case err == nil:
		res.Uninstalled = true
	case errors.As(err, &refused) && refused.Code != protocol.ErrHostUnreachable:
		return res, err // it answered, and said no: it stays paired
	case p.DeleteBackups:
		return res, perr(protocol.ErrHostUnreachable, "%s isn't answering, and only it can delete its backups: %v", p.Name, err)
	default:
		res.Message = fmt.Sprintf("%s didn't answer, so it's still installed there. %s", p.Name, cleanupByHand(p.Name, r.os))
	}
	if err := c.store.RemovePeer(p.Name); err != nil {
		return res, err
	}
	c.remotes.mu.Lock()
	delete(c.remotes.byID, p.Name)
	c.remotes.mu.Unlock()
	c.forgetRemoteView(p.Name)
	if r.cancel != nil {
		r.cancel()
	}
	if r.client != nil {
		r.client.Close()
	}
	// The Mac's copies of its secrets go with its backups; while those
	// are kept, so are the secrets that open them.
	if p.DeleteBackups {
		for _, svc := range []string{"charm-bak-repo", "charm-bak-env", "charm-bak-aws"} {
			c.removeSecret(svc, p.Name)
		}
	}
	logf("remote %s removed (uninstalled %v, backups deleted %v)", p.Name, res.Uninstalled, p.DeleteBackups)
	c.mu.Lock()
	c.publish(protocol.Event{Name: protocol.EventHostChanged, Host: p.Name,
		HostChanged: &protocol.HostChangedEvent{Host: protocol.Host{Name: p.Name, State: protocol.HostUnreachable, Error: "removed"}}})
	c.mu.Unlock()
	return res, nil
}

// cleanupByHand is what to run on a remote that couldn't be told to
// uninstall itself, for the OS its last hello named ("" if it never
// answered this run: both).
func cleanupByHand(name, goos string) string {
	linux := "sudo systemctl disable --now charm-bak && sudo rm /etc/systemd/system/charm-bak.service /usr/local/bin/charm-bak-daemon && sudo rm -r /var/lib/charm-bak"
	mac := "sudo launchctl bootout system/" + MacLabel + "; sudo rm /Library/LaunchDaemons/" + MacLabel + ".plist && sudo rm -r '/Library/Application Support/charm-bak'"
	switch goos {
	case "linux":
		return "On " + name + ": " + linux
	case "darwin":
		return "On " + name + ": " + mac
	}
	return "On " + name + ", if it runs Linux: " + linux + " If it's a Mac: " + mac
}

// uninstall handles uninstall on a remote: stop every job, delete this
// machine's snapshots if asked, and only then reply and hand over to main
// (Uninstall), which removes the service, executable and state and
// exits. A deletion that fails is the reply: nothing is uninstalled, so
// the controller keeps the pairing and its copies of the secrets, and
// the backups can still be opened.
func (c *Core) uninstall(p protocol.UninstallParams) (protocol.Empty, error) {
	c.mu.Lock()
	done := c.Uninstall
	wasPaused := c.paused
	c.mu.Unlock()
	if done == nil {
		return protocol.Empty{}, perr(protocol.ErrBadRequest, "%s wasn't installed by pairing, so it can't remove itself.", c.Name)
	}
	logf("uninstalling at the controller's request (delete backups %v)", p.DeleteBackups)
	c.SetPaused(true)
	c.Shutdown()
	if p.DeleteBackups {
		if err := c.deleteOwnSnapshots(); err != nil {
			logf("deleting backups failed, so staying installed: %v", err)
			c.SetPaused(wasPaused)
			return protocol.Empty{}, perr(protocol.ErrBadRequest, "%s couldn't delete its backups, so nothing was removed: %v", c.Name, err)
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond) // let the reply go out
		done()
	}()
	return protocol.Empty{}, nil
}

// deleteOwnSnapshots forgets every snapshot this daemon made and prunes.
// In a repository other machines also use, theirs stay: this one's are
// its sources' (by tag, for every source it has or ever ran) and its own
// state backups.
func (c *Core) deleteOwnSnapshots() error {
	engine, err := c.engine()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	own, err := c.store.sourceIDs()
	if err != nil {
		return err
	}
	snaps, err := engine.Snapshots(ctx, "")
	if err != nil {
		return err
	}
	var ids []string
	foreign := 0
	for _, s := range snaps {
		if c.ownSnapshot(s, own) {
			ids = append(ids, s.ID)
		} else {
			foreign++
		}
	}
	if foreign > 0 {
		logf("deleting backups: leaving %d snapshots that aren't this machine's", foreign)
	}
	if len(ids) > 0 {
		if err := engine.Forget(ctx, ids); err != nil {
			return err
		}
	}
	logf("deleting backups: forgot %d snapshots; pruning", len(ids))
	return engine.Maintain(ctx)
}

// ownSnapshot reports whether this daemon made e: it carries one of its
// sources' tags, or it's a state backup from this host.
func (c *Core) ownSnapshot(e EngineSnapshot, sources map[string]bool) bool {
	if id := sourceOf(e); id != "" {
		return sources[id]
	}
	if !slices.Contains(e.Tags, stateTag) {
		return false
	}
	host, _, _ := strings.Cut(strings.ToLower(e.Hostname), ".")
	return host == HostName() || host == strings.ToLower(c.Name) || (c.StateCopy != "" && slices.Equal(e.Paths, []string{c.StateCopy}) && host == "")
}

func (c *Core) removeSecret(service, account string) {
	if c.secrets != nil {
		c.secrets.remove(service, account)
		return
	}
	keychainDelete(service, account)
}
