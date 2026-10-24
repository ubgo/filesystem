# Extending: a driver for your own backend

A complete storage driver for a made-up key-value service, built the way every real driver is. It adapts the vendor's API (`Backend`) to the six methods of `filesystem.Driver`, maps the vendor's errors onto filesystem codes, declares exactly the capabilities it delivers, opens from a `kv://` DSN, and hands out its native client through `filesystem.As`. This is what you write when your storage is not S3, GCS, Azure, SFTP or a local directory: an internal blob service, a database-backed store, a vendor with its own SDK. It uses only the core `github.com/ubgo/filesystem` package and `filesystemtest` for the conformance suite. The guide that explains it step by step is [Write a driver](../../docs/extending/write-a-driver.md).

## Run it

```sh
cd examples
go run ./extending-driver
go test ./extending-driver
```

It runs entirely in memory: the "service" is a map behind the `Backend` interface, so nothing is created on disk and no port is opened. The program prints what it did and exits. The tests run the full conformance suite against the driver, check that `NewDisk` refuses a driver that claims a capability it does not implement, and check that a throttling error from the service is retried.

## What it does

1. Registers the driver's `Opener` under the `kv` scheme on a `filesystem.Mux` and opens `kv://demo`.
2. Writes `/docs//readme.txt`. The Disk cleans the key to `docs/readme.txt` before the driver sees it, sniffs the content type and hashes the bytes.
3. Copies it to `docs/copy.txt`. The driver does not declare server-side copy, so the Disk streams the copy through `Read` and `Write`.
4. Lists `docs`, prints the declared capabilities, and reaches the backend client through `filesystem.As[Backend]`.

```text
wrote docs/readme.txt (27 bytes, text/plain; charset=utf-8, sha256 1fc60926c6a2…)
listed [docs/copy.txt docs/readme.txt]
capabilities [metadata,content-type,conditional-write]
native client reachable through filesystem.As
```

## The code

The vendor's API, as an interface so the example can supply an in-memory implementation. In a real driver this is the vendor's SDK client.

<!-- ds:block id=backend-4xk4syeq -->
**Backend** · [`examples/extending-driver/main.go:32-42`](main.go#L32-L42)

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

The driver holds nothing but the backend, so it is safe for concurrent use whenever the backend is.

<!-- ds:block id=driver-kmz69jg5 -->
**Driver** · [`examples/extending-driver/main.go:63-65`](main.go#L63-L65)

```go
type Driver struct {
	backend Backend
}
```
<!-- /ds:block hash=a4ba3c -->

`Capabilities` declares what the driver really does. Every bit it sets is checked by the conformance suite, and the Disk falls back (streaming copy and move) or refuses (signed URLs without a signer) for the ones it leaves unset.

<!-- ds:block id=driver-capabilities-t392pkc6 -->
**Driver.Capabilities** · [`examples/extending-driver/main.go:76-82`](main.go#L76-L82)

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

`mapErr` turns the service's errors into filesystem codes and keeps the original as the cause. Throttling becomes `CodeUnavailable`, the one code `layer.Retry` retries; anything unrecognised becomes `CodeUnknown`.

<!-- ds:block id=maperr-zra7zy73 -->
**mapErr** · [`examples/extending-driver/main.go:88-103`](main.go#L88-L103)

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

`entry` builds the `filesystem.Entry` every method returns: size and modification time are required, content type and metadata come back because the driver declared them.

<!-- ds:block id=entry-pq75fsxj -->
**entry** · [`examples/extending-driver/main.go:108-116`](main.go#L108-L116)

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

`Stat` must answer `CodeNotFound` for a missing key; `Read` applies the byte range, where a length of 0 means to the end and a range past the end is an empty read.

<!-- ds:block id=driver-stat-aqyx7ujn -->
**Driver.Stat** · [`examples/extending-driver/main.go:120-126`](main.go#L120-L126)

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

<!-- ds:block id=driver-read-9fmufg7s -->
**Driver.Read** · [`examples/extending-driver/main.go:131-142`](main.go#L131-L142)

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

`Write` reads the body to the end before storing anything, so a failing body (including the Disk's size cap) stores nothing. It honours `IfNotExists` atomically through the backend.

<!-- ds:block id=driver-write-gnbg8y2n -->
**Driver.Write** · [`examples/extending-driver/main.go:150-160`](main.go#L150-L160)

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

`Delete` succeeds on a missing key. `List` gathers the objects under a prefix and lets `filesystem.PageOf` sort, fold directories, apply the cursor and the limit; a backend with native paging would page itself instead.

<!-- ds:block id=driver-delete-scc574x4 -->
**Driver.Delete** · [`examples/extending-driver/main.go:164-166`](main.go#L164-L166)

```go
func (d *Driver) Delete(ctx context.Context, key string) error {
	return mapErr(d.backend.Delete(ctx, key))
}
```
<!-- /ds:block hash=7fcd25 -->

<!-- ds:block id=driver-list-rckcrhms -->
**Driver.List** · [`examples/extending-driver/main.go:173-194`](main.go#L173-L194)

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

`Native` is the typed escape hatch, and `Opener` opens `kv://` DSNs, refusing any query parameter it does not know so a typo fails at start-up.

<!-- ds:block id=driver-native-p84djt65 -->
**Driver.Native** · [`examples/extending-driver/main.go:199`](main.go#L199-L199)

```go
func (d *Driver) Native() Backend { return d.backend }
```
<!-- /ds:block hash=1f9e63 -->

<!-- ds:block id=opener-yetbbr5h -->
**Opener** · [`examples/extending-driver/main.go:207-212`](main.go#L207-L212)

```go
func Opener(_ context.Context, u *url.URL) (filesystem.Driver, error) {
	if err := filesystem.RejectParams(u); err != nil {
		return nil, err
	}
	return New(NewMapBackend()), nil
}
```
<!-- /ds:block hash=c6e698 -->

The conformance suite is the proof: each case gets a fresh disk, cases for undeclared capabilities are skipped, and the declared ones must hold. A driver that declares a capability without implementing its interface is refused by `NewDisk`.

<!-- ds:block id=testconformance-7wqaf78d -->
**TestConformance** · [`examples/extending-driver/main_test.go:21-29`](main_test.go#L21-L29)

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

<!-- ds:block id=testnewdiskrefusesanoverclaimingdriver-773vjepn -->
**TestNewDiskRefusesAnOverclaimingDriver** · [`examples/extending-driver/main_test.go:53-58`](main_test.go#L53-L58)

```go
func TestNewDiskRefusesAnOverclaimingDriver(t *testing.T) {
	_, err := filesystem.NewDisk("kv", overclaiming{New(NewMapBackend())})
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument || !strings.Contains(err.Error(), "declares capability Copy") {
		t.Errorf("err = %v", err)
	}
}
```
<!-- /ds:block hash=5c8ad8 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The `Backend` interface, the `Driver`, error mapping, the DSN `Opener`, an in-memory backend, and the demo `run` |
| `main_test.go` | The conformance suite, the over-claiming driver check, and retry of mapped throttling errors |

## Next

- [Write a driver](../../docs/extending/write-a-driver.md): the guide for this program, including the optional interfaces (server-side copy, presign, public URLs).
- [Write an interceptor](../extending-interceptor): behaviour around operations, without a driver.
- Real drivers to compare with: the [S3](../adapters-s3), [GCS](../adapters-gcs), [Azure](../adapters-azure) and [SFTP](../adapters-sftp) examples.
