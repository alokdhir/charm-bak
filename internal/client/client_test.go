package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm-bak/internal/backup"
	"charm-bak/internal/protocol"
)

var _ backup.Repo = (*Client)(nil)

// fakeApp answers the protocol on a Unix socket from a handler.
type fakeApp struct {
	ln   net.Listener
	path string
	mu   sync.Mutex
	got  []string // methods seen
}

func startFake(t *testing.T, path string, handle func(protocol.Request) (any, *protocol.Error)) *fakeApp {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeApp{ln: ln, path: path}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				var wmu sync.Mutex
				for sc.Scan() {
					var req protocol.Request
					if json.Unmarshal(sc.Bytes(), &req) != nil {
						continue
					}
					f.mu.Lock()
					f.got = append(f.got, req.Method)
					f.mu.Unlock()
					go func(req protocol.Request) { // answer out of order, as the app may
						res, perr := handle(req)
						rep := protocol.Reply{ID: req.ID, Error: perr}
						if perr == nil {
							rep.Result, _ = json.Marshal(res)
						}
						b, _ := json.Marshal(rep)
						wmu.Lock()
						conn.Write(append(b, '\n'))
						wmu.Unlock()
					}(req)
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func sockPath(t *testing.T) string {
	// Unix socket paths are short (104 bytes on macOS); t.TempDir can be long.
	dir, err := os.MkdirTemp("/tmp", "cbc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func handler(req protocol.Request) (any, *protocol.Error) {
	switch req.Method {
	case protocol.MethodHello:
		var p protocol.HelloParams
		json.Unmarshal(req.Params, &p)
		if p.Protocol != protocol.Version {
			return nil, &protocol.Error{Code: protocol.ErrUnsupportedProtocol, Message: "old"}
		}
		return protocol.HelloResult{Protocol: protocol.Version, App: "fake 1"}, nil
	case protocol.MethodStatus:
		time.Sleep(20 * time.Millisecond) // slower than ls, to cross replies
		return protocol.StatusResult{State: protocol.StateIdle}, nil
	case protocol.MethodLs:
		var p protocol.LsParams
		json.Unmarshal(req.Params, &p)
		if p.Path == "/missing" {
			return nil, &protocol.Error{Code: protocol.ErrNotFound, Message: "not there"}
		}
		if p.Offset == 0 {
			next := 1
			return protocol.LsResult{Entries: []protocol.Entry{{Name: "a"}}, Total: 2, NextOffset: &next}, nil
		}
		return protocol.LsResult{Entries: []protocol.Entry{{Name: "b"}}, Total: 2}, nil
	}
	return nil, &protocol.Error{Code: protocol.ErrUnknownMethod, Message: req.Method}
}

func TestClientCallsPagesAndErrors(t *testing.T) {
	path := sockPath(t)
	startFake(t, path, handler)
	ctx := context.Background()
	c, err := Dial(ctx, path, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.AppVersion() != "fake 1" {
		t.Errorf("app version %q", c.AppVersion())
	}

	// Concurrent calls on one connection, replies out of order.
	var wg sync.WaitGroup
	var st protocol.StatusResult
	var entries []protocol.Entry
	var stErr, lsErr error
	wg.Add(2)
	go func() { defer wg.Done(); st, stErr = c.Status(ctx) }()
	go func() { defer wg.Done(); entries, lsErr = c.List(ctx, "s", "/x") }()
	wg.Wait()
	if stErr != nil || st.State != protocol.StateIdle {
		t.Errorf("status %+v, %v", st, stErr)
	}
	if lsErr != nil || len(entries) != 2 || entries[1].Name != "b" {
		t.Errorf("both pages: %+v, %v", entries, lsErr)
	}

	_, err = c.List(ctx, "s", "/missing")
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrNotFound {
		t.Errorf("want not_found, got %v", err)
	}
}

func TestClientNotRunningAndReconnect(t *testing.T) {
	path := sockPath(t)
	if _, err := Dial(context.Background(), path, "test"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("nothing listening: %v", err)
	}

	f := startFake(t, path, handler)
	c, err := Dial(context.Background(), path, "test")
	if err != nil {
		t.Fatal(err)
	}
	// The app restarts: the next call fails or redials, the one after works.
	f.ln.Close()
	c.Close()
	os.Remove(path)
	startFake(t, path, handler)
	if _, err := c.Status(context.Background()); err != nil {
		t.Errorf("after the app restarted: %v", err)
	}
}

func TestClientRejectsOtherProtocol(t *testing.T) {
	path := sockPath(t)
	startFake(t, path, func(req protocol.Request) (any, *protocol.Error) {
		if req.Method == protocol.MethodHello {
			return nil, &protocol.Error{Code: protocol.ErrUnsupportedProtocol, Message: "This app speaks protocol 2."}
		}
		return nil, nil
	})
	_, err := Dial(context.Background(), path, "test")
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrUnsupportedProtocol {
		t.Errorf("want unsupported_protocol, got %v", err)
	}
}

func TestSubscribeDeliversEvents(t *testing.T) {
	path := sockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte(`{"id":1,"result":{}}` + "\n"))
		conn.Write([]byte(`{"event":"state_changed","state":"paused"}` + "\n"))
		conn.Write([]byte(`{"event":"progress","job_id":"j-1","percent":0.5,"files_done":1,"total_files":2,"bytes_done":1,"total_bytes":2,"current":""}` + "\n"))
		time.Sleep(100 * time.Millisecond)
		conn.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := Subscribe(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for e := range ch {
		names = append(names, e.Name)
		if e.Name == protocol.EventProgress && e.Progress.Percent != 0.5 {
			t.Errorf("progress %+v", e.Progress)
		}
	}
	if len(names) != 2 || names[0] != "state_changed" || names[1] != "progress" {
		t.Errorf("events %v", names)
	}
}
