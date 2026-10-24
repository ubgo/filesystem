package sftp_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// allExtensions is pkg/sftp's default server extension list, restored after
// a test narrows it.
var allExtensions = []string{"hardlink@openssh.com", "posix-rename@openssh.com", "statvfs@openssh.com"}

// pipeConn is one in-process SFTP connection whose server can be killed.
type pipeConn struct {
	client *sftp.Client
	kill   func()
}

// pipeDialer dials in-process SFTP servers over pipes and remembers every
// connection, so a test can drop one and count the dials.
type pipeDialer struct {
	mu    sync.Mutex
	conns []pipeConn
	// fail, when set, is returned by the next dials instead of connecting.
	fail error
}

func (p *pipeDialer) dial(context.Context) (*sftp.Client, io.Closer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail != nil {
		return nil, nil, p.fail
	}
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	server, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{c2sR, s2cW})
	if err != nil {
		return nil, nil, err
	}
	go func() { _ = server.Serve() }()
	client, err := sftp.NewClientPipe(s2cR, c2sW)
	if err != nil {
		return nil, nil, err
	}
	kill := func() { _ = server.Close(); _ = c2sR.Close(); _ = s2cW.Close() }
	p.conns = append(p.conns, pipeConn{client: client, kill: kill})
	return client, closerFunc(func() error { kill(); return client.Close() }), nil
}

func (p *pipeDialer) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

// drop kills the newest connection's server and waits until drv has seen
// the connection end.
func (p *pipeDialer) drop(drv *fssftp.Driver) {
	p.mu.Lock()
	c := p.conns[len(p.conns)-1]
	p.mu.Unlock()
	dead := fssftp.Dead(drv)
	c.kill()
	<-dead
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func dialDisk(t *testing.T, p *pipeDialer) (*fssftp.Driver, *filesystem.Disk) {
	t.Helper()
	drv, err := fssftp.New(fssftp.Config{Root: t.TempDir(), Dial: p.dial})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("sftp", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return drv, d
}

// TestReconnect proves a dropped connection is redialled by the next call,
// which then succeeds on the new connection.
func TestReconnect(t *testing.T) {
	ctx := t.Context()
	p := &pipeDialer{}
	drv, d := dialDisk(t, p)
	if _, err := d.WriteBytes(ctx, "a/b.txt", []byte("hi"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	first := drv.Native()
	p.drop(drv)
	if b, err := d.ReadAll(ctx, "a/b.txt"); err != nil || string(b) != "hi" {
		t.Fatalf("read after a dropped connection = %q, %v", b, err)
	}
	if p.count() != 2 || drv.Native() == first {
		t.Fatalf("dials = %d, client replaced = %t", p.count(), drv.Native() != first)
	}
	if _, err := d.WriteBytes(ctx, "c.txt", []byte("x"), filesystem.WriteOptions{IfNotExists: true}); err != nil {
		t.Errorf("conditional write after reconnect = %v", err)
	}
	if p.count() != 2 {
		t.Errorf("a healthy connection was redialled: %d dials", p.count())
	}
}

// TestReconnectInFlight drops the connection without waiting for the
// client to notice: an operation may fail with CodeUnavailable, never with
// anything else, and the one after it reconnects.
func TestReconnectInFlight(t *testing.T) {
	ctx := t.Context()
	p := &pipeDialer{}
	_, d := dialDisk(t, p)
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.conns[0].kill()
	p.mu.Unlock()
	_, err := d.Stat(ctx, "k")
	if err != nil {
		wantCode(t, err, filesystem.CodeUnavailable)
		_, err = d.Stat(ctx, "k")
	}
	if err != nil || p.count() != 2 {
		t.Fatalf("stat after the in-flight failure = %v, dials %d", err, p.count())
	}
}

// TestReconnectOnce proves concurrent calls after a drop share one dial.
func TestReconnectOnce(t *testing.T) {
	ctx := t.Context()
	p := &pipeDialer{}
	drv, d := dialDisk(t, p)
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	p.drop(drv)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 8 {
		wg.Go(func() {
			if _, err := d.Stat(ctx, "k"); err != nil {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if failed.Load() != 0 || p.count() != 2 {
		t.Fatalf("%d calls failed, %d dials, want 0 and 2", failed.Load(), p.count())
	}
}

// TestReconnectFailure proves a failed redial is reported and retried by
// the next call, and that an error without a code is CodeUnavailable.
func TestReconnectFailure(t *testing.T) {
	ctx := t.Context()
	p := &pipeDialer{}
	drv, d := dialDisk(t, p)
	p.drop(drv)
	p.mu.Lock()
	p.fail = errors.New("connection refused")
	p.mu.Unlock()
	_, err := d.Stat(ctx, "k")
	wantCode(t, err, filesystem.CodeUnavailable)
	p.mu.Lock()
	p.fail = filesystem.Errorf(filesystem.CodePermission, "bad key")
	p.mu.Unlock()
	_, err = d.Stat(ctx, "k")
	wantCode(t, err, filesystem.CodePermission)
	p.mu.Lock()
	p.fail = nil
	p.mu.Unlock()
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("write once the server is back = %v", err)
	}
}

// TestReconnectChecksExtensions proves a reconnect re-reads the server's
// extensions: one without hard links refuses conditional writes (the Disk
// declared ConditionalWrite, so the driver cannot fall back), and one
// without posix-rename is refused outright.
func TestReconnectChecksExtensions(t *testing.T) {
	t.Cleanup(func() { _ = sftp.SetSFTPExtensions(allExtensions...) })
	ctx := t.Context()
	p := &pipeDialer{}
	drv, d := dialDisk(t, p)
	p.drop(drv)
	if err := sftp.SetSFTPExtensions("posix-rename@openssh.com"); err != nil {
		t.Fatal(err)
	}
	_, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{IfNotExists: true})
	wantCode(t, err, filesystem.CodeUnsupported)
	if !drv.Capabilities().ConditionalWrite {
		t.Error("capabilities changed after the disk opened")
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("plain write without hard links = %v", err)
	}
	p.drop(drv)
	if err := sftp.SetSFTPExtensions("hardlink@openssh.com"); err != nil {
		t.Fatal(err)
	}
	_, err = d.Stat(ctx, "k")
	wantCode(t, err, filesystem.CodeUnsupported)
}

// TestCloseStopsReconnecting proves a closed driver never dials again.
func TestCloseStopsReconnecting(t *testing.T) {
	p := &pipeDialer{}
	drv, err := fssftp.New(fssftp.Config{Root: t.TempDir(), Dial: p.dial})
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.Close(); err != nil {
		t.Fatal(err)
	}
	_ = p.conns[0].client.Wait()
	_, err = drv.Stat(t.Context(), "k")
	wantCode(t, err, filesystem.CodeUnavailable)
	if p.count() != 1 {
		t.Errorf("closed driver dialled: %d dials", p.count())
	}
}

// TestNewDialFailure proves New reports the first dial's failure.
func TestNewDialFailure(t *testing.T) {
	p := &pipeDialer{fail: errors.New("no route")}
	_, err := fssftp.New(fssftp.Config{Root: "/x", Dial: p.dial})
	wantCode(t, err, filesystem.CodeUnavailable)
}

// TestOpenerReconnects drops the TCP connection under a disk opened from a
// DSN and proves the opener's dialer brings it back.
func TestOpenerReconnects(t *testing.T) {
	ctx := t.Context()
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	addr, _ := sshServer(t, clientSigner.PublicKey(), true)
	// A proxy in front of the server records connections so the test can
	// cut them.
	var mu sync.Mutex
	var conns []net.Conn
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", addr)
			if err != nil {
				_ = in.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, in, out)
			mu.Unlock()
			go func() { _, _ = io.Copy(out, in); _ = out.Close() }()
			go func() { _, _ = io.Copy(in, out); _ = in.Close() }()
		}
	}()
	t.Setenv("SFTP_PW", "secret")
	mux := filesystem.NewMux()
	_ = mux.Register(fssftp.Scheme, fssftp.Opener)
	dsn := "sftp://alice@" + ln.Addr().String() + t.TempDir() + "?password_env=SFTP_PW&insecure_ignore_host_key=" + url.QueryEscape("true")
	d, err := mux.Open(ctx, "sftp", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for _, c := range conns {
		_ = c.Close()
	}
	mu.Unlock()
	// The first call may be the one that finds the connection gone.
	var b []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err = d.ReadAll(ctx, "k")
		if err == nil || time.Now().After(deadline) {
			break
		}
		wantCode(t, err, filesystem.CodeUnavailable)
	}
	if err != nil || string(b) != "x" {
		t.Fatalf("read after the connection was cut = %q, %v", b, err)
	}
}

// TestOpenerHandshakeDeadline proves the SSH handshake is bounded by the
// context: a server that accepts the TCP connection and never answers is
// CodeUnavailable once the deadline passes, not a hang.
func TestOpenerHandshakeDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	t.Setenv("SFTP_PW", "secret")
	mux := filesystem.NewMux()
	_ = mux.Register(fssftp.Scheme, fssftp.Opener)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = mux.Open(ctx, "sftp", "sftp://alice@"+ln.Addr().String()+"/tmp?password_env=SFTP_PW&insecure_ignore_host_key=true")
	wantCode(t, err, filesystem.CodeUnavailable)
}

// TestDialWithoutCloser proves a Dialer may return no closer: the driver
// then owns the client, closing it when the server fails the extension
// check and on Close.
func TestDialWithoutCloser(t *testing.T) {
	t.Cleanup(func() { _ = sftp.SetSFTPExtensions(allExtensions...) })
	// A server that hangs up when the client does, so the client's own
	// Close completes (pipeDialer's servers wait to be killed).
	var client *sftp.Client
	clientOnly := func(context.Context) (*sftp.Client, io.Closer, error) {
		c2sR, c2sW := io.Pipe()
		s2cR, s2cW := io.Pipe()
		server, err := sftp.NewServer(struct {
			io.Reader
			io.WriteCloser
		}{c2sR, s2cW})
		if err != nil {
			return nil, nil, err
		}
		go func() { _ = server.Serve(); _ = s2cW.Close() }()
		client, err = sftp.NewClientPipe(s2cR, c2sW)
		return client, nil, err
	}
	if err := sftp.SetSFTPExtensions("hardlink@openssh.com"); err != nil {
		t.Fatal(err)
	}
	_, err := fssftp.New(fssftp.Config{Root: "/x", Dial: clientOnly})
	wantCode(t, err, filesystem.CodeUnsupported)
	// Closing the client ends its connection; nothing else does here.
	done := make(chan struct{})
	go func() { _ = client.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the refused client was not closed")
		_ = client.Close()
	}
}
