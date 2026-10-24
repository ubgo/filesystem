# Disks and drivers: a tour of the Disk API

The `*filesystem.Disk` is what an application holds; a driver only moves bytes for one backend. This program tours the Disk API on the memory and local drivers: writing and reading (with the size, SHA-256 and content type the Disk computes), byte ranges, appends, conditional reads and writes, a local temporary copy, the conventions every call follows, `Stat` and `Exists`, listings both recursive and one level at a time with pages and folded directories, copy and move, the three deletes, a server-side multipart upload, the start-up `Check`, and the typed escape hatch `As` to the driver's native client. It uses the core `filesystem` package with `memory` and `local`. The guide [Disks and drivers](../../docs/concepts/disks-and-drivers.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-disks
go test ./concepts-disks
```

Most steps run on a memory disk; the last one creates a temporary directory for a local disk and removes it on exit. No network, no environment variables.

## What it does

```sh
go run ./concepts-disks
```

```text
wrote docs/hello.txt: 14 bytes, text/plain; charset=utf-8, sha256 60cf5d8443f5…
read "hello, storage"
range 7+7: "storage"
stat docs/hello.txt: 14 bytes, text/plain; charset=utf-8, visibility private
stat missing: not_found
exists missing: false
recursive: docs/a.txt docs/b.txt docs/hello.txt docs/img/logo.png
page 1: docs/a.txt docs/b.txt next="docs/b.txt"
page 2: docs/hello.txt docs/img/ next=""
copied and moved to archive/hello.txt (old key exists: false)
copy onto itself: invalid_argument
deleted a missing key: ok
delete the root: invalid_argument
left: docs/hello.txt docs/img/logo.png
multipart: 3 parts listed, video/clip.bin is 10 bytes
append past 32 bytes: too_large
log: 25 bytes, modified 2026-01-02T03:04:05Z
read if changed: not_modified
write with a stale etag: conflict
read if changed: {"v":2} (etag is the new one: true)
temp file: extension .txt, "hello, storage"
temp file over its cap: too_large
cancelled write: unavailable, is context.Canceled: true, stored: false
fs.ReadFile: "hello, storage"
bad option at start-up: invalid_argument
check: ok
as *os.Root: true, rooted at the disk's directory: true
as *strings.Builder: false
```

1. A write returns the size and SHA-256 computed while streaming, and the content type sniffed from the bytes because none was given.
2. A range read returns 7 bytes from offset 7; `Stat` shows the entry, private by default.
3. `Stat` of a missing key is `not_found`; `Exists` turns exactly that code into `false`.
4. `List` walks the tree recursively; `ListPage` returns one level, two entries per page, with `docs/img/` folded into a directory entry.
5. Copy then move: the old key is gone, and copying a key onto itself is `invalid_argument`.
6. Deleting a missing key succeeds, `DeleteKeys` and `DeletePrefix` remove the rest, and the disk root can never be deleted as a prefix.
7. A multipart upload in three parts is listed and completed into one 10-byte object.
8. `Append` grows a log and refuses, whole, the append that would pass its 32-byte cap; `SetModTime` stamps the time `Stat` then reports.
9. `ReadIf` with the cached ETag answers `not_modified`; a write with `IfMatch` succeeds once, and the second writer holding the same old ETag gets `conflict`; reading again returns the new body and ETag.
10. `TempFile` copies an object to a local file that keeps its `.txt` extension, and refuses an object over its `MaxSize`.
11. The conventions every call follows: a cancelled context stops a write (classified `unavailable`, still `context.Canceled`, nothing stored), `FS` makes the disk an `io/fs.FS`, and a malformed option fails when the disk is built.
12. `Check` proves a local disk works end to end, and `As` reaches its `*os.Root`; asking for a type the driver does not have is `false`.

## The code

A Disk over the memory driver; the Disk adds key cleaning, content-type detection, hashing, size limits, error classification and interceptors.

<!-- ds:block id=newmemorydisk-7us7uxyf -->
**newMemoryDisk** · [`examples/concepts-disks/main.go:95-97`](main.go#L95-L97)

```go
func newMemoryDisk() (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New())
}
```
<!-- /ds:block hash=63e54c -->

<!-- ds:block id=writeandread-sxu8j9t4 -->
**writeAndRead** · [`examples/concepts-disks/main.go:103-120`](main.go#L103-L120)

```go
func writeAndRead(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Write(ctx, greetingKey, strings.NewReader(greeting), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: %d bytes, %s, sha256 %s…\n", e.Key, e.Size, e.ContentType, e.SHA256[:12])
	rc, err := disk.Read(ctx, greetingKey)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read %q\n", b)
	return nil
}
```
<!-- /ds:block hash=f001d3 -->

<!-- ds:block id=readrange-bg2wcst7 -->
**readRange** · [`examples/concepts-disks/main.go:124-136`](main.go#L124-L136)

```go
func readRange(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	rc, err := disk.ReadRange(ctx, greetingKey, filesystem.ByteRange{Offset: wordOffset, Length: wordLength})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "range %d+%d: %q\n", wordOffset, wordLength, b)
	return nil
}
```
<!-- /ds:block hash=4a4cba -->

<!-- ds:block id=statandexists-jr3aucg4 -->
**statAndExists** · [`examples/concepts-disks/main.go:141-155`](main.go#L141-L155)

```go
func statAndExists(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	e, err := disk.Stat(ctx, greetingKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat %s: %d bytes, %s, visibility %s\n", e.Key, e.Size, e.ContentType, e.Visibility)
	_, err = disk.Stat(ctx, "docs/missing.txt")
	fmt.Fprintf(out, "stat missing: %s\n", filesystem.CodeOf(err))
	ok, err := disk.Exists(ctx, "docs/missing.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "exists missing: %t\n", ok)
	return nil
}
```
<!-- /ds:block hash=a4d368 -->

Two ways to list: the `List` iterator fetches pages lazily; `ListPage` returns one page and a cursor.

<!-- ds:block id=listtree-kzdkcv86 -->
**listTree** · [`examples/concepts-disks/main.go:161-187`](main.go#L161-L187)

```go
func listTree(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	for _, k := range []string{"docs/a.txt", "docs/b.txt", "docs/img/logo.png"} {
		if _, err := disk.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	var all []string
	for e, err := range disk.List(ctx, "docs", filesystem.ListOptions{Recursive: true, Limit: pageSize}) {
		if err != nil {
			return err
		}
		all = append(all, e.Key)
	}
	fmt.Fprintf(out, "recursive: %s\n", strings.Join(all, " "))
	opts := filesystem.ListOptions{Limit: pageSize}
	for page := 1; ; page++ {
		p, err := disk.ListPage(ctx, "docs", opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "page %d: %s next=%q\n", page, describe(p.Entries), p.NextCursor)
		if p.NextCursor == "" {
			return nil
		}
		opts.Cursor = p.NextCursor
	}
}
```
<!-- /ds:block hash=474b5c -->

The memory driver copies and moves natively; a driver without those capabilities gets the Disk's fallback with the same result.

<!-- ds:block id=copyandmove-yksfax76 -->
**copyAndMove** · [`examples/concepts-disks/main.go:205-221`](main.go#L205-L221)

```go
func copyAndMove(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if _, err := disk.Copy(ctx, greetingKey, "backup/hello.txt"); err != nil {
		return err
	}
	e, err := disk.Move(ctx, "backup/hello.txt", "archive/hello.txt")
	if err != nil {
		return err
	}
	moved, err := disk.Exists(ctx, "backup/hello.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "copied and moved to %s (old key exists: %t)\n", e.Key, moved)
	_, err = disk.Copy(ctx, greetingKey, greetingKey)
	fmt.Fprintf(out, "copy onto itself: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=de1628 -->

<!-- ds:block id=deletethings-a7pa7647 -->
**deleteThings** · [`examples/concepts-disks/main.go:227-249`](main.go#L227-L249)

```go
func deleteThings(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if err := disk.Delete(ctx, "never/written.txt"); err != nil {
		return err
	}
	fmt.Fprintln(out, "deleted a missing key: ok")
	if err := disk.DeleteKeys(ctx, []string{"docs/a.txt", "docs/b.txt", "docs/a.txt"}); err != nil {
		return err
	}
	if err := disk.DeletePrefix(ctx, "archive"); err != nil {
		return err
	}
	err := disk.DeletePrefix(ctx, "")
	fmt.Fprintf(out, "delete the root: %s\n", filesystem.CodeOf(err))
	var left []string
	for e, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		left = append(left, e.Key)
	}
	fmt.Fprintf(out, "left: %s\n", strings.Join(left, " "))
	return nil
}
```
<!-- /ds:block hash=236a08 -->

A client resuming an interrupted upload would call `ListParts` and send only the missing parts.

<!-- ds:block id=multipartupload-7z3ejr3z -->
**multipartUpload** · [`examples/concepts-disks/main.go:255-281`](main.go#L255-L281)

```go
func multipartUpload(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	key, body := clipKey, clip
	id, err := disk.CreateMultipart(ctx, key, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	var parts []filesystem.Part
	for n, off := filesystem.MinPartNumber, 0; off < len(body); n, off = n+1, off+partSize {
		chunk := body[off:min(off+partSize, len(body))]
		p, err := disk.UploadPart(ctx, key, id, n, strings.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			_ = disk.AbortMultipart(ctx, key, id)
			return err
		}
		parts = append(parts, p)
	}
	listed, err := disk.ListParts(ctx, key, id)
	if err != nil {
		return err
	}
	e, err := disk.CompleteMultipart(ctx, key, id, parts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "multipart: %d parts listed, %s is %d bytes\n", len(listed), e.Key, e.Size)
	return nil
}
```
<!-- /ds:block hash=448e0e -->

<!-- ds:block id=checkandescape-bt2t74mr -->
**checkAndEscape** · [`examples/concepts-disks/main.go:387-407`](main.go#L387-L407)

```go
func checkAndEscape(ctx context.Context, dir string, out io.Writer) error {
	drv, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("uploads", drv)
	if err != nil {
		_ = drv.Close()
		return err
	}
	defer func() { _ = disk.Close() }()
	if err := disk.Check(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "check: ok")
	root, ok := filesystem.As[*os.Root](disk)
	fmt.Fprintf(out, "as *os.Root: %t, rooted at the disk's directory: %t\n", ok, ok && root.Name() == dir)
	_, ok = filesystem.As[*strings.Builder](disk)
	fmt.Fprintf(out, "as *strings.Builder: %t\n", ok)
	return nil
}
```
<!-- /ds:block hash=6e19f3 -->

Append with a size cap, then stamp the modification time.

<!-- ds:block id=appendandmodtime-873u6nvw -->
**appendAndModTime** · [`examples/concepts-disks/main.go:289-307`](main.go#L289-L307)

```go
func appendAndModTime(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	opts := filesystem.AppendOptions{MaxSize: logCap}
	for _, line := range []string{"started\n", "uploaded 3 files\n"} {
		if _, err := disk.Append(ctx, logKey, strings.NewReader(line), opts); err != nil {
			return err
		}
	}
	_, err := disk.Append(ctx, logKey, strings.NewReader("one line too many\n"), opts)
	fmt.Fprintf(out, "append past %d bytes: %s\n", logCap, filesystem.CodeOf(err))
	if err := disk.SetModTime(ctx, logKey, imported); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, logKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "log: %d bytes, modified %s\n", e.Size, e.ModTime.UTC().Format(time.RFC3339))
	return nil
}
```
<!-- /ds:block hash=9ff432 -->

Conditional reads and compare-and-swap writes.

<!-- ds:block id=conditionalreadandwrite-8yeavz32 -->
**conditionalReadAndWrite** · [`examples/concepts-disks/main.go:314-338`](main.go#L314-L338)

```go
func conditionalReadAndWrite(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	cached, err := disk.WriteBytes(ctx, docKey, []byte(`{"v":1}`), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	_, _, err = disk.ReadIf(ctx, docKey, filesystem.ReadOptions{IfNoneMatch: cached.ETag})
	fmt.Fprintf(out, "read if changed: %s\n", filesystem.CodeOf(err))
	next, err := disk.WriteBytes(ctx, docKey, []byte(`{"v":2}`), filesystem.WriteOptions{IfMatch: cached.ETag})
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, docKey, []byte(`{"v":3}`), filesystem.WriteOptions{IfMatch: cached.ETag})
	fmt.Fprintf(out, "write with a stale etag: %s\n", filesystem.CodeOf(err))
	rc, e, err := disk.ReadIf(ctx, docKey, filesystem.ReadOptions{IfNoneMatch: cached.ETag})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read if changed: %s (etag is the new one: %t)\n", b, e.ETag == next.ETag)
	return nil
}
```
<!-- /ds:block hash=aab927 -->

A local temporary copy for tools that need a path.

<!-- ds:block id=tempcopy-v3f5qefn -->
**tempCopy** · [`examples/concepts-disks/main.go:344-358`](main.go#L344-L358)

```go
func tempCopy(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	path, err := disk.TempFile(ctx, greetingKey, filesystem.TempFileOptions{MaxSize: tempCap})
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "temp file: extension %s, %q\n", filepath.Ext(path), b)
	_, err = disk.TempFile(ctx, greetingKey, filesystem.TempFileOptions{MaxSize: wordLength})
	fmt.Fprintf(out, "temp file over its cap: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=f25139 -->

Context, standard library types and typed options.

<!-- ds:block id=conventions-cfsevhds -->
**conventions** · [`examples/concepts-disks/main.go:366-381`](main.go#L366-L381)

```go
func conventions(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := disk.WriteBytes(cancelled, "late.txt", []byte("x"), filesystem.WriteOptions{})
	stored, _ := disk.Exists(ctx, "late.txt")
	fmt.Fprintf(out, "cancelled write: %s, is context.Canceled: %t, stored: %t\n",
		filesystem.CodeOf(err), errors.Is(err, context.Canceled), stored)
	b, err := fs.ReadFile(disk.FS(ctx), greetingKey)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "fs.ReadFile: %q\n", b)
	_, err = filesystem.NewDisk(diskName, memory.New(), filesystem.WithPublicURL("cdn.example.com"))
	fmt.Fprintf(out, "bad option at start-up: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=aca327 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: every step of the tour |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Disks and drivers](../../docs/concepts/disks-and-drivers.md).
- [Keys](../../docs/concepts/keys.md) ([concepts-keys](../concepts-keys)), [Capabilities](../../docs/concepts/capabilities.md) ([concepts-capabilities](../concepts-capabilities)), [Configuration](../../docs/concepts/configuration.md) ([concepts-configuration](../concepts-configuration)).
- The drivers: [local](../adapters-local) and [memory](../adapters-memory).
