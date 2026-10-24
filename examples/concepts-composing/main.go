// Command concepts-composing builds disks out of other disks: a routed
// disk that sends each prefix to its own backend, reads that fail over to
// a replica, a copy-on-write overlay over data it never changes, reads
// served from a local cache, and bounded-parallel work over many keys
// with a report of every key that failed.
//
//	go run ./concepts-composing          (from the examples directory)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// Settings of the demo.
const (
	// cdnBase is where the avatars disk's public files are served from.
	cdnBase = "https://cdn.example.com"
	// cacheTTL is how long a cached copy is served without asking the
	// remote disk.
	cacheTTL = 10 * time.Minute
	// cacheBudget is the most the cache disk may hold after a trim.
	cacheBudget = 8
	// parallel is how many keys the batch step works on at once.
	parallel = 4
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{
		routeByPrefix, failover, overlay, cachedReads, batch,
	} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// mem returns an empty memory disk with the given name and options.
func mem(name string, opts ...filesystem.Option) (*filesystem.Disk, error) {
	return filesystem.NewDisk(name, memory.New(), opts...)
}

// keys lists every key on a disk, recursively, in key order.
func keys(ctx context.Context, d *filesystem.Disk) (string, error) {
	var all []string
	for e, err := range d.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return "", err
		}
		all = append(all, e.Key)
	}
	return strings.Join(all, " "), nil
}

// newRoute mounts three disks under one handle: avatars on a disk with a
// CDN, exports on cold storage, and everything else on a default disk. It
// returns the routed disk and the avatars disk, to look behind the route.
// ds:def id=newroute-zbnmzkph
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

// routeByPrefix writes through the routed disk. The longest matching
// prefix wins, the mounted disk sees the key without its prefix, and a
// copy across mounts streams from one backend to the other.
// ds:def id=routebyprefix-2p8jrm5h
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

// outage is a memory driver whose backend stopped answering: every read
// and write fails with unavailable, as a real backend's timeouts and
// connection resets are classified.
// ds:def id=outage-nszgrjg5
type outage struct{ *memory.Driver }

func (outage) Stat(context.Context, string) (filesystem.Entry, error) {
	return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "connection refused")
}

func (outage) Read(context.Context, string, filesystem.ByteRange) (io.ReadCloser, error) {
	return nil, filesystem.Errorf(filesystem.CodeUnavailable, "connection refused")
}

func (outage) Write(context.Context, string, io.Reader, filesystem.WriteOptions) (filesystem.Entry, error) {
	return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "connection refused")
}

// failover reads from a replica while the primary is down. Writes stay on
// the primary, so they fail during the outage; keep the replica in step
// with Mirror or a backfill.
// ds:def id=failover-ut5xt5yd
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

// overlay puts a scratch disk on top of shared data. Reads fall through
// to the lower disk, writes and deletes stay on top, and the lower disk
// never changes: a preview environment over production files.
// ds:def id=overlay-mryuhup4
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

// metered is a memory driver that counts the reads reaching it, as a
// metered bucket bills them.
// ds:def id=metered-hfvw72xc
type metered struct {
	*memory.Driver
	reads int
}

func (m *metered) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	m.reads++
	return m.Driver.Read(ctx, key, rng)
}

// cachedReads puts a local copy in front of a slow or metered disk. The
// first read copies the object; reads within the TTL come from the copy;
// a write through the cached disk drops the copy, so the next read fetches
// the new version.
// ds:def id=cachedreads-jmjc532a
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

// batch copies many keys with at most parallel copies at once, and gets
// back every key that failed, in input order, instead of only the first
// error.
// ds:def id=batch-t89u2ebb
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
