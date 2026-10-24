# Write a driver

You will have a storage backend of your own behind the same `*filesystem.Disk` every built-in backend uses: key cleaning, content-type detection, hashing, size caps, interceptors, signed URLs, uploads and records all work on it unchanged, and the conformance suite proves it behaves like the others. The example adapts a made-up key-value service; the same steps fit a vendor SDK the library does not ship (Bunny, Supabase Storage, an internal blob service), a database table of BLOBs, or a wrapper with behaviour of its own.

Read this page before writing a driver, and again before declaring a capability.

## Before you start

- [Disks and drivers](../concepts/disks-and-drivers.md) explains the split: the `Disk` is portable behaviour, the `Driver` only moves bytes.
- The program is [`examples/extending-driver`](../../examples/extending-driver/main.go). `go run ./extending-driver` in `examples/` uses the driver through a Disk; `go test ./extending-driver` runs the conformance suite against it.

## The contract

A driver implements the six methods of `filesystem.Driver`:

| Method | Returns |
|---|---|
| `Stat(ctx, key)` | the object's `Entry` |
| `Read(ctx, key, rng ByteRange)` | an `io.ReadCloser` over the range |
| `Write(ctx, key, body io.Reader, opts WriteOptions)` | the stored `Entry` |
| `Delete(ctx, key)` | an error or nil |
| `List(ctx, dir, opts ListOptions)` | one `Page` of entries and the next cursor |
| `Capabilities()` | the `Capabilities` it declares |

and honours these rules, every one of which the conformance suite checks:

- Keys and directories arrive already cleaned by the Disk (`filesystem.CleanKey`): no leading slash, no `..`, no empty segments. The driver never re-validates them, and must not interpret them as anything but names.
- `Stat` and `Read` of a missing key return an error with code `not_found`.
- `Write` reads the body to the end and never closes it. If reading fails, nothing is stored and the read error is returned. A successful write replaces an existing object atomically: a reader sees the old bytes or the new ones, never a mix.
- `Delete` of a missing key succeeds.
- `List` returns entries in ascending key order and honours `Cursor` and `Limit`; a directory with no keys lists as empty, not `not_found`.
- `Capabilities` is constant for the driver's lifetime and true only for what the driver really does.

## Step 1 — Wrap the vendor's API

A real driver holds the vendor's client. The example states the client's shape as an interface, so the tutorial can supply an in-memory one:

<!-- ds:block id=backend-4xk4syeq -->
**Backend** · [`examples/extending-driver/main.go:32-42`](../../examples/extending-driver/main.go#L32-L42)

```go
type Backend interface {
	// Get returns the object, or ErrNoSuchKey.
	Get(ctx context.Context, key string) (Object, error)
	// Put stores the object. With ifAbsent it fails with ErrConflict when
	// the key exists, atomically.
	Put(ctx context.Context, key string, o Object, ifAbsent bool) error
	// Delete removes the key; a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Keys lists the keys starting with prefix, in any order.
	Keys(ctx context.Context, prefix string) ([]string, error)
}
```
<!-- /ds:block hash=2d4e74 -->

<!-- ds:block id=driver-kmz69jg5 -->
**Driver** · [`examples/extending-driver/main.go:63-65`](../../examples/extending-driver/main.go#L63-L65)

```go
type Driver struct {
	backend Backend
}
```
<!-- /ds:block hash=a4ba3c -->

## Step 2 — Declare the capabilities

`Capabilities` is the driver's promise. Interface-backed bits (`Copy`, `Move`, `SetVisibility`, `DeletePrefix`, `DeleteKeys`, `Presign`, `PublicURL`, `Multipart`, `PresignPart`) require the matching optional interface; attribute bits (`Metadata`, `ContentType`, `Headers`, `ConditionalWrite`) say what `Write` stores and `Stat` returns. This service stores a content type and metadata, and can refuse a write when the key exists, atomically:

<!-- ds:block id=driver-capabilities-t392pkc6 -->
**Driver.Capabilities** · [`examples/extending-driver/main.go:76-82`](../../examples/extending-driver/main.go#L76-L82)

```go
func (d *Driver) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{
		ContentType:      true,
		Metadata:         true,
		ConditionalWrite: true,
	}
}
```
<!-- /ds:block hash=95e9e2 -->

What the Disk does with a missing bit is the point of declaring honestly:

| Capability false | The Disk |
|---|---|
| `Copy` | streams the object: `Stat`, `Read`, then `Write`, keeping type, metadata and visibility where stored |
| `Move` | copies, then deletes the source; not atomic |
| `DeletePrefix` | lists recursively and deletes key by key |
| `DeleteKeys` | deletes key by key, reporting failures in a `*filesystem.BatchError` |
| `Presign` | signs with the disk's `URLSigner` (`signedurl` plus `fshttp.Signed`), or answers `unsupported` |
| `PublicURL` | uses the base set by `WithPublicURL` or the `public_url` DSN parameter, or answers `unsupported` |
| `SetVisibility` | refuses a write whose visibility differs from the disk default, and `SetVisibility`, with `unsupported` |
| `Metadata`, `Headers` | refuses a write that sets them with `unsupported`, rather than dropping them |
| `ConditionalWrite` | refuses `IfNotExists` with `unsupported` |
| `Multipart`, `PresignPart` | refuses the multipart methods with `unsupported`; `upload.Uppy` answers 501 |

`filesystem.NewDisk` refuses a driver that declares an interface-backed bit without implementing the interface, so the mistake fails at start-up instead of at the first call:

<!-- ds:block id=testnewdiskrefusesanoverclaimingdriver-773vjepn -->
**TestNewDiskRefusesAnOverclaimingDriver** · [`examples/extending-driver/main_test.go:53-58`](../../examples/extending-driver/main_test.go#L53-L58)

```go
func TestNewDiskRefusesAnOverclaimingDriver(t *testing.T) {
	_, err := filesystem.NewDisk("kv", overclaiming{New(NewMapBackend())})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "declares capability Copy") {
		t.Errorf("err = %v", err)
	}
}
```
<!-- /ds:block hash=5c8ad8 -->

The opposite (implementing an interface while declaring it false) is allowed; wrapper drivers do it to report only what the driver underneath supports.

## Step 3 — Map the vendor's errors

Callers branch on `filesystem.Code`, never on vendor types. Map each error you can classify, keep the original as the cause so `errors.As` still reaches it, and let anything else be `unknown`. Transient failures must be `unavailable`: it is the only code `layer.Retry` retries.

<!-- ds:block id=maperr-zra7zy73 -->
**mapErr** · [`examples/extending-driver/main.go:88-103`](../../examples/extending-driver/main.go#L88-L103)

```go
func mapErr(err error) error {
	code := filesystem.CodeUnknown
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoSuchKey):
		code = filesystem.CodeNotFound
	case errors.Is(err, ErrConflict):
		code = filesystem.CodeExists
	case errors.Is(err, ErrThrottled):
		code = filesystem.CodeUnavailable // the one code layer.Retry retries
	case errors.Is(err, ErrForbidden):
		code = filesystem.CodePermission
	}
	return filesystem.NewError(code, err)
}
```
<!-- /ds:block hash=1b8541 -->

Errors from `io/fs` (`fs.ErrNotExist`, `fs.ErrExist`, `fs.ErrPermission`) are classified by the Disk itself, so a driver over a filesystem-like API can return them as they are.

## Step 4 — Stat and Read

`Entry` must carry `Size` and `ModTime`; `ContentType`, `ETag`, `Visibility`, `Metadata` and `Headers` when the driver stores them. The Disk fills in `Key`, and on a write `Size` and `SHA256` from what it streamed:

<!-- ds:block id=entry-pq75fsxj -->
**entry** · [`examples/extending-driver/main.go:108-116`](../../examples/extending-driver/main.go#L108-L116)

```go
func entry(key string, o Object) filesystem.Entry {
	return filesystem.Entry{
		Key:         key,
		Size:        int64(len(o.Data)),
		ModTime:     o.Modified,
		ContentType: o.ContentType,
		Metadata:    maps.Clone(o.Metadata),
	}
}
```
<!-- /ds:block hash=c5a18e -->

<!-- ds:block id=driver-stat-aqyx7ujn -->
**Driver.Stat** · [`examples/extending-driver/main.go:120-126`](../../examples/extending-driver/main.go#L120-L126)

```go
func (d *Driver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	o, err := d.backend.Get(ctx, key)
	if err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return entry(key, o), nil
}
```
<!-- /ds:block hash=e2eb7b -->

`ByteRange{Offset, Length}` selects part of an object. `Length` 0 means to the end, and a range past the end is an empty read, not an error. A backend with ranged GETs passes the range to the server instead of slicing:

<!-- ds:block id=driver-read-9fmufg7s -->
**Driver.Read** · [`examples/extending-driver/main.go:131-142`](../../examples/extending-driver/main.go#L131-L142)

```go
func (d *Driver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	o, err := d.backend.Get(ctx, key)
	if err != nil {
		return nil, mapErr(err)
	}
	size := int64(len(o.Data))
	start, end := min(rng.Offset, size), size
	if rng.Length > 0 {
		end = min(start+rng.Length, size)
	}
	return io.NopCloser(bytes.NewReader(o.Data[start:end])), nil
}
```
<!-- /ds:block hash=2c3c2a -->

## Step 5 — Write

The Disk hands the driver a body that counts, hashes and caps itself: when the body passes `WriteOptions.MaxSize` its `Read` fails, and the driver must store nothing. Reading to the end before committing is the simplest way to honour that; a streaming backend must abort its upload on a read error instead. Options arrive validated against the declared capabilities, with `Visibility` and a detected `ContentType` filled in:

<!-- ds:block id=driver-write-gnbg8y2n -->
**Driver.Write** · [`examples/extending-driver/main.go:150-160`](../../examples/extending-driver/main.go#L150-L160)

```go
func (d *Driver) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return filesystem.Entry{}, err
	}
	o := Object{Data: data, ContentType: opts.ContentType, Metadata: maps.Clone(opts.Metadata), Modified: time.Now().UTC()}
	if err := d.backend.Put(ctx, key, o, opts.IfNotExists); err != nil {
		return filesystem.Entry{}, mapErr(err)
	}
	return entry(key, o), nil
}
```
<!-- /ds:block hash=f7b723 -->

`SizeHint` is the body's length when the caller knows it, for choosing a single-request upload; never trust it as the size.

## Step 6 — Delete and List

<!-- ds:block id=driver-delete-scc574x4 -->
**Driver.Delete** · [`examples/extending-driver/main.go:164-166`](../../examples/extending-driver/main.go#L164-L166)

```go
func (d *Driver) Delete(ctx context.Context, key string) error {
	return mapErr(d.backend.Delete(ctx, key))
}
```
<!-- /ds:block hash=7fcd25 -->

`filesystem.PageOf` turns a slice of entries under a directory into one correct page: sorted, with sub-directories folded into `IsDir` entries when not recursive, the cursor applied (it is the last key returned, so pages stay correct while keys change) and the limit cut. A backend with its own paging pages natively instead, and must still return ascending keys:

<!-- ds:block id=driver-list-rckcrhms -->
**Driver.List** · [`examples/extending-driver/main.go:173-194`](../../examples/extending-driver/main.go#L173-L194)

```go
func (d *Driver) List(ctx context.Context, dir string, opts filesystem.ListOptions) (filesystem.Page, error) {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	keys, err := d.backend.Keys(ctx, prefix)
	if err != nil {
		return filesystem.Page{}, mapErr(err)
	}
	var objects []filesystem.Entry
	for _, k := range keys {
		o, err := d.backend.Get(ctx, k)
		if errors.Is(err, ErrNoSuchKey) {
			continue // deleted since Keys listed it
		}
		if err != nil {
			return filesystem.Page{}, mapErr(err)
		}
		objects = append(objects, entry(k, o))
	}
	return filesystem.PageOf(objects, dir, opts), nil
}
```
<!-- /ds:block hash=39d5ff -->

## Step 7 — Open from a DSN and offer the native client

An `Opener` builds the driver from a parsed DSN and must refuse every parameter it does not know with `filesystem.RejectParams`, so a typo fails at start-up. The Mux has already removed its own parameters (`prefix`, `public_url`, `visibility`, `readonly`). Never accept secrets in the DSN: read them from the environment or a file named by a parameter, as the built-in adapters do.

<!-- ds:block id=opener-yetbbr5h -->
**Opener** · [`examples/extending-driver/main.go:207-212`](../../examples/extending-driver/main.go#L207-L212)

```go
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u); err != nil {
		return nil, err
	}
	return New(NewMapBackend()), nil
}
```
<!-- /ds:block hash=c6e698 -->

A method `Native() T` makes `filesystem.As[T](disk)` return your client, through scoped and mirrored disks too, for the calls the library does not wrap:

<!-- ds:block id=driver-native-p84djt65 -->
**Driver.Native** · [`examples/extending-driver/main.go:199`](../../examples/extending-driver/main.go#L199-L199)

```go
func (d *Driver) Native() Backend { return d.backend }
```
<!-- /ds:block hash=1f9e63 -->

If the driver holds connections, implement `io.Closer`; `Disk.Close` and `Manager.Close` call it. It may be called more than once when scoped or mirrored disks share the driver.

Check: `go run ./extending-driver` opens `kv://demo`, writes through the Disk (which cleans `/docs//readme.txt`, detects the type and hashes the bytes), copies with the streaming fallback and lists:

```
wrote docs/readme.txt (27 bytes, text/plain; charset=utf-8, sha256 1fc60926c6a2…)
listed [docs/copy.txt docs/readme.txt]
capabilities [metadata,content-type,conditional-write]
native client reachable through filesystem.As
```

## Step 8 — Pass the conformance suite

`filesystemtest.Run` runs every case the built-in drivers pass, each on a fresh empty disk from your factory:

<!-- ds:block id=testconformance-7wqaf78d -->
**TestConformance** · [`examples/extending-driver/main_test.go:21-29`](../../examples/extending-driver/main_test.go#L21-L29)

```go
func TestConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		d, err := filesystem.NewDisk("kv", New(NewMapBackend()))
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
}
```
<!-- /ds:block hash=949a93 -->

The cases: `WriteThenRead`, `OverwriteReplaces`, `EmptyObject`, `LargeObject`, `WriteReportsSizeAndHash`, `StatMissingIsNotFound`, `ReadMissingIsNotFound`, `StatReportsSizeAndTime`, `Exists`, `DeleteRemoves`, `DeleteMissingSucceeds`, `ReadRanges`, `UnicodeAndSpacesInKeys`, `FailingBodyStoresNothing`, `MaxSizeStoresNothing`, `DetectsContentType`, `ListRecursive`, `ListFoldsDirectories`, `ListPaginates`, `ListEmptyDirectory`, `ListDoesNotMatchSiblingPrefix`, `CopyDuplicates`, `CopyMissingIsNotFound`, `MoveRenames`, `DeletePrefixScoped`, `DeleteKeys` and `ConcurrentWrites` always; `StoresContentType`, `StoresMetadata`, `StoresHeaders`, `ConditionalWrite`, `Visibility`, `Multipart` and `MultipartAbort` when the capability is declared; `HeadersUnsupportedIsRefused`, `MetadataUnsupportedIsRefused` and `ConditionalWriteUnsupportedIsRefused` when it is not. A case for an undeclared capability is skipped, never failed.

Check: `go test -v ./extending-driver` passes, skipping `StoresHeaders`, `MetadataUnsupportedIsRefused`, `ConditionalWriteUnsupportedIsRefused`, `Visibility`, `Multipart` and `MultipartAbort`. To see the suite catch a lie, make the backend ignore `ifAbsent` while the driver still declares `ConditionalWrite`:

```
--- FAIL: TestConformance (0.01s)
    --- FAIL: TestConformance/ConditionalWrite (0.00s)
        suite.go:48: second conditional write: error code "" (err <nil>), want "exists"
```

Run the suite against the real backend in CI as well as against a fake: the built-in S3 driver runs it on SeaweedFS (`task test:s3`) and the Azure driver on Azurite (`task test:azure`), because a fake agrees with your assumptions.

## Optional interfaces

| Interface | Methods | Declare |
|---|---|---|
| `filesystem.Copier` | `Copy(ctx, src, dst) (Entry, error)` | `Copy` |
| `filesystem.Mover` | `Move(ctx, src, dst) (Entry, error)` | `Move` |
| `filesystem.VisibilitySetter` | `SetVisibility(ctx, key, v) error`; `Stat` must then report `Visibility` | `SetVisibility` |
| `filesystem.PrefixDeleter` | `DeletePrefix(ctx, dir) error` (`dir` is never the root) | `DeletePrefix` |
| `filesystem.KeysDeleter` | `DeleteKeys(ctx, keys) error`, a `*BatchError` for partial failure | `DeleteKeys` |
| `filesystem.Presigner` | `Presign(ctx, key, PresignOptions) (SignedRequest, error)` | `Presign` |
| `filesystem.PublicURLer` | `PublicURL(ctx, key) (string, error)` | `PublicURL` |
| `filesystem.MultipartUploader` | `CreateMultipart`, `UploadPart`, `ListParts`, `CompleteMultipart`, `AbortMultipart` | `Multipart` |
| `filesystem.PartPresigner` | `PresignPart(ctx, key, uploadID, number, expires)` | `PresignPart` (requires `Multipart`) |
| `io.Closer` | `Close() error` | nothing |
| `filesystem.Unwrapper` | `Unwrap() Driver`, for wrapper drivers | nothing |
| `Native() T` | your client | nothing |

## Pitfalls

- **Closing the body.** The caller owns it; a driver that closes it breaks callers that reuse the reader.
- **Committing a partial object.** A backend that streams must abort on a read error. The Disk deletes an object a driver committed after the size cap tripped, but only for that case; any other partial write is a contract violation the suite catches (`FailingBodyStoresNothing`).
- **Unstable cursors.** A cursor that is an offset breaks when keys are added between pages. Use the last key returned, as `PageOf` does, or the backend's own token.
- **Listing `a/b` matches `a/bc`.** A string-prefix listing must add the slash (`dir + "/"`), as the example does; `ListDoesNotMatchSiblingPrefix` checks it.
- **Ignoring the context.** Return when `ctx` is done, so `layer.Timeout` and client disconnects stop work. Even the memory driver, which never blocks, fails a call whose context has ended and stops reading a body mid-stream. Returning the bare context error is enough: the Disk classifies it as `unavailable`, so `layer.Retry` retries a per-attempt timeout.
- **Claiming a capability the provider does not have everywhere.** The S3 driver declares `ConditionalWrite` only when configured with `conditional_write=true`, because some S3-compatible stores ignore `If-None-Match` and would turn `IfNotExists` into a silent overwrite. Make such capabilities configuration.

## Next

- [Capabilities](../reference/capabilities.md): every bit of every built-in driver.
- [Write an interceptor](write-an-interceptor.md): behaviour around operations, without a driver.
- [`filesystemtest` reference](../../filesystemtest/README.md): the suite and the `Fake` disk.
