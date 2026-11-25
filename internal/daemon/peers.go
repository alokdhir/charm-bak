package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm-bak/internal/client"
	"charm-bak/internal/protocol"
)

// Paired daemons (docs/design.md, "Revision 2026-11-19"). Every daemon
// has a long-lived identity key; paired daemons pin each other's public
// key and talk the same protocol over mutual TLS. A controller (the Mac)
// relays requests that name a remote's host and passes the remote's
// events through, tagged with its host. A remote accepts connections only
// from the controllers it has pinned.

// RemotePort is where a remote daemon listens for its controller.
const RemotePort = 7457

// Identity is this daemon's key and the self-signed certificate it
// presents. Peers check the key's pin, never the certificate's chain.
type Identity struct {
	Cert tls.Certificate
	Pin  string // "sha256//<base64>", curl's --pinnedpubkey form
}

// LoadIdentity reads the identity key at path, creating it (owner-only)
// the first time.
func LoadIdentity(path, name string) (*Identity, error) {
	// ECDSA P-256 rather than Ed25519: macOS's curl (and older ones on
	// remotes) can't complete a TLS handshake with an Ed25519 certificate.
	var key *ecdsa.PrivateKey
	if b, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("%s: no PEM key", path)
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok {
			// An Ed25519 key from the first build (2026-11-20), before any
			// pairing could have pinned it: set it aside and start over.
			if err := os.Rename(path, path+".old"); err != nil {
				return nil, err
			}
			return LoadIdentity(path, name)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	// The certificate is only a carrier for the key, so it's made fresh
	// on every start and never expires in practice.
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "charm-bak " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Identity{Cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, Pin: pinOf(leaf)}, nil
}

// pinOf is a certificate's public key pin.
func pinOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

// peerPin is the pin of what the other side presented.
func peerPin(rawCerts [][]byte) (string, error) {
	if len(rawCerts) == 0 {
		return "", errors.New("no certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "", err
	}
	return pinOf(cert), nil
}

// DefaultIdentityPath is the identity key, beside the database.
func DefaultIdentityPath() string {
	return filepath.Join(StateDir(), "identity.pem")
}

// Peer is a paired daemon. A remote has an address (the controller dials
// it); a controller has none (it dials in).
type Peer struct {
	Name    string
	Address string
	Pin     string
	Added   time.Time
}

func (s *Store) Peers() ([]Peer, error) {
	rows, err := s.db.Query("SELECT name, address, pin, added FROM peers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		var p Peer
		var added float64
		if err := rows.Scan(&p.Name, &p.Address, &p.Pin, &added); err != nil {
			return nil, err
		}
		p.Added = fromDB(added)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddPeer records (or replaces) a paired daemon.
func (s *Store) AddPeer(p Peer) error {
	_, err := s.db.Exec("INSERT INTO peers (name, address, pin, added) VALUES (?, ?, ?, ?) ON CONFLICT(name) DO UPDATE SET address = excluded.address, pin = excluded.pin",
		p.Name, p.Address, p.Pin, toDB(p.Added))
	return err
}

func (s *Store) RemovePeer(name string) error {
	if _, err := s.db.Exec("DELETE FROM peers WHERE name = ?", name); err != nil {
		return err
	}
	// What was saved about it goes with the pairing.
	_, err := s.db.Exec("DELETE FROM remote_cache WHERE host = ?", name)
	return err
}

// peerWithPin is the peer whose key has pin.
func (s *Store) peerWithPin(pin string) (Peer, bool, error) {
	var p Peer
	var added float64
	err := s.db.QueryRow("SELECT name, address, pin, added FROM peers WHERE pin = ?", pin).Scan(&p.Name, &p.Address, &p.Pin, &added)
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, false, nil
	}
	p.Added = fromDB(added)
	return p, err == nil, err
}

// ListenRemote serves core over mutual TLS on addr, to the peers it has
// pinned. Requests come in exactly as on the Unix socket.
func ListenRemote(core *Core, addr string, id *Identity) (*Server, error) {
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// No resumption: a resumed session skips the certificate checks,
		// so a controller removed since could come back on a saved
		// ticket. Every connection is a full handshake against the
		// peers table as it is now.
		SessionTicketsDisabled: true,
		// VerifyConnection, not VerifyPeerCertificate: it runs on every
		// handshake, resumed or not.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			pin := pinOf(cs.PeerCertificates[0])
			if _, ok, err := core.store.peerWithPin(pin); err != nil || !ok {
				logf("refused a connection from an unpaired key %s", pin)
				return fmt.Errorf("not a paired daemon (%s)", pin)
			}
			return nil
		},
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{core: core, ln: ln, tls: cfg, handshakes: make(chan struct{}, maxHandshakes)}, nil
}

// remoteDialer dials a remote's TLS port, presenting id and accepting
// only the remote's pinned key.
func remoteDialer(p Peer, id *Identity) client.DialFunc {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // the pin below is the check: self-signed keys have no chain
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			pin, err := peerPin(raw)
			if err != nil {
				return err
			}
			if pin != p.Pin {
				return fmt.Errorf("%s presented an unexpected key (%s, want %s)", p.Name, pin, p.Pin)
			}
			return nil
		},
	}
	return func(ctx context.Context) (net.Conn, error) {
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: cfg}
		return d.DialContext(ctx, "tcp", p.Address)
	}
}

// remote is a paired remote as the controller sees it.
// remoteRetryMax is the longest wait between tries of a remote that
// isn't answering.
const remoteRetryMax = 5 * time.Second

type remote struct {
	peer     Peer
	client   *client.Client // nil until the first request
	state    string
	err      string
	lastSeen *time.Time
	os       string             // GOOS from its last hello; "" before one
	cancel   context.CancelFunc // stops following it
	// What it's running, from its status and events, while connected:
	// reads of its repository wait out a cleanup (remoteread.go).
	job *protocol.Job
	// syncRemote: one at a time, another after if asked meanwhile.
	syncing, syncMore, syncFull bool
	// Self-update (update.go): one at a time, each build sent once.
	updating    bool
	sentBuild   string
	failedBuild string // the build updateFails counts for
	updateFails int
}

// host describes r for the protocol. Call with remotes.mu held.
func (r *remote) host() protocol.Host {
	return protocol.Host{Name: r.peer.Name, Address: r.peer.Address, State: r.state, Error: r.err, LastSeen: r.lastSeen}
}

// remotes are the controller's paired remotes, with a connection for
// requests and a subscription for events each.
type remotes struct {
	mu   sync.Mutex
	ctx  context.Context // the daemon's lifetime, from StartRemotes
	byID map[string]*remote
}

// StartRemotes connects to every paired remote that has an address:
// events are passed through until ctx ends, reconnecting with backoff.
func (c *Core) StartRemotes(ctx context.Context, id *Identity) error {
	peers, err := c.store.Peers()
	if err != nil {
		return err
	}
	c.identity = id
	c.remotes.mu.Lock()
	c.remotes.ctx = ctx
	c.remotes.mu.Unlock()
	for _, p := range peers {
		if p.Address != "" { // else a controller of ours, not a remote
			c.followNew(p)
		}
	}
	return nil
}

// followNew starts relaying to a remote: at startup, and when pairing
// adds one.
func (c *Core) followNew(p Peer) {
	c.remotes.mu.Lock()
	defer c.remotes.mu.Unlock()
	if c.remotes.byID == nil {
		c.remotes.byID = map[string]*remote{}
	}
	if old := c.remotes.byID[p.Name]; old != nil && old.client != nil {
		old.client.Close()
	}
	ctx, cancel := context.WithCancel(c.remotes.ctx)
	if old := c.remotes.byID[p.Name]; old != nil && old.cancel != nil {
		old.cancel()
	}
	r := &remote{peer: p, state: protocol.HostUnreachable, err: "connecting", cancel: cancel}
	c.remotes.byID[p.Name] = r
	go c.followRemote(ctx, r)
}

// followRemote passes a remote's events through until ctx ends.
func (c *Core) followRemote(ctx context.Context, r *remote) {
	backoff := time.Second
	for ctx.Err() == nil {
		events, err := client.SubscribeWith(ctx, remoteDialer(r.peer, c.identity))
		if err != nil {
			c.markRemote(r, err)
		} else {
			c.markRemote(r, nil)
			backoff = time.Second
			go c.updateRemote(ctx, r)
			go c.syncRemote(ctx, r, true)
			for e := range events {
				if e.Name == protocol.EventJobFinished {
					// It may have refused an update while that job ran.
					go c.updateRemote(ctx, r)
				}
				c.remoteEvent(ctx, r, e)
				e.Host = r.peer.Name
				if e.SourceChanged != nil {
					e.SourceChanged.Source.Host = r.peer.Name
				}
				c.mu.Lock()
				c.publish(e)
				c.mu.Unlock()
				c.markRemote(r, nil)
			}
			c.remotes.mu.Lock()
			r.job = nil // unknown from here on
			c.remotes.mu.Unlock()
			c.markRemote(r, errors.New("connection closed"))
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		// Capped low: the remote can't announce itself (it doesn't know
		// where its controller is, and the Mac doesn't listen), so how
		// soon a machine that's back shows up is this wait. A try costs
		// one connection attempt (Alok, 2026-11-24: a minute felt slow).
		backoff = min(backoff*2, remoteRetryMax)
	}
}

// markRemote records whether r answered, and tells subscribers when that
// changes (host_changed).
func (c *Core) markRemote(r *remote, err error) {
	c.remotes.mu.Lock()
	was := r.state
	if err == nil {
		now := c.now()
		r.state, r.err, r.lastSeen = protocol.HostOK, "", &now
	} else {
		r.state, r.err = protocol.HostUnreachable, err.Error()
	}
	h := r.host()
	c.remotes.mu.Unlock()
	if was != h.State {
		if h.State == protocol.HostOK {
			logf("remote %s answering (%s)", r.peer.Name, r.peer.Address)
		} else {
			logf("remote %s not answering: %s", r.peer.Name, h.Error)
		}
		c.mu.Lock()
		c.publish(protocol.Event{Name: protocol.EventHostChanged, Host: r.peer.Name, HostChanged: &protocol.HostChangedEvent{Host: h}})
		c.mu.Unlock()
	}
}

// relay sends req to the remote it names and returns the remote's raw
// result. The remote sees a plain request: no host.
func (c *Core) relay(ctx context.Context, req protocol.Request) (json.RawMessage, error) {
	cl, r, err := c.remoteClient(ctx, req.Host)
	if err != nil {
		return nil, err
	}
	req.Host = ""
	out, err := cl.Forward(ctx, req)
	var pe *protocol.Error
	if err != nil && !errors.As(err, &pe) {
		// Nobody answered: the connection failed or dropped (a dropped
		// one is a transport error from the client, never a reply). A
		// caller that stopped waiting says nothing about the remote.
		if ctx.Err() == nil {
			c.markRemote(r, err)
		}
		return nil, perr(protocol.ErrHostUnreachable, "%s isn't answering: %v", r.peer.Name, err)
	}
	if err == nil {
		c.markRemote(r, nil)
		out = sourcesOn(r.peer.Name, req.Method, out)
		c.remoteWritten(r, req.Method)
	}
	return out, err
}

// remoteClient is the connection to a paired remote, dialed on first use;
// the client redials by itself after drops.
func (c *Core) remoteClient(ctx context.Context, host string) (*client.Client, *remote, error) {
	c.remotes.mu.Lock()
	r := c.remotes.byID[host]
	var cl *client.Client
	if r != nil {
		cl = r.client
	}
	c.remotes.mu.Unlock()
	if r == nil {
		return nil, nil, perr(protocol.ErrNotFound, "No paired host named %s.", host)
	}
	if cl != nil {
		return cl, r, nil
	}
	cl, err := client.DialWith(ctx, remoteDialer(r.peer, c.identity), c.Version)
	if err != nil {
		c.markRemote(r, err)
		return nil, nil, perr(protocol.ErrHostUnreachable, "%s isn't answering: %v", host, err)
	}
	c.remotes.mu.Lock()
	if r.client == nil {
		r.client = cl
	} else { // another request connected meanwhile
		cl.Close()
		cl = r.client
	}
	r.os = cl.Hello().OS
	c.remotes.mu.Unlock()
	return cl, r, nil
}

// sourcesOn names host as the host of the sources in a relayed reply, in
// case the remote calls itself something else than its pairing name.
func sourcesOn(host, method string, raw json.RawMessage) json.RawMessage {
	switch method {
	case protocol.MethodSources:
		var r protocol.SourcesResult
		if json.Unmarshal(raw, &r) != nil {
			return raw
		}
		for i := range r.Sources {
			r.Sources[i].Host = host
		}
		b, _ := json.Marshal(r)
		return b
	case protocol.MethodSourceAdd, protocol.MethodSourceSet:
		var r protocol.SourceResult
		if json.Unmarshal(raw, &r) != nil {
			return raw
		}
		r.Source.Host = host
		b, _ := json.Marshal(r)
		return b
	}
	return raw
}

// hosts is this daemon and its remotes, for the hosts method.
func (c *Core) hosts() protocol.HostsResult {
	res := protocol.HostsResult{Hosts: []protocol.Host{{Name: c.Name, Local: true, State: protocol.HostOK}}}
	c.remotes.mu.Lock()
	defer c.remotes.mu.Unlock()
	for _, r := range c.remotes.byID {
		res.Hosts = append(res.Hosts, r.host())
	}
	slices.SortFunc(res.Hosts[1:], func(a, b protocol.Host) int { return strings.Compare(a.Name, b.Name) })
	// When charm-bak made each machine's AWS key, for the TUI to show its
	// age (and that it's rotated).
	made := c.madeKeys()
	for i := range res.Hosts {
		if k, ok := made[res.Hosts[i].Name]; ok {
			t := k.Made
			res.Hosts[i].KeyMade = &t
		}
	}
	return res
}

// HostName is this machine's short name: "studio", not "studio.local".
// It's kept to the letters, digits and hyphens a paired name may have
// (ValidName): the name goes into the install scripts a remote runs, and
// a host name can come from the network (DHCP, reverse DNS).
func HostName() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	h = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, h)
	h = strings.Trim(h, "-")
	if len(h) > 31 {
		h = strings.TrimRight(h[:31], "-")
	}
	if !ValidName(h) {
		return "mac"
	}
	return h
}

// ValidName reports whether name can name a machine: lowercase letters,
// digits and hyphens, up to 31, starting with a letter or digit.
func ValidName(name string) bool { return hostNameRE.MatchString(name) }

// setHostAddress handles host_set: a remote's new address, saved and
// reconnected to at once. The pin is unchanged, so a different machine
// at that address is still refused.
func (c *Core) setHostAddress(p protocol.HostSetParams) (protocol.HostResult, error) {
	addr := strings.TrimSpace(p.Address)
	if addr == "" || strings.ContainsAny(addr, " /") {
		return protocol.HostResult{}, perr(protocol.ErrBadRequest, "An address is a host name or IP, with :port if not %d.", RemotePort)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(RemotePort))
	}
	peers, err := c.store.Peers()
	if err != nil {
		return protocol.HostResult{}, err
	}
	i := slices.IndexFunc(peers, func(x Peer) bool { return x.Name == p.Name && x.Address != "" })
	if i < 0 {
		return protocol.HostResult{}, perr(protocol.ErrNotFound, "No paired remote named %s.", p.Name)
	}
	peer := peers[i]
	old := peer.Address
	peer.Address = addr
	if err := c.store.AddPeer(peer); err != nil {
		return protocol.HostResult{}, err
	}
	logf("remote %s: address %s -> %s", p.Name, old, addr)
	c.remotes.mu.Lock()
	following := c.remotes.ctx != nil
	c.remotes.mu.Unlock()
	if following {
		c.followNew(peer)
	}
	c.remotes.mu.Lock()
	var h protocol.Host
	if r := c.remotes.byID[p.Name]; r != nil {
		h = r.host()
	} else {
		h = protocol.Host{Name: p.Name, Address: addr, State: protocol.HostUnreachable}
	}
	c.remotes.mu.Unlock()
	c.mu.Lock()
	c.publish(protocol.Event{Name: protocol.EventHostChanged, Host: p.Name, HostChanged: &protocol.HostChangedEvent{Host: h}})
	c.mu.Unlock()
	return protocol.HostResult{Host: h}, nil
}
