package sftp_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// stallServer accepts TCP connections and never speaks SSH, like an sshd
// at MaxStartups.
func stallServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 16)
	t.Cleanup(func() {
		_ = ln.Close()
		close(conns)
		for c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	return ln.Addr().String()
}

// within fails the test unless f returns before d.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s still blocked after %v", what, d)
	}
}

// TestDialStopsOnCancel proves a cancelled context (with no deadline)
// stops the SSH handshake at once.
func TestDialStopsOnCancel(t *testing.T) {
	u, _ := url.Parse("sftp://u@" + stallServer(t) + "/root?insecure_ignore_host_key=true&password_env=SFTP_STALL_PW")
	t.Setenv("SFTP_STALL_PW", "x")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	var err error
	within(t, 2*time.Second, "Opener after cancel", func() { _, err = fssftp.Opener(ctx, u) })
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("cancelled dial = %v, want unavailable", err)
	}
}

// stuckDial starts a driver whose connection has dropped and whose next
// dial blocks until release is closed, ignoring its context the way a
// handshake used to.
func stuckDial(t *testing.T) (drv *fssftp.Driver, dialing <-chan struct{}, release func()) {
	t.Helper()
	p := &pipeDialer{}
	first := true
	started := make(chan struct{}, 1)
	block := make(chan struct{})
	dial := func(ctx context.Context) (*sftp.Client, io.Closer, error) {
		if !first {
			started <- struct{}{}
			<-block
		}
		first = false
		return p.dial(ctx)
	}
	drv, err := fssftp.New(fssftp.Config{Root: t.TempDir(), Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	p.drop(drv)
	released := false
	release = func() {
		if !released {
			released = true
			close(block)
		}
	}
	t.Cleanup(release)
	return drv, started, release
}

// TestWaiterKeepsItsDeadline proves a call waiting for another call's
// reconnect gives up at its own deadline, and that Close does not wait.
func TestWaiterKeepsItsDeadline(t *testing.T) {
	drv, dialing, release := stuckDial(t)
	go func() { _, _ = drv.Stat(context.Background(), "a") }() // starts the stuck dial
	<-dialing
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var err error
	within(t, 2*time.Second, "Stat behind a stuck reconnect", func() { _, err = drv.Stat(ctx, "b") })
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("waiter past its deadline = %v, want unavailable", err)
	}
	within(t, 2*time.Second, "Close during a dial", func() { _ = drv.Close() })
	release()
}

// TestCloseDuringDial proves a dial that finishes after Close is closed,
// not installed, and that Close is idempotent.
func TestCloseDuringDial(t *testing.T) {
	drv, dialing, release := stuckDial(t)
	errc := make(chan error, 1)
	go func() { _, err := drv.Stat(t.Context(), "a"); errc <- err }()
	<-dialing
	if err := drv.Close(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-errc; filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("call whose dial finished after Close = %v", err)
	}
	if err := drv.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

// TestCloseIsIdempotent proves a second Close does not close the
// connection again.
func TestCloseIsIdempotent(t *testing.T) {
	var closes int
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t), Root: t.TempDir(), Closer: closerFunc(func() error { closes++; return nil })})
	if err != nil {
		t.Fatal(err)
	}
	_ = drv.Close()
	_ = drv.Close()
	if closes != 1 {
		t.Errorf("closer called %d times, want 1", closes)
	}
}

// TestNewClosesDialledSessionOnFailure proves New closes a connection it
// dialled itself when preparing Root fails.
func TestNewClosesDialledSessionOnFailure(t *testing.T) {
	p := &pipeDialer{}
	var closed bool
	dial := func(ctx context.Context) (*sftp.Client, io.Closer, error) {
		c, closer, err := p.dial(ctx)
		return c, closerFunc(func() error { closed = true; return closer.Close() }), err
	}
	file := t.TempDir() + "/file"
	if err := writeFile(file); err != nil {
		t.Fatal(err)
	}
	if _, err := fssftp.New(fssftp.Config{Root: file + "/root", Dial: dial}); err == nil {
		t.Fatal("New under a file succeeded")
	}
	if !closed {
		t.Error("the dialled connection was left open")
	}
}

func writeFile(p string) error { return os.WriteFile(p, []byte("x"), 0o600) }

// silentSFTP runs an SSH server that completes the handshake and accepts
// the SFTP subsystem, then never answers, so the client's SFTP start hangs.
func silentSFTP(t *testing.T) string {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, requests, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range requests {
							_ = req.Reply(true, nil) // accept, then say nothing
						}
						_ = ch.Close()
					}()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// TestDialStopsDuringSFTPStart proves cancellation also interrupts the
// SFTP start that follows a successful handshake.
func TestDialStopsDuringSFTPStart(t *testing.T) {
	u, _ := url.Parse("sftp://u@" + silentSFTP(t) + "/root?insecure_ignore_host_key=true&password_env=SFTP_STALL_PW")
	t.Setenv("SFTP_STALL_PW", "x")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	var err error
	within(t, 3*time.Second, "Opener during the SFTP start", func() { _, err = fssftp.Opener(ctx, u) })
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("cancelled SFTP start = %v, want unavailable", err)
	}
}
