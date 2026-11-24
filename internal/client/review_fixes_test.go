package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"charm-bak/internal/protocol"
)

// listen serves each accepted connection with serve.
func listen(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	path := sockPath(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return path
}

func reply(conn net.Conn, id int64, result any) {
	b, _ := json.Marshal(result)
	line, _ := json.Marshal(protocol.Reply{ID: id, Result: b})
	conn.Write(append(line, '\n'))
}

// A connection that drops before the reply is "nobody answered"
// (ErrNotRunning), not an answer: a *protocol.Error would tell a relay
// the other side refused.
func TestDroppedConnectionIsNotAnAnswer(t *testing.T) {
	path := listen(t, func(conn net.Conn) {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req protocol.Request
			json.Unmarshal(sc.Bytes(), &req)
			if req.Method == protocol.MethodHello {
				reply(conn, req.ID, protocol.HelloResult{Protocol: protocol.Version, App: "fake"})
				continue
			}
			conn.Close() // goes away with the request in flight
			return
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, path, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Status(ctx)
	var pe *protocol.Error
	if err == nil || errors.As(err, &pe) || !errors.Is(err, ErrNotRunning) {
		t.Errorf("a dropped connection: %#v", err)
	}
}

// A peer that takes the connection and closes it without accepting the
// subscription (a remote that no longer pins this controller) is an
// error from SubscribeWith, not a stream that ends at once.
func TestSubscribeNeedsItsReply(t *testing.T) {
	path := listen(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n')
		conn.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ch, err := Subscribe(ctx, path); err == nil || !errors.Is(err, ErrNotRunning) {
		t.Errorf("subscribed to a peer that hung up: %v %v", ch, err)
	}

	refuse := listen(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n')
		line, _ := json.Marshal(protocol.Reply{ID: 1, Error: &protocol.Error{Code: protocol.ErrBadRequest, Message: "no"}})
		conn.Write(append(line, '\n'))
	})
	var pe *protocol.Error
	if _, err := Subscribe(ctx, refuse); !errors.As(err, &pe) || pe.Code != protocol.ErrBadRequest {
		t.Errorf("a refused subscription: %v", err)
	}
}

// Events sent before the subscribe reply aren't lost, and the connection
// closes when the stream ends, not only when ctx does.
func TestSubscribeKeepsEarlyEventsAndCloses(t *testing.T) {
	var closed atomic.Bool
	path := listen(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		r.ReadString('\n')
		conn.Write([]byte(`{"event":"state_changed","state":"paused"}` + "\n"))
		conn.Write([]byte(`{"id":1,"result":{}}` + "\n"))
		conn.Write([]byte(`{"event":"state_changed","state":"idle"}` + "\n"))
		conn.(*net.UnixConn).CloseWrite() // the stream ends; reads still wait
		r.ReadString('\n')                // returns when the client closes its side
		closed.Store(true)
		conn.Close()
	})
	ch, err := Subscribe(context.Background(), path) // a ctx that never ends
	if err != nil {
		t.Fatal(err)
	}
	var states []string
	for e := range ch {
		states = append(states, e.StateChanged.State)
	}
	if len(states) != 2 || states[0] != "paused" || states[1] != "idle" {
		t.Errorf("events %v", states)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !closed.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !closed.Load() {
		t.Error("the client kept the connection open after the stream ended")
	}
}

// A connection that ended fails only its own calls: a call on the
// connection that replaced it is still answered.
func TestOldReaderLeavesNewCallsAlone(t *testing.T) {
	path := listen(t, func(conn net.Conn) {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req protocol.Request
			json.Unmarshal(sc.Bytes(), &req)
			if req.Method == protocol.MethodHello {
				reply(conn, req.ID, protocol.HelloResult{Protocol: protocol.Version, App: "fake"})
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, path, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	old := c.conn
	c.mu.Unlock()
	// A call registered for a newer connection while the old reader is
	// still to run its cleanup.
	newer, _ := net.Pipe()
	ch := make(chan answer, 1)
	c.mu.Lock()
	c.waiting[999] = waiter{ch: ch, conn: newer}
	c.mu.Unlock()
	old.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		gone := c.conn == nil
		c.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case a := <-ch:
		t.Errorf("the old connection's end failed a newer call: %+v", a)
	default:
	}
}

// A peer that stops reading can't hold a call past its deadline, or
// every other call behind it.
func TestWriteGivesUpAtTheDeadline(t *testing.T) {
	path := listen(t, func(conn net.Conn) {
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		var req protocol.Request
		json.Unmarshal([]byte(line), &req)
		reply(conn, req.ID, protocol.HelloResult{Protocol: protocol.Version, App: "fake"})
		time.Sleep(10 * time.Second) // never reads again
	})
	c, err := Dial(context.Background(), path, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	big := protocol.SelfUpdateParams{Data: make([]byte, 8<<20)} // more than the socket buffers
	start := time.Now()
	err = c.Call(ctx, protocol.MethodSelfUpdate, big, nil)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("a write to a stalled peer: %v after %v", err, time.Since(start))
	}
	if !errors.Is(err, ErrNotRunning) && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v", err)
	}
}

// DialAnyVersion says hello in the other side's protocol version, so a
// controller can still send a newer daemon to an older remote.
func TestDialAnyVersion(t *testing.T) {
	path := listen(t, func(conn net.Conn) {
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req protocol.Request
			json.Unmarshal(sc.Bytes(), &req)
			var p protocol.HelloParams
			json.Unmarshal(req.Params, &p)
			if p.Protocol != protocol.Version+1 { // a daemon one version on
				data, _ := json.Marshal(protocol.UnsupportedProtocolData{Protocol: protocol.Version + 1})
				line, _ := json.Marshal(protocol.Reply{ID: req.ID, Error: &protocol.Error{Code: protocol.ErrUnsupportedProtocol, Message: "other", Data: data}})
				conn.Write(append(line, '\n'))
				continue
			}
			reply(conn, req.ID, protocol.HelloResult{Protocol: p.Protocol, App: "other", Build: "abc", OS: "linux", Arch: "arm64"})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, path, "test"); err == nil {
		t.Fatal("the usual dial accepted another protocol version")
	}
	c, err := DialAnyVersion(ctx, UnixDialer(path), "test")
	if err != nil {
		t.Fatal(err)
	}
	if h := c.Hello(); h.Protocol != protocol.Version+1 || h.Build != "abc" || h.OS != "linux" {
		t.Errorf("hello %+v", h)
	}
}
