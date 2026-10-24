package webdav_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/webdav"
)

// match selects the requests an intercept answers.
type match func(r *http.Request) bool

// req matches method on a path ending in suffix ("" for any path).
func req(method, suffix string) match {
	return func(r *http.Request) bool {
		return r.Method == method && strings.HasSuffix(r.URL.Path, suffix)
	}
}

// answer answers the requests m matches with status.
func answer(m match, status int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		w.WriteHeader(status)
		return true
	}
}

// drop hangs up on the requests m matches, a transport failure.
func drop(m match) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return true
	}
}

// multistatus answers the requests m matches with a 207 holding body.
func multistatus(m match, body string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, body)
		return true
	}
}

// seeded returns a server and a driver holding the given keys.
func seeded(t *testing.T, keys ...string) (*server, *webdav.Driver) {
	t.Helper()
	s := newServer(t)
	drv := newDriver(t, s)
	for _, k := range keys {
		if _, err := drv.Write(t.Context(), k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return s, drv
}

// TestReservedKeysAreRefused pins that every operation refuses a key in
// the staging collection before any request.
func TestReservedKeysAreRefused(t *testing.T) {
	s, drv := seeded(t)
	ctx := t.Context()
	k := webdav.StagingDir + "/tmp/x"
	errs := map[string]error{}
	_, errs["Stat"] = drv.Stat(ctx, k)
	_, errs["Read"] = drv.Read(ctx, k, filesystem.ByteRange{})
	_, errs["Write"] = drv.Write(ctx, k, strings.NewReader("x"), filesystem.WriteOptions{})
	errs["Delete"] = drv.Delete(ctx, k)
	_, errs["Copy"] = drv.Copy(ctx, "a", k)
	_, errs["List"] = drv.List(ctx, webdav.StagingDir, filesystem.ListOptions{})
	errs["DeletePrefix"] = drv.DeletePrefix(ctx, webdav.StagingDir)
	for name, err := range errs {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("%s: %v, want CodeInvalidKey", name, err)
		}
	}
	if n := s.counts["PROPFIND"].Load() + s.counts["PUT"].Load(); n > 2 {
		t.Fatalf("reserved keys reached the server (%d requests beyond New's probe)", n)
	}
}

// TestNewRefusals pins New's remaining refusals and acceptances: an
// endpoint that is not an http(s) URL, one carrying credentials, and plain
// http on the name localhost (accepted, like a loopback address).
func TestNewRefusals(t *testing.T) {
	for _, ep := range []string{"ftp://h/x", "https://user:pw@h/x"} {
		if _, err := webdav.New(t.Context(), webdav.Config{Endpoint: ep}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", ep, err)
		}
	}
	s := newServer(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	cfg := s.config()
	cfg.Endpoint = "http://localhost:" + port + davPrefix + "/app"
	if _, err := webdav.New(t.Context(), cfg); err != nil {
		t.Fatalf("localhost: %v", err)
	}
}

// TestCallersRedirectPolicyStillApplies pins that the driver's same-origin
// check runs first and then the caller's own CheckRedirect.
func TestCallersRedirectPolicyStillApplies(t *testing.T) {
	s, _ := seeded(t, "k")
	refused := errors.New("no redirects here")
	called := false
	cfg := s.config()
	cfg.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { called = true; return refused }}
	drv, err := webdav.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.on(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/old") {
			http.Redirect(w, r, strings.Replace(r.URL.Path, "/old", "/k", 1), http.StatusFound)
			return true
		}
		return false
	})
	if _, err := drv.Read(t.Context(), "old", filesystem.ByteRange{}); !called || err == nil {
		t.Fatalf("caller's policy called %v, Read error %v", called, err)
	}
}

// TestCaseProbeEdges pins New's probe on servers that refuse it: a
// read-only account (MKCOL and PUT refused, the collection readable) is
// case-sensitive and works; a refused MKCOL with an unreadable collection,
// a PUT failing otherwise, and a dropped PUT fail New.
func TestCaseProbeEdges(t *testing.T) {
	staging := "/" + webdav.StagingDir + "/"
	cases := map[string]struct {
		intercept func(http.ResponseWriter, *http.Request) bool
		ok        bool
	}{
		"read-only account": {answer(func(r *http.Request) bool { return r.Method == "MKCOL" }, http.StatusForbidden), true},
		"put forbidden":     {answer(req(http.MethodPut, ""), http.StatusForbidden), true},
		"unreadable": {func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "MKCOL" || r.Method == "PROPFIND" {
				w.WriteHeader(http.StatusForbidden)
				return true
			}
			return false
		}, false},
		"put fails": {answer(func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.Contains(r.URL.Path, staging)
		}, http.StatusInternalServerError), false},
		"put dropped": {drop(func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.Contains(r.URL.Path, staging)
		}), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newServer(t)
			if err := os.MkdirAll(filepath.Join(s.dir, "app"), 0o700); err != nil {
				t.Fatal(err)
			}
			s.on(c.intercept)
			drv, err := webdav.New(t.Context(), s.config())
			if (err == nil) != c.ok {
				t.Fatalf("New = %v, want success %v", err, c.ok)
			}
			if err == nil && drv.Capabilities().CaseInsensitive {
				t.Fatal("a server refusing the probe was reported case-insensitive")
			}
		})
	}
}

// TestStatusCodes pins the statuses the classification test does not
// reach: 412 is CodeExists and 413 CodeTooLarge.
func TestStatusCodes(t *testing.T) {
	s, drv := seeded(t)
	for status, want := range map[int]filesystem.Code{
		http.StatusPreconditionFailed:    filesystem.CodeExists,
		http.StatusRequestEntityTooLarge: filesystem.CodeTooLarge,
	} {
		s.on(answer(req("PROPFIND", ""), status))
		if _, err := drv.Stat(t.Context(), "k"); filesystem.CodeOf(err) != want {
			t.Errorf("%d: %v, want %s", status, err, want)
		}
	}
}

// TestPropfindAnswers pins how a multistatus is read: an answer that is
// not XML is CodeUnavailable, an href that does not parse is dropped, and
// the endpoint's own path without its trailing slash is the root.
func TestPropfindAnswers(t *testing.T) {
	s, drv := seeded(t, "d/k")
	s.on(multistatus(req("PROPFIND", ""), "<not xml"))
	if _, err := drv.Stat(t.Context(), "d/k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("garbled multistatus: %v, want CodeUnavailable", err)
	}
	const ok = `<D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>`
	s.on(multistatus(req("PROPFIND", ""), `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`+
		`<D:response><D:href>`+davPrefix+`/app</D:href>`+ok+`</D:response>`+
		`<D:response><D:href>%zz</D:href>`+ok+`</D:response>`+
		`</D:multistatus>`))
	p, err := drv.List(t.Context(), "", filesystem.ListOptions{Recursive: true})
	if err != nil || len(p.Entries) != 0 {
		t.Fatalf("listing %+v, %v; want the root alone, nothing listed", p.Entries, err)
	}
}

// TestStatOfACollection pins that a collection is not an object.
func TestStatOfACollection(t *testing.T) {
	_, drv := seeded(t, "d/k")
	if _, err := drv.Stat(t.Context(), "d"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("got %v, want CodeNotFound", err)
	}
}

// TestReadEdges pins Read's transport failure and a body cut short while
// it skips to the range's offset on a server that ignores Range.
func TestReadEdges(t *testing.T) {
	s, drv := seeded(t, "k")
	s.on(drop(req(http.MethodGet, "/k")))
	if _, err := drv.Read(t.Context(), "k", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("dropped GET: %v, want CodeUnavailable", err)
	}
	s.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !req(http.MethodGet, "/k")(r) {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(100))
		_, _ = io.WriteString(w, "01234")
		return true
	})
	if _, err := drv.Read(t.Context(), "k", filesystem.ByteRange{Offset: 50}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("truncated answer: %v, want CodeUnavailable", err)
	}
}

// TestWriteEdges pins the staging PUT's failures: a 409 (the staging
// collection was removed) is CodeUnavailable and the next write recreates
// it; any other refusal is classified and leaves no staged file; a
// failing MOVE removes the staged file.
func TestWriteEdges(t *testing.T) {
	ctx := t.Context()
	staging := "/" + webdav.StagingDir + "/tmp/"
	s, drv := seeded(t)
	s.on(answer(func(r *http.Request) bool { return r.Method == http.MethodPut && strings.Contains(r.URL.Path, staging) }, http.StatusConflict))
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("409 on the staging PUT: %v, want CodeUnavailable", err)
	}
	s.on(nil)
	mkcols := s.counts["MKCOL"].Load()
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if s.counts["MKCOL"].Load() == mkcols {
		t.Fatal("the retry did not recreate the staging collection")
	}
	s.on(answer(req(http.MethodPut, ""), http.StatusInsufficientStorage))
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("507 PUT: %v, want CodeQuotaExceeded", err)
	}
	s.on(answer(req("MOVE", ""), http.StatusLocked))
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("423 MOVE: %v, want CodeUnavailable", err)
	}
	s.on(nil)
	left, err := os.ReadDir(filepath.Join(s.dir, "app", webdav.StagingDir, "tmp"))
	if err != nil || len(left) != 0 {
		t.Fatalf("staged files left: %v (%v)", left, err)
	}
}

// TestStagingCollectionCannotBeMade pins that a write whose staging
// collection cannot be recreated fails with the MKCOL's refusal.
func TestStagingCollectionCannotBeMade(t *testing.T) {
	staging := "/" + webdav.StagingDir + "/tmp/"
	s, drv := seeded(t)
	s.on(answer(func(r *http.Request) bool { return r.Method == http.MethodPut && strings.Contains(r.URL.Path, staging) }, http.StatusConflict))
	_, _ = drv.Write(t.Context(), "k", strings.NewReader("x"), filesystem.WriteOptions{})
	s.on(answer(req("MKCOL", ""), http.StatusInsufficientStorage))
	if _, err := drv.Write(t.Context(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("got %v, want CodeQuotaExceeded", err)
	}
}

// TestRelocateEdges pins COPY and MOVE failures: a dropped request, a
// refused creation of dst's parents, and a refusal after the retry.
func TestRelocateEdges(t *testing.T) {
	ctx := t.Context()
	s, drv := seeded(t, "src")
	s.on(drop(req("COPY", "")))
	if _, err := drv.Copy(ctx, "src", "dst"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("dropped COPY: %v", err)
	}
	s.on(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.Method {
		case "COPY":
			w.WriteHeader(http.StatusConflict)
		case "MKCOL":
			w.WriteHeader(http.StatusForbidden)
		default:
			return false
		}
		return true
	})
	if _, err := drv.Copy(ctx, "src", "new/dst"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused parents: %v, want CodePermission", err)
	}
	s.on(answer(req("MOVE", ""), http.StatusForbidden))
	if _, err := drv.Move(ctx, "src", "dst"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused MOVE: %v, want CodePermission", err)
	}
}

// TestDeleteEdges pins that a failing stat and a dropped DELETE fail
// Delete.
func TestDeleteEdges(t *testing.T) {
	s, drv := seeded(t, "k")
	s.on(answer(req("PROPFIND", "/k"), http.StatusInternalServerError))
	if err := drv.Delete(t.Context(), "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing stat: %v", err)
	}
	s.on(drop(req(http.MethodDelete, "/k")))
	if err := drv.Delete(t.Context(), "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("dropped DELETE: %v", err)
	}
}

// TestListAndDeletePrefixEdges pins List's failure and DeletePrefix's
// paths: a missing collection, a failing lookup, a file named like the
// prefix, and the root emptied child by child (keeping the staging
// collection, failing on a refused child).
func TestListAndDeletePrefixEdges(t *testing.T) {
	ctx := t.Context()
	s, drv := seeded(t, "file", "d/k")
	s.on(answer(req("PROPFIND", ""), http.StatusBadGateway))
	if _, err := drv.List(ctx, "", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing listing: %v", err)
	}
	if err := drv.DeletePrefix(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing lookup: %v", err)
	}
	if err := drv.DeletePrefix(ctx, ""); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing root listing: %v", err)
	}
	s.on(nil)
	if err := drv.DeletePrefix(ctx, "missing"); err != nil {
		t.Fatalf("missing: %v", err)
	}
	if err := drv.DeletePrefix(ctx, "file"); err != nil {
		t.Fatalf("a file named like the prefix: %v", err)
	}
	if _, err := drv.Stat(ctx, "file"); err != nil {
		t.Fatalf("the file named like the prefix is gone: %v", err)
	}
	s.on(answer(req(http.MethodDelete, ""), http.StatusForbidden))
	if err := drv.DeletePrefix(ctx, ""); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused child: %v, want CodePermission", err)
	}
	s.on(nil)
	if err := drv.DeletePrefix(ctx, ""); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(filepath.Join(s.dir, "app"))
	if err != nil || len(left) != 1 || left[0].Name() != webdav.StagingDir {
		t.Fatalf("left %v (%v), want only the staging collection", left, err)
	}
}

// TestCancelledRequest pins that a request whose context ended returns
// the context's own error, unclassified.
func TestCancelledRequest(t *testing.T) {
	_, drv := seeded(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var fe *filesystem.Error
	if _, err := drv.Stat(ctx, "k"); !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("got %v, want the context error itself", err)
	}
}

// TestOpenerSchemes pins the Opener's schemes: webdavs is https (refused
// by a plain server), any other scheme is refused.
func TestOpenerSchemes(t *testing.T) {
	s := newServer(t)
	u, err := url.Parse("webdavs://" + strings.TrimPrefix(s.URL, "http://") + davPrefix)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := webdav.Opener(ctx, u); err == nil {
		t.Fatal("webdavs succeeded against a plain http server")
	}
	u.Scheme = "http"
	if _, err := webdav.Opener(t.Context(), u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("http: %v, want CodeInvalidArgument", err)
	}
}
