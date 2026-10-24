# In-memory driver

The memory driver keeps objects in a map inside the process: the backend for tests, for caches that need no persistence, and the reference implementation that supports every capability needing no network. This program opens a memory disk with a fixed clock, writes with every option a backend can store (visibility, metadata, headers, a conditional write), shows the one thing a memory disk cannot do (two opens of the same DSN never share data), moves a clock forward to sweep an abandoned multipart upload, and keeps a version history. It uses the core `filesystem` package and `github.com/ubgo/filesystem/memory`. The guide [In memory](../../docs/adapters/memory.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-memory
go test ./adapters-memory
```

It needs no network, files or environment variables, prints eight lines and exits. Nothing it writes outlives the process.

## What it does

```sh
go run ./adapters-memory
```

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

1. The disk reports its capabilities: everything except signed URLs, which a memory disk only has when the application gives it a signer.
2. A write stores visibility, metadata and a `Cache-Control` header, and `Stat` reads them back; the content type is sniffed from the bytes, and the timestamp is the fixed clock's.
3. A second write with `IfNotExists` fails with the code `exists`.
4. Asking for a signed URL without a signer is `unsupported`, never a fake link.
5. Opening `mem://cache` twice gives two empty stores: a file written through one disk is absent from the other.
6. A multipart upload is started, the driver's clock moves two days on, and `FindAbandoned` lists the upload that `SweepAbandoned` then drops: no sleeping.
7. A disk opened with `memory.WithVersions()` keeps both writes of `notes.txt`, and `ReadVersion` reads the first one back.

## The code

`memory.New` takes one option, a clock, so a test can assert exact modification times.

<!-- ds:block id=openwithclock-d37tffgx -->
**openWithClock** · [`examples/adapters-memory/main.go:57-59`](main.go#L57-L59)

```go
func openWithClock() (*filesystem.Disk, error) {
	return filesystem.NewDisk("cache", memory.New(memory.WithClock(func() time.Time { return frozenTime })))
}
```
<!-- /ds:block hash=7236fb -->

Every option a backend can store, read back with `Stat`, then the two refusals: a repeated conditional write and a signed URL with no signer.

<!-- ds:block id=storeeverything-cywhsa7y -->
**storeEverything** · [`examples/adapters-memory/main.go:64-85`](main.go#L64-L85)

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

The same DSN opened twice through a Mux: each `Open` is a new store, so use one shared `*filesystem.Disk` when two parts of a program must see the same files.

<!-- ds:block id=separatestores-s9sudcqc -->
**separateStores** · [`examples/adapters-memory/main.go:90-116`](main.go#L90-L116)

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

The driver's clock is the only time a sweep sees, so a test makes an upload old by moving it.

<!-- ds:block id=sweepwithclock-8b8vvdut -->
**sweepWithClock** · [`examples/adapters-memory/main.go:127-153`](main.go#L127-L153)

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

A versioned memory disk: every write is a version, listed newest first.

<!-- ds:block id=versionhistory-sgnt5kvr -->
**versionHistory** · [`examples/adapters-memory/main.go:158-183`](main.go#L158-L183)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: open with a clock, write and stat, the separate-stores check, a sweep driven by the clock, version history |
| `main_test.go` | Runs the program and checks each line the guide quotes |

## Next

- The guide: [In memory](../../docs/adapters/memory.md), and [testing](../../docs/start/testing.md) for `filesystemtest.NewFake`, a memory disk with assertions.
- Other backends: [local](../adapters-local), [S3](../adapters-s3), [GCS](../adapters-gcs), [Azure](../adapters-azure), [SFTP](../adapters-sftp).
