package onedrive_test

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
	"github.com/ubgo/filesystem/adapters/onedrive"
)

// answer is an intercept that answers every request for which match holds
// with status and a Graph error body.
func answer(match func(r *http.Request) bool, status int) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !match(r) {
			return false
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"code":"injected"}}`)
		return true
	}
}

// method matches requests by method and URL path suffix.
func method(m, suffix string) func(r *http.Request) bool {
	return func(r *http.Request) bool { return r.Method == m && strings.HasSuffix(r.URL.Path, suffix) }
}

// seeded returns a driver over a fake holding the given keys.
func seeded(t *testing.T, cfg func(*onedrive.Config), keys ...string) (*onedrive.Driver, *fake) {
	t.Helper()
	f := newFake(t)
	c := config(f)
	if cfg != nil {
		cfg(&c)
	}
	drv := newDriver(t, c)
	for _, k := range keys {
		if _, err := drv.Write(context.Background(), k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return drv, f
}

// TestConfigEdges pins that a Config with no token says so when printed,
// and that New refuses a base URL that is neither https nor http.
func TestConfigEdges(t *testing.T) {
	if s := (onedrive.Config{}).String(); !strings.Contains(s, "none") {
		t.Fatalf("String() = %s, want the token reported as none", s)
	}
	if _, err := onedrive.New(onedrive.Config{Token: staticToken{"t"}, BaseURL: "ftp://graph.example.com"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("ftp base URL: %v, want CodeInvalidArgument", err)
	}
}

// TestFailingTokenOnAWrite pins that a token source failing on a request
// with a body is CodePermission and sends nothing.
func TestFailingTokenOnAWrite(t *testing.T) {
	f := newFake(t)
	cfg := config(f)
	cfg.Token = onedrive.TokenFunc(func(context.Context) (string, error) { return "", errors.New("revoked") })
	if _, err := newDriver(t, cfg).Write(context.Background(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("got %v, want CodePermission", err)
	}
	if n := f.requests["PUT /content"]; n != 0 {
		t.Fatalf("%d uploads reached the server", n)
	}
}

// TestDriveRootWithoutAFolder pins the item URL of the drive's root when
// Config.Root is empty: keys at the top of the drive list and stat.
func TestDriveRootWithoutAFolder(t *testing.T) {
	ctx := context.Background()
	drv, _ := seeded(t, func(c *onedrive.Config) { c.Root = "" }, "top", "d/inner")
	p, err := drv.List(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 || p.Entries[0].Key != "d/inner" || p.Entries[1].Key != "top" {
		t.Fatalf("entries %+v", p.Entries)
	}
}

// TestStatusFallbacks pins the classification past the named statuses: a
// 5xx is CodeUnavailable carrying Retry-After, any other status is
// CodeUnknown.
func TestStatusFallbacks(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, nil)
	f.on(func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	_, err := drv.Stat(ctx, "x")
	if d, ok := filesystem.RetryAfterOf(err); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !ok || d != 5*time.Second {
		t.Fatalf("502: %v (retry %v), want CodeUnavailable after 5s", err, d)
	}
	f.on(answer(func(*http.Request) bool { return true }, http.StatusTeapot))
	if _, err := drv.Stat(ctx, "x"); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("418: %v, want CodeUnknown", err)
	}
}

// TestCancelledContext pins that a request whose context ended returns
// the context's own error, not a classified one.
func TestCancelledContext(t *testing.T) {
	drv, _ := seeded(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var fe *filesystem.Error
	if _, err := drv.Stat(ctx, "x"); !errors.Is(err, context.Canceled) || errors.As(err, &fe) {
		t.Fatalf("got %v, want the context error itself, unclassified", err)
	}
}

// TestStatOfAFolderIsNotFound pins that a folder is not an object.
func TestStatOfAFolderIsNotFound(t *testing.T) {
	drv, _ := seeded(t, nil, "dir/f")
	if _, err := drv.Stat(context.Background(), "dir"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("got %v, want CodeNotFound", err)
	}
}

// TestReadWhenTheHostIgnoresRange pins the client-side cut of a 200 that
// answers a ranged read with the whole file, and that a body cut short
// before the range's offset is CodeUnavailable.
func TestReadWhenTheHostIgnoresRange(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, nil)
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/download/") && !strings.HasSuffix(r.URL.Path, ":/content") {
			return false
		}
		if strings.HasSuffix(r.URL.Path, ":/content") {
			http.Redirect(w, r, f.files.URL+"/download/x", http.StatusFound)
			return true
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
		if strings.HasSuffix(r.URL.Path, ":/content") {
			http.Redirect(w, r, f.files.URL+"/download/x", http.StatusFound)
			return true
		}
		w.Header().Set("Content-Length", strconv.Itoa(100))
		_, _ = io.WriteString(w, "01234")
		return true
	})
	if _, err := drv.Read(ctx, "r", filesystem.ByteRange{Offset: 50}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("truncated answer: %v, want CodeUnavailable", err)
	}
}

// TestPutTransportFailure pins that a dropped connection on a one-request
// upload is CodeUnavailable.
func TestPutTransportFailure(t *testing.T) {
	drv, f := seeded(t, nil)
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		hangUp(w, r)
		return true
	})
	if _, err := drv.Write(context.Background(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
}

// elsewhere returns a client that delivers every connection to a host
// under example.com to a local recorder, and a count of the requests the
// recorder got: a test can then hand the driver a foreign or plain-http URL
// and see whether the driver would have sent anything there.
func elsewhere(t *testing.T) (*http.Client, func() int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	rec := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"value":[]}`)
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

// TestSessionURLMustBeSecure pins that an upload URL that is not https
// (nor http on loopback) is refused before any byte is sent to it.
func TestSessionURLMustBeSecure(t *testing.T) {
	client, hits := elsewhere(t)
	drv, f := seeded(t, func(c *onedrive.Config) { c.ChunkSize, c.Client = onedrive.FragmentUnit, client })
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, ":/createUploadSession") {
			return false
		}
		_, _ = io.WriteString(w, `{"uploadUrl":"http://upload.example.com/s"}`)
		return true
	})
	body := bytes.NewReader(make([]byte, 2*onedrive.FragmentUnit))
	if _, err := drv.Write(context.Background(), "big", body, filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
	if n := hits(); n != 0 {
		t.Fatalf("%d requests sent to the plain-http upload URL", n)
	}
}

// TestSessionFragmentFailures pins every fragment failure of an upload
// session: a refused intermediate fragment, a refused final fragment of a
// body that is a whole number of chunks, and a dropped or refused last
// fragment of one that is not. Each fails the write and commits nothing.
func TestSessionFragmentFailures(t *testing.T) {
	unit := onedrive.FragmentUnit
	// fragmentAt matches the upload PUT starting at offset.
	fragmentAt := func(offset int) func(r *http.Request) bool {
		return func(r *http.Request) bool {
			return r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/") &&
				strings.HasPrefix(r.Header.Get("Content-Range"), "bytes "+strconv.Itoa(offset)+"-")
		}
	}
	cases := map[string]struct {
		size      int
		intercept func(w http.ResponseWriter, r *http.Request) bool
		want      filesystem.Code
	}{
		"intermediate refused": {2 * unit, answer(fragmentAt(0), http.StatusInternalServerError), filesystem.CodeUnavailable},
		"final refused":        {2 * unit, answer(fragmentAt(unit), http.StatusInsufficientStorage), filesystem.CodeQuotaExceeded},
		"last refused":         {2*unit + unit/2, answer(fragmentAt(2*unit), http.StatusInsufficientStorage), filesystem.CodeQuotaExceeded},
		"last dropped": {2*unit + unit/2, func(w http.ResponseWriter, r *http.Request) bool {
			if !fragmentAt(2 * unit)(r) {
				return false
			}
			hangUp(w, r)
			return true
		}, filesystem.CodeUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			drv, f := seeded(t, func(cfg *onedrive.Config) { cfg.ChunkSize = unit })
			f.on(c.intercept)
			body := bytes.NewReader(make([]byte, c.size))
			if _, err := drv.Write(context.Background(), "big", body, filesystem.WriteOptions{}); filesystem.CodeOf(err) != c.want {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.items["apps/test/big"] != nil {
				t.Fatal("a failed session committed the file")
			}
		})
	}
}

// TestDeleteEdges pins that a failing stat is returned and that an item
// gone between the stat and the delete is a success.
func TestDeleteEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, nil, "k")
	f.on(answer(method(http.MethodDelete, ":"), http.StatusNotFound))
	if err := drv.Delete(ctx, "k"); err != nil {
		t.Fatalf("vanished item: %v, want success", err)
	}
	f.on(answer(method(http.MethodGet, ":"), http.StatusServiceUnavailable))
	if err := drv.Delete(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing stat: %v, want CodeUnavailable", err)
	}
}

// TestMoveEdges pins Move onto itself (the object, untouched), a move to
// the top of the root (no folder to create), and the failures of creating
// dst's folders and of the PATCH itself.
func TestMoveEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, nil, "a/x", "b")
	if e, err := drv.Move(ctx, "b", "b"); err != nil || e.Key != "b" {
		t.Fatalf("move onto itself: %+v, %v", e, err)
	}
	if e, err := drv.Move(ctx, "a/x", "top"); err != nil || e.Key != "top" || f.requests["POST /children"] != 0 {
		t.Fatalf("move to the top: %+v, %v, %d folders created", e, err, f.requests["POST /children"])
	}
	f.on(answer(method(http.MethodPost, ":/children"), http.StatusForbidden))
	if _, err := drv.Move(ctx, "top", "new/dir/k"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing folder creation: %v, want CodePermission", err)
	}
	f.on(answer(method(http.MethodPatch, ":"), http.StatusInsufficientStorage))
	if _, err := drv.Move(ctx, "top", "b"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("failing PATCH: %v, want CodeQuotaExceeded", err)
	}
}

// TestListEdges pins that a failing page fails the listing, that names
// this driver could not have written are skipped, and that a nextLink
// leaving the Graph origin is refused rather than followed with the token.
func TestListEdges(t *testing.T) {
	ctx := context.Background()
	client, hits := elsewhere(t)
	drv, f := seeded(t, func(c *onedrive.Config) { c.Client = client }, "d/ok")
	f.on(answer(method(http.MethodGet, ":/children"), http.StatusInternalServerError))
	if _, err := drv.List(ctx, "d", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing page: %v, want CodeUnavailable", err)
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !method(http.MethodGet, ":/children")(r) {
			return false
		}
		_, _ = io.WriteString(w, `{"value":[{"name":"bad%zz","file":{}},{"name":"","file":{}},{"name":"ok","file":{},"size":2}]}`)
		return true
	})
	p, err := drv.List(ctx, "d", filesystem.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Key != "d/ok" {
		t.Fatalf("entries %+v, want only d/ok", p.Entries)
	}
	f.on(func(w http.ResponseWriter, r *http.Request) bool {
		if !method(http.MethodGet, ":/children")(r) {
			return false
		}
		_, _ = io.WriteString(w, `{"value":[],"@odata.nextLink":"http://elsewhere.example.com/v1.0/next"}`)
		return true
	})
	if _, err := drv.List(ctx, "d", filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("foreign nextLink: %v, want CodeUnavailable", err)
	}
	if n := hits(); n != 0 {
		t.Fatalf("%d requests followed the foreign nextLink", n)
	}
}

// TestDeletePrefixEdges pins that a missing folder is a success and a
// failing lookup is returned.
func TestDeletePrefixEdges(t *testing.T) {
	ctx := context.Background()
	drv, f := seeded(t, nil)
	if err := drv.DeletePrefix(ctx, "missing"); err != nil {
		t.Fatalf("missing folder: %v", err)
	}
	f.on(answer(method(http.MethodGet, ":"), http.StatusUnauthorized))
	if err := drv.DeletePrefix(ctx, "d"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing lookup: %v, want CodePermission", err)
	}
}

// TestControlCharacterInDrive pins that a Drive path New lets through but
// no URL can hold (a control character) fails with CodeInvalidArgument
// before any request is sent.
func TestControlCharacterInDrive(t *testing.T) {
	f := newFake(t)
	cfg := config(f)
	cfg.Drive = "/me/dri\x7fve"
	if _, err := newDriver(t, cfg).Stat(context.Background(), "k"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("got %v, want CodeInvalidArgument", err)
	}
	if len(f.requests) != 0 {
		t.Fatalf("requests reached the server: %v", f.requests)
	}
}
