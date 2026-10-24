package filesystem

import (
	"context"
	"errors"
	"io"
	"slices"
	"time"
)

// cacheETagMeta is the metadata key under which a cache disk that stores
// metadata keeps the remote ETag of each cached copy.
const cacheETagMeta = "cache-etag"

// CacheOptions configure CachedRead.
type CacheOptions struct {
	// TTL is how long a copy is served without asking remote. Required,
	// positive.
	TTL time.Duration
	// OnCacheError, when set, is told of every cache failure the disk
	// absorbs: a copy that could not be made, read, refreshed or dropped.
	// No such failure fails the call, so a copy that could not be dropped
	// after a write stays stale until TTL passes; this hook is how the
	// application finds out (log it, count it, alert on it).
	OnCacheError func(ctx context.Context, key string, err error)
	// Now is the clock a copy's age is judged by against TTL, and the time
	// a revalidated copy is marked fresh at; nil means time.Now. A fake
	// clock makes TTL expiry testable without sleeping. A copy's age is
	// Now minus its ModTime on cache, so cache's clock and this one must
	// agree.
	Now func() time.Time
}

// CachedRead returns a Disk named name over remote whose reads are served
// from a copy on cache, typically a local disk in front of a slow or
// metered bucket. A read of a key not on cache copies the whole object
// there first, then serves the read (ranges included) from the copy; a
// copy younger than opts.TTL is served without asking remote. An older copy is
// revalidated with a Stat of remote: when cache stores metadata and remote
// reports the same ETag it was copied with, the copy is served (and, where
// cache can set modification times, made fresh again); otherwise it is
// copied anew. Writes, deletes, copies, moves, appends and completed
// multipart uploads go to remote and then drop cache's copy.
//
// Semantics, which callers must accept:
//   - Freshness is bounded by TTL, not exact: a copy made while another
//     process writes the key may be stale until it is revalidated. Use it
//     for content that changes rarely or under new keys.
//   - Stat, List, URLs and every other capability are remote's and are
//     never cached. Signed PUT URLs and form uploads are refused or not
//     offered (an upload straight to remote would leave a stale copy).
//   - A conditional read (ReadIf) asks remote for the current ETag and
//     serves the copy only when it was made from that ETag, else reads
//     remote, so its entry and body always match.
//   - A cache that fails (full, unavailable) never fails a call: reads are
//     served from remote, writes and deletes return remote's result, and
//     OnCacheError is told.
//   - cache grows with every key read; bound it with TrimCache, run as a
//     job.
//
// cache must be a disk of its own: keys on it are
// remote's keys, and TrimCache deletes from it.
func CachedRead(name string, remote, cache *Disk, opts CacheOptions) (*Disk, error) {
	if remote == nil || cache == nil || remote == cache {
		return nil, Errorf(CodeInvalidArgument, "cached read needs two distinct, non-nil disks")
	}
	if opts.TTL <= 0 {
		return nil, Errorf(CodeInvalidArgument, "cached read TTL must be positive, got %s", opts.TTL)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	drv := &cachedDriver{scopedDriver: newScopedDriver(remote.driver, ""), cache: cache, opts: opts}
	return remote.derive(name, drv, remote.publicBase)
}

// cachedDriver serves reads from cache; see CachedRead. The embedded
// driver forwards everything else to remote unchanged.
type cachedDriver struct {
	*scopedDriver
	cache *Disk
	opts  CacheOptions
}

// Capabilities are remote's, without form uploads, pinned PUT headers
// and pinned visibility (an upload straight to remote would leave the
// copy stale) and
// SweepAbandoned and FindAbandoned (sweep remote directly), and with ConditionalRead, which
// the disk answers itself (see ReadIf).
func (c *cachedDriver) Capabilities() Capabilities {
	r := c.inner.Capabilities()
	r.PresignPost, r.PresignHeaders, r.SweepAbandoned, r.FindAbandoned = false, false, false, false
	r.PresignVisibility = false
	r.ConditionalRead = true
	return r
}

// report tells OnCacheError about a cache failure the disk absorbed.
func (c *cachedDriver) report(ctx context.Context, key string, err error) {
	if err != nil && c.opts.OnCacheError != nil {
		c.opts.OnCacheError(ctx, key, err)
	}
}

// ReadIf answers a conditional read from remote's current entry, serving
// the cached copy only when it was made from that entry's ETag, so the
// body always matches the entry returned.
func (c *cachedDriver) ReadIf(ctx context.Context, key string, opts ReadOptions) (io.ReadCloser, Entry, error) {
	remote, err := c.inner.Stat(ctx, key)
	if err != nil {
		return nil, Entry{}, err
	}
	if NotModified(remote, opts) {
		return nil, Entry{}, Errorf(CodeNotModified, "object unchanged")
	}
	if remote.ETag != "" {
		if copied, err := c.cache.Stat(ctx, key); err == nil && copied.Metadata[cacheETagMeta] == remote.ETag {
			rc, err := c.cache.Read(ctx, key)
			if err == nil {
				return rc, remote, nil
			}
			c.report(ctx, key, err)
		}
	}
	rc, err := c.inner.Read(ctx, key, ByteRange{})
	return rc, remote, err
}

// Read serves key from cache, copying it from remote when the cache has no
// usable copy.
func (c *cachedDriver) Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	if c.usable(ctx, key) {
		rc, err := c.cache.ReadRange(ctx, key, rng)
		if err == nil {
			return rc, nil
		}
		c.report(ctx, key, err)
	}
	remote, err := c.inner.Stat(ctx, key)
	if err != nil {
		if CodeOf(err) == CodeNotFound {
			// Deleted behind the cache's back: drop the stale copy too,
			// best effort, as the read's answer is the same.
			c.report(ctx, key, c.cache.Delete(ctx, key))
		}
		return nil, err
	}
	err = c.fill(ctx, key, remote)
	if err == nil {
		var rc io.ReadCloser
		if rc, err = c.cache.ReadRange(ctx, key, rng); err == nil {
			return rc, nil
		}
	}
	c.report(ctx, key, err)
	return c.inner.Read(ctx, key, rng)
}

// usable reports whether cache holds a copy of key that may be served: one
// younger than ttl, or one with the ETag remote reports now.
func (c *cachedDriver) usable(ctx context.Context, key string) bool {
	copied, err := c.cache.Stat(ctx, key)
	if err != nil {
		return false
	}
	if c.opts.Now().Sub(copied.ModTime) < c.opts.TTL {
		return true
	}
	etag := copied.Metadata[cacheETagMeta]
	if etag == "" {
		return false
	}
	remote, err := c.inner.Stat(ctx, key)
	if err != nil || remote.ETag != etag {
		return false
	}
	if c.cache.caps.SetModTime {
		// A failure only means the next read revalidates.
		c.report(ctx, key, c.cache.SetModTime(ctx, key, c.opts.Now()))
	}
	return true
}

// fill copies key from remote to cache.
func (c *cachedDriver) fill(ctx context.Context, key string, remote Entry) error {
	rc, err := c.inner.Read(ctx, key, ByteRange{})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	opts := WriteOptions{ContentType: remote.ContentType}
	if c.cache.caps.Metadata && remote.ETag != "" {
		opts.Metadata = map[string]string{cacheETagMeta: remote.ETag}
	}
	_, err = c.cache.Write(ctx, key, rc, opts)
	return err
}

// drop removes cache's copies of keys after remote changed them. A
// failure is reported, never returned: remote has changed already, and
// the caller must get remote's result.
func (c *cachedDriver) drop(ctx context.Context, keys ...string) {
	if len(keys) == 1 {
		c.report(ctx, keys[0], c.cache.Delete(ctx, keys[0]))
		return
	}
	err := c.cache.DeleteKeys(ctx, keys)
	var batch *BatchError
	if errors.As(err, &batch) {
		for _, f := range batch.Failures {
			c.report(ctx, f.Key, f.Err)
		}
		return
	}
	for _, k := range keys {
		c.report(ctx, k, err)
	}
}

func (c *cachedDriver) Write(ctx context.Context, key string, body io.Reader, opts WriteOptions) (Entry, error) {
	e, err := c.inner.Write(ctx, key, body, opts)
	if err != nil {
		return Entry{}, err
	}
	c.drop(ctx, key)
	return e, nil
}

func (c *cachedDriver) Delete(ctx context.Context, key string) error {
	if err := c.inner.Delete(ctx, key); err != nil {
		return err
	}
	c.drop(ctx, key)
	return nil
}

func (c *cachedDriver) Copy(ctx context.Context, src, dst string) (Entry, error) {
	e, err := c.scopedDriver.Copy(ctx, src, dst)
	if err != nil {
		return Entry{}, err
	}
	c.drop(ctx, dst)
	return e, nil
}

func (c *cachedDriver) Move(ctx context.Context, src, dst string) (Entry, error) {
	e, err := c.scopedDriver.Move(ctx, src, dst)
	if err != nil {
		return Entry{}, err
	}
	c.drop(ctx, src, dst)
	return e, nil
}

func (c *cachedDriver) DeletePrefix(ctx context.Context, dir string) error {
	if err := c.scopedDriver.DeletePrefix(ctx, dir); err != nil {
		return err
	}
	c.report(ctx, dir, c.cache.DeletePrefix(ctx, dir))
	return nil
}

func (c *cachedDriver) DeleteKeys(ctx context.Context, keys []string) error {
	if err := c.scopedDriver.DeleteKeys(ctx, keys); err != nil {
		return err
	}
	c.drop(ctx, keys...)
	return nil
}

func (c *cachedDriver) Append(ctx context.Context, key string, body io.Reader, opts AppendOptions) (Entry, error) {
	e, err := c.scopedDriver.Append(ctx, key, body, opts)
	if err != nil {
		return Entry{}, err
	}
	c.drop(ctx, key)
	return e, nil
}

func (c *cachedDriver) CompleteMultipart(ctx context.Context, key, id string, parts []Part) (Entry, error) {
	e, err := c.scopedDriver.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return Entry{}, err
	}
	c.drop(ctx, key)
	return e, nil
}

// RestoreVersion changes the current content, so it drops the copy.
func (c *cachedDriver) RestoreVersion(ctx context.Context, key, id string) error {
	if err := c.scopedDriver.RestoreVersion(ctx, key, id); err != nil {
		return err
	}
	c.drop(ctx, key)
	return nil
}

// presignsMethod reports that Presign signs downloads only (see
// methodPresigner).
func (c *cachedDriver) presignsMethod(m HTTPMethod) bool { return m == MethodGet }

// Presign signs downloads only: an upload straight to remote would leave
// the cached copy stale.
func (c *cachedDriver) Presign(ctx context.Context, key string, opts PresignOptions) (SignedRequest, error) {
	if opts.Method == MethodPut {
		return SignedRequest{}, Errorf(CodeUnsupported, "a signed upload to a cached disk would leave its cache stale")
	}
	return c.scopedDriver.Presign(ctx, key, opts)
}

// TrimCache deletes the oldest copies on cache (by modification time, so
// by when they were copied or last revalidated) until the bytes it holds
// are at most maxBytes, and returns how many it deleted. Run it from a job
// (jobs.Func) to bound a CachedRead cache; deleting a copy only costs a
// fetch from remote on its next read. It lists the whole cache disk, so
// point it only at a disk used as a cache.
func TrimCache(ctx context.Context, cache *Disk, maxBytes int64) (int, error) {
	if maxBytes < 0 {
		return 0, Errorf(CodeInvalidArgument, "negative cache size %d", maxBytes)
	}
	var (
		entries []Entry
		total   int64
	)
	for e, err := range cache.List(ctx, "", ListOptions{Recursive: true}) {
		if err != nil {
			return 0, err
		}
		entries = append(entries, e)
		total += e.Size
	}
	if total <= maxBytes {
		return 0, nil
	}
	slices.SortFunc(entries, func(a, b Entry) int { return a.ModTime.Compare(b.ModTime) })
	removed := 0
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if err := cache.Delete(ctx, e.Key); err != nil {
			return removed, err
		}
		total -= e.Size
		removed++
	}
	return removed, nil
}
