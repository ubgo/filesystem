package ftp_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	jftp "github.com/jlaffaye/ftp"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/ftp"
)

// resumeCounter makes a server TLS config count handshakes that resumed an
// earlier session, the way vsftpd's require_ssl_reuse checks data
// connections.
func resumeCounter(cfg *tls.Config) *atomic.Int32 {
	var resumed atomic.Int32
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if cs.DidResume {
			resumed.Add(1)
		}
		return nil
	}
	return &resumed
}

// caFile writes the certificates in roots' source (the server's own
// certificate) to a PEM file and returns its path.
func caFile(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: serverTLS.Certificates[0].Certificate[0]}
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func openDSN(t *testing.T, dsn string) (*filesystem.Disk, error) {
	t.Helper()
	mux := filesystem.NewMux()
	for _, s := range []string{ftp.Scheme, ftp.SchemeSecure, ftp.SchemeImplicit} {
		if err := mux.Register(s, ftp.Opener); err != nil {
			t.Fatal(err)
		}
	}
	d, err := mux.Open(t.Context(), "ftp", dsn)
	if err == nil {
		t.Cleanup(func() { _ = d.Close() })
	}
	return d, err
}

// TestOpenerFTPSResumesSessions proves an ftps DSN trusts the ca_file
// bundle and that its data connections resume the control connection's
// TLS session, which servers with require_ssl_reuse insist on.
func TestOpenerFTPSResumesSessions(t *testing.T) {
	serverTLS, _ := selfSigned(t)
	resumed := resumeCounter(serverTLS)
	m, addr := startServer(t, serverTLS)
	m.settings.TLSRequired = ftpserver.MandatoryEncryption
	t.Setenv("FTP_PW", testPassword)
	d, err := openDSN(t, "ftps://"+testUser+"@"+addr+"/app?password_env=FTP_PW&ca_file="+url.QueryEscape(caFile(t, serverTLS)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, err := d.ReadAll(t.Context(), "a.txt"); err != nil || string(b) != "x" {
		t.Fatalf("read %q, %v", b, err)
	}
	if resumed.Load() == 0 {
		t.Fatal("no data connection resumed the control connection's TLS session")
	}
}

// TestOpenerImplicitScheme proves ftps-implicit speaks TLS from the first
// byte and defaults to port 990, and that ca_file is refused on plain ftp
// and when it holds no certificate.
func TestOpenerImplicitScheme(t *testing.T) {
	serverTLS, _ := selfSigned(t)
	_, addr := startServerWith(t, serverTLS, func(s *ftpserver.Settings) {
		s.TLSRequired, s.Listener = ftpserver.ImplicitEncryption, tls.NewListener(s.Listener, serverTLS)
	})
	t.Setenv("FTP_PW", testPassword)
	ca := url.QueryEscape(caFile(t, serverTLS))
	d, err := openDSN(t, "ftps-implicit://"+testUser+"@"+addr+"/app?password_env=FTP_PW&ca_file="+ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("ftps-implicit://127.0.0.1/app")
	if _, err := ftp.Opener(shortCtx(t), u); err == nil || !strings.Contains(err.Error(), ":990") {
		t.Errorf("default implicit port: %v, want a dial to :990", err)
	}
	notPEM := filepath.Join(t.TempDir(), "x")
	_ = os.WriteFile(notPEM, []byte("nope"), 0o600)
	for _, dsn := range []string{
		"ftp://" + addr + "/app?ca_file=" + ca,
		"ftps://" + addr + "/app?ca_file=" + url.QueryEscape(notPEM),
		"ftps://" + addr + "/app?ca_file=/does/not/exist",
	} {
		u, _ := url.Parse(dsn)
		if _, err := ftp.Opener(t.Context(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", dsn, err)
		}
	}
}

// TestStagingDir proves a custom staging directory is used, reserved and
// hidden from listings, frees the default name, and that invalid names
// are refused.
func TestStagingDir(t *testing.T) {
	ctx := t.Context()
	m, addr := startServer(t, nil)
	cfg := config(addr)
	cfg.StagingDir = "_staging"
	drv := newDriver(t, cfg)
	d, _ := filesystem.NewDisk("ftp", drv)
	if _, err := os.Stat(filepath.Join(m.dir, "app", "_staging", "tmp")); err != nil {
		t.Fatalf("custom staging dir not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "app", ftp.StagingDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default staging dir created anyway: %v", err)
	}
	if _, err := d.WriteBytes(ctx, "_staging/x", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Fatalf("write under the staging dir: %v, want CodeInvalidKey", err)
	}
	if _, err := d.WriteBytes(ctx, ftp.StagingDir+"/x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("the default name is an ordinary key now: %v", err)
	}
	page, err := d.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Key != ftp.StagingDir+"/x" {
		t.Errorf("listing = %+v", page.Entries)
	}
	for _, bad := range []string{"a/b", ".", "..", `a\b`} {
		cfg := config(addr)
		cfg.StagingDir = bad
		if _, err := ftp.New(ctx, cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("StagingDir %q: %v, want CodeInvalidArgument", bad, err)
		}
	}
}

// TestIdleCheck proves a short IdleCheck notices a session the server
// dropped while it idled and dials a new one, where the default (longer
// than the server's idle timeout here) reuses the dead session and fails.
func TestIdleCheck(t *testing.T) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		check time.Duration
		ok    bool
	}{{"default", 0, false}, {"short", time.Millisecond, true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, addr := startServerWith(t, nil, func(s *ftpserver.Settings) { s.IdleTimeout = 1 })
			cfg := config(addr)
			cfg.MaxConns, cfg.IdleCheck = 1, tc.check
			drv := newDriver(t, cfg)
			put(t, drv, "a.txt", "x")
			time.Sleep(1500 * time.Millisecond) // past the server's 1 s idle timeout
			_, err := drv.Stat(ctx, "a.txt")
			if (err == nil) != tc.ok {
				t.Fatalf("stat after the server dropped the idle session = %v, want success %v", err, tc.ok)
			}
		})
	}
	if _, err := ftp.New(ctx, ftp.Config{Addr: "h", Root: "/", IdleCheck: -1}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("negative IdleCheck: %v", err)
	}
}

// TestDialHook proves Config.Dial opens the control and the data
// connections, and that its failure fails the call as CodeUnavailable.
func TestDialHook(t *testing.T) {
	_, addr := startServer(t, nil)
	var mu sync.Mutex
	var dialled []string
	cfg := config(addr)
	cfg.Dial = func(ctx context.Context, network, a string) (net.Conn, error) {
		mu.Lock()
		dialled = append(dialled, a)
		mu.Unlock()
		var nd net.Dialer
		return nd.DialContext(ctx, network, a)
	}
	drv := newDriver(t, cfg)
	put(t, drv, "a.txt", "x")
	mu.Lock()
	got := len(dialled)
	mu.Unlock()
	if got < 2 || dialled[0] != addr {
		t.Fatalf("dialled %v, want the control connection then data connections", dialled)
	}
	refused := errors.New("proxy refused")
	cfg.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, refused }
	if _, err := ftp.New(t.Context(), cfg); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, refused) {
		t.Fatalf("failing dial: %v, want CodeUnavailable wrapping the dial error", err)
	}
}

// lockedBuffer is a bytes.Buffer safe for the pool's concurrent sessions.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// epsvCommand matches an EPSV command in a jlaffaye/ftp debug log.
var epsvCommand = regexp.MustCompile(`(?m)^EPSV\r?$`)

// TestDialOptions proves Config.DialOptions reach jlaffaye/ftp: with EPSV
// disabled the session uses PASV, where by default it uses EPSV.
func TestDialOptions(t *testing.T) {
	_, addr := startServer(t, nil)
	for _, disable := range []bool{false, true} {
		var log lockedBuffer
		cfg := config(addr)
		cfg.DialOptions = []jftp.DialOption{jftp.DialWithDebugOutput(&log), jftp.DialWithDisabledEPSV(disable)}
		drv := newDriver(t, cfg)
		put(t, drv, "a.txt", "x")
		// Commands start a line; the server's FEAT reply lists EPSV
		// indented, whatever the client then uses.
		if got := epsvCommand.MatchString(log.String()); got == disable {
			t.Errorf("EPSV disabled %v, session sent EPSV: %v", disable, got)
		}
	}
}

// TestDo proves the escape hatch runs on a pooled connection and keeps
// the error mapping: a server reply keeps its code, a *filesystem.Error
// from fn passes through.
func TestDo(t *testing.T) {
	ctx := t.Context()
	_, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	put(t, drv, "a.txt", "hello")
	var size int64
	if err := drv.Do(ctx, func(sc *jftp.ServerConn) error {
		var err error
		size, err = sc.FileSize("/app/a.txt")
		return err
	}); err != nil || size != 5 {
		t.Fatalf("Do = %d, %v", size, err)
	}
	err := drv.Do(ctx, func(sc *jftp.ServerConn) error { _, err := sc.FileSize("/app/missing"); return err })
	if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("reply through Do: %v, want CodeNotFound", err)
	}
	mine := filesystem.Errorf(filesystem.CodeInvalidArgument, "mine")
	if err := drv.Do(ctx, func(*jftp.ServerConn) error { return mine }); !errors.Is(err, mine) {
		t.Errorf("own verdict through Do: %v", err)
	}
}
