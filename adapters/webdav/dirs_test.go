package webdav_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/webdav"
)

// TestFindAbandoned proves the preview reports exactly the staged files a
// sweep removes, removes nothing, and agrees with the sweep after it.
func TestFindAbandoned(t *testing.T) {
	ctx := t.Context()
	s := newServer(t)
	d, err := filesystem.NewDisk("webdav", newDriver(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().FindAbandoned {
		t.Fatal("FindAbandoned not declared")
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
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
	cutoff := time.Now().Add(-time.Hour)
	found, err := d.FindAbandoned(ctx, cutoff)
	if err != nil || len(found) != 1 {
		t.Fatalf("preview = %+v, %v; want one staged file", found, err)
	}
	if a := found[0]; a.Kind != filesystem.AbandonedStaging || a.ID != strings.Repeat("a", 26) || a.Key != "" || !a.LastActivity.Before(cutoff) {
		t.Errorf("reported %+v", a)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the preview removed the file: %v", err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("sweep after the preview = %d, %v", n, err)
	}
	if found, err := d.FindAbandoned(ctx, cutoff); err != nil || len(found) != 0 {
		t.Errorf("preview after the sweep = %+v, %v", found, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.FindAbandoned(cancelled, cutoff); err == nil {
		t.Error("preview with a cancelled context succeeded")
	}
}

// TestMakeDirAndDirExists covers what the conformance suite cannot: a
// file on the path, a reserved key, a made directory outliving its files.
func TestMakeDirAndDirExists(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t)
	if _, err := d.WriteBytes(ctx, "f.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.MakeDir(ctx, "f.txt"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Errorf("MakeDir over a file: %v", err)
	}
	if err := d.MakeDir(ctx, "f.txt/sub"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("MakeDir under a file: %v", err)
	}
	for dir, want := range map[string]bool{"f.txt": false, "f.txt/sub": false, "none": false} {
		if ok, err := d.DirExists(ctx, dir); ok != want || err != nil {
			t.Errorf("DirExists(%q) = %v, %v", dir, ok, err)
		}
	}
	if err := d.MakeDir(ctx, webdav.StagingDir+"/x"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("MakeDir in the staging collection: %v", err)
	}
	if _, err := d.DirExists(ctx, webdav.StagingDir); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("DirExists of the staging collection: %v", err)
	}
	// Deleting files never removes a collection.
	if err := d.MakeDir(ctx, "e"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "e/k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "e/k"); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.DirExists(ctx, "e"); !ok || err != nil {
		t.Errorf("a made collection went with its last file: %v, %v", ok, err)
	}
}

// TestDirsServerFailures: a server failing the PROPFIND fails MakeDir and
// DirExists, and one that describes some other resource is a missing one.
func TestDirsServerFailures(t *testing.T) {
	ctx := t.Context()
	failing := fakeServer(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	t.Cleanup(failing.Close)
	drv := fakeDriver(t, failing.URL)
	if err := drv.MakeDir(ctx, "x"); err == nil {
		t.Error("MakeDir against a failing server succeeded")
	}
	if _, err := drv.DirExists(ctx, "x"); err == nil {
		t.Error("DirExists against a failing server succeeded")
	}
	// The scripted server has no staging collection: nothing is abandoned.
	if found, err := drv.FindAbandoned(ctx, time.Now()); err != nil || len(found) != 0 {
		t.Errorf("preview without a staging collection = %+v, %v", found, err)
	}
	other := fakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"></D:multistatus>`))
	})
	t.Cleanup(other.Close)
	if ok, err := fakeDriver(t, other.URL).DirExists(ctx, "x"); ok || err != nil {
		t.Errorf("DirExists of an undescribed resource = %v, %v", ok, err)
	}
}

// fakeDriver opens a driver on a scripted server.
func fakeDriver(t *testing.T, endpoint string) *webdav.Driver {
	t.Helper()
	drv, err := webdav.New(t.Context(), webdav.Config{Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	return drv
}

// TestDirsRefusalUnderACollection: a 405 for a path whose parent is a real
// collection is the server's error, not a missing directory.
func TestDirsRefusalUnderACollection(t *testing.T) {
	srv := fakeServer(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/a/") || strings.HasSuffix(r.URL.Path, "/a") {
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/a/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`))
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	t.Cleanup(srv.Close)
	if _, err := fakeDriver(t, srv.URL).DirExists(t.Context(), "a/b"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("DirExists under a collection refusing PROPFIND: %v", err)
	}
}

// TestAbandonedScanFailures: a failing listing fails the preview and the
// sweep, a failing DELETE fails the sweep, and a context cancelled during
// the scan stops it.
func TestAbandonedScanFailures(t *testing.T) {
	ctx := t.Context()
	var fail atomic.Value
	fail.Store("")
	var listed atomic.Bool
	listing := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/.filesystem/tmp/` + strings.Repeat("a", 26) + `</D:href><D:propstat><D:prop><D:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case fail.Load() == r.Method:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "PROPFIND" && strings.HasSuffix(r.URL.Path, "/tmp/"):
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(listing))
			listed.Store(true)
		case r.Method == "PROPFIND":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(srv.Close)
	drv := fakeDriver(t, srv.URL)
	if found, err := drv.FindAbandoned(ctx, time.Now()); err != nil || len(found) != 1 {
		t.Fatalf("preview = %+v, %v", found, err)
	}
	fail.Store(http.MethodDelete)
	if n, err := drv.SweepAbandoned(ctx, time.Now()); err == nil || n != 0 {
		t.Errorf("sweep with a failing DELETE = %d, %v", n, err)
	}
	fail.Store("PROPFIND")
	if _, err := drv.FindAbandoned(ctx, time.Now()); err == nil {
		t.Error("preview with a failing listing succeeded")
	}
	fail.Store("")
	listed.Store(false)
	stop := &cancelOnceListed{Context: ctx, listed: &listed}
	if _, err := drv.FindAbandoned(stop, time.Now()); err == nil {
		t.Error("a preview cancelled during the scan succeeded")
	}
}

// cancelOnceListed reports Canceled from Err once the server has sent
// the staging listing, so a scan gets its listing and stops inside its
// loop. Done stays the parent's, so the request itself is not aborted.
type cancelOnceListed struct {
	context.Context
	listed *atomic.Bool
}

func (c *cancelOnceListed) Err() error {
	if c.listed.Load() {
		return context.Canceled
	}
	return nil
}
