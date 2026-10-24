package remote_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// faults makes chosen operations of a memory disk fail: each armed kind
// fails its next n calls with code (n < 0: every call).
type faults struct {
	mu   sync.Mutex
	arms map[filesystem.OpKind]struct {
		n    int
		code filesystem.Code
	}
}

func (f *faults) arm(kind filesystem.OpKind, n int, code filesystem.Code) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arms[kind] = struct {
		n    int
		code filesystem.Code
	}{n, code}
}

func (f *faults) intercept(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
	f.mu.Lock()
	a, ok := f.arms[op.Kind]
	fail := ok && a.n != 0
	if fail && a.n > 0 {
		a.n--
		f.arms[op.Kind] = a
	}
	f.mu.Unlock()
	if fail {
		return filesystem.Errorf(a.code, "injected %s failure", op.Kind)
	}
	return call(ctx)
}

// faultyCache returns a cache (as newCache) on a memory disk whose
// operations f can make fail.
func faultyCache(t *testing.T, mutate ...func(*remote.Options)) (*remote.Cache, *filesystem.Disk, *faults) {
	t.Helper()
	f := &faults{arms: map[filesystem.OpKind]struct {
		n    int
		code filesystem.Code
	}{}}
	disk, err := filesystem.NewDisk("cache", memory.New(), filesystem.WithInterceptors(f.intercept))
	if err != nil {
		t.Fatal(err)
	}
	o := remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}}
	for _, m := range mutate {
		m(&o)
	}
	c, err := remote.New(disk, o)
	if err != nil {
		t.Fatal(err)
	}
	return c, disk, f
}

// TestNewRefusesBadArguments pins New's remaining refusals: no disk and a
// negative limit.
func TestNewRefusesBadArguments(t *testing.T) {
	if _, err := remote.New(nil, remote.Options{Dir: "r", Policy: imagePolicy()}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("no disk: %v", err)
	}
	for name, o := range map[string]remote.Options{
		"page bytes": {MaxPageBytes: -1},
		"tags":       {MaxTags: -1},
		"fetches":    {MaxConcurrentFetches: -1},
	} {
		o.Dir, o.Policy = "r", imagePolicy()
		if _, err := remote.New(newDisk(t), o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("negative %s: %v", name, err)
		}
	}
}

// TestBadURLsAreRefusedEverywhere pins that every entry point refuses a
// URL that does not parse before touching the disk or the network.
func TestBadURLsAreRefusedEverywhere(t *testing.T) {
	ctx := context.Background()
	c, _ := newCache(t)
	const bad = "http://[::1"
	if _, err := remote.Canonical(bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("Canonical: %v", err)
	}
	errs := map[string]error{}
	_, errs["MediaKey"] = c.MediaKey(bad)
	_, errs["Get"] = c.Get(ctx, bad)
	_, _, errs["Open"] = c.Open(ctx, bad)
	errs["Purge"] = c.Purge(ctx, bad)
	_, errs["LinkPreview"] = c.LinkPreview(ctx, bad)
	p := newProxy(t, c, newClock())
	_, errs["Proxy.URL"] = p.URL(ctx, bad, time.Minute)
	for name, err := range errs {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", name, err)
		}
	}
}

// TestDiskFailuresAreReturned pins that a failing Stat (other than not
// found) and a failing read of a hit's first bytes are returned by Get,
// not taken for a miss that would fetch again.
func TestDiskFailuresAreReturned(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	c, _, f := faultyCache(t)
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	f.arm(filesystem.OpStat, -1, filesystem.CodePermission)
	if _, err := c.Get(ctx, srv.URL+"/a.png"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing stat: %v, want CodePermission", err)
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("%d fetches, want the failing stat not to refetch", srv.hits.Load())
	}
}

// typeless is a memory driver that stores no content type, as the local
// driver does, and whose reads fail after their first byte once broken.
type typeless struct {
	*memory.Driver
	mu     sync.Mutex
	broken bool
}

func (d *typeless) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	e, err := d.Driver.Stat(ctx, key)
	e.ContentType = ""
	return e, err
}

func (d *typeless) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := d.Driver.Read(ctx, key, rng)
	d.mu.Lock()
	broken := d.broken
	d.mu.Unlock()
	if err != nil || !broken {
		return rc, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 1), failingReader{}), rc}, nil
}

// failingReader fails every read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("disk read failed") }

// TestTypingAHitThatCannotBeRead pins that a hit on a disk that stores no
// type, whose first bytes cannot be read, fails Get rather than returning
// the entry with no type.
func TestTypingAHitThatCannotBeRead(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	drv := &typeless{Driver: memory.New()}
	c, err := remote.New(mustDisk(t, drv), remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	drv.broken = true
	drv.mu.Unlock()
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err == nil {
		t.Fatal("a hit whose bytes cannot be read was returned")
	}
}

// TestTypingAHitWhoseReadIsRefused pins the same for a ranged read the
// disk refuses outright.
func TestTypingAHitWhoseReadIsRefused(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	drv := &typeless{Driver: memory.New()}
	f := &faults{arms: map[filesystem.OpKind]struct {
		n    int
		code filesystem.Code
	}{}}
	disk, err := filesystem.NewDisk("cache", drv, filesystem.WithInterceptors(f.intercept))
	if err != nil {
		t.Fatal(err)
	}
	c, err := remote.New(disk, remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	f.arm(filesystem.OpRead, -1, filesystem.CodeUnavailable)
	if _, err := c.Get(ctx, srv.URL+"/a.png"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("got %v, want CodeUnavailable", err)
	}
}

// TestOpenRefetchesWhatVanished pins Open's recovery: media swept between
// its Stat and its Read is fetched again and served, and a failing fetch
// is returned.
func TestOpenRefetchesWhatVanished(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	c, _, f := faultyCache(t)
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	f.arm(filesystem.OpRead, 1, filesystem.CodeNotFound)
	rc, _, err := c.Open(ctx, srv.URL+"/a.png")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != string(pngBytes) || srv.hits.Load() != 2 {
		t.Fatalf("served %d bytes after %d fetches, want the image fetched again", len(got), srv.hits.Load())
	}
	srv.Close()
	f.arm(filesystem.OpRead, 1, filesystem.CodeNotFound)
	if _, _, err := c.Open(ctx, srv.URL+"/a.png"); err == nil {
		t.Fatal("a vanished entry whose refetch fails was served")
	}
}

// TestLinkPreviewEdges pins the preview's edges: a stored preview that is
// not JSON is a miss (fetched again), a page that cannot be stored fails,
// a duplicate or over-long tag keeps the first value capped, the head
// ends at <body> (even with no </head>), and an image reference that does not parse is reported
// as CodeInvalidArgument without a fetch.
func TestLinkPreviewEdges(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("t", 3000)
	page := `<html><head><meta property="og:title" content="` + long + `"><meta property="og:title" content="second">` +
		`<meta property="og:description" content="  "><meta property="og:image" content="%zz"><body><meta property="og:site_name" content="after body"></body></html>`
	srv := serve(t, "127.0.0.1:0", body("text/html", []byte(page)))
	c, disk, f := faultyCache(t)
	p, err := c.LinkPreview(ctx, srv.URL+"/p")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Title) != 2048 || p.Description != "" || p.SiteName != "" || p.ImageError != filesystem.CodeInvalidArgument || p.ImageURL != "" {
		t.Fatalf("preview %+v", p)
	}
	// Corrupt the stored preview: the next call fetches the page again.
	var stored string
	for e, err := range disk.List(ctx, "remote", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		stored = e.Key
	}
	if _, err := disk.WriteBytes(ctx, stored, []byte("not json"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	hits := srv.hits.Load()
	if _, err := c.LinkPreview(ctx, srv.URL+"/p"); err != nil || srv.hits.Load() != hits+1 {
		t.Fatalf("corrupt stored preview: %v after %d fetches", err, srv.hits.Load()-hits)
	}
	f.arm(filesystem.OpWrite, -1, filesystem.CodeQuotaExceeded)
	if _, err := c.LinkPreview(ctx, srv.URL+"/other"); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("unstorable preview: %v, want CodeQuotaExceeded", err)
	}
}

// TestProxyEdges pins the proxy's remaining paths: a zero MaxAge takes the
// default, an expiry the signer refuses fails URL, a body that cannot be
// read is 503, and a cached object larger than the policy allows (stored
// before the policy shrank) is refused with 413 rather than served.
func TestProxyEdges(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	clk := newClock()
	c, disk, f := faultyCache(t, func(o *remote.Options) { o.Now = clk.Now; o.Policy.MaxSize = int64(len(pngBytes)) })
	p := newProxyWithMaxAge(t, c, clk, 0)
	if _, err := p.URL(ctx, srv.URL+"/a.png", 0); err == nil {
		t.Fatal("a URL without an expiry was signed")
	}
	u, err := p.URL(ctx, srv.URL+"/a.png", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := get(p, http.MethodGet, u); w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "max-age=60") {
		t.Fatalf("default max-age: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	key, err := c.MediaKey(srv.URL + "/a.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(ctx, key, append(append([]byte{}, pngBytes...), 0), filesystem.WriteOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if w := get(p, http.MethodGet, u); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized cached object: %d, want 413", w.Code)
	}
	f.arm(filesystem.OpRead, -1, filesystem.CodeUnavailable)
	if w := get(p, http.MethodGet, u); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("refused read: %d, want 503", w.Code)
	}
}

// brokenBody is a memory driver whose whole-object reads fail after their
// first byte while broken is set; ranged reads still work.
type brokenBody struct {
	*memory.Driver
	mu     sync.Mutex
	broken bool
}

func (d *brokenBody) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	rc, err := d.Driver.Read(ctx, key, rng)
	d.mu.Lock()
	broken := d.broken && rng.Length == 0
	d.mu.Unlock()
	if err != nil || !broken {
		return rc, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(io.LimitReader(rc, 1), failingReader{}), rc}, nil
}

// TestProxyBodyFailingMidStream pins that a cached body whose first bytes
// cannot be read is 503, not a 200 with a truncated or untyped body.
func TestProxyBodyFailingMidStream(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	clk := newClock()
	drv := &brokenBody{Driver: memory.New()}
	c, err := remote.New(mustDisk(t, drv), remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	p := newProxy(t, c, clk)
	u, err := p.URL(ctx, srv.URL+"/a.png", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := get(p, http.MethodGet, u); w.Code != http.StatusOK {
		t.Fatalf("first GET: %d", w.Code)
	}
	drv.mu.Lock()
	drv.broken = true
	drv.mu.Unlock()
	if w := get(p, http.MethodGet, u); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("body failing mid-stream: %d, want 503", w.Code)
	}
}

// TestSweepEdges pins that a sweep with nothing to evict deletes nothing,
// and that a failing listing or delete fails the sweep.
func TestSweepEdges(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	c, _, f := faultyCache(t)
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Sweep(ctx, remote.SweepOptions{MaxBytes: 1 << 20}); err != nil || res.Deleted != 0 || res.Kept != 1 {
		t.Fatalf("nothing to evict: %+v, %v", res, err)
	}
	f.arm(filesystem.OpDeleteKeys, -1, filesystem.CodePermission)
	if _, err := c.Sweep(ctx, remote.SweepOptions{MaxBytes: 1}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("failing delete: %v, want CodePermission", err)
	}
	f.arm(filesystem.OpList, -1, filesystem.CodeUnavailable)
	if _, err := c.Sweep(ctx, remote.SweepOptions{MaxBytes: 1}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("failing list: %v, want CodeUnavailable", err)
	}
}
