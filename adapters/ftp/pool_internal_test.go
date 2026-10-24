package ftp

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/ubgo/filesystem"
)

// stalledConn is a control connection to a server that greets and then
// never answers a command, the way a hung server behaves.
func stalledConn(t *testing.T) *conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		server, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = server.Close() }()
		_, _ = server.Write([]byte("220 hello\r\n"))
		r := bufio.NewReader(server)
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ftp.Dial(ln.Addr().String(), ftp.DialWithDialFunc(func(string, string) (net.Conn, error) { return client, nil }))
	if err != nil {
		t.Fatal(err)
	}
	return &conn{sc: sc, raw: client}
}

// TestRunCancelsAStalledCommand pins that run bounds a command by its
// context: jlaffaye/ftp takes none, so run closes the socket when ctx
// ends, returns ctx's error promptly, and never pools the connection.
func TestRunCancelsAStalledCommand(t *testing.T) {
	c := stalledConn(t)
	dials := 0
	p := newPool(func(context.Context) (*conn, error) { dials++; return c, nil }, 1, DefaultIdleCheck)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.run(ctx, func(c *conn) error { return c.sc.NoOp() }) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run = %v, want the context's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled command outlived its context")
	}
	if len(p.idle) != 0 {
		t.Fatal("a connection cut by its context went back to the pool")
	}
}

// TestGetReplacesADeadIdleConnection pins that an idle connection whose
// NOOP fails (the server dropped it while it sat in the pool) is closed
// and replaced by a fresh dial, never handed to a caller.
func TestGetReplacesADeadIdleConnection(t *testing.T) {
	dead := droppingConn(t)
	fresh := stalledConn(t)
	dials := 0
	p := newPool(func(context.Context) (*conn, error) { dials++; return fresh, nil }, 1, 0)
	p.slots <- struct{}{}
	p.put(dead, false)
	got, err := p.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != fresh || dials != 1 {
		t.Fatalf("got the dead connection back (%d dials)", dials)
	}
}

// droppingConn is a control connection to a server that greets and then
// hangs up on the first NOOP.
func droppingConn(t *testing.T) *conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		server, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = server.Close() }()
		_, _ = server.Write([]byte("220 hello\r\n"))
		r := bufio.NewReader(server)
		for {
			line, err := r.ReadString('\n')
			if err != nil || strings.HasPrefix(line, "NOOP") {
				return
			}
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ftp.Dial(ln.Addr().String(), ftp.DialWithDialFunc(func(string, string) (net.Conn, error) { return client, nil }))
	if err != nil {
		t.Fatal(err)
	}
	return &conn{sc: sc, raw: client}
}

// TestClassifyReplies pins the reply codes classify maps beyond the ones
// the server tests reach: both storage-exceeded codes, a bad file name, a
// transient 4xx and any other reply.
func TestClassifyReplies(t *testing.T) {
	for code, want := range map[int]filesystem.Code{
		ftp.StatusExceededStorage:  filesystem.CodeQuotaExceeded,
		statusInsufficientStorage:  filesystem.CodeQuotaExceeded,
		ftp.StatusBadFileName:      filesystem.CodeInvalidKey,
		ftp.StatusTransfertAborted: filesystem.CodeUnavailable,
		ftp.StatusBadCommand:       filesystem.CodeUnknown,
	} {
		if got := filesystem.CodeOf(classify(&textproto.Error{Code: code, Msg: "x"})); got != want {
			t.Errorf("%d: %s, want %s", code, got, want)
		}
	}
	cause := errors.New("disk read failed")
	if err := (bodyError{cause}); err.Error() != cause.Error() || !errors.Is(err, cause) {
		t.Fatalf("bodyError %v does not carry its cause", err)
	}
}

// TestGetGivesUpWaitingForASlot pins that a caller waiting for a free
// connection stops when its context ends, as CodeUnavailable carrying the
// context's error, without dialling. Every slot is taken first, so the
// context is the only ready case and the outcome does not depend on
// select's random choice.
func TestGetGivesUpWaitingForASlot(t *testing.T) {
	dials := 0
	p := newPool(func(context.Context) (*conn, error) { dials++; return nil, errors.New("unexpected dial") }, 1, 0)
	p.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, err := p.get(ctx)
	if c != nil || filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, context.Canceled) || dials != 0 {
		t.Fatalf("get with every slot taken and an ended context: %v, %v (%d dials)", c, err, dials)
	}
}
