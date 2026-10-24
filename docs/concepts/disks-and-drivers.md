# Disks and drivers

Everything in this library starts with a `*filesystem.Disk`: the one type your application holds for a storage location, whether the bytes live in a local directory, in memory, in an S3 bucket or on an SFTP server. This page explains what a Disk does on top of the backend, every method it offers, the `Driver` contract a backend implements, and the optional interfaces that make up its capabilities. Read it before anything else in the concepts section; every other page builds on it.

The snippets come from `examples/concepts-disks`, which runs the whole tour on the memory and local drivers. Run it with `go run ./concepts-disks` from the `examples` directory.

## Two types, two jobs

A **driver** moves bytes for one backend. It implements six methods and nothing portable: no key checks, no content-type detection, no hashing, no retries. A **disk** wraps one driver and adds everything that should behave the same on every backend:

| The Disk does | So that |
|---|---|
| Cleans every key with `CleanKey` and refuses `..` before the driver runs | path traversal is impossible whatever the driver does ([Keys](keys.md)) |
| Detects the content type from the first 512 bytes when the write does not give one | the type is the content's, not a client's claim ([Security](security.md)) |
| Counts bytes and computes SHA-256 while streaming a write | every `Write` returns the stored size and digest, on every backend |
| Enforces `WriteOptions.MaxSize` while streaming | nothing larger than the limit is ever stored |
| Checks every option against the driver's `Capabilities` | an option the backend cannot honour fails with `CodeUnsupported` instead of being dropped ([Capabilities](capabilities.md)) |
| Falls back to portable code for Copy, Move, DeleteKeys and DeletePrefix | those calls work on every backend |
| Classifies every error into a `Code` and stamps the operation, disk and key on it | error handling never depends on a vendor's error types ([Errors](errors.md)) |
| Runs every driver call through the interceptor chain as one `Op` | retry, timeouts, logging and metrics work on every backend ([Interceptors](interceptors.md)) |
| Builds public and signed URLs | links work the same on local disk as on S3 ([Serving files](serving-files.md)) |

A Disk is safe for concurrent use. Build it with `filesystem.NewDisk(name, driver, options...)`; the zero value is not usable. The name identifies the disk in errors, logs and signed URLs and must match `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`, because it appears in URL paths and configuration keys where nothing may need escaping.

<!-- ds:block id=newmemorydisk-7us7uxyf -->
**newMemoryDisk** · [`examples/concepts-disks/main.go:95-97`](../../examples/concepts-disks/main.go#L95-L97)

```go
func newMemoryDisk() (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New())
}
```
<!-- /ds:block hash=63e54c -->

Most applications do not call `NewDisk` themselves: they open disks from DSN strings with a `Mux` and hold them in a `Manager`. See [Configuration](configuration.md).

`NewDisk` takes these options:

| Option | Effect |
|---|---|
| `WithInterceptors(i...)` | appends interceptors; the first given runs outermost |
| `WithPublicURL(base)` | `PublicURL` returns `base + "/" + escaped key`, typically a CDN; base must be an absolute http(s) URL |
| `WithURLSigner(s, methods...)` | the signer `SignedURL` uses: by default for every download, and for uploads when the driver cannot sign (local, memory, SFTP); naming methods routes exactly those to it ([Serving files](serving-files.md#which-path-signs-which-method)); `signedurl.Signer` and the CDN signers are such signers |
| `WithDefaultVisibility(v)` | the visibility of writes that choose none; the default is `VisibilityPrivate` |
| `WithoutContentTypeDetection()` | writes without a `ContentType` are stored with none instead of a detected one |
| `WithContentDetector(fn)` | a `sniff.Detector` for formats the built-in detection does not know (DICOM, STL, Parquet); consulted only when the bytes come back `application/octet-stream`, so it can never relabel a known type ([Security](security.md#formats-the-detection-does-not-know)) |

## Step 1 — Write and read

`Write(ctx, key, body, opts)` streams `body` to the backend and replaces any existing object. The returned `Entry` carries the size and SHA-256 the Disk computed while streaming, and the content type: the one you passed in `WriteOptions.ContentType`, or the one detected from the first bytes and the key's extension. `Read` returns an `io.ReadCloser` you must close; `ReadAll` reads the whole object into memory and is for small objects only. `WriteBytes` is `Write` over a byte slice, which is always retryable because the Disk can rewind it.

<!-- ds:block id=writeandread-sxu8j9t4 -->
**writeAndRead** · [`examples/concepts-disks/main.go:103-120`](../../examples/concepts-disks/main.go#L103-L120)

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

Check: the program prints `wrote docs/hello.txt: 14 bytes, text/plain; charset=utf-8, sha256 60cf5d8443f5…` and `read "hello, storage"`.

The fields of `WriteOptions`:

| Field | Meaning | Needs |
|---|---|---|
| `ContentType` | stored with the object; empty means detected | `Capabilities.ContentType` to be stored (the entry reports it either way) |
| `Visibility` | `VisibilityPublic` or `VisibilityPrivate`; empty means the disk default | `Capabilities.SetVisibility` for anything other than the default |
| `Metadata` | key/value pairs stored with the object | `Capabilities.Metadata` |
| `Headers` | `CacheControl`, `ContentDisposition`, `ContentEncoding`, sent when the object is served | `Capabilities.Headers` |
| `IfNotExists` | fail with `CodeExists` if the key exists, atomically | `Capabilities.ConditionalWrite` |
| `IfMatch` | compare-and-swap: replace the object only while its ETag is still this one (quotes and `W/` from an HTTP header are ignored), else `CodeConflict` and nothing written; two writers that read the same version never both win | `Capabilities.CompareAndSwap` |
| `SizeHint` | the body's length when known; lets a driver choose a single request, and is held to the body: a body that ends early or runs past it fails with `CodeInvalidArgument` and nothing is stored | nothing |
| `MaxSize` | abort with `CodeTooLarge` once the body passes this many bytes; nothing is stored | nothing |
| `SHA256` | the hex SHA-256 the body must have; the Disk hashes every body and refuses a mismatch with `CodeInvalidArgument` before the driver commits, so a corrupted body never replaces what was stored; drivers with `ChecksumSHA256` also have the backend verify it; refused on `CreateMultipart` (`CodeUnsupported`) | nothing (the Disk checks it on every driver) |

A body the driver fails to read stores nothing: every driver writes atomically, so readers see the old bytes or the new bytes, never a mix.

## Step 2 — Read part of an object

`ReadRange(ctx, key, ByteRange{Offset, Length})` reads part of an object. `Length` 0 means "to the end", because a zero-length read is never useful; a range past the end yields an empty read rather than an error. Negative values are `CodeInvalidArgument`. This is what `fshttp` uses to answer HTTP `Range` requests without reading the whole object.

<!-- ds:block id=readrange-bg2wcst7 -->
**readRange** · [`examples/concepts-disks/main.go:124-136`](../../examples/concepts-disks/main.go#L124-L136)

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

Check: `range 7+7: "storage"`.

## Step 3 — Ask about a key

`Stat` returns the `Entry` for a key or an error with `CodeNotFound`. `Exists` is `Stat` with exactly that code turned into `false, nil`; any other error (a permission problem, a network failure) is still returned, so a backend outage is never mistaken for a missing file.

<!-- ds:block id=statandexists-jr3aucg4 -->
**statAndExists** · [`examples/concepts-disks/main.go:141-155`](../../examples/concepts-disks/main.go#L141-L155)

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

Which `Entry` fields are set depends on the backend: `ContentType`, `ETag`, `Visibility`, `Metadata` and `Headers` are empty when the driver does not store them (see the capability table in [Capabilities](capabilities.md)). The local driver derives `ContentType` from the extension on `Stat`.

Check: `stat docs/hello.txt: 14 bytes, text/plain; charset=utf-8, visibility private`, then `stat missing: not_found` and `exists missing: false`.

## Step 4 — List

A directory is not an object: it is the set of keys sharing a prefix, which is how every object store works. Listing comes in two forms:

- `List(ctx, dir, opts)` returns an `iter.Seq2[Entry, error]` for a `for range` loop. It fetches pages lazily, one intercepted (and so retryable) driver call each; breaking out of the loop stops fetching. A failure is yielded once as `(Entry{}, err)` and ends the iteration.
- `ListPage(ctx, dir, opts)` returns one `Page` with `Entries` and `NextCursor`, for APIs that hand a cursor to their own clients. Pass `NextCursor` back as `ListOptions.Cursor`; it is empty on the last page.

`ListOptions.Recursive` lists every key under the directory; without it, keys in sub-directories fold into one entry per sub-directory with `IsDir` set. `Limit` caps a page (0 means `DefaultPageSize`, 1000, S3's maximum). Entries come in ascending key order, and `dir` `""` is the root of the disk. A directory with no keys lists as empty, never as `CodeNotFound`.

<!-- ds:block id=listtree-kzdkcv86 -->
**listTree** · [`examples/concepts-disks/main.go:161-187`](../../examples/concepts-disks/main.go#L161-L187)

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

Check: `recursive: docs/a.txt docs/b.txt docs/hello.txt docs/img/logo.png`, then two pages, the second ending with the folded directory: `page 2: docs/hello.txt docs/img/ next=""`.

## Step 5 — Copy and move

`Copy(ctx, src, dst)` and `Move(ctx, src, dst)` replace `dst` if it exists. With the `Copy` or `Move` capability the backend does the work (an S3 server-side copy, a rename on local disk); without it the Disk streams the object through the application, keeping its content type, metadata, headers and visibility where the driver stores them. A fallback Move is a Copy followed by a Delete, which is not atomic: a failure between the two leaves both keys, and the error is returned. Copying a key onto itself is `CodeInvalidArgument`, because backends disagree on what it means.

<!-- ds:block id=copyandmove-yksfax76 -->
**copyAndMove** · [`examples/concepts-disks/main.go:205-221`](../../examples/concepts-disks/main.go#L205-L221)

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

Copying between two disks is `filesystem.CopyAcross(ctx, src, srcKey, dst, dstKey)` and `MoveAcross`, described in [Configuration](configuration.md).

Check: `copied and moved to archive/hello.txt (old key exists: false)` and `copy onto itself: invalid_argument`.

## Step 6 — Delete

| Method | Behaviour |
|---|---|
| `Delete(ctx, key)` | removes one key; a missing key succeeds, so deletes are idempotent and safe to retry |
| `DeleteKeys(ctx, keys)` | removes many keys, 1,000 per request on S3, one by one elsewhere; duplicates are deleted once; keys that fail (invalid ones included) come back in a `*BatchError` while every other key is deleted |
| `DeletePrefix(ctx, dir)` | removes every key under a directory, in bulk where the backend can, otherwise by listing and deleting; the root of the disk is refused with `CodeInvalidArgument`, because emptying a whole disk is never a prefix operation |

<!-- ds:block id=deletethings-a7pa7647 -->
**deleteThings** · [`examples/concepts-disks/main.go:227-249`](../../examples/concepts-disks/main.go#L227-L249)

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

Check: `deleted a missing key: ok`, `delete the root: invalid_argument`, and `left: docs/hello.txt docs/img/logo.png`.

## Step 7 — Multipart uploads

Large objects can be uploaded in parts through the server: `CreateMultipart` returns an upload ID, `UploadPart` sends one numbered part (1 to 10,000, S3's limits, exported as `MinPartNumber` and `MaxPartNumber`), `ListParts` returns what arrived so a client can resume, `CompleteMultipart` assembles the listed parts, and `AbortMultipart` discards the upload (an unknown upload aborts successfully, so cleanup code can call it unconditionally). `CreateMultipart` checks its `WriteOptions` against the capabilities exactly as `Write` does. All of them need the `Multipart` capability; the memory, local, S3 and Azure drivers have it.

<!-- ds:block id=multipartupload-7z3ejr3z -->
**multipartUpload** · [`examples/concepts-disks/main.go:255-281`](../../examples/concepts-disks/main.go#L255-L281)

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

On S3 every part but the last must be at least 5 MiB; the memory and local drivers have no minimum. `PresignPart(ctx, key, uploadID, number, expires)` signs a part for a browser to send straight to the bucket; it needs `PresignPart`, which only the S3 driver has. Browser multipart uploads are normally run by `upload.Uppy` rather than by hand; see [Uploads](uploads.md).

Check: `multipart: 3 parts listed, video/clip.bin is 10 bytes`.

## Step 8 — Check at start-up, and reach the native client

`Check(ctx)` writes a small random object under `.filesystem-check/`, reads it back, compares the bytes and deletes it. Run it at start-up (or `Manager.Check` for every disk) so a wrong credential or a missing bucket fails the deploy instead of the first user upload. A read-only disk reports `CodeReadOnly`.

`filesystem.As[T](disk)` is the typed escape hatch to a backend's own client: it returns the value of the first driver in the wrapper chain that has a `Native() T` method. The local driver offers `*os.Root`, the S3 driver `*s3.Client`; it returns false when no driver offers a `T`. Code that uses it is no longer portable, so keep it at the edges.

<!-- ds:block id=checkandescape-bt2t74mr -->
**checkAndEscape** · [`examples/concepts-disks/main.go:387-407`](../../examples/concepts-disks/main.go#L387-L407)

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

`Close()` releases the driver's resources when it holds any (the local driver's directory handle, an SFTP connection) and does nothing otherwise.

Check: `check: ok`, `as *os.Root: true, rooted at the disk's directory: true` and `as *strings.Builder: false`.

## Step 9 — Append, conditional reads and object state

Operations on one object beyond writing it whole, each behind its capability (`CodeUnsupported` without it). The full list is in the table at the end of this step; three of them come up in most applications.

**Grow an object with `Append`.** A log, an export written in batches, a chat transcript: `Append` adds bytes to the end and creates the object on the first call. `AppendOptions.MaxSize` caps the object's size after the append; an append that would pass it is refused whole with `too_large`, so a log cannot grow without bound however many appends arrive. `SetModTime` stamps the time `Stat` reports, which an importer uses to keep the source file's time:

<!-- ds:block id=appendandmodtime-873u6nvw -->
**appendAndModTime** · [`examples/concepts-disks/main.go:289-307`](../../examples/concepts-disks/main.go#L289-L307)

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

The program prints `append past 32 bytes: too_large` and then `log: 25 bytes, modified 2026-01-02T03:04:05Z`: the two lines that fitted are there, the third was refused without a byte of it written.

**Read only when something changed, write only when nothing did.** `ReadIf` with the ETag of the caller's copy answers `CodeNotModified` and no body while the object is unchanged, which is what a cache or an HTTP handler answering `If-None-Match` needs. A write with `WriteOptions.IfMatch` set to the ETag the caller read succeeds only when nobody wrote in between; the loser of a race gets `CodeConflict`, reads again and retries:

<!-- ds:block id=conditionalreadandwrite-8yeavz32 -->
**conditionalReadAndWrite** · [`examples/concepts-disks/main.go:314-338`](../../examples/concepts-disks/main.go#L314-L338)

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

`ReadIf` works on every disk: a driver with `ConditionalRead` decides in the backend (S3, GCS and Azure send the condition with the GET), and on the others the Disk compares against a `Stat` first. `IfMatch` needs `ConditionalWrite`; without it the write fails with `CodeUnsupported` rather than writing unconditionally.

| Method | What it does | Capability (who has it) |
|---|---|---|
| `Append(ctx, key, r, AppendOptions{Visibility, MaxSize})` | adds `r` to the end of the object, creating it (with the disk's default visibility unless set); a failing body appends nothing, two appends never interleave, and an append that would take the object past `MaxSize` is refused with `too_large` without appending; never retried | `Append` (local, memory, SFTP, Azure append blobs) |
| `ReadIf(ctx, key, ReadOptions{IfNoneMatch, IfModifiedSince})` | the body and entry, or `CodeNotModified` without a body when the caller's ETag matches (`"*"` matches any object) or, without one, the object is not newer than the time (to the second), as HTTP decides; every disk supports it, natively with `ConditionalRead` and otherwise by comparing against a `Stat` | none needed |
| `SetModTime(ctx, key, t)` | sets the modification time `Stat` reports | `SetModTime` (local, memory, SFTP) |
| `Tags`, `SetTags(ctx, key, map)` | key-value tags kept beside the bytes; `SetTags` replaces them all | `Tags` (memory, S3, Azure) |
| `Hold`, `SetHold(ctx, key, Hold{Legal, RetainUntil})` | legal holds and retention dates (object lock) | `Hold` (S3, GCS, Azure) |
| `Versions(ctx, key)` | the stored versions, newest first, with delete markers | `Versions` (versioned buckets) |
| `Restore(ctx, key, RestoreOptions{Days})` | starts bringing an archived object back online for a while | `Restore` (S3 Glacier, Azure archive) |

A scoped disk forwards all of them under its prefix. A mirrored disk declares none of them, since changing one object in place would leave the mirrors behind; `ReadIf` still works there through its `Stat` fallback.


## Step 10 — A local file for tools that need a path

Some tools only take a path: ffmpeg, libvips, pdftotext, a virus scanner's command line. `TempFile` copies an object to a new local temporary file and returns its path. The file keeps the key's extension when it is short and alphanumeric (so the tool can guess the format), is created with mode 0600, and is yours to remove. `TempFileOptions.MaxSize` refuses an object larger than the cap with `too_large`, checked from `Stat` before anything is written and again while copying, so an object that grew in between cannot fill the local disk; any failure leaves no file behind:

<!-- ds:block id=tempcopy-v3f5qefn -->
**tempCopy** · [`examples/concepts-disks/main.go:344-358`](../../examples/concepts-disks/main.go#L344-L358)

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

Set `TempFileOptions.Dir` when the temporary directory is small (a container's `/tmp` in memory) and the files are large.

## Conventions every call follows

Three rules hold for every method on this page, and for every package built on the Disk:

- **The context comes first.** Every call that can touch the backend takes a `context.Context` as its first argument, and the interceptors and the driver see that same context. Cancelling it or passing its deadline stops the call; the error is classified `CodeUnavailable` (the code the retry interceptor understands) and still matches `context.Canceled` or `context.DeadlineExceeded` with `errors.Is`. A write stopped this way stores nothing. A `Read` stream keeps using the context it was opened with, so do not cancel it before you have read the body.
- **Standard library types in and out.** Bodies are `io.Reader` and `io.ReadCloser`, listings are `iter.Seq2[Entry, error]`, times are `time.Time`, expiries `time.Duration`. `disk.FS(ctx)` is an `io/fs.FS`, so `fs.ReadFile`, `fs.WalkDir`, `http.FS` and `template.ParseFS` work on any backend. Logging and tracing reach the core through `log/slog` and interceptors, never through a logging library of its own.
- **Typed configuration, checked at construction.** Options are Go values (`WithPublicURL`, `WithInterceptors`, `WriteOptions`, `PresignOptions`), not string maps, and `NewDisk` and the DSN `Mux` validate them when the disk is built. A malformed option fails at start-up with `CodeInvalidArgument` instead of on the first request that needs it.

<!-- ds:block id=conventions-cfsevhds -->
**conventions** · [`examples/concepts-disks/main.go:366-381`](../../examples/concepts-disks/main.go#L366-L381)

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

## The rest of the Disk API

| Method | What it does | Page |
|---|---|---|
| `Name()`, `DefaultVisibility()`, `Capabilities()` | the disk's configuration | [Capabilities](capabilities.md) |
| `SetVisibility(ctx, key, v)` | makes one object public or private; needs `SetVisibility` | [Public and private files](public-and-private.md) |
| `PublicURL(ctx, key)` | a URL anyone can read, from `WithPublicURL` or the driver | [Serving files](serving-files.md) |
| `SignedURL(ctx, key, opts)` | a pre-authorised GET or PUT that expires | [Serving files](serving-files.md) |
| `TempFile(ctx, key, TempFileOptions{Dir, MaxSize})` | copies an object to a new local temporary file (keeping a short alphanumeric extension, mode 0600) and returns its path, for tools that need a path (ffmpeg, libvips); the caller removes it; `MaxSize` is checked from `Stat` and again while copying (`too_large`); any failure leaves no file | — |
| `PresignPost(ctx, PostPolicy{Key or KeyPrefix, ContentType, MinSize, MaxSize, Expires, Headers})` | a browser form (URL, fields, expiry) for uploading straight to the backend, which itself enforces the size range, key, type and expiry; needs `PresignPost` (S3, GCS) | [Uploads](uploads.md) |
| `SweepAbandoned(ctx, before)` | removes multipart uploads and staging files idle since `before`; needs `SweepAbandoned` | [Uploads](uploads.md) |
| `FindAbandoned(ctx, before)` | the dry run of `SweepAbandoned`: what it would remove now, oldest first, removing nothing; needs `FindAbandoned` | [Uploads](uploads.md) |
| `MakeDir(ctx, dir)`, `DirExists(ctx, dir)` | create a directory (a real, possibly empty one with `Dirs`; nothing on object stores) and ask whether one exists | [Keys](keys.md) |
| `Scope(name, prefix)` | a disk that sees only the keys under a prefix | [Multi-tenancy](multi-tenancy.md) |
| `filesystem.Mirror(name, primary, mirrors...)` | a disk that writes to several | [Consistency](consistency.md) |

## The Driver contract

A backend implements `filesystem.Driver`:

| Method | Contract |
|---|---|
| `Stat(ctx, key) (Entry, error)` | a missing key is an error with `CodeNotFound` |
| `Read(ctx, key, rng) (io.ReadCloser, error)` | honours the `ByteRange`; a missing key is `CodeNotFound` |
| `Write(ctx, key, body, opts) (Entry, error)` | streams `body` to completion and never closes it; if reading it fails, nothing is stored and the read error is returned; a successful write replaces the object atomically |
| `Delete(ctx, key) error` | a missing key succeeds |
| `List(ctx, dir, opts) (Page, error)` | ascending key order, honours `Cursor` and `Limit`, folds directories when not recursive; an empty directory is an empty page |
| `Capabilities() Capabilities` | constant for the driver's lifetime and true only for what the driver really does |

Keys and directories reach the driver already cleaned. The optional interfaces below are called only when the matching capability is true:

| Interface | Methods | Capability |
|---|---|---|
| `Copier` | `Copy` | `Copy` |
| `Mover` | `Move` | `Move` |
| `VisibilitySetter` | `SetVisibility` | `SetVisibility` |
| `PrefixDeleter` | `DeletePrefix` | `DeletePrefix` |
| `KeysDeleter` | `DeleteKeys` | `DeleteKeys` |
| `Presigner` | `Presign` | `Presign` |
| `PublicURLer` | `PublicURL` | `PublicURL` |
| `MultipartUploader` | `CreateMultipart`, `UploadPart`, `ListParts`, `CompleteMultipart`, `AbortMultipart` | `Multipart` |
| `PartPresigner` | `PresignPart` | `PresignPart` (requires `Multipart`) |
| `io.Closer` | `Close` | none: `Disk.Close` calls it when present |
| `Unwrapper` | `Unwrap() Driver` | none: lets `As` see through wrapper drivers |

`ContentType`, `Metadata`, `Headers` and `ConditionalWrite` are attribute capabilities with no interface of their own: they say whether the driver stores and honours the matching `WriteOptions` field. Drivers report failures with `filesystem.NewError(code, cause)` or `filesystem.Errorf(code, format, ...)`, keeping the native error as the cause. Writing a driver, running the conformance suite against it and shipping it are covered in [Write a driver](../extending/write-a-driver.md).

## Pitfalls

- Holding a `Driver` instead of a `*Disk` skips every guarantee on this page: key checks, detection, size limits, error classification. Applications call the Disk; only Disk and wrapper code calls drivers.
- `Read` returns a stream: close it, and do not cancel its context until you have consumed it. Interceptors that time out calls leave reads alone for the same reason.
- `ReadAll` reads the whole object into memory. Use `Read` and stream for anything a user uploaded.
- A fallback `Move` (no `Move` capability) and `MoveAcross` are not atomic. Design cleanup for "both keys exist" after a failure.
- On the local driver a key cannot be both a file and a directory (`report` and `report/2026.pdf`); object stores allow it. See [Keys](keys.md).

## Next

- [Keys](keys.md): the rules every key passes before a driver sees it.
- [Capabilities](capabilities.md): what each backend can do and what happens when it cannot.
- [Configuration](configuration.md): opening disks from DSNs and the environment.
- Reference: [local driver](../../local/README.md), [memory driver](../../memory/README.md), [adapters compared](../adapters/README.md).
