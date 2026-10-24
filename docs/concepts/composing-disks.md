# Composing disks

A `Disk` can stand in front of other disks. `Disk.Scope` and `filesystem.Mirror` are covered in [Disks and drivers](disks-and-drivers.md) and [Backups and mirrors](../recipes/backups-and-mirrors.md); this page covers the four other compositions in the core package, and the batch helper they lean on. Each returns an ordinary `*filesystem.Disk`, passes the [conformance suite](../../filesystemtest/README.md), and declares only the capabilities it can honour, so code that holds it needs no special case.

| Function | What it builds | Typical use |
|---|---|---|
| `filesystem.Route(name, mounts, opts...)` | One disk that dispatches each key to the disk mounted at its longest prefix | Avatars on a CDN bucket, exports on cold storage, the rest on a default disk |
| `filesystem.Failover(name, primary, secondaries, opts)` | Reads that move to the next disk when the primary is down | Keep serving files through a backend outage |
| `filesystem.Overlay(name, upper, lower, opts...)` | Copy-on-write layer: reads fall through, writes and deletes stay on top | Preview environments, tests and sandboxes over shared read-only data |
| `filesystem.CachedRead(name, remote, cache, opts)` | Reads served from a local copy, invalidated by writes | A local disk in front of a slow or metered bucket |
| `filesystem.Batch(ctx, keys, parallel, fn)` | Bounded-parallel per-key work with a `*BatchError` | Fanning out copies, deletes or re-encodes |

Wrapper disks are the exception to the interceptor rule: they exist only because they change which disk or key an operation reaches. Cross-cutting behaviour (retries, limits, throttling) stays in [interceptors](interceptors.md).

The snippets come from `examples/concepts-composing`, which builds each composition on memory disks and prints what it did. Run it with `go run ./concepts-composing` from the `examples` directory.

## Before you start

- Every composition takes `*filesystem.Disk` values, so build the disks it composes first, each with its own driver, interceptors and options. The example uses a small helper for empty memory disks.
- A composed disk copies the interceptor chain of the disk it derives from (the primary of a failover, the remote of a cached disk), so it logs and retries like that disk. A routed or overlay disk starts with none of its own; pass them as options.
- Hold the composed disk like any other: put it in a `Manager`, scope it, serve it.


<a id="route-by-prefix"></a>

## Step 1 — Route by prefix

One handle for several backends: avatars on a bucket behind a CDN, exports on cold storage, everything else on a default disk. Application code writes `avatars/u1.png` and never learns which backend holds it; moving a prefix to another backend is a configuration change.

<!-- ds:block id=newroute-zbnmzkph -->
**newRoute** · [`examples/concepts-composing/main.go:76-95`](../../examples/concepts-composing/main.go#L76-L95)

```go
func newRoute() (*filesystem.Disk, *filesystem.Disk, error) {
	root, err := mem("default")
	if err != nil {
		return nil, nil, err
	}
	avatars, err := mem("avatars", filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return nil, nil, err
	}
	exports, err := mem("exports")
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.Route("app", []filesystem.Mount{
		{Disk: root}, // "" is the catch-all
		{Prefix: "avatars", Disk: avatars},
		{Prefix: "exports", Disk: exports},
	})
	return disk, avatars, err
}
```
<!-- /ds:block hash=11d80a -->

<!-- ds:block id=routebyprefix-2p8jrm5h -->
**routeByPrefix** · [`examples/concepts-composing/main.go:101-127`](../../examples/concepts-composing/main.go#L101-L127)

```go
func routeByPrefix(ctx context.Context, out io.Writer) error {
	disk, avatars, err := newRoute()
	if err != nil {
		return err
	}
	for _, k := range []string{"avatars/u1.png", "exports/2026-10.zip", "notes.txt"} {
		if _, err := disk.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	onAvatars, err := keys(ctx, avatars)
	if err != nil {
		return err
	}
	all, err := keys(ctx, disk)
	if err != nil {
		return err
	}
	link, err := avatars.PublicURL(ctx, "u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "route: avatars disk holds %q, routed listing %q, avatar url %s\n", onAvatars, all, link)
	_, err = disk.Copy(ctx, "notes.txt", "exports/notes.txt")
	fmt.Fprintf(out, "route: copy across mounts: %v\n", err)
	return nil
}
```
<!-- /ds:block hash=fbd5d0 -->

Check: `route: avatars disk holds "u1.png", routed listing "avatars/u1.png exports/2026-10.zip notes.txt", avatar url https://cdn.example.com/u1.png`, then `route: copy across mounts: <nil>`.

- The longest matching prefix wins, so a mount at `a/b` takes `a/b/…` from a mount at `a`. Keys of a lower mount under a deeper one are shadowed: unreachable and not listed.
- Without a catch-all, a key under no prefix does not exist: `Stat` and `Read` report `CodeNotFound`, `Delete` succeeds, `Write` fails with `CodeInvalidArgument`.
- Listing a directory above mounts merges them: the root above shows `avatars/` and `exports/` beside the catch-all's own entries (a directory with no visible key, such as an empty mount or one whose keys an empty mount shadows, is not shown), and a recursive listing returns every mount's keys in key order. Cursors of a merged listing are the routed disk's own; pass them back unchanged.
- `Copy` and `Move` inside one mount use that disk (server-side where it can); across mounts they stream with `CopyAcross` and `MoveAcross`, and a move across mounts is not atomic.
- A capability is declared only when every mount has it (`Copy`, `Move`, `SetVisibility`, `Presign`, `PublicURL`, `PresignHeaders`, `PresignIP`, `PresignVisibility`, the write attributes). `Presign` is declared only when every mount can sign both downloads and uploads, each through its driver or its `URLSigner` as `WithURLSigner` routes that method (see [Serving files](serving-files.md#which-path-signs-which-method)); a mirrored or cached mount signs downloads only, so a route over one does not declare it. `PresignIP` and `PresignHeaders` likewise follow whichever signs each method. Signer URLs name the mounted disk, so serve that disk with `fshttp.Signed`. `DeleteKeys` is always declared, each mount deleting its share its own way. Multipart, form uploads, appends, tags, holds, versions, version reads, storage classes and sweeps are not routed: call the mounted disk.

<a id="fail-reads-over"></a>

## Step 2 — Fail reads over

Keep serving files through a backend outage by reading from a replica. The example's primary is a driver that stopped answering, which is how a real outage looks to the Disk: every call fails with `unavailable`.

<!-- ds:block id=outage-nszgrjg5 -->
**outage** · [`examples/concepts-composing/main.go:133`](../../examples/concepts-composing/main.go#L133-L133)

```go
type outage struct{ *memory.Driver }
```
<!-- /ds:block hash=3ac45f -->

<!-- ds:block id=failover-ut5xt5yd -->
**failover** · [`examples/concepts-composing/main.go:151-179`](../../examples/concepts-composing/main.go#L151-L179)

```go
func failover(ctx context.Context, out io.Writer) error {
	replica, err := mem("replica")
	if err != nil {
		return err
	}
	if _, err := replica.WriteBytes(ctx, "logo.png", []byte("logo"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	primary, err := filesystem.NewDisk("primary", outage{memory.New()})
	if err != nil {
		return err
	}
	disk, err := filesystem.Failover("media", primary, []*filesystem.Disk{replica}, filesystem.FailoverOptions{
		OnFailover: func(_ context.Context, e filesystem.FailoverEvent) {
			fmt.Fprintf(out, "failover: %s %s moved from %s to %s\n", e.Op, e.Key, e.From, e.To)
		},
	})
	if err != nil {
		return err
	}
	b, err := disk.ReadAll(ctx, "logo.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "failover: read %q while the primary is down\n", b)
	_, err = disk.WriteBytes(ctx, "new.png", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "failover: write during the outage: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=888fa3 -->

Check: `failover: read logo.png moved from primary to replica`, `failover: read "logo" while the primary is down`, and `failover: write during the outage: unavailable`.

`Stat` and `Read` go to the primary; when it fails with `CodeUnavailable` or `CodeUnknown` (change that with `ShouldFailover`), each secondary is tried in turn and `OnFailover` is told. An answer such as `CodeNotFound` or `CodePermission` is the primary's answer and is returned. Writes, deletes, listings (a cursor means nothing to another backend) and every capability stay on the primary, which keeps its interceptors; conditional reads run as a `Stat` and a `Read`, so they fail over too. Secondaries are only read: keep them in step with `Mirror` or a backfill, and expect a failed-over read to return whatever the secondary holds.

<a id="overlay-copy-on-write"></a>

## Step 3 — Overlay copy on write

A preview environment, a test or a sandbox that sees shared data and changes it freely, without ever touching it:

<!-- ds:block id=overlay-mryuhup4 -->
**overlay** · [`examples/concepts-composing/main.go:185-223`](../../examples/concepts-composing/main.go#L185-L223)

```go
func overlay(ctx context.Context, out io.Writer) error {
	production, err := mem("production")
	if err != nil {
		return err
	}
	for _, k := range []string{"a.txt", "b.txt"} {
		if _, err := production.WriteBytes(ctx, k, []byte("prod "+k), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	sandbox, err := mem("sandbox")
	if err != nil {
		return err
	}
	preview, err := filesystem.Overlay("preview", sandbox, production)
	if err != nil {
		return err
	}
	if _, err := preview.WriteBytes(ctx, "a.txt", []byte("edited in preview"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if err := preview.Delete(ctx, "b.txt"); err != nil {
		return err
	}
	a, err := preview.ReadAll(ctx, "a.txt")
	if err != nil {
		return err
	}
	seen, err := keys(ctx, preview)
	if err != nil {
		return err
	}
	untouched, err := keys(ctx, production)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "overlay: preview reads %q, lists %q; production still lists %q\n", a, seen, untouched)
	return nil
}
```
<!-- /ds:block hash=2f6052 -->

Check: `overlay: preview reads "edited in preview", lists "a.txt"; production still lists "a.txt b.txt"`.

Reads find a key on the upper disk first and fall through to the lower one; writes go to the upper disk only. Deleting a key the lower disk holds writes an empty whiteout at `.overlay-whiteouts/<key>` on the upper disk, which hides it; writing the key again removes the whiteout. Keys under `filesystem.OverlayWhiteouts`, in any letter case, are refused with `CodeInvalidKey`, so a case-folding upper disk cannot be handed a forged whiteout. Listings merge both layers, upper winning, and hide whiteouts and lower directories whose keys are all whited out. The lower disk is never changed. The overlay declares the write attributes its upper disk stores and nothing else: `Copy` and `Move` stream through the Disk (a move of a lower key copies it up and whites it out), deletes go key by key, and URLs, visibility, conditional writes and multipart are not offered, because an object can sit on either layer.

<a id="cache-reads"></a>

## Step 4 — Cache reads

A local copy in front of a slow or metered bucket. The example counts the reads that reach the bucket with a driver wrapper; an interceptor on the remote disk would not do, because the cached disk copies the remote's chain and the interceptor would also see every read the cache served.

<!-- ds:block id=metered-hfvw72xc -->
**metered** · [`examples/concepts-composing/main.go:228-231`](../../examples/concepts-composing/main.go#L228-L231)

```go
type metered struct {
	*memory.Driver
	reads int
}
```
<!-- /ds:block hash=a7d218 -->

<!-- ds:block id=cachedreads-jmjc532a -->
**cachedReads** · [`examples/concepts-composing/main.go:243-280`](../../examples/concepts-composing/main.go#L243-L280)

```go
func cachedReads(ctx context.Context, out io.Writer) error {
	bucket := &metered{Driver: memory.New()}
	remote, err := filesystem.NewDisk("bucket", bucket)
	if err != nil {
		return err
	}
	cache, err := mem("cache")
	if err != nil {
		return err
	}
	disk, err := filesystem.CachedRead("media", remote, cache, filesystem.CacheOptions{TTL: cacheTTL})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "video.mp4", []byte("first cut"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	for range 3 {
		if _, err := disk.ReadAll(ctx, "video.mp4"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "cache: 3 reads, %d reached the bucket\n", bucket.reads)
	if _, err := disk.WriteBytes(ctx, "video.mp4", []byte("final cut"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	b, err := disk.ReadAll(ctx, "video.mp4")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cache: after a write, read %q (bucket reads: %d)\n", b, bucket.reads)
	trimmed, err := filesystem.TrimCache(ctx, cache, cacheBudget)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cache: trimmed %d copy to stay under %d bytes\n", trimmed, cacheBudget)
	return nil
}
```
<!-- /ds:block hash=71a3e0 -->

Check: `cache: 3 reads, 1 reached the bucket`, `cache: after a write, read "final cut" (bucket reads: 2)`, and `cache: trimmed 1 copy to stay under 8 bytes`.

A read of a key the cache does not hold copies the whole object to the cache, then serves the read (ranges included) from the copy. A copy younger than the TTL is served without asking the remote; an older one is revalidated with a `Stat`: the same ETag (the cache must store metadata to remember it) serves the copy and, where the cache can set modification times, makes it fresh again. Writes, deletes, copies, moves, appends, completed multipart uploads and restored versions through the disk drop the copy. A failing cache never fails a call: reads come from the remote, writes and deletes return the remote's result, and `OnCacheError` is told, since a copy that could not be dropped stays stale until the TTL passes. A conditional read (`ReadIf`) serves the copy only when it was made from the ETag the remote reports now, so its entry and body always match. Signed PUT URLs are refused and form uploads not offered, since an upload straight to the remote would leave the copy stale; `Stat`, `List` and URLs are always the remote's. Freshness is bounded by the TTL, not exact, when other processes write the remote. `CacheOptions.Now` (nil means `time.Now`) is the clock a copy's age is judged by, so a test can expire copies by moving a fake clock instead of sleeping; it must agree with the cache disk's clock, which stamps the copies. `TrimCache` deletes the oldest copies until the cache holds at most the bytes given; run it as a [maintenance job](maintenance-jobs.md).

Run `TrimCache` from a [maintenance job](maintenance-jobs.md) (`jobs.Func`) to keep the cache under a budget; deleting a copy only costs a fetch on its next read.

<a id="batch-per-key-work"></a>

## Step 5 — Batch per-key work

Fanning out copies, deletes or re-encodes over many keys, with a bound on how many run at once and a report of every key that failed rather than only the first error:

<!-- ds:block id=batch-t89u2ebb -->
**batch** · [`examples/concepts-composing/main.go:286-320`](../../examples/concepts-composing/main.go#L286-L320)

```go
func batch(ctx context.Context, out io.Writer) error {
	src, err := mem("src")
	if err != nil {
		return err
	}
	dst, err := mem("dst")
	if err != nil {
		return err
	}
	ks := []string{"a.txt", "b.txt", "missing-1.txt", "c.txt", "missing-2.txt"}
	for _, k := range ks {
		if !strings.HasPrefix(k, "missing") {
			if _, err := src.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
				return err
			}
		}
	}
	err = filesystem.Batch(ctx, ks, parallel, func(ctx context.Context, key string) error {
		_, err := filesystem.CopyAcross(ctx, src, key, dst, key)
		return err
	})
	var failed *filesystem.BatchError
	if !errors.As(err, &failed) {
		return fmt.Errorf("expected a batch error, got %v", err)
	}
	copied, err := keys(ctx, dst)
	if err != nil {
		return err
	}
	for _, f := range failed.Failures {
		fmt.Fprintf(out, "batch: %s failed: %s\n", f.Key, filesystem.CodeOf(f.Err))
	}
	fmt.Fprintf(out, "batch: copied %q\n", copied)
	return nil
}
```
<!-- /ds:block hash=3e1047 -->

Check: `batch: missing-1.txt failed: not_found`, `batch: missing-2.txt failed: not_found`, then `batch: copied "a.txt b.txt c.txt"`.

`Batch` runs at most `parallel` calls at once and returns `nil` or a `*BatchError` with every failed key in input order. Once the context ends no call starts and the remaining keys fail with `CodeUnavailable`, so the error still names every key that was not processed. The Disk's own fallbacks use it: `DeleteKeys` and `DeletePrefix` on a driver without bulk deletes delete eight keys at a time (a `layer.Limit` on the disk still caps the total), and return a `*BatchError` naming the keys that could not be deleted.

## Which composition

| You need | Use | Not |
|---|---|---|
| Different prefixes on different backends | `Route` | several disks threaded through the code by hand |
| Reads to survive an outage | `Failover`, with the replica kept in step by `Mirror` | `Mirror` alone, which writes everywhere but reads only the primary |
| Writes to land on several backends | `Mirror` ([Backups and mirrors](../recipes/backups-and-mirrors.md)) | `Failover`, which never writes a secondary |
| Changes that must not reach shared data | `Overlay` | a copy of the data |
| Fewer reads of a slow or metered backend | `CachedRead` | a CDN in front of private files, which cannot check your links |
| One tenant confined to a prefix | `Disk.Scope` ([Multi-tenancy](multi-tenancy.md)) | `Route`, which maps prefixes but does not confine |

## Pitfalls

- Mounting one disk twice, or a disk inside another's tree, under a route: keys then have two names, and a delete under one is visible under the other.
- Giving a cached or overlay disk an upper or cache disk that something else writes: `TrimCache` deletes from the cache disk, and stray keys on an upper disk show through the overlay.
- Expecting `Failover` to keep secondaries in step: it only reads them.
- Counting backend calls with an interceptor on a disk that a failover or cached disk wraps. The composed disk copies that chain, so the interceptor also sees calls that never reached the backend; count in a driver wrapper, as step 4 does.
- A move across route mounts after a crash. It is a copy then a delete, not atomic: design cleanup for both keys existing.
- A `CachedRead` cache disk that cannot store metadata. Without the remote's ETag the copy cannot be revalidated, so every read past the TTL fetches the object again.

## Next


- [Interceptors](interceptors.md): behaviour around every operation, including bandwidth limits, fault injection and hedged reads.
- [Capabilities](capabilities.md): what each composition declares, and the fallbacks the Disk runs when one is missing.
