package ftp_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/ftp"
	"github.com/ubgo/filesystem/filesystemtest"
)

// The test servers' passive port range: below every OS's ephemeral range,
// above the shared dev stack's ports.
const (
	passivePortStart = 31000
	passivePortEnd   = 31999
)

const (
	testUser     = "alice"
	testPassword = "s3cret-pass"
)

// mainDriver is an in-process FTP server's configuration: one user whose
// files live in dir, optionally over TLS.
type mainDriver struct {
	dir      string
	settings *ftpserver.Settings
	tls      *tls.Config

	// wrapFs, when set, wraps every session's filesystem, so a test can
	// make the server misbehave.
	wrapFs func(afero.Fs) afero.Fs

	mu       sync.Mutex
	sessions int // control connections opened
}

func (m *mainDriver) GetSettings() (*ftpserver.Settings, error) { return m.settings, nil }

func (m *mainDriver) ClientConnected(ftpserver.ClientContext) (string, error) {
	m.mu.Lock()
	m.sessions++
	m.mu.Unlock()
	return "test server", nil
}

func (m *mainDriver) ClientDisconnected(ftpserver.ClientContext) {}

func (m *mainDriver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user != testUser || pass != testPassword {
		return nil, errors.New("bad credentials")
	}
	fs := afero.NewBasePathFs(afero.NewOsFs(), m.dir)
	if m.wrapFs != nil {
		return m.wrapFs(fs), nil
	}
	return fs, nil
}

func (m *mainDriver) GetTLSConfig() (*tls.Config, error) {
	if m.tls == nil {
		return nil, errors.New("no TLS")
	}
	return m.tls, nil
}

func (m *mainDriver) opened() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions
}

// startServer runs an FTP server on a loopback port until the test ends.
func startServer(t *testing.T, serverTLS *tls.Config) (*mainDriver, string) {
	t.Helper()
	return startServerWith(t, serverTLS, nil)
}

// startServerWith is startServer with settings adjusted by mutate before
// the server listens (implicit TLS, for one, is set up by Listen).
func startServerWith(t *testing.T, serverTLS *tls.Config, mutate func(*ftpserver.Settings)) (*mainDriver, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mainDriver{
		dir: t.TempDir(),
		tls: serverTLS,
		settings: &ftpserver.Settings{
			Listener:            ln,
			PublicHost:          "127.0.0.1",
			DefaultTransferType: ftpserver.TransferTypeBinary,
			// Passive ports below the ephemeral range, so a data listener
			// never shares a port with a client socket: on random ports one
			// run in fifteen stalled in the server's passive accept.
			PassiveTransferPortRange: &ftpserver.PortRange{Start: passivePortStart, End: passivePortEnd},
		},
	}
	if mutate != nil {
		mutate(m.settings)
	}
	srv := ftpserver.NewFtpServer(m)
	srv.Logger = slog.New(slog.DiscardHandler)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() { _ = srv.Stop() })
	return m, ln.Addr().String()
}

func config(addr string) ftp.Config {
	return ftp.Config{Addr: addr, Username: testUser, Password: testPassword, Root: "/app"}
}

func newDriver(t *testing.T, cfg ftp.Config) *ftp.Driver {
	t.Helper()
	drv, err := ftp.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drv.Close() })
	return drv
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	_, addr := startServer(t, nil)
	d, err := filesystem.NewDisk("ftp", newDriver(t, config(addr)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, newDisk)
}

// TestCredentialsNeverPrint pins the redaction: no fmt verb, no slog
// handler, and no print of the driver shows the password.
func TestCredentialsNeverPrint(t *testing.T) {
	_, addr := startServer(t, nil)
	cfg := config(addr)
	drv := newDriver(t, cfg)
	var buf bytes.Buffer
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		fmt.Fprintf(&buf, f+"\n", cfg)
		fmt.Fprintf(&buf, f+"\n", drv)
	}
	// Format answers every verb, so fmt never reaches GoString; call it
	// directly so a direct caller cannot see the secret either.
	buf.WriteString(cfg.GoString())
	buf.WriteString("\n")
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("open", "cfg", cfg, "driver", drv)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("open", "cfg", cfg)
	if strings.Contains(buf.String(), testPassword) {
		t.Fatalf("password printed:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") || !strings.Contains(buf.String(), testUser) {
		t.Fatalf("expected the user and a redaction marker:\n%s", buf.String())
	}
}

func TestWrongPasswordIsPermission(t *testing.T) {
	_, addr := startServer(t, nil)
	cfg := config(addr)
	cfg.Password = "wrong"
	if _, err := ftp.New(context.Background(), cfg); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("New with a wrong password: %v, want CodePermission", err)
	}
}

// TestPoolReusesConnections pins the pool: many sequential calls share one
// control connection, and concurrent calls never open more than MaxConns.
func TestPoolReusesConnections(t *testing.T) {
	ctx := context.Background()
	m, addr := startServer(t, nil)
	cfg := config(addr)
	cfg.MaxConns = 2
	drv := newDriver(t, cfg)
	for i := range 5 {
		if _, err := drv.Write(ctx, fmt.Sprintf("seq/%d", i), strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.opened(); got != 1 {
		t.Fatalf("%d control connections for sequential calls, want 1", got)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if _, err := drv.Stat(ctx, fmt.Sprintf("seq/%d", i%5)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := m.opened(); got > cfg.MaxConns {
		t.Fatalf("%d control connections, MaxConns is %d", got, cfg.MaxConns)
	}
}

// TestWriteIsAtomic pins the staging: a body that fails part-way stores
// nothing under the key, the error is the body's own (not a lost server),
// and the old object survives.
func TestWriteIsAtomic(t *testing.T) {
	ctx := context.Background()
	_, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	if _, err := drv.Write(ctx, "a/doc.txt", strings.NewReader("old"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("client went away")
	_, err := drv.Write(ctx, "a/doc.txt", io.MultiReader(strings.NewReader("new-partial"), errReader{boom}), filesystem.WriteOptions{})
	if !errors.Is(err, boom) || filesystem.CodeOf(err) == filesystem.CodeUnavailable {
		t.Fatalf("failing body: %v, want the body's error", err)
	}
	rc, err := drv.Read(ctx, "a/doc.txt", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "old" {
		t.Fatalf("after a failed write the key holds %q, want old", b)
	}
	if _, err := drv.Stat(ctx, "a/doc.txt"); err != nil {
		t.Fatalf("the connection was not replaced after the broken transfer: %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestEarlyCloseKeepsDriverWorking reads a few bytes of a large object,
// closes the reader, and checks the next calls still work.
func TestEarlyCloseKeepsDriverWorking(t *testing.T) {
	ctx := context.Background()
	_, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	big := bytes.Repeat([]byte("0123456789"), 200_000)
	if _, err := drv.Write(ctx, "big.bin", bytes.NewReader(big), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		rc, err := drv.Read(ctx, "big.bin", filesystem.ByteRange{Offset: 10, Length: 5})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		if err := rc.Close(); err != nil {
			t.Fatal(err)
		}
		if string(b) != "01234" {
			t.Fatalf("range read %q", b)
		}
		if _, err := drv.Stat(ctx, "big.bin"); err != nil {
			t.Fatalf("the aborted transfer's answer leaked into the next command: %v", err)
		}
	}
	if _, err := drv.Stat(ctx, "big.bin"); err != nil {
		t.Fatal(err)
	}
}

// TestCancelledContextBreaksTheCommand pins that an ended context fails a
// call and that the driver keeps working afterwards.
func TestCancelledContextBreaksTheCommand(t *testing.T) {
	_, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := drv.Stat(ctx, "x"); !errors.Is(err, context.Canceled) && filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("Stat with a cancelled context: %v", err)
	}
	if _, err := drv.Write(context.Background(), "after", strings.NewReader("ok"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("driver unusable after a cancelled call: %v", err)
	}
}

// TestCaseProbeMatchesHost pins New's probe against the host filesystem
// the test server writes to, and that no probe file is left behind.
func TestCaseProbeMatchesHost(t *testing.T) {
	m, addr := startServer(t, nil)
	if err := os.WriteFile(filepath.Join(m.dir, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(m.dir, "PROBE"))
	hostFolds := err == nil
	drv := newDriver(t, config(addr))
	if got := drv.Capabilities().CaseInsensitive; got != hostFolds {
		t.Fatalf("CaseInsensitive = %v, host filesystem folds case: %v", got, hostFolds)
	}
	left, err := os.ReadDir(filepath.Join(m.dir, "app", ftp.StagingDir, "tmp"))
	if err != nil || len(left) != 0 {
		t.Fatalf("probe left %v behind (%v)", left, err)
	}
}

// TestSweepAbandoned plants staged files older and newer than the cut and
// a file of another shape, and checks only the old staged one goes.
func TestSweepAbandoned(t *testing.T) {
	m, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	tmp := filepath.Join(m.dir, "app", ftp.StagingDir, "tmp")
	old, fresh, other := filepath.Join(tmp, strings.Repeat("a", 26)), filepath.Join(tmp, strings.Repeat("b", 26)), filepath.Join(tmp, "keep.me")
	for _, f := range []string{old, fresh, other} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, f := range []string{old, other} {
		if err := os.Chtimes(f, past, past); err != nil {
			t.Fatal(err)
		}
	}
	n, err := drv.SweepAbandoned(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("swept %d (%v), want 1", n, err)
	}
	for f, want := range map[string]bool{old: false, fresh: true, other: true} {
		if _, err := os.Stat(f); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(f), err == nil, want)
		}
	}
}

// TestExplicitTLS runs a write and a read over explicit FTPS, with the
// server's self-signed certificate trusted by the client.
func TestExplicitTLS(t *testing.T) {
	serverTLS, roots := selfSigned(t)
	m, addr := startServer(t, serverTLS)
	m.settings.TLSRequired = ftpserver.MandatoryEncryption
	cfg := config(addr)
	cfg.TLS = &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	drv := newDriver(t, cfg)
	ctx := context.Background()
	if _, err := drv.Write(ctx, "secure.txt", strings.NewReader("over tls"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := drv.Read(ctx, "secure.txt", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); string(b) != "over tls" {
		t.Fatalf("read %q", b)
	}
	// A TLS config without ServerName verifies Addr's host, on the control
	// and the data connections alike.
	unnamed := config(addr)
	unnamed.TLS = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if got := readKey(t, newDriver(t, unnamed), "secure.txt"); got != "over tls" {
		t.Fatalf("read %q without ServerName", got)
	}
	plain := config(addr)
	if _, err := ftp.New(ctx, plain); err == nil {
		t.Fatal("a plain login succeeded on a server that requires TLS")
	}
}

// selfSigned makes a certificate for 127.0.0.1 and the pool trusting it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "ftp test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, roots
}

func TestOpener(t *testing.T) {
	m, addr := startServer(t, nil)
	t.Setenv("FTP_PW", testPassword)
	mux := filesystem.NewMux()
	if err := mux.Register(ftp.Scheme, ftp.Opener); err != nil {
		t.Fatal(err)
	}
	d, err := mux.Open(context.Background(), "drop", "ftp://"+testUser+"@"+addr+"/app?password_env=FTP_PW&max_conns=2")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err := d.Write(context.Background(), "hello.txt", strings.NewReader("hi"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.dir, "app", "hello.txt")); err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{
		"ftp://" + testUser + ":pw@" + addr + "/app",
		"ftp://" + addr + "/app?password_env=FTP_PW",
		"ftp://" + addr + "/app?max_conns=0",
		"ftp://" + addr + "/app?typo=1",
		"ftp://" + addr + "/relative",
	} {
		u, _ := url.Parse(dsn)
		if u.Path == "/relative" {
			u.Path = "relative"
		}
		if _, err := ftp.Opener(context.Background(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", dsn, err)
		}
	}
}

// renameFs makes RNTO fail the ways real servers do: refusing to rename
// onto an existing file with IIS's words, refusing every rename, or
// refusing the next few.
type renameFs struct {
	afero.Fs
	mu             sync.Mutex
	refuseExisting bool
	refuseAll      bool
	refuseReply    string // refuseAll's reply text; "" means permission denied
	refuseNext     int
	// vanish removes the source and answers with vanishReply, the way a
	// server that lost the staged file (a quota sweeper, an antivirus)
	// does.
	vanish      bool
	vanishReply string
}

func (f *renameFs) Rename(oldname, newname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuseAll {
		return errors.New(cmp.Or(f.refuseReply, "permission denied"))
	}
	if f.vanish {
		_ = f.Remove(oldname)
		return errors.New(f.vanishReply)
	}
	if _, err := f.Stat(newname); err == nil && f.refuseExisting {
		return errors.New("Cannot create a file when that file already exists.")
	}
	if f.refuseNext > 0 {
		f.refuseNext--
		return errors.New("try again later")
	}
	return f.Fs.Rename(oldname, newname)
}

// replaceSetup stores "old" under doc.txt on a server whose renames fs
// controls once the old object exists.
func replaceSetup(t *testing.T, fs *renameFs) (*mainDriver, *ftp.Driver) {
	t.Helper()
	m, addr := startServer(t, nil)
	m.wrapFs = func(inner afero.Fs) afero.Fs { fs.Fs = inner; return fs }
	drv := newDriver(t, config(addr))
	if _, err := drv.Write(context.Background(), "doc.txt", strings.NewReader("old"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	return m, drv
}

func readKey(t *testing.T, drv *ftp.Driver, key string) string {
	t.Helper()
	rc, err := drv.Read(context.Background(), key, filesystem.ByteRange{})
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func staged(t *testing.T, m *mainDriver) []os.DirEntry {
	t.Helper()
	left, err := os.ReadDir(filepath.Join(m.dir, "app", ftp.StagingDir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// TestFailedRenameKeepsTheOldObject pins that a rename refused for any
// reason but "the target exists" never deletes the old object: the write
// fails as a refusal (not not_found) and "old" is still there.
func TestFailedRenameKeepsTheOldObject(t *testing.T) {
	fs := &renameFs{}
	m, drv := replaceSetup(t, fs)
	fs.mu.Lock()
	fs.refuseAll = true
	fs.mu.Unlock()
	_, err := drv.Write(context.Background(), "doc.txt", strings.NewReader("new"), filesystem.WriteOptions{})
	if filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused rename: %v, want CodePermission", err)
	}
	if got := readKey(t, drv, "doc.txt"); got != "old" {
		t.Fatalf("after a refused replace the key holds %q, want old", got)
	}
	if left := staged(t, m); len(left) != 0 {
		t.Fatalf("a refused write left %d staged files", len(left))
	}
}

// TestReplaceOnServerRefusingOverwrite pins the IIS path: a server that
// will not rename over a file gets the old one deleted first, and a
// refusal or two after that is retried.
func TestReplaceOnServerRefusingOverwrite(t *testing.T) {
	fs := &renameFs{refuseExisting: true}
	_, drv := replaceSetup(t, fs)
	fs.mu.Lock()
	fs.refuseNext = 2
	fs.mu.Unlock()
	if _, err := drv.Write(context.Background(), "doc.txt", strings.NewReader("new"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readKey(t, drv, "doc.txt"); got != "new" {
		t.Fatalf("key holds %q, want new", got)
	}
}

// TestRefusedReplaceKeepsTheOldObject pins that a server refusing every
// rename during a replace costs nothing: the old object is moved aside
// only by a rename, so when that is refused the write fails as a refusal,
// "old" is still there, and no staged file is left behind.
func TestRefusedReplaceKeepsTheOldObject(t *testing.T) {
	fs := &renameFs{refuseExisting: true}
	m, drv := replaceSetup(t, fs)
	fs.mu.Lock()
	fs.refuseNext = 100
	fs.mu.Unlock()
	_, err := drv.Write(context.Background(), "doc.txt", strings.NewReader("new"), filesystem.WriteOptions{})
	if filesystem.CodeOf(err) != filesystem.CodePermission || errors.As(err, new(*ftp.StrandedError)) {
		t.Fatalf("refused replace: %v, want a plain CodePermission", err)
	}
	fs.mu.Lock()
	fs.refuseNext = 0
	fs.mu.Unlock()
	if got := readKey(t, drv, "doc.txt"); got != "old" {
		t.Fatalf("key holds %q, want old", got)
	}
	if left := staged(t, m); len(left) != 0 {
		t.Fatalf("a refused replace left %d staged files", len(left))
	}
}

// stallFs serves files whose reads stall for stall after the first
// chunk, the way a hung server stops sending mid-transfer.
type stallFs struct {
	afero.Fs
	stall time.Duration
	// on turns the stall on, after the object is stored.
	on *atomic.Bool
}

func (f stallFs) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	file, err := f.Fs.OpenFile(name, flag, perm)
	if err != nil || flag != os.O_RDONLY || !f.on.Load() {
		return file, err
	}
	return &stallFile{File: file, stall: f.stall}, nil
}

func (f stallFs) Open(name string) (afero.File, error) { return f.OpenFile(name, os.O_RDONLY, 0) }

type stallFile struct {
	afero.File
	stall time.Duration
	sent  bool
}

func (f *stallFile) Read(p []byte) (int, error) {
	if f.sent {
		time.Sleep(f.stall)
	}
	f.sent = true
	return f.File.Read(p)
}

// slowServer stores a 1 MB object, then serves reads that stall.
func slowServer(t *testing.T, cfg func(*ftp.Config)) *ftp.Driver {
	t.Helper()
	m, addr := startServer(t, nil)
	on := new(atomic.Bool)
	m.wrapFs = func(inner afero.Fs) afero.Fs { return stallFs{Fs: inner, stall: 3 * time.Second, on: on} }
	c := config(addr)
	cfg(&c)
	drv := newDriver(t, c)
	if _, err := drv.Write(context.Background(), "big.bin", bytes.NewReader(make([]byte, 1<<20)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	on.Store(true)
	return drv
}

// readAllWithin reads rc to the end and fails the test unless that ends,
// with an error, well before the server's stall would.
func readAllWithin(t *testing.T, rc io.ReadCloser) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(rc)
		_ = rc.Close()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled read outlived its bound")
		return nil
	}
}

// TestReadIsBoundByItsContext pins that the reader Read returns still
// honours the caller's context: cancelling it fails a read in progress.
func TestReadIsBoundByItsContext(t *testing.T) {
	drv := slowServer(t, func(*ftp.Config) {})
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := drv.Read(ctx, "big.bin", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := readAllWithin(t, rc); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel: %v, want context.Canceled", err)
	}
	if _, err := drv.Stat(context.Background(), "big.bin"); err != nil {
		t.Fatalf("driver unusable after a cancelled read: %v", err)
	}
}

// TestReadStallHitsDataTimeout pins Config.DataTimeout: a server that
// stops sending fails the read with CodeUnavailable.
func TestReadStallHitsDataTimeout(t *testing.T) {
	drv := slowServer(t, func(c *ftp.Config) { c.DataTimeout = 200 * time.Millisecond })
	rc, err := drv.Read(context.Background(), "big.bin", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	if err := readAllWithin(t, rc); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("stalled read: %v, want CodeUnavailable", err)
	}
}

// TestListSkipsNamesTheDiskWouldRefuse plants files whose names are not
// clean keys on the server and checks a listing never reports them.
func TestListSkipsNamesTheDiskWouldRefuse(t *testing.T) {
	m, addr := startServer(t, nil)
	drv := newDriver(t, config(addr))
	for _, name := range []string{`back\slash.txt`, "bell\a.txt", "ok.txt"} {
		if err := os.WriteFile(filepath.Join(m.dir, "app", name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := drv.List(context.Background(), "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "ok.txt" {
		t.Fatalf("entries %+v, want only ok.txt", p.Entries)
	}
}

// TestDoesNotExistIsNotExists pins the reply match: a refusal saying
// something "does not exist", with the staged file still in place, is not
// a refusal because the target exists, so the old object is never deleted.
func TestDoesNotExistIsNotExists(t *testing.T) {
	fs := &renameFs{}
	_, drv := replaceSetup(t, fs)
	fs.mu.Lock()
	fs.refuseAll, fs.refuseReply = true, "directory does not exist"
	fs.mu.Unlock()
	_, err := drv.Write(context.Background(), "doc.txt", strings.NewReader("new"), filesystem.WriteOptions{})
	if filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("write: %v, want CodePermission", err)
	}
	if got := readKey(t, drv, "doc.txt"); got != "old" {
		t.Fatalf("key holds %q, want old", got)
	}
}

// TestVanishedSourceKeepsTheOldObject pins that a rename failing because
// the staged file is gone never costs the old object: neither a "source
// does not exist" reply (which must not read as "target exists") nor an
// "already exists" reply with the staged file missing leads to a delete.
func TestVanishedSourceKeepsTheOldObject(t *testing.T) {
	for _, reply := range []string{"source does not exist", "Cannot create a file when that file already exists."} {
		t.Run(reply, func(t *testing.T) {
			fs := &renameFs{}
			_, drv := replaceSetup(t, fs)
			fs.mu.Lock()
			fs.vanish, fs.vanishReply = true, reply
			fs.mu.Unlock()
			_, err := drv.Write(context.Background(), "doc.txt", strings.NewReader("new"), filesystem.WriteOptions{})
			if err == nil || errors.As(err, new(*ftp.StrandedError)) {
				t.Fatalf("write with a vanished staged file: %v, want a plain refusal", err)
			}
			if got := readKey(t, drv, "doc.txt"); got != "old" {
				t.Fatalf("key holds %q, want old", got)
			}
		})
	}
}
