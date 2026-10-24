package remote_test

import (
	"context"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/upload"
)

// fill caches three images a, b, c, one hour apart in that order, and
// returns their keys.
func fill(t *testing.T, c *remote.Cache, disk *filesystem.Disk, base string, clk *clock) map[string]string {
	t.Helper()
	ctx := context.Background()
	keys := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		e, err := c.Get(ctx, base+"/"+name+".png")
		if err != nil {
			t.Fatal(err)
		}
		if err := disk.SetModTime(ctx, e.Key, clk.Now()); err != nil {
			t.Fatal(err)
		}
		keys[name] = e.Key
		clk.add(time.Hour)
	}
	return keys
}

func exists(t *testing.T, disk *filesystem.Disk, key string) bool {
	t.Helper()
	ok, err := disk.Exists(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestSweepEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	clk := newClock()
	c, disk := newCache(t, func(o *remote.Options) { o.Now = clk.Now; o.TouchInterval = time.Minute })
	keys := fill(t, c, disk, srv.URL, clk)
	// A hit on a (the oldest) makes it the most recently used.
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	size := int64(len(pngBytes))
	res, err := c.Sweep(ctx, remote.SweepOptions{MaxBytes: 2 * size})
	if err != nil {
		t.Fatal(err)
	}
	if res != (remote.SweepResult{Deleted: 1, Freed: size, Kept: 2, Bytes: 2 * size}) {
		t.Fatalf("result %+v", res)
	}
	if !exists(t, disk, keys["a"]) || exists(t, disk, keys["b"]) || !exists(t, disk, keys["c"]) {
		t.Fatal("the sweep did not evict b, the least recently used")
	}
}

func TestSweepMaxAge(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	clk := newClock()
	c, disk := newCache(t, func(o *remote.Options) { o.Now = clk.Now })
	keys := fill(t, c, disk, srv.URL, clk) // a at -3h, b at -2h, c at -1h
	res, err := c.Sweep(ctx, remote.SweepOptions{MaxAge: 150 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || exists(t, disk, keys["a"]) || !exists(t, disk, keys["b"]) {
		t.Fatalf("result %+v", res)
	}
	if _, err := c.Sweep(ctx, remote.SweepOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a sweep without bounds: %v", err)
	}
}

func TestSweepTouchesOnlyTheCache(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	disk := mustDisk(t, mustLocal(t))
	if _, err := disk.WriteBytes(ctx, "remote/keep.txt", []byte("app file"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := remote.New(disk, remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LinkPreview(ctx, srv.URL+"/a.png"); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("preview of an image: %v", err)
	}
	res, err := c.Sweep(ctx, remote.SweepOptions{MaxBytes: 1})
	if err != nil || res.Deleted != 1 || res.Kept != 0 {
		t.Fatalf("sweep: %+v, %v", res, err)
	}
	if !exists(t, disk, "remote/keep.txt") {
		t.Fatal("the sweep deleted a file the cache did not write")
	}
}

func mustLocal(t *testing.T) *local.Driver {
	t.Helper()
	d, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestGetReportsTheTypeOnADiskThatStoresNone(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("text/plain", pngBytes))
	c, err := remote.New(mustDisk(t, mustLocal(t)), remote.Options{Dir: "remote", Policy: imagePolicy(), Fetch: upload.FetchOptions{AllowAddr: allowLoopback4}})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"miss", "hit"} {
		if e, err := c.Get(ctx, srv.URL+"/a.png"); err != nil || e.ContentType != "image/png" {
			t.Fatalf("%s: type %q, %v", step, e.ContentType, err)
		}
	}
}
