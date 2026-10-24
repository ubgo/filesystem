# In memory

The memory driver keeps objects in a map inside your process. Use it in tests, for caches that need no persistence, and as the reference backend: it supports every capability that needs no network, so code written against it exercises the whole Disk API. Nothing survives the process.

## Before you start

- The package is `github.com/ubgo/filesystem/memory`, in the core module. Its reference is [memory/README.md](../../memory/README.md).
- The runnable program is [`examples/adapters-memory`](../../examples/adapters-memory/): `go run ./adapters-memory` from `examples/`.
- For application tests, `filesystemtest.NewFake(t, "media")` is usually more convenient: it is a memory disk with assertions such as `AssertExists`. See [testing](../start/testing.md).

## 1. Open a disk

From a DSN, `mem://` (the host part is ignored, so `mem://cache` is a readable label). The opener takes no parameters of its own; the Mux's `prefix`, `public_url`, `visibility` and `readonly` work as for every scheme, and anything else is `CodeInvalidArgument`.

In code, `memory.New` takes options. The example opens one with a fixed clock, so tests can assert exact modification times:

<!-- ds:block id=openwithclock-d37tffgx -->
**openWithClock** · [`examples/adapters-memory/main.go:57-59`](../../examples/adapters-memory/main.go#L57-L59)

```go
func openWithClock() (*filesystem.Disk, error) {
	return filesystem.NewDisk("cache", memory.New(memory.WithClock(func() time.Time { return frozenTime })))
}
```
<!-- /ds:block hash=7236fb -->

| Option | Default | What it does |
|---|---|---|
| `memory.WithClock(func() time.Time)` | `time.Now` | The time recorded as each object's `ModTime`, and the time a multipart upload was created or last given a part, which `SweepAbandoned` and `FindAbandoned` compare with their cutoff (step 4) |
| `memory.WithVersions()` | off | Keeps every version of every key, like a versioned bucket, and declares the `versions` and `version-read` capabilities (step 5) |

## 2. What it stores

Everything a backend can store: content type, visibility per object, metadata, the three HTTP headers, and atomic conditional writes.

It also has real directories (`Capabilities().Dirs`): `disk.MakeDir` records one, so `disk.DirExists` reports it while it is empty, as on local disk. It lasts until `DeletePrefix` removes it or a parent, where local disk removes a directory with its last file; `MakeDir` over an object's key is `exists`.

<!-- ds:block id=storeeverything-cywhsa7y -->
**storeEverything** · [`examples/adapters-memory/main.go:64-85`](../../examples/adapters-memory/main.go#L64-L85)

```go
func storeEverything(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.WriteBytes(ctx, "thumbs/1.json", []byte(`{"w":64}`), filesystem.WriteOptions{
		Visibility:  filesystem.VisibilityPublic,
		Metadata:    map[string]string{"source": "1.png"},
		Headers:     filesystem.HTTPHeaders{CacheControl: "public, max-age=60"},
		IfNotExists: true,
	})
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "thumbs/1.json")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %s %s source=%s cache-control=%q at %s\n",
		e.Key, e.ContentType, e.Visibility, e.Metadata["source"], e.Headers.CacheControl, e.ModTime.Format(time.RFC3339))
	_, err = disk.WriteBytes(ctx, "thumbs/1.json", []byte("{}"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "second conditional write: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "thumbs/1.json", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	fmt.Fprintf(out, "signed URL without a signer: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=018563 -->

Check:

```text
capabilities: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]
stat: thumbs/1.json application/json public source=1.png cache-control="public, max-age=60" at 2026-10-03T12:00:00Z
second conditional write: exists
signed URL without a signer: unsupported
```

What it does not have:

- **Signed URLs.** There is no server to sign for. Give the disk `filesystem.WithURLSigner` with a `signedurl.Signer` and serve the URLs with `fshttp.Signed`, as on [local disk](local.md); without a signer, `SignedURL` is `CodeUnsupported`.
- **Public URLs** come from `public_url` or `filesystem.WithPublicURL`, as on every backend.
- **Presigned parts.** Multipart works through the server (`UploadPart`), not from a browser. Uploads nobody finished stay in memory until `disk.SweepAbandoned(ctx, before)` drops those created or last given a part before `before`, by the driver's clock (`memory.WithClock` sets it in tests); `disk.FindAbandoned(ctx, before)` lists them without dropping them.
- **An escape hatch.** There is no native client, so `filesystem.As` finds nothing.

## 3. Every Open is a new store

Each call to the opener (or to `memory.New`) creates an empty store. Two disks opened from the same DSN do not share files, and two Managers built from the same configuration do not either:

<!-- ds:block id=separatestores-s9sudcqc -->
**separateStores** · [`examples/adapters-memory/main.go:90-116`](../../examples/adapters-memory/main.go#L90-L116)

```go
func separateStores(ctx context.Context, out io.Writer) error {
	mux := filesystem.NewMux()
	if err := mux.Register(memory.Scheme, memory.Opener); err != nil {
		return err
	}
	a, err := mux.Open(ctx, "a", cacheDSN)
	if err != nil {
		return err
	}
	b, err := mux.Open(ctx, "b", cacheDSN)
	if err != nil {
		return err
	}
	if _, err := a.WriteBytes(ctx, "k", []byte("v"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	inA, err := a.Exists(ctx, "k")
	if err != nil {
		return err
	}
	inB, err := b.Exists(ctx, "k")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "same DSN opened twice: in a=%t, in b=%t\n", inA, inB)
	return nil
}
```
<!-- /ds:block hash=23f149 -->

Check: `same DSN opened twice: in a=true, in b=false`. To share one store between parts of a program, open it once and pass the `*filesystem.Disk` around. This is deliberate: there is no package-level registry, so two tests running in parallel never see each other's files.

## 4. Move the clock to sweep abandoned uploads

A multipart upload nobody completes or aborts stays in memory. `disk.SweepAbandoned(ctx, before)` drops every upload created or last given a part before `before`, and `disk.FindAbandoned(ctx, before)` lists the same uploads without dropping them. Both read the driver's clock, never the wall clock, so a test that wants an old upload moves the clock instead of sleeping. The clock here is a closure over a variable the test advances:

<!-- ds:block id=sweepwithclock-8b8vvdut -->
**sweepWithClock** · [`examples/adapters-memory/main.go:127-153`](../../examples/adapters-memory/main.go#L127-L153)

```go
func sweepWithClock(ctx context.Context, out io.Writer) error {
	now := frozenTime
	disk, err := filesystem.NewDisk("uploads", memory.New(memory.WithClock(func() time.Time { return now })))
	if err != nil {
		return err
	}
	id, err := disk.CreateMultipart(ctx, "videos/big.mp4", filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	now = now.Add(2 * abandonAfter)
	cutoff := now.Add(-abandonAfter)
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "would sweep: %s upload for %s\n", a.Kind, a.Key)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	_, err = disk.ListParts(ctx, "videos/big.mp4", id)
	fmt.Fprintf(out, "swept %d; the upload is now %s\n", n, filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=530da3 -->

Check: `would sweep: multipart upload for videos/big.mp4` and `swept 1; the upload is now not_found`. A sweep in production uses the same calls with `time.Now().Add(-maxUploadAge)`; [maintenance jobs](../concepts/maintenance-jobs.md) shows scheduling it.

## 5. Keep every version

`memory.WithVersions()` makes the disk behave like a versioned bucket: each write, copy, move, append and completed multipart upload adds a version, and each delete adds a delete marker. `disk.Versions` lists them newest first and `disk.ReadVersion` opens one by ID. Use it to test version-aware code (a restore button, the records `versions` plugin) without a real versioned bucket:

<!-- ds:block id=versionhistory-sgnt5kvr -->
**versionHistory** · [`examples/adapters-memory/main.go:158-183`](../../examples/adapters-memory/main.go#L158-L183)

```go
func versionHistory(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("history", memory.New(memory.WithVersions()))
	if err != nil {
		return err
	}
	for _, body := range []string{"draft", "final"} {
		if _, err := disk.WriteBytes(ctx, "notes.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	vs, err := disk.Versions(ctx, "notes.txt") // newest first
	if err != nil {
		return err
	}
	rc, err := disk.ReadVersion(ctx, "notes.txt", vs[len(vs)-1].ID)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	var first strings.Builder
	if _, err := io.Copy(&first, rc); err != nil {
		return err
	}
	fmt.Fprintf(out, "versions: %d, oldest holds %q\n", len(vs), first.String())
	return nil
}
```
<!-- /ds:block hash=3f642b -->

Check: `versions: 2, oldest holds "draft"`. Without the option the disk declares neither capability, and `Versions` is `unsupported`.

## 6. Run the example

```sh
cd examples
go run ./adapters-memory
go test ./adapters-memory
```

It needs no network, files or environment variables, and prints:

```text
capabilities: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]
stat: thumbs/1.json application/json public source=1.png cache-control="public, max-age=60" at 2026-10-03T12:00:00Z
second conditional write: exists
signed URL without a signer: unsupported
same DSN opened twice: in a=true, in b=false
would sweep: multipart upload for videos/big.mp4
swept 1; the upload is now not_found
versions: 2, oldest holds "draft"
```

`main_test.go` runs the program and fails if any of these lines changes.

## Pitfalls

- **Memory use.** Every object is held in full on the process's heap: a write reads the whole body before storing it, and nothing is ever spilled to disk. Ten uploads of 100 MB cost 1 GB of RAM until they are deleted. With `WithVersions` nothing is ever freed by an overwrite, since every old version is kept (a delete only adds a marker). Unfinished multipart uploads hold their parts until they are completed, aborted or swept. Do not use it for large files, or as a cache without something that deletes old entries.
- **No persistence.** Everything is gone when the process exits, crashes or restarts, including during a rolling deploy. Use it for data you can rebuild (a cache, a test fixture), never for anything a user uploaded and expects to keep; for development with real files, use [local disk](local.md).
- **Not shared across processes.** Two instances of your application, a worker and a web server, or a CLI and the server it talks to each have their own store, and so does every `Open` in one process (step 3). A file written by the web process is invisible to the worker that is meant to process it. Anything that crosses a process boundary needs local disk on a shared volume or an object store.
- **More capable than your production backend.** It declares `copy`, `metadata`, `headers`, `tags`, `storage-class`, `compare-and-swap` and more, so code tested only against it can rely on something your real backend refuses (local disk stores no metadata, for one). Check `disk.Capabilities()` in code paths that need an optional feature, and run the same tests against the backend you deploy on (a local disk in a `t.TempDir()`, or the conformance suite).
- **Memory driver or `filesystemtest`?** In application tests, prefer `filesystemtest.NewFake(t, "media")`: it is a memory disk plus assertions (`AssertExists`, `AssertContent`, `AssertCount`, `AssertEmpty`) that fail the test with a clear message. Use `memory.New` directly when you need a driver option the fake does not take (a clock for sweeps or exact timestamps, version history), or a disk outside a test (a cache, a CLI dry run). To check a driver of your own, use neither: run `filesystemtest.Run` against it. See [testing](../start/testing.md).
- **No signed or public URLs of its own.** `SignedURL` is `unsupported` until you give the disk a signer (step 2), and there is no escape hatch: `filesystem.As` finds no native client.
- **Concurrency.** It is safe for concurrent use: a write replaces an object atomically, `Read` returns a snapshot that a concurrent overwrite does not change, and `DeleteKeys` deletes in one locked pass.

## Next

- [Testing](../start/testing.md): `filesystemtest.NewFake` and the conformance suite.
- [Local disk](local.md), for development with real files.
- [Comparison of every backend](README.md).
