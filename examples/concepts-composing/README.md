# Composing disks: route, failover, overlay, cache and batch

A `Disk` can stand in front of other disks, and the result is still an ordinary `*filesystem.Disk`. This program builds each composition in the core package on memory disks: a routed disk that sends each prefix to its own backend, reads that fail over to a replica while the primary is down, a copy-on-write overlay that never changes the data under it, reads served from a local cache with the bucket's reads counted, and bounded-parallel copies that report every key that failed. It uses the core `filesystem` package and `memory`. The guide [Composing disks](../../docs/concepts/composing-disks.md) explains each one and when to use which.

## Run it

```sh
cd examples
go run ./concepts-composing
go test ./concepts-composing
```

Everything runs on memory disks: no network, no environment variables.

## What it does

```sh
go run ./concepts-composing
```

```text
route: avatars disk holds "u1.png", routed listing "avatars/u1.png exports/2026-10.zip notes.txt", avatar url https://cdn.example.com/u1.png
route: copy across mounts: <nil>
failover: read logo.png moved from primary to replica
failover: read "logo" while the primary is down
failover: write during the outage: unavailable
overlay: preview reads "edited in preview", lists "a.txt"; production still lists "a.txt b.txt"
cache: 3 reads, 1 reached the bucket
cache: after a write, read "final cut" (bucket reads: 2)
cache: trimmed 1 copy to stay under 8 bytes
batch: missing-1.txt failed: not_found
batch: missing-2.txt failed: not_found
batch: copied "a.txt b.txt c.txt"
```

1. Three disks mounted under one handle: the avatars disk holds `u1.png` without its prefix and serves it from its CDN, the routed listing shows every mount's keys, and a copy across mounts streams between them.
2. While the primary answers `unavailable`, a read moves to the replica and `OnFailover` is told; a write stays on the primary and fails.
3. A preview overlay edits one file and deletes another; it sees its own changes, and the production disk underneath still lists both originals.
4. Three reads through the cached disk reach the bucket once; a write drops the copy, so the next read fetches the new version; `TrimCache` deletes the oldest copy to stay under a byte budget.
5. `Batch` copies five keys, four at a time, and its `*BatchError` names both keys that failed, in input order.

## The code

The routed disk and its mounts.

<!-- ds:block id=newroute-zbnmzkph -->
**newRoute** · [`examples/concepts-composing/main.go:76-95`](main.go#L76-L95)

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
**routeByPrefix** · [`examples/concepts-composing/main.go:101-127`](main.go#L101-L127)

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

A backend that stopped answering, and reads failing over from it.

<!-- ds:block id=outage-nszgrjg5 -->
**outage** · [`examples/concepts-composing/main.go:133`](main.go#L133-L133)

```go
type outage struct{ *memory.Driver }
```
<!-- /ds:block hash=3ac45f -->

<!-- ds:block id=failover-ut5xt5yd -->
**failover** · [`examples/concepts-composing/main.go:151-179`](main.go#L151-L179)

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

A copy-on-write overlay.

<!-- ds:block id=overlay-mryuhup4 -->
**overlay** · [`examples/concepts-composing/main.go:185-223`](main.go#L185-L223)

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

A bucket that counts its reads, and a cache in front of it.

<!-- ds:block id=metered-hfvw72xc -->
**metered** · [`examples/concepts-composing/main.go:228-231`](main.go#L228-L231)

```go
type metered struct {
	*memory.Driver
	reads int
}
```
<!-- /ds:block hash=a7d218 -->

<!-- ds:block id=cachedreads-jmjc532a -->
**cachedReads** · [`examples/concepts-composing/main.go:243-280`](main.go#L243-L280)

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

Bounded-parallel copies.

<!-- ds:block id=batch-t89u2ebb -->
**batch** · [`examples/concepts-composing/main.go:286-320`](main.go#L286-L320)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: one function per composition, and the two driver wrappers |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Composing disks](../../docs/concepts/composing-disks.md).
- Writing to several backends at once: [Backups and mirrors](../../docs/recipes/backups-and-mirrors.md) ([recipe-backups-and-mirrors](../recipe-backups-and-mirrors)).
- [Interceptors](../../docs/concepts/interceptors.md) ([concepts-interceptors](../concepts-interceptors)): behaviour around every operation rather than a different backend.
