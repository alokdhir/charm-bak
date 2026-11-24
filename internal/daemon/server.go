package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"charm-bak/internal/protocol"
)

// maxLine is the longest request line accepted (docs/protocol.md).
const maxLine = 16 << 20

// ErrAlreadyServing means another daemon answers on the socket.
var ErrAlreadyServing = errors.New("another charm-bak daemon is already serving this socket")

// Server serves a Core on a Unix socket: one JSON object per line. Each
// request runs in its own goroutine and replies may come back out of
// order; a client that closes its writing side still gets every reply.
type Server struct {
	core *Core
	path string
	ln   net.Listener
	wg   sync.WaitGroup
	// A remote's TLS listener (ListenRemote): each accepted connection
	// must finish its handshake within handshakeTimeout, at most
	// maxHandshakes at once, before it's served.
	tls        *tls.Config
	handshakes chan struct{}
}

// A connection that never handshakes would otherwise hold a goroutine and
// a descriptor forever, and enough of them stop the daemon accepting its
// controller (or opening its own database and restic's pipes).
const (
	handshakeTimeout = 10 * time.Second
	maxHandshakes    = 64
)

// Listen binds the socket at path, owner-only. A leftover socket from a
// crashed run is removed; a live one is ErrAlreadyServing.
func Listen(core *Core, path string) (*Server, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	os.Chmod(dir, 0o700) // owner-only even if an older build made it wider
	if _, err := os.Stat(path); err == nil {
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			conn.Close()
			return nil, ErrAlreadyServing
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{core: core, path: path, ln: ln}, nil
}

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	retries := 0
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Out of descriptors, or the peer hung up first: wait and
			// accept again rather than stop listening for good.
			var ne net.Error
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ECONNABORTED) ||
				(errors.As(err, &ne) && ne.Timeout()) {
				if retries++; retries == 1 {
					logf("accepting connections: %v; trying again", err)
				}
				time.Sleep(min(time.Duration(retries)*50*time.Millisecond, time.Second))
				continue
			}
			return err
		}
		retries = 0
		if s.tls != nil {
			select {
			case s.handshakes <- struct{}{}:
			default:
				conn.Close() // too many waiting to handshake: a flood
				continue
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if s.tls != nil {
				tc := tls.Server(conn, s.tls)
				ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
				err := tc.HandshakeContext(ctx)
				cancel()
				<-s.handshakes
				if err != nil {
					conn.Close()
					return
				}
				conn = tc
			}
			s.serveConn(conn)
		}()
	}
}

// Close stops accepting and removes the socket. Open connections end when
// their clients go.
func (s *Server) Close() error {
	err := s.ln.Close()
	if s.path != "" {
		os.Remove(s.path)
	}
	return err
}

func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	var wmu sync.Mutex
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		_, err = conn.Write(append(b, '\n'))
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var pending sync.WaitGroup
	var unsubscribe func()
	subscribed := make(chan struct{})

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var req protocol.Request
		if err := json.Unmarshal(line, &req); err != nil {
			write(protocol.Reply{Error: perr(protocol.ErrBadRequest, "Couldn't read the request: %v", err)})
			continue
		}
		if req.Method == protocol.MethodSubscribe {
			// The reply goes first, so a subscriber reads it before any
			// event.
			write(protocol.Reply{ID: req.ID, Result: json.RawMessage("{}")})
			if unsubscribe == nil {
				events, unsub := s.core.Subscribe()
				unsubscribe = unsub
				go func() {
					for e := range events {
						if write(e) != nil {
							cancel()
							return
						}
					}
				}()
				close(subscribed)
			}
			continue
		}
		pending.Add(1)
		go func(req protocol.Request) {
			defer pending.Done()
			res, err := s.core.Handle(ctx, req)
			rep := protocol.Reply{ID: req.ID}
			if err != nil {
				rep.Error = s.core.protocolError(err)
			} else if rep.Result, err = json.Marshal(res); err != nil {
				rep.Error = perr(protocol.ErrInternal, "encoding the reply: %v", err)
				rep.Result = nil
			}
			write(rep)
		}(req)
	}
	// The client stopped sending (or went away): answer what's pending.
	pending.Wait()
	if unsubscribe != nil {
		// A subscriber that half-closed still gets events until a write
		// fails, i.e. until it really goes away.
		<-ctx.Done()
		unsubscribe()
	}
}

// DefaultSocketPath is where the daemon listens on this machine.
func DefaultSocketPath() string { return protocol.SocketPath() }

// DefaultStorePath is the daemon's database, beside the socket.
func DefaultStorePath() string {
	return filepath.Join(StateDir(), "charm-bak.db")
}

// StateDir holds the daemon's database and identity key: the Mac app's
// support folder; on Linux /var/lib/charm-bak for the root service, else
// ~/.local/state/charm-bak.
func StateDir() string {
	if runtime.GOOS != "linux" {
		return filepath.Dir(protocol.SocketPath())
	}
	if os.Geteuid() == 0 {
		return "/var/lib/charm-bak"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "charm-bak")
}

// Run is the daemon's main loop: serve, tick the scheduler every minute,
// and shut down cleanly when ctx ends.
func Run(ctx context.Context, core *Core, socket string) error {
	srv, err := Listen(core, socket)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()
	core.Tick()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			srv.Close()
			core.Shutdown()
			return nil
		case err := <-errc:
			core.Shutdown()
			return fmt.Errorf("socket server: %w", err)
		case <-ticker.C:
			core.Tick()
		}
	}
}
