package gdrive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/gdrive"
)

// match selects the requests an intercept answers.
type match func(r *http.Request) bool

// isList matches files.list; isListOf matches the files.list looking for
// one name.
func isList(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/drive/v3/files"
}

func isListOf(name string) match {
	return func(r *http.Request) bool {
		return isList(r) && strings.Contains(r.URL.Query().Get("q"), "name = '"+name+"'")
	}
}

// is matches a method on a path prefix.
func is(method, prefix string) match {
	return func(r *http.Request) bool { return r.Method == method && strings.HasPrefix(r.URL.Path, prefix) }
}

// isUploadChunk matches a chunk PUT of a resumable upload starting at offset.
func isUploadChunk(offset int) match {
	return func(r *http.Request) bool {
		return r.Method == http.MethodPut && r.URL.Query().Get("upload_id") != "" &&
			strings.HasPrefix(r.Header.Get("Content-Range"), "bytes "+strconv.Itoa(offset)+"-")
	}
}

// nth matches only the n-th request (from 1) that m matches.
func nth(n int, m match) match {
	seen := 0
	return func(r *http.Request) bool {
		if !m(r) {
			return false
		}
		seen++
		return seen == n
	}
}

// answer is an intercept answering the requests m matches with status.
func answer(m match, status int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"errors":[{"reason":"injected"}]}}`)
		return true
	}
}

// drop is an intercept dropping the connection of the requests m matches.
func drop(m match) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !m(r) {
			return false
		}
		hangUp(w, r)
		return true
	}
}

// elsewhere returns a client that delivers every connection to a host
// under example.com to a local recorder, and a count of the requests the
// recorder got: a test can hand the driver a foreign URL and see whether
// the driver would have sent anything there.
func elsewhere(t *testing.T) (*http.Client, func() int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	rec := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	t.Cleanup(rec.Close)
	var d net.Dialer
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.Contains(addr, "example.com") {
			addr = rec.Listener.Addr().String()
		}
		return d.DialContext(ctx, network, addr)
	}}}
	return client, func() int { mu.Lock(); defer mu.Unlock(); return hits }
}

// seeded returns a driver over a fake holding the given keys; every
// connection to a host under example.com reaches the recorder whose count
// the fake's hits returns.
func seeded(t *testing.T, chunk int, keys ...string) (*gdrive.Driver, *fake) {
	t.Helper()
	f := newFake(t)
	cfg := config(f)
	cfg.ChunkSize = chunk
	cfg.Client, f.hits = elsewhere(t)
	drv := newDriver(t, cfg)
	for _, k := range keys {
		if _, err := drv.Write(context.Background(), k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return drv, f
}

// TestConfigEdges pins that a Config with no token says so when printed,
// that New accepts https endpoints and refuses a plain-http upload host
// off the loopback interface.
func TestConfigEdges(t *testing.T) {
	if s := (gdrive.Config{}).String(); !strings.Contains(s, "none") {
		t.Fatalf("String() = %s, want the token reported as none", s)
	}
	if _, err := gdrive.New(gdrive.Config{Token: staticToken{"t"}, APIURL: "https://api.example.com", UploadURL: "https://up.example.com"}); err != nil {
		t.Fatalf("https endpoints: %v", err)
	}
	if _, err := gdrive.New(gdrive.Config{Token: staticToken{"t"}, UploadURL: "http://up.example.com"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("plain-http upload host: %v, want CodeInvalidArgument", err)
	}
}

// TestFailingTokenOnAWrite pins that a token source failing on a request
// with a body (here the upload start, after the name lookup got a token)
// is CodePermission and starts no upload.
func TestFailingTokenOnAWrite(t *testing.T) {
	f := newFake(t)
	cfg := config(f)
	calls := 0
	cfg.Token = gdrive.TokenFunc(func(context.Context) (string, error) {
		if calls++; calls > 1 {
			return "", errors.New("revoked")
		}
		return testToken, nil
	})
	if _, err := newDriver(t, cfg).Write(context.Background(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("got %v, want CodePermission", err)
	}
	if calls != 2 || len(f.uploads) != 0 {
		t.Fatalf("%d token calls, %d uploads; want the second call to fail before any upload", calls, len(f.uploads))
	}
}

// TestStatusFallbacks pins the statuses past the named reasons: 413 is
// CodeTooLarge, a 5xx is CodeUnavailable carrying Retry-After, anything
// else is CodeUnknown.
func TestStatusFallbacks(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0)
	f.on(answer(isList, http.StatusRequestEntityTooLarge))
	if _, err := drv.Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("413: %v, want CodeTooLarge", err)
	}
	f.on(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "4")
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	_, err := drv.Stat(ctx, "x")
	if d, ok := filesystem.RetryAfterOf(err); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !ok || d != 4*time.Second {
		t.Fatalf("503: %v (retry %v), want CodeUnavailable after 4s", err, d)
	}
	f.on(answer(isList, http.StatusTeapot))
	if _, err := drv.Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("418: %v, want CodeUnknown", err)
	}
}

// TestCancelledContext pins that a request whose context ended returns
// the context's own error, not a classified one.
func TestCancelledContext(t *testing.T) {
	drv, _ := seeded(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var fe *filesystem.Error
	if _, err := drv.Stat(ctx, "x"); !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("got %v, want the context error itself, unclassified", err)
	}
}

// TestStatEdges pins that a failing folder lookup is returned and that a
// key under a file (which cannot exist) is CodeNotFound.
func TestStatEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0, "file")
	if _, err := drv.Stat(ctx, "file/x"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("key under a file: %v, want CodeNotFound", err)
	}
	f.on(answer(isListOf("a"), http.StatusInternalServerError))
	if _, err := drv.Stat(ctx, "a/b"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing folder lookup: %v, want CodeUnavailable", err)
	}
}

// TestReadEdges pins Read's answers past the happy path: a dropped
// connection, a 200 that ignored the Range (cut here), a body cut short
// before the offset, and a refused download.
func TestReadEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0, "r")
	media := func(r *http.Request) bool { return r.URL.Query().Get("alt") == "media" }
	f.on(drop(media))
	if _, err := drv.Read(ctx, "r", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("dropped download: %v, want CodeUnavailable", err)
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !media(r) {
			return false
		}
		_, _ = io.WriteString(w, "0123456789")
		return true
	})
	for _, c := range []struct {
		rng  filesystem.ByteRange
		want string
	}{
		{filesystem.ByteRange{Offset: 2, Length: 3}, "234"},
		{filesystem.ByteRange{Offset: 7}, "789"},
	} {
		rc, err := drv.Read(ctx, "r", c.rng)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != c.want {
			t.Errorf("Read(%+v) = %q, want %q", c.rng, got, c.want)
		}
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !media(r) {
			return false
		}
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "01234")
		return true
	})
	if _, err := drv.Read(ctx, "r", filesystem.ByteRange{Offset: 50}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("truncated answer: %v, want CodeUnavailable", err)
	}
	f.on(answer(media, http.StatusForbidden))
	if _, err := drv.Read(ctx, "r", filesystem.ByteRange{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused download: %v, want CodePermission", err)
	}
}

// TestWriteFolderFailures pins the failures of finding or creating a
// key's folders and of looking up the key: a file in the way, a folder
// lookup failing mid-creation, a refused folder creation and a failing
// lookup of the name. None of them uploads anything.
func TestWriteFolderFailures(t *testing.T) {
	ctx := context.Background()
	var nthFile match // reset per case, after the seeding lookups
	cases := map[string]struct {
		key       string
		intercept func(http.ResponseWriter, *http.Request) bool
		want      filesystem.Code
	}{
		"file in the way": {"file/x", nil, filesystem.CodeInvalidArgument},
		// The first lookup misses the file, which appears before the
		// creation loop looks again: a concurrent writer's file.
		"file appears": {"file/x", func(w http.ResponseWriter, r *http.Request) bool {
			if !nthFile(r) {
				return false
			}
			_, _ = io.WriteString(w, `{"files":[]}`)
			return true
		}, filesystem.CodeInvalidArgument},
		"lookup fails":     {"newdir/x", answer(nth(2, isListOf("newdir")), http.StatusInternalServerError), filesystem.CodeUnavailable},
		"creation refused": {"newdir/x", answer(is(http.MethodPost, "/drive/v3/files"), http.StatusForbidden), filesystem.CodePermission},
		"name lookup":      {"k", answer(isListOf("k"), http.StatusInternalServerError), filesystem.CodeUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			drv, f := seeded(t, 0, "file")
			nthFile = nth(1, isListOf("file"))
			f.on(c.intercept)
			if _, err := drv.Write(ctx, c.key, strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			if len(f.uploads) != 0 {
				t.Fatal("an upload session was started")
			}
		})
	}
}

// TestResumableFailures pins every failure of a resumable upload: the
// start dropped or refused, a session URL off the upload host, a refused
// intermediate chunk and a refused final chunk. Each fails the write and
// no file appears.
func TestResumableFailures(t *testing.T) {
	ctx := context.Background()
	unit := gdrive.ChunkUnit
	start := func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/")
	}
	cases := map[string]struct {
		intercept func(http.ResponseWriter, *http.Request) bool
		want      filesystem.Code
	}{
		"start dropped": {drop(start), filesystem.CodeUnavailable},
		"start refused": {answer(start, http.StatusForbidden), filesystem.CodePermission},
		"foreign session": {func(w http.ResponseWriter, r *http.Request) bool {
			if !start(r) {
				return false
			}
			w.Header().Set("Location", "http://elsewhere.example.com/upload?upload_id=1")
			return true
		}, filesystem.CodeUnavailable},
		"intermediate refused": {answer(isUploadChunk(0), http.StatusBadRequest), filesystem.CodeInvalidArgument},
		"final refused":        {answer(isUploadChunk(unit), http.StatusInternalServerError), filesystem.CodeUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			drv, f := seeded(t, unit)
			f.on(c.intercept)
			body := bytes.NewReader(make([]byte, 2*unit))
			if _, err := drv.Write(ctx, "big", body, filesystem.WriteOptions{}); filesystem.CodeOf(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			if len(f.items) != 0 {
				t.Fatalf("a failed upload left %d items", len(f.items))
			}
			if n := f.hits(); n != 0 {
				t.Fatalf("%d requests went to a host off the upload origin", n)
			}
		})
	}
}

// TestDeleteEdges pins that a file gone before its DELETE is a success, and
// that a failing lookup or a refused DELETE is returned.
func TestDeleteEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0, "k")
	f.on(answer(is(http.MethodDelete, "/drive/v3/files/"), http.StatusNotFound))
	if err := drv.Delete(ctx, "k"); err != nil {
		t.Fatalf("vanished file: %v, want success", err)
	}
	f.on(answer(is(http.MethodDelete, "/drive/v3/files/"), http.StatusForbidden))
	if err := drv.Delete(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused delete: %v, want CodePermission", err)
	}
	f.on(answer(isList, http.StatusInternalServerError))
	if err := drv.Delete(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing lookup: %v, want CodeUnavailable", err)
	}
}

// TestRelocateEdges pins Copy and Move past the happy path: onto
// themselves (the object, untouched), under a file, a failing lookup of
// dst, a failing lookup of src's folder (Move), a refused copy, and a
// refused delete of the file dst named before.
func TestRelocateEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0, "s", "a/s", "file", "old")
	if e, err := drv.Copy(ctx, "s", "s"); err != nil || e.Key != "s" {
		t.Fatalf("copy onto itself: %+v, %v", e, err)
	}
	if _, err := drv.Copy(ctx, "s", "file/x"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("copy under a file: %v, want CodeInvalidArgument", err)
	}
	f.on(answer(isListOf("dst"), http.StatusInternalServerError))
	if _, err := drv.Copy(ctx, "s", "dst"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing dst lookup: %v, want CodeUnavailable", err)
	}
	f.on(answer(nth(2, isListOf("a")), http.StatusInternalServerError))
	if _, err := drv.Move(ctx, "a/s", "t"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing src folder lookup: %v, want CodeUnavailable", err)
	}
	f.on(answer(is(http.MethodPost, "/drive/v3/files/"), http.StatusInsufficientStorage))
	if _, err := drv.Copy(ctx, "s", "c"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("refused copy: %v, want CodeUnavailable", err)
	}
	f.on(answer(is(http.MethodDelete, "/drive/v3/files/"), http.StatusForbidden))
	if _, err := drv.Copy(ctx, "s", "old"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused delete of the old dst: %v, want CodePermission", err)
	}
}

// TestListEdges pins that a failing page fails the listing and that names
// no clean key can hold are skipped.
func TestListEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0)
	f.on(answer(isList, http.StatusInternalServerError))
	if _, err := drv.List(ctx, "", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing page: %v, want CodeUnavailable", err)
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !isList(r) {
			return false
		}
		_, _ = io.WriteString(w, `{"files":[{"id":"1","name":""},{"id":"2","name":"."},{"id":"3","name":".."},{"id":"4","name":"a/b"},{"id":"5","name":"ok","size":"2"}]}`)
		return true
	})
	p, err := drv.List(ctx, "", filesystem.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "ok" {
		t.Fatalf("entries %+v, want only ok", p.Entries)
	}
}

// TestDeletePrefixEdges pins DeletePrefix past the happy path: a missing
// parent folder is a success, and a failing parent lookup, a failing
// lookup of the folder and a refused delete are returned.
func TestDeletePrefixEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, 0, "d/x")
	if err := drv.DeletePrefix(ctx, "nope/x"); err != nil {
		t.Fatalf("missing parent: %v", err)
	}
	f.on(answer(isListOf("p"), http.StatusInternalServerError))
	if err := drv.DeletePrefix(ctx, "p/q"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing parent lookup: %v, want CodeUnavailable", err)
	}
	f.on(answer(isListOf("d"), http.StatusInternalServerError))
	if err := drv.DeletePrefix(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing folder lookup: %v, want CodeUnavailable", err)
	}
	f.on(answer(is(http.MethodDelete, "/drive/v3/files/"), http.StatusForbidden))
	if err := drv.DeletePrefix(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("refused delete: %v, want CodePermission", err)
	}
}
