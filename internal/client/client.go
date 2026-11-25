// Package client talks to the menu bar app over its Unix socket
// (docs/protocol.md) and implements backup.Repo. One connection carries
// every request; a reader goroutine routes replies to callers by id, so
// concurrent loads from the TUI share it. If the app goes away, the next
// call redials.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

// Name identifies this client in hello.
const Name = "charm-bak-tui"

// ErrNotRunning means nothing answers on the socket: the app isn't running.
var ErrNotRunning = errors.New("the charm-bak app isn't running")

// DialFunc opens a connection to a daemon: the local Unix socket, or TLS
// to a paired remote.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Client is a view of a daemon connection: requests go to its host (""
// for the daemon itself, else a remote that daemon relays to). Views made
// with WithHost share one connection. Safe for concurrent use.
type Client struct {
	*transport
	host string
}

// transport is the connection every view of a Client shares.
type transport struct {
	dial    DialFunc
	version string
	// anyVersion: say hello in whatever protocol version the other side
	// speaks instead of failing (DialAnyVersion).
	anyVersion bool

	dialMu sync.Mutex // one reconnect at a time

	wmu sync.Mutex // one write at a time; never held with mu

	mu      sync.Mutex
	conn    net.Conn
	nextID  int64
	waiting map[int64]waiter
	app     string // the app's version, from hello
	hello   protocol.HelloResult
}

// waiter is a call waiting for its reply, and the connection it was sent
// on: when that connection ends, only its own calls fail.
type waiter struct {
	ch   chan answer
	conn net.Conn
}

// answer is a reply, or err when the connection ended first (a transport
// error, never a *protocol.Error: nobody answered).
type answer struct {
	reply protocol.Reply
	err   error
}

// Dial connects to the app at path (protocol.SocketPath() normally) and
// checks the protocol version.
func Dial(ctx context.Context, path, version string) (*Client, error) {
	return DialWith(ctx, UnixDialer(path), version)
}

// DialWith connects with dial and checks the protocol version.
func DialWith(ctx context.Context, dial DialFunc, version string) (*Client, error) {
	c := &Client{transport: &transport{dial: dial, version: version, waiting: map[int64]waiter{}}}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// DialAnyVersion is DialWith for the one caller that must reach a daemon
// speaking another protocol version: the controller sending it a newer
// daemon (self_update). Hello then reports the other side's version, and
// only requests that version understands should follow.
func DialAnyVersion(ctx context.Context, dial DialFunc, version string) (*Client, error) {
	c := &Client{transport: &transport{dial: dial, version: version, anyVersion: true, waiting: map[int64]waiter{}}}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// UnixDialer dials the daemon's socket at path.
func UnixDialer(path string) DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return nil, fmt.Errorf("%w (%v)", ErrNotRunning, err)
		}
		return conn, nil
	}
}

// WithHost is a view of the same connection whose requests go to host,
// a remote the daemon relays to ("" for the daemon itself).
func (c *Client) WithHost(host string) *Client { return &Client{transport: c.transport, host: host} }

// Host is the host this view's requests go to.
func (c *Client) Host() string { return c.host }

// AppVersion is the version the app reported.
func (c *transport) AppVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.app
}

// Hello is the other side's hello reply on the current connection: its
// version, build hash and platform.
func (c *transport) Hello() protocol.HelloResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hello
}

// Close drops the connection.
func (c *transport) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// connect dials and says hello. Called with c.mu not held.
func (c *transport) connect(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	go c.read(conn)

	var hello protocol.HelloResult
	err = c.call(ctx, "", protocol.MethodHello, protocol.HelloParams{Protocol: protocol.Version, Client: Name + " " + c.version}, &hello)
	var pe *protocol.Error
	if c.anyVersion && errors.As(err, &pe) && pe.Code == protocol.ErrUnsupportedProtocol {
		// Its reply names the version it does speak: say hello in that.
		var theirs protocol.UnsupportedProtocolData
		if json.Unmarshal(pe.Data, &theirs) == nil && theirs.Protocol > 0 {
			err = c.call(ctx, "", protocol.MethodHello, protocol.HelloParams{Protocol: theirs.Protocol, Client: Name + " " + c.version}, &hello)
		}
	}
	if err != nil {
		c.Close()
		return err
	}
	c.mu.Lock()
	c.app = hello.App
	c.hello = hello
	c.mu.Unlock()
	return nil
}

// read routes replies until the connection ends, then fails every call
// waiting on it so nothing hangs.
func (c *transport) read(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r protocol.Reply
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		c.mu.Lock()
		w, ok := c.waiting[r.ID]
		if ok && w.conn == conn {
			delete(c.waiting, r.ID)
		}
		c.mu.Unlock()
		if ok && w.conn == conn {
			w.ch <- answer{reply: r}
		}
	}
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	// Only this connection's calls: a newer connection's are still live.
	for id, w := range c.waiting {
		if w.conn == conn {
			w.ch <- answer{err: fmt.Errorf("%w (the connection closed before it answered)", ErrNotRunning)}
			delete(c.waiting, id)
		}
	}
	c.mu.Unlock()
	conn.Close()
}

// Call sends one request and decodes its result into out (which may be
// nil). Protocol errors come back as *protocol.Error.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if err := c.ensure(ctx); err != nil {
		return err
	}
	return c.call(ctx, c.host, method, params, out)
}

// Forward sends req as is (its host included) and returns the raw
// result: how a daemon relays a request to a remote.
func (c *Client) Forward(ctx context.Context, req protocol.Request) (json.RawMessage, error) {
	if err := c.ensure(ctx); err != nil {
		return nil, err
	}
	var out json.RawMessage
	err := c.send(ctx, req, &out)
	return out, err
}

// ensure redials if the connection is gone, once even when several calls
// notice at the same moment.
func (c *transport) ensure(ctx context.Context) error {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	c.mu.Lock()
	connected := c.conn != nil
	c.mu.Unlock()
	if connected {
		return nil
	}
	return c.connect(ctx)
}

func (c *transport) call(ctx context.Context, host, method string, params, out any) error {
	req := protocol.Request{Method: method, Host: host}
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		req.Params = b
	}
	return c.send(ctx, req, out)
}

// send writes req (with a fresh id) and waits for its reply. A connection
// that ends first is ErrNotRunning (wrapped), not a protocol error.
func (c *transport) send(ctx context.Context, req protocol.Request, out any) error {
	ch := make(chan answer, 1)
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return ErrNotRunning
	}
	c.nextID++
	req.ID = c.nextID
	conn := c.conn
	c.waiting[req.ID] = waiter{ch: ch, conn: conn}
	c.mu.Unlock()
	line, err := json.Marshal(req)
	if err == nil {
		err = c.write(ctx, conn, append(line, '\n'))
	}
	if err != nil {
		c.mu.Lock()
		delete(c.waiting, req.ID)
		c.mu.Unlock()
		return fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}

	select {
	case a := <-ch:
		if a.err != nil {
			return a.err
		}
		if a.reply.Error != nil {
			return a.reply.Error
		}
		if out != nil && a.reply.Result != nil {
			return json.Unmarshal(a.reply.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiting, req.ID)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// write sends one line, one writer at a time, giving up at ctx's deadline
// (requestTimeout without one) so a peer that stopped reading can't hold
// every other call. A failed write may have sent part of a line, so the
// connection is closed: the next call redials.
func (c *transport) write(ctx context.Context, conn net.Conn, line []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(requestTimeout)
	}
	conn.SetWriteDeadline(deadline)
	_, err := conn.Write(line)
	if err != nil {
		conn.Close() // the reader sees it and clears c.conn
		return err
	}
	conn.SetWriteDeadline(time.Time{})
	return nil
}

// requestTimeout bounds calls made without a deadline: listings over S3
// can take a while, but never this long.
const requestTimeout = 2 * time.Minute

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, requestTimeout)
}

// The backup.Repo methods.

func (c *Client) Hosts(ctx context.Context) ([]protocol.Host, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.HostsResult
	err := c.Call(ctx, protocol.MethodHosts, nil, &r)
	return r.Hosts, err
}

func (c *Client) ForHost(host string) backup.Repo { return c.WithHost(host) }

func (c *Client) Status(ctx context.Context) (protocol.StatusResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.StatusResult
	err := c.Call(ctx, protocol.MethodStatus, nil, &r)
	return r, err
}

func (c *Client) Sources(ctx context.Context) ([]protocol.Source, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.SourcesResult
	err := c.Call(ctx, protocol.MethodSources, nil, &r)
	return r.Sources, err
}

func (c *Client) Snapshots(ctx context.Context, source string) ([]protocol.Snapshot, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.SnapshotsResult
	err := c.Call(ctx, protocol.MethodSnapshots, protocol.SnapshotsParams{Source: source}, &r)
	return r.Snapshots, err
}

// List fetches every page of a directory.
func (c *Client) List(ctx context.Context, snapshot, dir string) ([]protocol.Entry, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var all []protocol.Entry
	offset := 0
	for {
		var r protocol.LsResult
		err := c.Call(ctx, protocol.MethodLs, protocol.LsParams{Snapshot: snapshot, Path: dir, Offset: offset, Limit: 1000}, &r)
		if err != nil {
			return nil, err
		}
		all = append(all, r.Entries...)
		if r.NextOffset == nil {
			return all, nil
		}
		offset = *r.NextOffset
	}
}

func (c *Client) Restore(ctx context.Context, p protocol.RestoreParams) (protocol.Job, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.JobResult
	err := c.Call(ctx, protocol.MethodRestore, p, &r)
	return r.Job, err
}

func (c *Client) SetSnapshot(ctx context.Context, p protocol.SnapshotSetParams) (protocol.Snapshot, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.SnapshotResult
	err := c.Call(ctx, protocol.MethodSnapshotSet, p, &r)
	return r.Snapshot, err
}

func (c *Client) Repository(ctx context.Context) (protocol.RepositoryResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.RepositoryResult
	err := c.Call(ctx, protocol.MethodRepository, nil, &r)
	return r, err
}

func (c *Client) SetHostAddress(ctx context.Context, p protocol.HostSetParams) (protocol.Host, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.HostResult
	err := c.Call(ctx, protocol.MethodHostSet, p, &r)
	return r.Host, err
}

func (c *Client) ProbeRepository(ctx context.Context, p protocol.RepositoryProbeParams) (protocol.RepositoryProbeResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.RepositoryProbeResult
	err := c.Call(ctx, protocol.MethodRepoProbe, p, &r)
	return r, err
}

func (c *Client) SetupRepository(ctx context.Context, p protocol.RepositorySetupParams) (protocol.RepositorySetupResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.RepositorySetupResult
	err := c.Call(ctx, protocol.MethodRepoSetup, p, &r)
	return r, err
}

func (c *Client) PairStart(ctx context.Context, p protocol.PairStartParams) (protocol.PairStartResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.PairStartResult
	err := c.Call(ctx, protocol.MethodPairStart, p, &r)
	return r, err
}

func (c *Client) HostRepair(ctx context.Context, name string) (protocol.PairStartResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.PairStartResult
	err := c.Call(ctx, protocol.MethodHostRepair, protocol.HostRepairParams{Name: name}, &r)
	return r, err
}

func (c *Client) OpenAccessSettings(ctx context.Context) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return c.Call(ctx, protocol.MethodAccessSettings, nil, &protocol.Empty{})
}

func (c *Client) AWSStatus(ctx context.Context) (protocol.AWSStatusResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.AWSStatusResult
	err := c.Call(ctx, protocol.MethodAWSStatus, nil, &r)
	return r, err
}

// AWSKeys waits up to 15 minutes: signing in happens in a browser, at the
// person's pace (the daemon gives up after 10).
func (c *Client) AWSKeys(ctx context.Context, p protocol.AWSKeysParams) (protocol.AWSKeysResult, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
	}
	var r protocol.AWSKeysResult
	err := c.Call(ctx, protocol.MethodAWSKeys, p, &r)
	return r, err
}

// RemoveHost waits as long as deleting takes when the remote's backups
// go too: it forgets and prunes them before it answers (the daemon gives
// that an hour).
func (c *Client) RemoveHost(ctx context.Context, p protocol.HostRemoveParams) (protocol.HostRemoveResult, error) {
	if _, ok := ctx.Deadline(); !ok && p.DeleteBackups {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 70*time.Minute)
		defer cancel()
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.HostRemoveResult
	err := c.Call(ctx, protocol.MethodHostRemove, p, &r)
	return r, err
}

func (c *Client) SetDefaults(ctx context.Context, p protocol.DefaultsSetParams) (protocol.Defaults, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.DefaultsResult
	err := c.Call(ctx, protocol.MethodDefaultsSet, p, &r)
	return r.Defaults, err
}

func (c *Client) Diff(ctx context.Context, snapshot, dir string) ([]protocol.DiffEntry, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.DiffResult
	err := c.Call(ctx, protocol.MethodDiff, protocol.DiffParams{Snapshot: snapshot, Path: dir}, &r)
	return r.Entries, err
}

func (c *Client) Changes(ctx context.Context, snapshot, base string) (protocol.ChangesResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.ChangesResult
	err := c.Call(ctx, protocol.MethodChanges, protocol.ChangesParams{Snapshot: snapshot, Base: base}, &r)
	return r, err
}

func (c *Client) Read(ctx context.Context, snapshot, file string, max int) (protocol.DumpResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.DumpResult
	err := c.Call(ctx, protocol.MethodDump, protocol.DumpParams{Snapshot: snapshot, Path: file, Max: max}, &r)
	return r, err
}

// Changes.

func (c *Client) AddSource(ctx context.Context, p protocol.SourceAddParams) (protocol.Source, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.SourceResult
	err := c.Call(ctx, protocol.MethodSourceAdd, p, &r)
	return r.Source, err
}

func (c *Client) SetSource(ctx context.Context, p protocol.SourceSetParams) (protocol.Source, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.SourceResult
	err := c.Call(ctx, protocol.MethodSourceSet, p, &r)
	return r.Source, err
}

// History is the runs that ended since since, oldest first.
func (c *Client) History(ctx context.Context, since time.Time) ([]protocol.Run, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.HistoryResult
	err := c.Call(ctx, protocol.MethodHistory, protocol.HistoryParams{Since: since}, &r)
	return r.Runs, err
}

// RunDetail is one run and the files it couldn't read.
func (c *Client) RunDetail(ctx context.Context, job string) (protocol.RunResult, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.RunResult
	err := c.Call(ctx, protocol.MethodRun, protocol.RunParams{Job: job}, &r)
	return r, err
}

func (c *Client) RewriteSource(ctx context.Context, p protocol.SourceRewriteParams) (protocol.SourceRewriteResult, error) {
	if _, ok := ctx.Deadline(); !ok && p.DryRun {
		// A dry run reads every snapshot's tree: slow on a big source.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.SourceRewriteResult
	err := c.Call(ctx, protocol.MethodSourceRewrite, p, &r)
	return r, err
}

func (c *Client) RemoveSource(ctx context.Context, id string, keepSnapshots bool) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return c.Call(ctx, protocol.MethodSourceRemove, protocol.SourceRemoveParams{ID: id, KeepSnapshots: keepSnapshots}, &protocol.Empty{})
}

func (c *Client) BackupNow(ctx context.Context, source string) (protocol.Job, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var r protocol.JobResult
	err := c.Call(ctx, protocol.MethodBackupNow, protocol.BackupNowParams{Source: source}, &r)
	return r.Job, err
}

func (c *Client) Cancel(ctx context.Context, job string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return c.Call(ctx, protocol.MethodCancel, protocol.CancelParams{Job: job}, &protocol.Empty{})
}

// Events subscribes on a second connection (SubscribeWith). The daemon
// passes its remotes' events through, tagged with their host, so one
// subscription covers every host.
func (c *Client) Events(ctx context.Context) (<-chan protocol.Event, error) {
	return SubscribeWith(ctx, c.dial)
}

// Subscribe opens a second connection that receives the app's events
// until ctx ends or the app goes away; the channel closes then.
func Subscribe(ctx context.Context, path string) (<-chan protocol.Event, error) {
	return SubscribeWith(ctx, UnixDialer(path))
}

// subscribeTimeout is how long the other side has to accept a
// subscription.
const subscribeTimeout = 15 * time.Second

// SubscribeWith is Subscribe over any connection. It returns once the
// other side has accepted the subscription: a peer that takes the
// connection and then refuses it (a remote that no longer pins this
// controller only says so on the first read) is an error here, not a
// stream that ends at once.
func SubscribeWith(ctx context.Context, dial DialFunc) (<-chan protocol.Event, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	line, _ := json.Marshal(protocol.Request{ID: 1, Method: protocol.MethodSubscribe})
	deadline := time.Now().Add(subscribeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetDeadline(deadline)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	// Events may come before the reply; keep them.
	var early []protocol.Event
	for accepted := false; !accepted; {
		if !sc.Scan() {
			err := sc.Err()
			if err == nil {
				err = errors.New("the connection closed")
			}
			conn.Close()
			return nil, fmt.Errorf("%w (subscribing: %v)", ErrNotRunning, err)
		}
		if e, ok := eventLine(sc.Bytes()); ok {
			early = append(early, e)
			continue
		}
		var r protocol.Reply
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID != 1 {
			continue
		}
		if r.Error != nil {
			conn.Close()
			return nil, r.Error
		}
		accepted = true
	}
	conn.SetDeadline(time.Time{})

	out := make(chan protocol.Event, 64)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		conn.Close()
	}()
	go func() {
		defer close(out)
		defer close(done) // the stream ended: the connection goes too
		send := func(e protocol.Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, e := range early {
			if !send(e) {
				return
			}
		}
		for sc.Scan() {
			if e, ok := eventLine(sc.Bytes()); ok && !send(e) {
				return
			}
		}
	}()
	return out, nil
}

// eventLine decodes line if it's an event (not a reply, not junk).
func eventLine(line []byte) (protocol.Event, bool) {
	var head struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(line, &head) != nil || head.Event == "" {
		return protocol.Event{}, false
	}
	var e protocol.Event
	if json.Unmarshal(line, &e) != nil {
		return protocol.Event{}, false
	}
	return e, true
}
