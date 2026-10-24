package filesystem_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// errBroken is the failure breakable injects. Its code, CodePermission,
// is one no tested path produces by itself, so seeing it proves the
// injected failure is what came back.
var errBroken = filesystem.NewError(filesystem.CodePermission, errors.New("injected failure"))

// breakable is a versioned memory driver whose chosen operations fail with
// errBroken, so a wrapper's error paths can be driven at the driver level,
// below any interceptor of the disk under test.
type breakable struct {
	*memory.Driver
	mu   sync.Mutex
	fail map[filesystem.OpKind]bool
}

func newBreakable() *breakable {
	return &breakable{Driver: memory.New(memory.WithVersions()), fail: map[filesystem.OpKind]bool{}}
}

// set makes the operations fail (on true) or work again (on false).
func (b *breakable) set(on bool, ops ...filesystem.OpKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, op := range ops {
		b.fail[op] = on
	}
}

func (b *breakable) broken(op filesystem.OpKind) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fail[op]
}

func (b *breakable) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if b.broken(filesystem.OpStat) {
		return filesystem.Entry{}, errBroken
	}
	return b.Driver.Stat(ctx, key)
}

func (b *breakable) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if b.broken(filesystem.OpRead) {
		return nil, errBroken
	}
	return b.Driver.Read(ctx, key, rng)
}

func (b *breakable) Delete(ctx context.Context, key string) error {
	if b.broken(filesystem.OpDelete) {
		return errBroken
	}
	return b.Driver.Delete(ctx, key)
}

func (b *breakable) DeletePrefix(ctx context.Context, dir string) error {
	if b.broken(filesystem.OpDeletePrefix) {
		return errBroken
	}
	return b.Driver.DeletePrefix(ctx, dir)
}

func (b *breakable) DeleteKeys(ctx context.Context, keys []string) error {
	if b.broken(filesystem.OpDeleteKeys) {
		return errBroken
	}
	return b.Driver.DeleteKeys(ctx, keys)
}

func (b *breakable) CompleteMultipart(ctx context.Context, key, id string, parts []filesystem.Part) (filesystem.Entry, error) {
	if b.broken(filesystem.OpMultipartComplete) {
		return filesystem.Entry{}, errBroken
	}
	return b.Driver.CompleteMultipart(ctx, key, id, parts)
}

func (b *breakable) RestoreVersion(ctx context.Context, key, id string) error {
	if b.broken(filesystem.OpRestoreVersion) {
		return errBroken
	}
	return b.Driver.RestoreVersion(ctx, key, id)
}

// failOps is an interceptor failing the given operations with errBroken
// while *on is true.
func failOps(on *bool, ops ...filesystem.OpKind) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		for _, k := range ops {
			if *on && op.Kind == k {
				return errBroken
			}
		}
		return call(ctx)
	}
}

// cacheErrors collects what OnCacheError was told, by key.
type cacheErrors struct {
	mu   sync.Mutex
	keys []string
}

func (c *cacheErrors) hook(_ context.Context, key string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if isBroken(err) {
		c.keys = append(c.keys, key)
	}
}

func (c *cacheErrors) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.keys...)
}

func TestCachedReadRefusesBadDisks(t *testing.T) {
	d := mustDisk(t, "d", memory.New())
	opts := filesystem.CacheOptions{TTL: time.Hour}
	for name, pair := range map[string][2]*filesystem.Disk{"nil remote": {nil, d}, "nil cache": {d, nil}, "same disk": {d, d}} {
		_, err := filesystem.CachedRead("c", pair[0], pair[1], opts)
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", name, err)
		}
	}
}

// TestCachedReadIfServesTheMatchingCopy proves a conditional read serves
// the cached copy when it was made from remote's current ETag, and falls
// back to remote, telling OnCacheError, when that copy cannot be read.
func TestCachedReadIfServesTheMatchingCopy(t *testing.T) {
	ctx := t.Context()
	remoteDrv := newBreakable()
	remote := mustDisk(t, "remote", remoteDrv)
	cacheDown := false
	cache := mustDisk(t, "cache", memory.New(), filesystem.WithInterceptors(failOps(&cacheDown, filesystem.OpRead)))
	var seen cacheErrors
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: time.Hour, OnCacheError: seen.hook})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("body"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "k"); err != nil { // make the copy
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		rc, _, err := c.ReadIf(ctx, "k", filesystem.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// With remote's Read broken, only the copy can answer.
	remoteDrv.set(true, filesystem.OpRead)
	if got := read(); got != "body" {
		t.Fatalf("matching copy served %q", got)
	}
	remoteDrv.set(false, filesystem.OpRead)
	cacheDown = true
	if got := read(); got != "body" {
		t.Fatalf("read with an unreadable copy served %q", got)
	}
	if got := seen.got(); len(got) != 1 || got[0] != "k" {
		t.Fatalf("OnCacheError told %v, want the unreadable copy of k", got)
	}
}

// TestCachedReadSurvivesAnUnreadableFreshCopy proves a fresh copy that
// cannot be read is reported and the read is served from remote.
func TestCachedReadSurvivesAnUnreadableFreshCopy(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", memory.New())
	cacheDown := false
	cache := mustDisk(t, "cache", memory.New(), filesystem.WithInterceptors(failOps(&cacheDown, filesystem.OpRead)))
	var seen cacheErrors
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: time.Hour, OnCacheError: seen.hook})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("body"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	cacheDown = true
	if b, err := c.ReadAll(ctx, "k"); err != nil || string(b) != "body" {
		t.Fatalf("read = %q, %v; want remote's body", b, err)
	}
	if len(seen.got()) == 0 {
		t.Fatal("the unreadable copy was not reported")
	}
}

// TestCachedReadRevalidationFailures proves a stale copy is not served
// when remote cannot be asked about it, and that a read whose remote copy
// cannot be read fails with remote's error.
func TestCachedReadRevalidationFailures(t *testing.T) {
	ctx := t.Context()
	remoteDrv := newBreakable()
	remote := mustDisk(t, "remote", remoteDrv)
	cache := mustDisk(t, "cache", memory.New())
	const ttl = time.Millisecond
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("body"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * ttl)
	remoteDrv.set(true, filesystem.OpStat)
	if _, err := c.ReadAll(ctx, "k"); !isBroken(err) {
		t.Fatalf("stale copy with remote down: %v, want remote's error", err)
	}
	remoteDrv.set(false, filesystem.OpStat)
	remoteDrv.set(true, filesystem.OpRead)
	if _, err := c.ReadAll(ctx, "fresh"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := remote.WriteBytes(ctx, "other", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "other"); !isBroken(err) {
		t.Fatalf("unreadable remote: %v, want remote's error", err)
	}
	if ok, _ := cache.Exists(ctx, "other"); ok {
		t.Fatal("a failed fill left a copy")
	}
}

// TestCachedReadMutationsFailWithRemote proves every mutation that remote
// refuses returns remote's error and leaves the cached copy in place
// (nothing changed, so nothing is stale).
func TestCachedReadMutationsFailWithRemote(t *testing.T) {
	ctx := t.Context()
	remoteDrv := newBreakable()
	remote := mustDisk(t, "remote", remoteDrv)
	cache := mustDisk(t, "cache", memory.New())
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "d/k", []byte("v1"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "d/k", []byte("v2"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadAll(ctx, "d/k"); err != nil {
		t.Fatal(err)
	}
	vs, err := remote.Versions(ctx, "d/k")
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.CreateMultipart(ctx, "d/k", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := c.UploadPart(ctx, "d/k", id, 1, strings.NewReader("p"), 1)
	if err != nil {
		t.Fatal(err)
	}
	remoteDrv.set(true, filesystem.OpDelete, filesystem.OpDeletePrefix, filesystem.OpDeleteKeys, filesystem.OpMultipartComplete, filesystem.OpRestoreVersion)
	calls := map[string]error{
		"Delete":         c.Delete(ctx, "d/k"),
		"DeletePrefix":   c.DeletePrefix(ctx, "d"),
		"RestoreVersion": c.RestoreVersion(ctx, "d/k", vs[1].ID),
	}
	_, calls["CompleteMultipart"] = c.CompleteMultipart(ctx, "d/k", id, []filesystem.Part{part})
	var batch *filesystem.BatchError
	if err := c.DeleteKeys(ctx, []string{"d/k"}); !errors.As(err, &batch) || !isBroken(batch.Failures[0].Err) {
		t.Errorf("DeleteKeys: %v, want a batch failing with remote's error", err)
	}
	for name, err := range calls {
		if !isBroken(err) {
			t.Errorf("%s: %v, want remote's error", name, err)
		}
	}
	if ok, _ := cache.Exists(ctx, "d/k"); !ok {
		t.Fatal("a refused mutation dropped the copy")
	}
}

// TestCachedReadReportsEachUndroppableCopy proves a move whose two stale
// copies cannot be dropped reports each key to OnCacheError and still
// returns remote's success.
func TestCachedReadReportsEachUndroppableCopy(t *testing.T) {
	ctx := t.Context()
	remote := mustDisk(t, "remote", memory.New())
	stuck := false
	// A cache without DeleteKeys deletes key by key, each through the
	// failing interceptor, so its failures come back per key.
	cache := mustDisk(t, "cache", newBasic(), filesystem.WithInterceptors(failOps(&stuck, filesystem.OpDelete)))
	var seen cacheErrors
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: time.Hour, OnCacheError: seen.hook})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	stuck = true
	if _, err := c.Move(ctx, "a", "b"); err != nil {
		t.Fatalf("move: %v", err)
	}
	got := seen.got()
	if len(got) != 2 || !(got[0] == "a" && got[1] == "b" || got[0] == "b" && got[1] == "a") {
		t.Fatalf("OnCacheError told %v, want a and b", got)
	}
}

// TestTrimCacheFailures proves TrimCache refuses a negative size and
// returns the cache's listing and delete failures, with how many copies
// it had deleted before.
func TestTrimCacheFailures(t *testing.T) {
	ctx := t.Context()
	if _, err := filesystem.TrimCache(ctx, mustDisk(t, "cache", memory.New()), -1); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("negative size: %v", err)
	}
	listDown, deleteDown := false, false
	cache := mustDisk(t, "cache", memory.New(), filesystem.WithInterceptors(failOps(&listDown, filesystem.OpList), failOps(&deleteDown, filesystem.OpDelete)))
	for _, k := range []string{"a", "b"} {
		if _, err := cache.WriteBytes(ctx, k, []byte("xx"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	listDown = true
	if n, err := filesystem.TrimCache(ctx, cache, 0); !isBroken(err) || n != 0 {
		t.Fatalf("listing down: %d, %v", n, err)
	}
	listDown, deleteDown = false, true
	if n, err := filesystem.TrimCache(ctx, cache, 0); !isBroken(err) || n != 0 {
		t.Fatalf("deletes down: %d, %v", n, err)
	}
	if keys := allKeys(t, cache, ""); len(keys) != 2 {
		t.Fatalf("keys after failed trims: %v", keys)
	}
}

// isBroken reports whether err is errBroken after the Disk annotated it
// (annotation keeps the code, not the identity).
func isBroken(err error) bool {
	return filesystem.CodeOf(err) == filesystem.CodePermission
}

// TestCachedReadRefetchesAStaleCopyWithoutAnETag proves an expired copy on
// a cache that cannot store the remote ETag is never revalidated, only
// copied anew, since nothing ties it to a remote version.
func TestCachedReadRefetchesAStaleCopyWithoutAnETag(t *testing.T) {
	ctx := t.Context()
	rd := &countingReads{Driver: memory.New()}
	remote := mustDisk(t, "remote", rd)
	const ttl = time.Millisecond
	c, err := filesystem.CachedRead("c", remote, mustDisk(t, "cache", newBasic()), filesystem.CacheOptions{TTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.WriteBytes(ctx, "k", []byte("one"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if b, err := c.ReadAll(ctx, "k"); err != nil || string(b) != "one" {
			t.Fatalf("read = %q, %v", b, err)
		}
		time.Sleep(5 * ttl)
	}
	if n := rd.reads.Load(); n != 2 {
		t.Fatalf("%d remote reads, want 2 (the expired copy refetched)", n)
	}
}
