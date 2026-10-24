package filesystem_test

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// countingReads is a memory driver that counts reads, to tell cache hits
// from trips to the remote.
type countingReads struct {
	*memory.Driver
	reads atomic.Int64
}

func (c *countingReads) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	c.reads.Add(1)
	return c.Driver.Read(ctx, key, rng)
}

func TestCachedReadConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		c, err := filesystem.CachedRead("cached", mustDisk(t, "remote", memory.New()), mustDisk(t, "cache", memory.New()), filesystem.CacheOptions{TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return c
	})
}

func TestCachedReadServesAndInvalidates(t *testing.T) {
	ctx := t.Context()
	rd := &countingReads{Driver: memory.New()}
	remote := mustDisk(t, "remote", rd)
	cache := mustDisk(t, "cache", memory.New())
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteBytes(ctx, "k", []byte("hello world"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if b, err := c.ReadAll(ctx, "k"); err != nil || string(b) != "hello world" {
			t.Fatalf("read %q, %v", b, err)
		}
	}
	if n := rd.reads.Load(); n != 1 {
		t.Errorf("remote read %d times for three reads, want 1", n)
	}
	rc, err := c.ReadRange(ctx, "k", filesystem.ByteRange{Offset: 6, Length: 5})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "world" || rd.reads.Load() != 1 {
		t.Errorf("range from the cache %q after %d remote reads", b, rd.reads.Load())
	}

	// A write through the disk drops the copy.
	if _, err := c.WriteBytes(ctx, "k", []byte("changed"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.ReadAll(ctx, "k"); string(b) != "changed" {
		t.Errorf("stale read after a write: %q", b)
	}
	if _, err := c.Move(ctx, "k", "m"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := cache.Exists(ctx, "k"); ok {
		t.Error("move left the source's copy")
	}
	if err := c.Delete(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "m"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("read after delete: %v", err)
	}
	if _, err := c.SignedURL(ctx, "m", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}); err == nil {
		t.Error("signed PUT issued on a cached disk")
	}
}

func TestCachedReadRevalidatesByETag(t *testing.T) {
	ctx := t.Context()
	rd := &countingReads{Driver: memory.New()}
	remote := mustDisk(t, "remote", rd)
	cache := mustDisk(t, "cache", memory.New())
	const ttl = 20 * time.Millisecond
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("one"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * ttl)
	// Expired but unchanged: revalidated by ETag, no second download.
	if b, _ := c.ReadAll(ctx, "k"); string(b) != "one" || rd.reads.Load() != 1 {
		t.Errorf("revalidated read %q after %d remote reads", b, rd.reads.Load())
	}
	// Changed behind the cache's back: stale until expiry, then refetched.
	if _, err := remote.WriteBytes(ctx, "k", []byte("two"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * ttl)
	if b, _ := c.ReadAll(ctx, "k"); string(b) != "two" {
		t.Errorf("expired copy with another ETag served: %q", b)
	}
}

func TestCachedReadSurvivesAFailingCache(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", memory.New())
	broken := mustDisk(t, "cache", &downDriver{Driver: memory.New(), down: true, err: filesystem.ErrUnavailable})
	c, err := filesystem.CachedRead("c", remote, broken, filesystem.CacheOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, err := c.ReadAll(ctx, "k"); err != nil || string(b) != "x" {
		t.Errorf("read through a failing cache: %q, %v", b, err)
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := filesystem.CachedRead("c", remote, broken, filesystem.CacheOptions{TTL: ttl}); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
}

func TestTrimCacheDeletesOldestFirst(t *testing.T) {
	ctx := t.Context()
	cache := mustDisk(t, "cache", memory.New())
	for _, k := range []string{"old", "mid", "new"} {
		if _, err := cache.WriteBytes(ctx, k, make([]byte, 10), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	n, err := filesystem.TrimCache(ctx, cache, 15)
	if err != nil || n != 2 {
		t.Fatalf("trimmed %d, %v", n, err)
	}
	if got := allKeys(t, cache, ""); len(got) != 1 || got[0] != "new" {
		t.Errorf("kept %v", got)
	}
	if n, _ := filesystem.TrimCache(ctx, cache, 100); n != 0 {
		t.Errorf("trimmed %d under the bound", n)
	}
}

// failingDelete is a memory driver whose deletes fail, as a full or
// unreachable cache disk's might.
type failingDelete struct{ *memory.Driver }

func (f *failingDelete) Capabilities() filesystem.Capabilities {
	c := f.Driver.Capabilities()
	c.DeleteKeys, c.DeletePrefix = false, false
	return c
}

func (f *failingDelete) Delete(context.Context, string) error { return filesystem.ErrUnavailable }

func TestCachedReadReportsCacheFailuresWithoutFailingTheCall(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", memory.New())
	cache := mustDisk(t, "cache", &failingDelete{Driver: memory.New()})
	var reported []string
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{
		TTL:          time.Hour,
		OnCacheError: func(_ context.Context, key string, _ error) { reported = append(reported, key) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteBytes(ctx, "k", []byte("one"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("write failed over a cache failure: %v", err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("delete failed over a cache failure: %v", err)
	}
	if ok, _ := remote.Exists(ctx, "k"); ok {
		t.Error("remote still holds the deleted key")
	}
	if len(reported) != 2 || reported[0] != "k" {
		t.Errorf("reported %v, want the write's and the delete's failed drops", reported)
	}
}

// TestCachedReadIfMatchesBodyAndEntry pins that a conditional read never
// pairs the remote's new ETag with a stale cached body.
func TestCachedReadIfMatchesBodyAndEntry(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", memory.New())
	cache := mustDisk(t, "cache", memory.New())
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("old"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "k"); err != nil { // cache the old copy
		t.Fatal(err)
	}
	fresh, err := remote.WriteBytes(ctx, "k", []byte("new"), filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rc, e, err := c.ReadIf(ctx, "k", filesystem.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "new" || e.ETag != fresh.ETag {
		t.Errorf("ReadIf returned %q with ETag %s, want %q with %s", b, e.ETag, "new", fresh.ETag)
	}
	if _, _, err := c.ReadIf(ctx, "k", filesystem.ReadOptions{IfNoneMatch: fresh.ETag}); filesystem.CodeOf(err) != filesystem.CodeNotModified {
		t.Errorf("matching ETag: %v", err)
	}
	// An unchanged object is served from the copy.
	if _, err := c.ReadAll(ctx, "k"); err != nil {
		t.Fatal(err)
	}
}

// TestCachedReadRestoreVersionDropsTheCopy pins that restoring a version
// through a cached disk discards the cached copy, so the next read serves
// the restored content rather than the copy made before the restore.
func TestCachedReadRestoreVersionDropsTheCopy(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", memory.New(memory.WithVersions()))
	c, err := filesystem.CachedRead("c", remote, mustDisk(t, "cache", memory.New()), filesystem.CacheOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two"} {
		if _, err := c.WriteBytes(ctx, "k", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := c.ReadAll(ctx, "k"); err != nil || string(b) != "two" {
		t.Fatalf("read %q, %v", b, err)
	}
	vs, err := c.Versions(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	var older string
	for _, v := range vs {
		if !v.Latest {
			older = v.ID
		}
	}
	if err := c.RestoreVersion(ctx, "k", older); err != nil {
		t.Fatal(err)
	}
	if b, err := c.ReadAll(ctx, "k"); err != nil || string(b) != "one" {
		t.Errorf("after restore the cached disk reads %q, %v; want the restored one", b, err)
	}
}

// TestCachedReadSignsDownloadsOnly pins that a cached disk forwards signed
// GETs to remote's signer but refuses a signed PUT, which would replace
// the object behind the cache's back and leave the copy stale.
func TestCachedReadSignsDownloadsOnly(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", fullDriver{memory.New()})
	c, err := filesystem.CachedRead("c", remote, mustDisk(t, "cache", memory.New()), filesystem.CacheOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	get, err := c.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil || get.URL != "https://backend.test/k?sig=1" {
		t.Errorf("signed GET %q, %v", get.URL, err)
	}
	_, err = c.SignedURL(ctx, "k", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute})
	if filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("signed PUT on a cached disk: %v", err)
	}
}
