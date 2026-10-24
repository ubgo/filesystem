package sftp_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

func driver(t *testing.T, opts ...sftp.ServerOption) (*fssftp.Driver, *filesystem.Disk, string) {
	t.Helper()
	root := t.TempDir()
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t, opts...), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("sftp", drv)
	if err != nil {
		t.Fatal(err)
	}
	return drv, d, root
}

func TestNewValidation(t *testing.T) {
	c := pipeClient(t)
	for _, cfg := range []fssftp.Config{{Root: "/x"}, {Client: c, Root: "relative"}} {
		if _, err := fssftp.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%+v) = %v", cfg, err)
		}
	}
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, []byte("x"), 0o600)
	if _, err := fssftp.New(fssftp.Config{Client: c, Root: file}); err == nil {
		t.Error("root under a file accepted")
	}
	drv, d, _ := driver(t)
	if got, ok := filesystem.As[*sftp.Client](d); !ok || got != drv.Native() {
		t.Error("As did not return the client")
	}
	if !drv.Capabilities().ConditionalWrite {
		t.Error("hard links not detected")
	}
	if err := drv.Close(); err != nil {
		t.Errorf("Close without a closer = %v", err)
	}
}

func TestEdges(t *testing.T) {
	ctx := t.Context()
	_, d, root := driver(t)
	_, err := d.WriteBytes(ctx, fssftp.StagingDir+"/x", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, _ = d.WriteBytes(ctx, "dir/file.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
	_, err = d.Stat(ctx, "dir")
	wantCode(t, err, filesystem.CodeNotFound)
	if err := d.Delete(ctx, "dir"); err != nil {
		t.Errorf("delete of a directory name = %v", err)
	}
	if err := d.DeletePrefix(ctx, "dir/file.txt"); err != nil {
		t.Errorf("delete prefix naming a file = %v", err)
	}
	if err := d.DeletePrefix(ctx, "nothing"); err != nil {
		t.Errorf("delete prefix of nothing = %v", err)
	}
	_, err = d.WriteBytes(ctx, "dir/file.txt/child", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, _ = d.WriteBytes(ctx, "other.txt", []byte("x"), filesystem.WriteOptions{})
	_, err = d.Move(ctx, "other.txt", "dir/file.txt/child")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.Move(ctx, "missing", "x")
	wantCode(t, err, filesystem.CodeNotFound)
	wantCode(t, d.SetVisibility(ctx, "missing", filesystem.VisibilityPublic), filesystem.CodeNotFound)
	if e, _ := d.Stat(ctx, "dir/file.txt"); e.Visibility != filesystem.VisibilityPublic || e.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("entry = %+v", e)
	}
	// A symlink is not an object.
	_ = os.Symlink(filepath.Join(root, "dir", "file.txt"), filepath.Join(root, "link"))
	_, err = d.Stat(ctx, "link")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.Read(ctx, "link")
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestCancelledContext(t *testing.T) {
	drv, _, _ := driver(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	checks := map[string]error{
		"delete":         drv.Delete(ctx, "k"),
		"delete prefix":  drv.DeletePrefix(ctx, "k"),
		"set visibility": drv.SetVisibility(ctx, "k", filesystem.VisibilityPublic),
	}
	_, checks["stat"] = drv.Stat(ctx, "k")
	_, checks["read"] = drv.Read(ctx, "k", filesystem.ByteRange{})
	_, checks["write"] = drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{})
	_, checks["list"] = drv.List(ctx, "", filesystem.ListOptions{})
	_, checks["move"] = drv.Move(ctx, "a", "b")
	for name, err := range checks {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v", name, err)
		}
	}
	live, stop := context.WithCancel(t.Context())
	_, err := drv.Write(live, "k", &cancelAfter{cancel: stop}, filesystem.WriteOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("mid-write cancel = %v", err)
	}
}

type cancelAfter struct {
	cancel context.CancelFunc
	done   bool
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	c.done = true
	c.cancel()
	return copy(p, "x"), nil
}

func TestReadOnlyServer(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, fssftp.StagingDir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "existing.txt"), []byte("abc"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "d"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "d", "x"), []byte("x"), 0o644)
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t, sftp.ReadOnly()), Root: root})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("ro", drv)
	if _, err := d.WriteBytes(ctx, "new.txt", []byte("x"), filesystem.WriteOptions{}); err == nil {
		t.Error("write on a read-only server succeeded")
	}
	if err := d.Delete(ctx, "existing.txt"); err == nil {
		t.Error("delete on a read-only server succeeded")
	}
	if _, err := d.Move(ctx, "existing.txt", "moved.txt"); err == nil {
		t.Error("move on a read-only server succeeded")
	}
	if err := d.SetVisibility(ctx, "existing.txt", filesystem.VisibilityPrivate); err == nil {
		t.Error("chmod on a read-only server succeeded")
	}
	if err := d.DeletePrefix(ctx, "d"); err == nil {
		t.Error("delete prefix on a read-only server succeeded")
	}
	if b, err := d.ReadAll(ctx, "existing.txt"); err != nil || string(b) != "abc" {
		t.Errorf("read on a read-only server = %q, %v", b, err)
	}
}

func TestBrokenConnection(t *testing.T) {
	ctx := t.Context()
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
	drv, err := fssftp.New(fssftp.Config{Client: c, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = drv.Write(ctx, "a/b.txt", strings.NewReader("x"), filesystem.WriteOptions{})
	// The server goes away mid-session.
	_ = server.Close()
	_ = c2sR.Close()
	_ = s2cW.Close()
	t.Cleanup(func() { _ = c.Close() })
	// Every operation on the dead connection is unavailable, the code
	// layer.Retry retries and fshttp answers 503.
	checks := map[string]error{
		"delete":         drv.Delete(ctx, "a/b.txt"),
		"delete prefix":  drv.DeletePrefix(ctx, "a"),
		"set visibility": drv.SetVisibility(ctx, "a/b.txt", filesystem.VisibilityPublic),
	}
	_, checks["stat"] = drv.Stat(ctx, "a/b.txt")
	_, checks["read"] = drv.Read(ctx, "a/b.txt", filesystem.ByteRange{})
	_, checks["write"] = drv.Write(ctx, "c.txt", strings.NewReader("x"), filesystem.WriteOptions{})
	_, checks["list"] = drv.List(ctx, "a", filesystem.ListOptions{})
	_, checks["move"] = drv.Move(ctx, "a/b.txt", "c.txt")
	for name, err := range checks {
		if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
			t.Errorf("%s on a dead connection = %v, want unavailable", name, err)
		}
	}
}

// failingBody fails mid-stream the way a client's dropped upload does.
type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestBodyFailureIsNotTheConnection proves a caller's broken body is not
// mistaken for a lost server connection, though both can surface as
// io.ErrUnexpectedEOF.
func TestBodyFailureIsNotTheConnection(t *testing.T) {
	drv, _, _ := driver(t)
	_, err := drv.Write(t.Context(), "k", failingBody{}, filesystem.WriteOptions{})
	if !errors.Is(err, io.ErrUnexpectedEOF) || filesystem.CodeOf(err) == filesystem.CodeUnavailable || err.Error() != io.ErrUnexpectedEOF.Error() {
		t.Fatalf("body failure = %v (code %s)", err, filesystem.CodeOf(err))
	}
}

// sshServer runs an in-process SSH server with an SFTP subsystem, accepting
// the password "secret" and the given public key, and returns its address
// and host key.
func sshServer(t *testing.T, clientKey ssh.PublicKey, sftpEnabled bool) (string, ssh.PublicKey) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == "secret" {
				return nil, nil
			}
			return nil, errors.New("wrong password")
		},
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if string(k.Marshal()) == string(clientKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
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
			go serveSSH(conn, cfg, sftpEnabled)
		}
	}()
	return ln.Addr().String(), hostSigner.PublicKey()
}

func serveSSH(conn net.Conn, cfg *ssh.ServerConfig, sftpEnabled bool) {
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
				ok := sftpEnabled && req.Type == "subsystem" && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if ok {
					if srv, err := sftp.NewServer(ch); err == nil {
						_ = srv.Serve()
					}
					_ = ch.Close()
				}
			}
		}()
	}
}

func TestOpener(t *testing.T) {
	ctx := t.Context()
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientSigner, _ := ssh.NewSignerFromKey(clientPriv)
	addr, hostKey := sshServer(t, clientSigner.PublicKey(), true)
	noSFTP, _ := sshServer(t, clientSigner.PublicKey(), false)
	dir := t.TempDir()
	block, _ := ssh.MarshalPrivateKey(clientPriv, "")
	identity := filepath.Join(dir, "id")
	_ = os.WriteFile(identity, pem.EncodeToMemory(block), 0o600)
	known := filepath.Join(dir, "known_hosts")
	_ = os.WriteFile(known, []byte(knownhosts.Line([]string{addr}, hostKey)+"\n"), 0o600)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(otherPriv)
	wrongKnown := filepath.Join(dir, "wrong_known_hosts")
	_ = os.WriteFile(wrongKnown, []byte(knownhosts.Line([]string{addr}, otherSigner.PublicKey())+"\n"), 0o600)
	t.Setenv("SFTP_PW", "secret")
	t.Setenv("SFTP_BAD_PW", "nope")

	mux := filesystem.NewMux()
	_ = mux.Register(fssftp.Scheme, fssftp.Opener)
	root := t.TempDir()
	q := url.QueryEscape
	good := []string{
		"sftp://alice@" + addr + root + "?password_env=SFTP_PW&insecure_ignore_host_key=true",
		"sftp://alice@" + addr + root + "?identity=" + q(identity) + "&known_hosts=" + q(known),
	}
	for _, dsn := range good {
		d, err := mux.Open(ctx, "sftp", dsn)
		if err != nil {
			t.Fatalf("Open(%s) = %v", dsn, err)
		}
		if _, err := d.WriteBytes(ctx, "hello.txt", []byte("hi"), filesystem.WriteOptions{}); err != nil {
			t.Errorf("write via %s = %v", dsn, err)
		}
		if err := d.Close(); err != nil {
			t.Errorf("close = %v", err)
		}
	}
	badKey := filepath.Join(dir, "bad")
	_ = os.WriteFile(badKey, []byte("not a key"), 0o600)
	file := filepath.Join(root, "hello.txt")
	cases := []struct {
		dsn  string
		code filesystem.Code
	}{
		{"sftp://" + addr + root + "?password_env=SFTP_PW&insecure_ignore_host_key=true", filesystem.CodeInvalidArgument},
		{"sftp://alice:pw@" + addr + root + "?insecure_ignore_host_key=true", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?nope=1", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?insecure_ignore_host_key=maybe", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?password_env=SFTP_PW", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?password_env=SFTP_PW&known_hosts=/does/not/exist", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?identity=/does/not/exist&insecure_ignore_host_key=true", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?identity=" + q(badKey) + "&insecure_ignore_host_key=true", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?insecure_ignore_host_key=true", filesystem.CodeInvalidArgument},
		{"sftp://alice@" + addr + root + "?password_env=SFTP_BAD_PW&insecure_ignore_host_key=true", filesystem.CodePermission},
		{"sftp://alice@" + addr + root + "?password_env=SFTP_PW&known_hosts=" + q(wrongKnown), filesystem.CodePermission},
		{"sftp://alice@127.0.0.1:1" + root + "?password_env=SFTP_PW&insecure_ignore_host_key=true", filesystem.CodeUnavailable},
		{"sftp://alice@" + addr + file + "/sub?password_env=SFTP_PW&insecure_ignore_host_key=true", filesystem.CodeUnknown},
		{"sftp://alice@" + noSFTP + root + "?password_env=SFTP_PW&insecure_ignore_host_key=true", filesystem.CodeUnavailable},
	}
	for _, c := range cases {
		if _, err := mux.Open(ctx, "sftp", c.dsn); filesystem.CodeOf(err) != c.code {
			t.Errorf("Open(%s) = %v, want %s", c.dsn, err, c.code)
		}
	}
}

func TestDefaultPort(t *testing.T) {
	t.Setenv("SFTP_PW", "x")
	mux := filesystem.NewMux()
	_ = mux.Register(fssftp.Scheme, fssftp.Opener)
	// Nothing should accept an SFTP session on port 22 of this address.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := mux.Open(ctx, "sftp", "sftp://alice@127.0.0.9/tmp?password_env=SFTP_PW&insecure_ignore_host_key=true"); err == nil {
		t.Error("connected to port 22 of an unused address")
	}
}

func TestReadOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ctx := t.Context()
	_, d, root := driver(t)
	for _, k := range []string{"keep/a.txt", "keep/b.txt", "locked/x.txt"} {
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Deleting one of two files keeps their directory.
	if err := d.Delete(ctx, "keep/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep")); err != nil {
		t.Error("pruning removed a non-empty directory")
	}
	locked := filepath.Join(root, "locked")
	_ = os.Chmod(locked, 0o555)
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := d.WriteBytes(ctx, "locked/new.txt", []byte("x"), filesystem.WriteOptions{}); err == nil {
		t.Error("rename into a read-only directory succeeded")
	}
	if _, err := d.WriteBytes(ctx, "locked/new.txt", []byte("x"), filesystem.WriteOptions{IfNotExists: true}); err == nil || filesystem.CodeOf(err) == filesystem.CodeExists {
		t.Errorf("link into a read-only directory = %v", err)
	}
	if _, err := d.Move(ctx, "keep/b.txt", "locked/b.txt"); err == nil {
		t.Error("move into a read-only directory succeeded")
	}
}

// TestReadFailsAfterStat covers the two failures Read can meet after the
// file was found: a file the server will not open (mode 000) and an offset
// the client cannot seek to. The Disk validates ranges, so the second is
// driven through the driver directly.
func TestReadFailsAfterStat(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	ctx := t.Context()
	drv, d, root := driver(t)
	if _, err := d.WriteBytes(ctx, "locked.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Read(ctx, "locked.txt", filesystem.ByteRange{Offset: -1}); err == nil {
		t.Error("read at a negative offset succeeded")
	}
	_ = os.Chmod(filepath.Join(root, "locked.txt"), 0)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked.txt"), 0o600) })
	if _, err := drv.Read(ctx, "locked.txt", filesystem.ByteRange{}); err == nil || filesystem.CodeOf(err) == filesystem.CodeUnavailable {
		t.Errorf("read of an unreadable file = %v", err)
	}
}
