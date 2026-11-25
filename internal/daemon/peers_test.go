package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"charm-bak/internal/client"
	"charm-bak/internal/protocol"
)

// remotePair is a controller ("studio") paired with a remote ("pantry"),
// both in this process, the remote on a loopback TLS port.
type remotePair struct {
	mac, pantry     *Core
	macID, pantryID *Identity
	pantryAddr      string
	tui             *client.Client // the controller's socket, as the TUI sees it
	pantrySrv       *Server
}

func newRemotePair(t *testing.T, setup ...func(mac, pantry *Core)) *remotePair {
	t.Helper()
	dir := t.TempDir()
	macID, err := LoadIdentity(filepath.Join(dir, "mac.pem"), "studio")
	if err != nil {
		t.Fatal(err)
	}
	pantryID, err := LoadIdentity(filepath.Join(dir, "pantry.pem"), "pantry")
	if err != nil {
		t.Fatal(err)
	}
	engine := func(RepositoryConfig) Engine { return &statsEngine{} }

	pantryStore := memStore(t)
	pantryStore.SetRepository(RepositoryConfig{Location: "/srv/pantry-repo"})
	pantry := NewCore("test", pantryStore, engine, nil)
	pantry.Name = "pantry"
	pantryStore.AddPeer(Peer{Name: "studio", Pin: macID.Pin, Added: time.Now()})
	mac := NewCore("test", nil, engine, nil) // store set below
	mac.Name = "studio"
	// Never the real Keychain: the controller looks there for a remote's
	// secrets before reading its repository.
	mac.secrets = &fakeKeychain{items: map[string]string{}}
	for _, f := range setup {
		f(mac, pantry)
	}
	srv, err := ListenRemote(pantry, "127.0.0.1:0", pantryID)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close(); pantry.Shutdown() })

	macStore := memStore(t)
	macStore.SetRepository(RepositoryConfig{Location: "mac-repo"})
	mac.store = macStore
	addr := srv.ln.Addr().String()
	macStore.AddPeer(Peer{Name: "pantry", Address: addr, Pin: pantryID.Pin, Added: time.Now()})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := mac.StartRemotes(ctx, macID); err != nil {
		t.Fatal(err)
	}
	tui, _ := serve(t, mac)
	return &remotePair{mac: mac, pantry: pantry, macID: macID, pantryID: pantryID, pantryAddr: addr, tui: tui, pantrySrv: srv}
}

// Requests naming pantry run on pantry; its events come back tagged.
func TestRelayToRemote(t *testing.T) {
	p := newRemotePair(t)
	events, unsub := p.mac.Subscribe()
	defer unsub()

	// hosts: the Mac first, then pantry, which connects shortly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := call[protocol.HostsResult](t, p.tui, protocol.MethodHosts, nil)
		if len(h.Hosts) != 2 || h.Hosts[0].Name != "studio" || !h.Hosts[0].Local || h.Hosts[1].Name != "pantry" {
			t.Fatalf("hosts %+v", h.Hosts)
		}
		if h.Hosts[1].State == protocol.HostOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pantry never connected: %+v", h.Hosts[1])
		}
		time.Sleep(20 * time.Millisecond)
	}

	pantry := p.tui.WithHost("pantry")
	st := call[protocol.StatusResult](t, pantry, protocol.MethodStatus, nil)
	if st.Repository == nil || st.Repository.Location != "/srv/pantry-repo" {
		t.Fatalf("status via relay: %+v", st.Repository)
	}
	src := call[protocol.SourceResult](t, pantry, protocol.MethodSourceAdd, protocol.SourceAddParams{Name: "videos", Paths: []string{"/srv/videos"}})
	if got := call[protocol.SourcesResult](t, p.tui, protocol.MethodSources, nil); len(got.Sources) != 0 {
		t.Errorf("the source landed on the Mac too: %+v", got.Sources)
	}
	if src.Source.Host != "pantry" {
		t.Errorf("source_add via pantry: host %q", src.Source.Host)
	}
	if got := call[protocol.SourcesResult](t, pantry, protocol.MethodSources, nil); len(got.Sources) != 1 || got.Sources[0].ID != src.Source.ID || got.Sources[0].Host != "pantry" {
		t.Errorf("pantry's sources %+v", got.Sources)
	}

	// pantry's source_changed reached the Mac's subscribers as pantry's.
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Name == protocol.EventSourceChanged && e.Host == "pantry" && e.SourceChanged.Source.Name == "videos" && e.SourceChanged.Source.Host == "pantry" {
				goto relayed
			}
		case <-timeout:
			t.Fatal("no source_changed from pantry")
		}
	}
relayed:

	// Errors from pantry come back as they are.
	var pe *protocol.Error
	err := pantry.Call(context.Background(), protocol.MethodSourceRemove, protocol.SourceRemoveParams{ID: "nope"}, nil)
	if !errors.As(err, &pe) || pe.Code != protocol.ErrNotFound {
		t.Errorf("remove unknown on pantry: %v", err)
	}
	if code := callErr(p.tui.WithHost("nowhere"), protocol.MethodStatus, nil); code != protocol.ErrNotFound {
		t.Errorf("unknown host: %s", code)
	}
}

// pantry only talks to daemons it has pinned, and the Mac only to the key
// it pinned for pantry.
func TestRemoteRefusesStrangers(t *testing.T) {
	p := newRemotePair(t)
	stranger, err := LoadIdentity(filepath.Join(t.TempDir(), "x.pem"), "x")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := remoteDialer(Peer{Name: "pantry", Address: p.pantryAddr, Pin: p.pantryID.Pin}, stranger)
	if c, err := client.DialWith(ctx, dial, "test"); err == nil {
		c.Close()
		t.Error("pantry accepted an unpaired key")
	}
	wrongPin := remoteDialer(Peer{Name: "pantry", Address: p.pantryAddr, Pin: stranger.Pin}, p.macID)
	if c, err := client.DialWith(ctx, wrongPin, "test"); err == nil {
		c.Close()
		t.Error("the Mac accepted pantry under the wrong pin")
	}
}

// The identity key survives restarts: same pin.
func TestIdentityPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.pem")
	a, err := LoadIdentity(path, "x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(path, "x")
	if err != nil || a.Pin != b.Pin {
		t.Errorf("pin changed across loads: %s vs %s (%v)", a.Pin, b.Pin, err)
	}
}

// host_set moves a remote to a new address: saved, reconnected at once;
// a bare host gets the default port; unknown names are refused.
func TestHostSet(t *testing.T) {
	p := newRemotePair(t)
	pantryState := func() protocol.Host {
		for _, h := range call[protocol.HostsResult](t, p.tui, protocol.MethodHosts, nil).Hosts {
			if h.Name == "pantry" {
				return h
			}
		}
		t.Fatal("no pantry")
		return protocol.Host{}
	}
	waitState := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for pantryState().State != want {
			if time.Now().After(deadline) {
				t.Fatalf("pantry never %s: %+v", want, pantryState())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitState(protocol.HostOK)

	// Somewhere nothing listens: saved, and pantry stops answering.
	r := call[protocol.HostResult](t, p.tui, protocol.MethodHostSet, protocol.HostSetParams{Name: "pantry", Address: "127.0.0.1:1"})
	if r.Host.Address != "127.0.0.1:1" {
		t.Errorf("reply %+v", r.Host)
	}
	waitState(protocol.HostUnreachable)
	if peers, _ := p.mac.store.Peers(); peers[0].Address != "127.0.0.1:1" {
		t.Errorf("stored %q", peers[0].Address)
	}

	// Back to the real one: reconnects without a restart.
	call[protocol.HostResult](t, p.tui, protocol.MethodHostSet, protocol.HostSetParams{Name: "pantry", Address: p.pantryAddr})
	waitState(protocol.HostOK)

	// A bare host gets the default port.
	r = call[protocol.HostResult](t, p.tui, protocol.MethodHostSet, protocol.HostSetParams{Name: "pantry", Address: "pantry.local"})
	if r.Host.Address != "pantry.local:7457" {
		t.Errorf("bare host became %q", r.Host.Address)
	}
	if code := callErr(p.tui, protocol.MethodHostSet, protocol.HostSetParams{Name: "nope", Address: "x"}); code != protocol.ErrNotFound {
		t.Errorf("unknown host: %s", code)
	}
}
