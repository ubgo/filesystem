package webdav_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xwebdav "golang.org/x/net/webdav"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/webdav"
	"github.com/ubgo/filesystem/filesystemtest"
)

const (
	testUser     = "alice"
	testPassword = "s3cret-pass"
	davPrefix    = "/dav/files/alice"
)

// server serves dir over WebDAV under davPrefix with Basic authentication,
// the way Nextcloud lays out a user's files. It counts requests by method.
type server struct {
	*httptest.Server
	dir    string
	counts map[string]*atomic.Int64
	// directPuts counts PUTs outside the staging collection.
	directPuts atomic.Int64
	// expectPuts counts PUTs carrying Expect: 100-continue.
	expectPuts atomic.Int64
	// intercept, when set, sees every authorised request first and
	// answers it instead of the server when it returns true, to inject a
	// failure or an odd answer.
	intercept atomic.Pointer[func(w http.ResponseWriter, r *http.Request) bool]
}

// on sets the intercept (nil clears it).
func (s *server) on(h func(w http.ResponseWriter, r *http.Request) bool) {
	if h == nil {
		s.intercept.Store(nil)
		return
	}
	s.intercept.Store(&h)
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{dir: t.TempDir(), counts: map[string]*atomic.Int64{}}
	for _, m := range []string{"PROPFIND", "GET", "PUT", "DELETE", "MKCOL", "MOVE", "COPY"} {
		s.counts[m] = new(atomic.Int64)
	}
	h := &xwebdav.Handler{Prefix: davPrefix, FileSystem: xwebdav.Dir(s.dir), LockSystem: xwebdav.NewMemLS()}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != testUser || p != testPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if c := s.counts[r.Method]; c != nil {
			c.Add(1)
		}
		if h := s.intercept.Load(); h != nil && (*h)(w, r) {
			return
		}
		if r.Method == http.MethodPut && !strings.Contains(r.URL.Path, "/"+webdav.StagingDir+"/") {
			s.directPuts.Add(1)
		}
		if r.Method == http.MethodPut && strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
			s.expectPuts.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) config() webdav.Config {
	return webdav.Config{Endpoint: s.URL + davPrefix + "/app", Username: testUser, Password: testPassword}
}

func newDriver(t *testing.T, s *server) *webdav.Driver {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(s.dir, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	drv, err := webdav.New(context.Background(), s.config())
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("webdav", newDriver(t, newServer(t)))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, newDisk)
}

// TestCredentialsNeverPrint pins the redaction: no fmt verb, no slog
// handler, and no print of the driver itself shows the password.
func TestCredentialsNeverPrint(t *testing.T) {
	s := newServer(t)
	cfg := s.config()
	drv := newDriver(t, s)
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

// TestWrongPasswordIsPermission pins that New's probe fails fast on a 401.
func TestWrongPasswordIsPermission(t *testing.T) {
	s := newServer(t)
	cfg := s.config()
	cfg.Password = "wrong"
	if _, err := webdav.New(context.Background(), cfg); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("New with a wrong password: %v, want CodePermission", err)
	}
}

// TestWriteIsStagedThenMoved pins atomicity: a PUT only ever targets the
// staging collection, never the object's own URL, so no reader sees
// partial bytes; and
// a failing body leaves nothing behind (neither the key nor a staged file).
func TestWriteIsStagedThenMoved(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	drv := newDriver(t, s)
	if _, err := drv.Write(ctx, "a/b/c.txt", strings.NewReader("hello"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := s.directPuts.Load(); got != 0 {
		t.Fatalf("%d PUTs outside the staging collection", got)
	}
	boom := errors.New("client went away")
	_, err := drv.Write(ctx, "a/b/d.txt", io.MultiReader(strings.NewReader("part"), errReader{boom}), filesystem.WriteOptions{})
	if !errors.Is(err, boom) || filesystem.CodeOf(err) == filesystem.CodeUnavailable {
		t.Fatalf("failing body: %v, want the body's error, not an unavailable server", err)
	}
	if _, err := drv.Stat(ctx, "a/b/d.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a failed write stored the key: %v", err)
	}
	left, _ := os.ReadDir(filepath.Join(s.dir, "app", webdav.StagingDir, "tmp"))
	if len(left) != 0 {
		t.Fatalf("a failed write left %d staged files", len(left))
	}
}

// TestWritePutWaitsForContinue pins that every PUT a Write sends carries
// Expect: 100-continue, so the body (and so a body's failure) comes only
// after the server has started the PUT; without it a failed body's cleanup
// DELETE can overtake the aborted PUT and leave its staged file behind.
func TestWritePutWaitsForContinue(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	drv := newDriver(t, s)
	puts, expects := s.counts["PUT"].Load(), s.expectPuts.Load()
	for i := range 3 {
		if _, err := drv.Write(ctx, fmt.Sprintf("e/k%d", i), strings.NewReader("hello"), filesystem.WriteOptions{SizeHint: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := drv.Write(ctx, "e/streamed", io.MultiReader(strings.NewReader("no size")), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	gotPuts, gotExpects := s.counts["PUT"].Load()-puts, s.expectPuts.Load()-expects
	if gotPuts != 4 || gotExpects != gotPuts {
		t.Fatalf("%d PUTs, %d with Expect: 100-continue; want 4 and 4", gotPuts, gotExpects)
	}
}

// customTransport is a RoundTripper that is not an *http.Transport.
type customTransport struct{ http.RoundTripper }

// TestExpectContinueTransport pins how New makes Expect: 100-continue
// actually wait: a transport that would send the body at once (an
// *http.Transport with a zero ExpectContinueTimeout) is replaced by a copy
// with ExpectContinueTimeout set, leaving the caller's untouched; a
// transport that already waits (http.DefaultTransport does, so a nil
// transport keeps sharing its connection pool), or one that is not an
// *http.Transport, is used as given.
func TestExpectContinueTransport(t *testing.T) {
	zero := &http.Transport{}
	waits := &http.Transport{ExpectContinueTimeout: 3 * time.Second}
	custom := customTransport{http.DefaultTransport}
	for _, tc := range []struct {
		name   string
		client *http.Client
		same   http.RoundTripper // non-nil: the driver must use exactly this
	}{
		{"default", nil, http.DefaultTransport},
		{"caller-nil-transport", &http.Client{}, http.DefaultTransport},
		{"caller-zero-timeout", &http.Client{Transport: zero}, nil},
		{"caller-waits", &http.Client{Transport: waits}, waits},
		{"caller-custom", &http.Client{Transport: custom}, custom},
	} {
		s := newServer(t)
		if err := os.MkdirAll(filepath.Join(s.dir, "app"), 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := s.config()
		cfg.Client = tc.client
		drv, err := webdav.New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := filesystem.NewDisk(tc.name, drv)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := filesystem.As[*http.Client](disk)
		if !ok {
			t.Fatalf("%s: no *http.Client", tc.name)
		}
		if tc.same != nil {
			if got.Transport != tc.same {
				t.Errorf("%s: transport %v, want the caller's own", tc.name, got.Transport)
			}
			continue
		}
		tr, ok := got.Transport.(*http.Transport)
		if !ok || tr == zero {
			t.Fatalf("%s: transport %T %p, want a copy", tc.name, got.Transport, got.Transport)
		}
		if tr.ExpectContinueTimeout != webdav.ExpectContinueTimeout {
			t.Errorf("%s: ExpectContinueTimeout %v, want %v", tc.name, tr.ExpectContinueTimeout, webdav.ExpectContinueTimeout)
		}
	}
	if zero.ExpectContinueTimeout != 0 {
		t.Error("New modified the caller's transport")
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestStagingCollectionMadeOnce pins that writes after the first do not
// recreate the staging collection.
func TestStagingCollectionMadeOnce(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	drv := newDriver(t, s)
	for i := range 3 {
		if _, err := drv.Write(ctx, fmt.Sprintf("k%d", i), strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.counts["MKCOL"].Load(); got != 2 { // .filesystem and .filesystem/tmp
		t.Fatalf("%d MKCOLs for three root writes, want 2", got)
	}
}

// TestKeysAreEscaped writes keys holding URL syntax and reads them back by
// listing, so "?" and "#" never become a query or fragment.
func TestKeysAreEscaped(t *testing.T) {
	ctx := context.Background()
	drv := newDriver(t, newServer(t))
	keys := []string{"what?.txt", "hash#tag.txt", "space dir/100% sure.txt"}
	for _, k := range keys {
		if _, err := drv.Write(ctx, k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
	}
	p, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range p.Entries {
		got = append(got, e.Key)
	}
	if want := "hash#tag.txt,space dir/100% sure.txt,what?.txt"; strings.Join(got, ",") != want {
		t.Fatalf("listing %v, want %s", got, want)
	}
}

// TestReadRangeFallback pins Read against a server that ignores Range and
// answers 200 with the whole object: the driver cuts the range itself.
func TestReadRangeFallback(t *testing.T) {
	srv := fakeServer(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "0123456789")
	})
	defer srv.Close()
	drv, err := webdav.New(context.Background(), webdav.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := drv.Read(context.Background(), "k", filesystem.ByteRange{Offset: 2, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); string(b) != "234" {
		t.Fatalf("read %q, want 234", b)
	}
}

// TestStatusClassification pins how server answers map to codes.
func TestStatusClassification(t *testing.T) {
	for status, want := range map[int]filesystem.Code{
		http.StatusNotFound:            filesystem.CodeNotFound,
		http.StatusForbidden:           filesystem.CodePermission,
		http.StatusInsufficientStorage: filesystem.CodeQuotaExceeded,
		http.StatusTooManyRequests:     filesystem.CodeUnavailable,
		http.StatusBadGateway:          filesystem.CodeUnavailable,
		http.StatusTeapot:              filesystem.CodeUnknown,
	} {
		srv := fakeServer(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(status)
		})
		drv, err := webdav.New(context.Background(), webdav.Config{Endpoint: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = drv.Stat(context.Background(), "k")
		srv.Close()
		if filesystem.CodeOf(err) != want {
			t.Errorf("%d: %v, want %s", status, err, want)
		}
		if ra, _ := filesystem.RetryAfterOf(err); want == filesystem.CodeUnavailable && ra != 7*time.Second {
			t.Errorf("%d: Retry-After %v, want 7s", status, ra)
		}
	}
}

// TestHostileHrefsAreDropped pins that a listing never trusts a server's
// href outside the endpoint or one that is not a clean key.
func TestHostileHrefsAreDropped(t *testing.T) {
	srv := fakeServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`+
			`<d:response><d:href>/root/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`+
			`<d:response><d:href>/root/ok.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>2</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`+
			`<d:response><d:href>/elsewhere/secret.txt</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`+
			`<d:response><d:href>/root/a/../../x.txt</d:href><d:propstat><d:prop><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`+
			`</d:multistatus>`)
	})
	defer srv.Close()
	drv, err := webdav.New(context.Background(), webdav.Config{Endpoint: srv.URL + "/root"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := drv.List(context.Background(), "", filesystem.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "ok.txt" || p.Entries[0].Size != 2 {
		t.Fatalf("entries %+v, want only ok.txt", p.Entries)
	}
}

func TestOpener(t *testing.T) {
	s := newServer(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAV_PW", testPassword)
	host := strings.TrimPrefix(s.URL, "http://")
	mux := filesystem.NewMux()
	if err := mux.Register(webdav.Scheme, webdav.Opener); err != nil {
		t.Fatal(err)
	}
	d, err := mux.Open(context.Background(), "files", "webdav://"+testUser+"@"+host+davPrefix+"/app?password_env=DAV_PW")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(context.Background(), "hello.txt", strings.NewReader("hi"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "app", "hello.txt")); err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{
		"webdav://" + testUser + ":pw@" + host + "/x",
		"webdav://" + host + "/x?password_env=DAV_PW",
		"webdav://" + host + "/x?typo=1",
	} {
		u, _ := url.Parse(dsn)
		if _, err := webdav.Opener(context.Background(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", dsn, err)
		}
	}
}

// TestSweepAbandoned plants a staged file older than the cut and one
// newer, and checks only the old one is removed.
func TestSweepAbandoned(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	drv := newDriver(t, s)
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(s.dir, "app", webdav.StagingDir, "tmp")
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
	n, err := drv.SweepAbandoned(ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("swept %d (%v), want 1", n, err)
	}
	for f, want := range map[string]bool{old: false, fresh: true, other: true} {
		if _, err := os.Stat(f); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(f), err == nil, want)
		}
	}
}

// fakeServer serves h for every request outside the staging collection and
// answers New's probe inside it as a case-sensitive server would, so a
// test can script one kind of answer.
func fakeServer(h http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/"+webdav.StagingDir) {
			h(w, r)
			return
		}
		switch r.Method {
		case "PROPFIND":
			w.WriteHeader(http.StatusNotFound)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
}

// TestCaseProbeMatchesHost pins New's probe: the test server stores files
// in a local temp dir, so the driver must declare exactly what the host
// filesystem does (folding on macOS APFS, distinct on Linux), and leave no
// probe file behind.
func TestCaseProbeMatchesHost(t *testing.T) {
	s := newServer(t)
	probe := filepath.Join(s.dir, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(s.dir, "PROBE"))
	hostFolds := err == nil
	drv := newDriver(t, s)
	if got := drv.Capabilities().CaseInsensitive; got != hostFolds {
		t.Fatalf("CaseInsensitive = %v, host filesystem folds case: %v", got, hostFolds)
	}
	left, err := os.ReadDir(filepath.Join(s.dir, "app", webdav.StagingDir, "tmp"))
	if err != nil || len(left) != 0 {
		t.Fatalf("probe left %v behind (%v)", left, err)
	}
}

// TestPlainHTTPNeedsOptIn pins that Basic credentials never go over plain
// http to another machine unless the configuration says so: refused up
// front without the opt-in, attempted (and failing only to resolve the
// host) with it, from code and from a DSN alike.
func TestPlainHTTPNeedsOptIn(t *testing.T) {
	ctx := context.Background()
	const remote = "http://dav.example.invalid/files"
	cfg := webdav.Config{Endpoint: remote, Username: testUser, Password: testPassword}
	if _, err := webdav.New(ctx, cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("plain http without the opt-in: %v, want CodeInvalidArgument", err)
	}
	cfg.AllowInsecureHTTP = true
	if _, err := webdav.New(ctx, cfg); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("plain http with the opt-in: %v, want an attempt (CodeUnavailable)", err)
	}
	for dsn, want := range map[string]filesystem.Code{
		"webdav://dav.example.invalid/files":                     filesystem.CodeInvalidArgument,
		"webdav://dav.example.invalid/files?insecure_http=true":  filesystem.CodeUnavailable,
		"webdav://dav.example.invalid/files?insecure_http=maybe": filesystem.CodeInvalidArgument,
	} {
		u, _ := url.Parse(dsn)
		if _, err := webdav.Opener(ctx, u); filesystem.CodeOf(err) != want {
			t.Errorf("%s: %v, want %s", dsn, err, want)
		}
	}
}

// TestRedirectsStayOnHost pins the redirect policy: a server answering
// with a redirect to another host never gets that host a request, so the
// credentials and bodies stay with the endpoint.
func TestRedirectsStayOnHost(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusMultiStatus)
	}))
	defer other.Close()
	srv := fakeServer(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	defer srv.Close()
	drv, err := webdav.New(context.Background(), webdav.Config{Endpoint: srv.URL, Username: testUser, Password: testPassword, Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(context.Background(), "k"); err == nil {
		t.Fatal("a redirected Stat succeeded")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host received %d requests", n)
	}
}

// TestRedirectLoopIsCapped pins that a server redirecting to itself stops
// after maxRedirects hops instead of looping.
func TestRedirectLoopIsCapped(t *testing.T) {
	var hops atomic.Int64
	srv := fakeServer(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
	})
	defer srv.Close()
	drv, err := webdav.New(context.Background(), webdav.Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(context.Background(), "k"); err == nil {
		t.Fatal("a redirect loop succeeded")
	}
	if n := hops.Load(); n > 11 {
		t.Fatalf("followed %d hops of a redirect loop", n)
	}
}
