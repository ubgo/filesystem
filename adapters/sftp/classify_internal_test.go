package sftp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/ubgo/filesystem"
)

// TestClassify pins which errors mean a lost connection.
func TestClassify(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want filesystem.Code
	}{
		{"in-flight loss", sftp.ErrSSHFxConnectionLost, filesystem.CodeUnavailable},
		{"no connection", sftp.ErrSSHFxNoConnection, filesystem.CodeUnavailable},
		{"status lost", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxConnectionLost)}, filesystem.CodeUnavailable},
		{"status no connection", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxNoConnection)}, filesystem.CodeUnavailable},
		{"closed after the drop", io.EOF, filesystem.CodeUnavailable},
		{"cut mid-packet", io.ErrUnexpectedEOF, filesystem.CodeUnavailable},
		{"closed socket", net.ErrClosed, filesystem.CodeUnavailable},
		{"closed pipe", io.ErrClosedPipe, filesystem.CodeUnavailable},
		{"reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, filesystem.CodeUnavailable},
		{"broken pipe", &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}, filesystem.CodeUnavailable},
		{"status failure", &sftp.StatusError{Code: uint32(sftp.ErrSSHFxFailure)}, filesystem.CodeUnknown},
		{"not a directory", &os.PathError{Op: "mkdir", Path: "/f", Err: syscall.ENOTDIR}, filesystem.CodeUnknown},
		{"missing", fs.ErrNotExist, filesystem.CodeNotFound},
		{"body", bodyError{io.ErrUnexpectedEOF}, filesystem.CodeUnknown},
		{"already classified", filesystem.Errorf(filesystem.CodeInvalidKey, "bad"), filesystem.CodeInvalidKey},
	} {
		err := c.err
		classify(&err)
		if got := filesystem.CodeOf(err); got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%s: code %q (err %v), want %q", c.name, got, err, c.want)
		}
	}
	var none error
	if classify(&none); none != nil {
		t.Errorf("nil became %v", none)
	}
}

// TestMkParentsLostConnection proves a dropped connection while creating
// parents is not blamed on the key (invalid_argument).
func TestMkParentsLostConnection(t *testing.T) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	server, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{c2sR, s2cW})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	c, err := sftp.NewClientPipe(s2cR, c2sW)
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{Client: c, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_ = server.Close()
	_ = c2sR.Close()
	_ = s2cW.Close()
	t.Cleanup(func() { _ = c.Close() })
	err = d.mkParents(d.cur, "a/b")
	classify(&err)
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("mkParents on a dead connection = %v", err)
	}
}

// TestSessionClassifyKills proves an operation that sees the connection go
// marks its session dead at once, so the next call reconnects without
// waiting for the client's watcher, and that a body failure does not.
func TestSessionClassifyKills(t *testing.T) {
	s := &session{dead: make(chan struct{})}
	var err error = bodyError{io.ErrUnexpectedEOF}
	s.classify(&err)
	if !s.alive() {
		t.Fatal("a body failure killed the session")
	}
	err = sftp.ErrSSHFxConnectionLost
	s.classify(&err)
	if s.alive() || filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("connection lost: alive %t, err %v", s.alive(), err)
	}
}

// TestDialTimeoutFallback proves a dial whose context has no deadline is
// still bounded, by the config's Timeout, against a server that never
// answers.
func TestDialTimeoutFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 4)
	t.Cleanup(func() {
		_ = ln.Close()
		close(held)
		for c := range held {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held <- c
		}
	}()
	cfg := &ssh.ClientConfig{User: "u", Auth: []ssh.AuthMethod{ssh.Password("x")}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, _, err = sshDialer(ln.Addr().String(), cfg, "")(context.Background())
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || time.Since(start) > 2*time.Second {
		t.Fatalf("dial against a silent server = %v after %v", err, time.Since(start))
	}
}
