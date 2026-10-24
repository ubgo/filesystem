package webdav_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/webdav"
)

// slowFail fails after a pause, long enough for the server to have
// stored what came before it.
type slowFail struct{}

func (slowFail) Read([]byte) (int, error) {
	time.Sleep(200 * time.Millisecond)
	return 0, errors.New("client went away")
}

// dropServer is a server whose inbox collection refuses MKCOL inside it
// (everywhere with noMkcol), as write-only drop folders do, and counts
// MOVEs; the config's endpoint is the inbox.
func dropServer(t *testing.T, noMkcol bool) (*server, webdav.Config) {
	t.Helper()
	s := newServer(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.on(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "MKCOL" && (noMkcol || strings.HasPrefix(r.URL.Path, davPrefix+"/inbox/")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return true
		}
		return false
	})
	cfg := s.config()
	cfg.Endpoint = s.URL + davPrefix + "/inbox"
	return s, cfg
}

// dirNames lists dir under the server's directory.
func dirNames(t *testing.T, s *server, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.dir, dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// TestStagingPathDropFolder proves a staging collection outside the
// endpoint's opens a drop folder that refuses MKCOL inside it (where the
// default fails on the first write), still PUTs there and MOVEs in,
// reserves no key, sweeps only there, and that a misplaced StagingPath is
// refused before any request.
func TestStagingPathDropFolder(t *testing.T) {
	ctx := t.Context()
	s, cfg := dropServer(t, false)
	def, err := webdav.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := def.Write(ctx, "a.txt", strings.NewReader("x"), filesystem.WriteOptions{}); err == nil {
		t.Fatal("the default staging collection wrote into a drop folder that refuses MKCOL")
	}
	cfg.StagingPath = davPrefix + "/staging"
	drv, err := webdav.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("webdav", drv)
	moves := s.counts["MOVE"].Load()
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "two" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if s.counts["MOVE"].Load()-moves != 2 {
		t.Error("writes were not moved into place")
	}
	if _, err := d.WriteBytes(ctx, webdav.StagingDir, []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("the default staging name is reserved without a staging collection under the endpoint: %v", err)
	}
	if page, err := d.ListPage(ctx, "", filesystem.ListOptions{}); err != nil || len(page.Entries) != 2 {
		t.Errorf("listing = %+v, %v; want both keys", page.Entries, err)
	}
	if got := dirNames(t, s, "inbox"); !slices.Equal(got, []string{webdav.StagingDir, "a.txt"}) {
		t.Errorf("inbox holds %v, want only the keys", got)
	}
	if got := dirNames(t, s, "staging"); len(got) != 0 {
		t.Errorf("staging kept %v", got)
	}
	if err := os.WriteFile(filepath.Join(s.dir, "staging", strings.Repeat("a", 26)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if found, err := d.FindAbandoned(ctx, later); err != nil || len(found) != 1 {
		t.Errorf("FindAbandoned = %v, %v", found, err)
	}
	if n, err := d.SweepAbandoned(ctx, later); err != nil || n != 1 {
		t.Errorf("SweepAbandoned = %d, %v", n, err)
	}
	if got := dirNames(t, s, "staging"); len(got) != 0 {
		t.Errorf("after the sweep staging holds %v", got)
	}
	before := s.counts["PROPFIND"].Load() + s.counts["MKCOL"].Load()
	for _, bad := range []webdav.Config{
		{StagingPath: "staging"},
		{StagingPath: davPrefix + "/inbox"},
		{StagingPath: davPrefix + "/inbox/tmp"},
		{StagingPath: davPrefix},
		{StagingPath: "/"},
		{StagingPath: davPrefix + "/staging", StagingDir: "_s"},
	} {
		bad.Endpoint, bad.Username, bad.Password = cfg.Endpoint, cfg.Username, cfg.Password
		if _, err := webdav.New(context.Background(), bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(StagingPath %q, StagingDir %q) = %v, want CodeInvalidArgument", bad.StagingPath, bad.StagingDir, err)
		}
	}
	if s.counts["PROPFIND"].Load()+s.counts["MKCOL"].Load() != before {
		t.Error("an invalid StagingPath reached the server")
	}
}

// TestWriteInPlaceDropFolder proves WriteInPlace opens a drop folder where
// no collection may be created anywhere, PUTs and overwrites keys
// directly (no MKCOL, no MOVE, nothing else left in the collection),
// deletes a failed write, declares nothing to sweep, sends nothing at New,
// and needs AllowNonAtomicWrites.
func TestWriteInPlaceDropFolder(t *testing.T) {
	ctx := t.Context()
	s, cfg := dropServer(t, true)
	for _, bad := range []webdav.Config{
		{WriteInPlace: true},
		{WriteInPlace: true, AllowNonAtomicWrites: true, StagingDir: "_s"},
		{WriteInPlace: true, AllowNonAtomicWrites: true, StagingPath: davPrefix + "/staging"},
	} {
		bad.Endpoint = cfg.Endpoint
		if _, err := webdav.New(context.Background(), bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%v) = %v, want CodeInvalidArgument", bad, err)
		}
	}
	cfg.WriteInPlace, cfg.AllowNonAtomicWrites = true, true
	sent := func() int64 {
		var n int64
		for _, c := range s.counts {
			n += c.Load()
		}
		return n
	}
	before := sent()
	drv, err := webdav.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sent() != before {
		t.Error("New sent a request under WriteInPlace")
	}
	d, _ := filesystem.NewDisk("webdav", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "two" {
		t.Fatalf("read = %q, %v", b, err)
	}
	// The body fails only after the server has the file open and some of
	// its bytes, so a partial file exists until the driver deletes it.
	part := io.MultiReader(strings.NewReader(strings.Repeat("p", 64<<10)), slowFail{})
	if _, err := d.Write(ctx, "c.txt", part, filesystem.WriteOptions{}); err == nil {
		t.Error("a failing body was stored")
	}
	if got := dirNames(t, s, "inbox"); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("inbox holds %v, want only the written key", got)
	}
	if n := s.counts["MKCOL"].Load() + s.counts["MOVE"].Load(); n != 0 {
		t.Errorf("in-place writes sent %d MKCOL or MOVE requests", n)
	}
	caps := drv.Capabilities()
	if caps.SweepAbandoned || caps.FindAbandoned || caps.CaseInsensitive {
		t.Errorf("capabilities = %+v", caps)
	}
	if _, err := drv.SweepAbandoned(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("SweepAbandoned = %v, want CodeUnsupported", err)
	}
}

// TestWriteInPlaceRemovesLockedPartial proves a failed in-place write
// still removes its partial file when the server answers the cleanup
// DELETE with 423 for a while, as a server whose handler for the aborted
// PUT still holds the resource's lock does.
func TestWriteInPlaceRemovesLockedPartial(t *testing.T) {
	ctx := t.Context()
	s, cfg := dropServer(t, true)
	cfg.WriteInPlace, cfg.AllowNonAtomicWrites = true, true
	var locked atomic.Int64
	s.on(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete && locked.Add(1) <= 3 {
			w.WriteHeader(http.StatusLocked)
			return true
		}
		return false
	})
	drv, err := webdav.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("webdav", drv)
	part := io.MultiReader(strings.NewReader(strings.Repeat("p", 64<<10)), slowFail{})
	if _, err := d.Write(ctx, "c.txt", part, filesystem.WriteOptions{}); err == nil {
		t.Fatal("a failing body was stored")
	}
	if got := dirNames(t, s, "inbox"); len(got) != 0 {
		t.Errorf("inbox holds %v after a failed write whose DELETE met 423 three times", got)
	}
	if n := locked.Load(); n < 4 {
		t.Errorf("DELETE sent %d times, want a retry after each 423", n)
	}
}

// TestWriteInPlaceRefusals proves an in-place write whose parent MKCOL
// the server refuses fails before any PUT, and that a PUT the server
// answers with an error status is that status's error, with nothing
// stored either time.
func TestWriteInPlaceRefusals(t *testing.T) {
	ctx := t.Context()
	s, cfg := dropServer(t, true)
	cfg.WriteInPlace, cfg.AllowNonAtomicWrites = true, true
	// One intercept at a time: this one keeps dropServer's MKCOL refusal.
	s.on(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "MKCOL" || (r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/denied.txt")) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return true
		}
		return false
	})
	drv, err := webdav.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("webdav", drv)
	putsSent := func() int64 {
		if c := s.counts[http.MethodPut]; c != nil {
			return c.Load()
		}
		return 0
	}
	puts := putsSent()
	if _, err := d.WriteBytes(ctx, "sub/a.txt", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("a nested key whose MKCOL is refused = %v, want CodePermission", err)
	}
	if n := putsSent() - puts; n != 0 {
		t.Errorf("a write whose parent could not be made sent %d PUTs", n)
	}
	if _, err := d.WriteBytes(ctx, "denied.txt", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("a refused PUT = %v, want CodePermission", err)
	}
	if got := dirNames(t, s, "inbox"); len(got) != 0 {
		t.Errorf("inbox holds %v after refused writes", got)
	}
}
