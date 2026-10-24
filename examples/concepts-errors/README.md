# Errors: one set of codes for every backend

Every storage error is classified into a small set of codes, so application code branches the same way on local disk, S3 or SFTP. This program shows how to handle them: branching on the code (a missing avatar falls back to a default), the `Err*` sentinels with `errors.Is` (through wrapping), the operation, disk and key stamped on every error, reaching the backend's native error with `errors.As`, the per-key failures of a batch delete, and the HTTP status `fshttp.StatusOf` gives each code. It uses the core `filesystem` package, `memory`, `local` and `fshttp`. The guide [Errors](../../docs/concepts/errors.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-errors
go test ./concepts-errors
```

It creates a temporary directory for the local-disk step and removes it on exit. No network, no environment variables.

## What it does

```sh
go run ./concepts-errors
```

```text
avatar: default.png
errors.Is ErrNotFound: true
wrapped, still not_found
code=not_found op=stat disk=media key=avatars/nobody.png
message: load profile: filesystem: stat media:avatars/nobody.png: not_found: no object "avatars/nobody.png"
local: not_found, native *fs.PathError: true
batch failure: "../escape" invalid_key
batch errors.Is ErrInvalidKey: true
tmp/a still there: false
not_found -> 404 Not Found
exists -> 409 Conflict
invalid_key -> 400 Bad Request
invalid_argument -> 400 Bad Request
unsupported -> 501 Not Implemented
read_only -> 403 Forbidden
too_large -> 413 Request Entity Too Large
rejected -> 415 Unsupported Media Type
quota_exceeded -> 507 Insufficient Storage
permission -> 403 Forbidden
unavailable -> 503 Service Unavailable
unknown -> 500 Internal Server Error
upload over the plan, default: 507 application/json {"error":"the free plan holds 1 GB","code":"quota_exceeded"}
upload over the plan, problem: 402 application/problem+json {"code":"quota_exceeded","status":402,"title":"the free plan holds 1 GB"}
unsigned download, problem: 403 application/problem+json {"code":"permission","status":403,"title":"Forbidden"}
```

1. A missing avatar is `not_found`, an expected answer, so the default image is served; any other code would be a real failure.
2. `errors.Is(err, filesystem.ErrNotFound)` and `CodeOf` still work after the app wraps the error.
3. The `*filesystem.Error` carries the code, operation, disk and key, and its message reads as one line.
4. On local disk the classified error keeps the driver's `*fs.PathError` as its cause.
5. `DeleteKeys` with one invalid key deletes every other key and reports the bad one alone.
6. The status table is the mapping the library's own HTTP handlers answer with.
7. Every handler takes an `fshttp.ErrorWriter` in its options. The same refused upload (a plan check in `Policy.Inspect` answering `quota_exceeded`) gets the default JSON and `507` without one, and an RFC 9457 `application/problem+json` body with `402` from [`problem`](ds:block?id=problem-k2n8v4wq), which also answers the signed-URL handler's refusal of a link that was never signed. The title is `fshttp.ClientMessage(err)`, never `err.Error()`.

## The code

Branch on the code, never on a vendor error type.

<!-- ds:block id=avatarordefault-e4fy6urs -->
**avatarOrDefault** · [`examples/concepts-errors/main.go:64-76`](main.go#L64-L76)

```go
func avatarOrDefault(ctx context.Context, disk *filesystem.Disk, key string) string {
	_, err := disk.Stat(ctx, key)
	switch filesystem.CodeOf(err) {
	case "":
		return key
	case filesystem.CodeNotFound:
		return "default.png"
	case filesystem.CodeUnavailable:
		return "retry later"
	default:
		return "error: " + err.Error()
	}
}
```
<!-- /ds:block hash=122cda -->

The fields stamped on every error.

<!-- ds:block id=describe-meqmwbzm -->
**describe** · [`examples/concepts-errors/main.go:81-88`](main.go#L81-L88)

```go
func describe(out io.Writer, err error) {
	var fe *filesystem.Error
	if !errors.As(err, &fe) {
		return
	}
	fmt.Fprintf(out, "code=%s op=%s disk=%s key=%s\n", fe.Code, fe.Op, fe.Disk, fe.Key)
	fmt.Fprintf(out, "message: %v\n", err)
}
```
<!-- /ds:block hash=8fa181 -->

The escape hatch for vendor-specific handling; portable code stops at the code.

<!-- ds:block id=nativecause-z956hs3j -->
**nativeCause** · [`examples/concepts-errors/main.go:95-110`](main.go#L95-L110)

```go
func nativeCause(ctx context.Context, out io.Writer, dir string) error {
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
	_, err = disk.Stat(ctx, missingKey)
	var pe *fs.PathError
	fmt.Fprintf(out, "local: %s, native *fs.PathError: %t\n", filesystem.CodeOf(err), errors.As(err, &pe))
	return nil
}
```
<!-- /ds:block hash=d983c9 -->

Per-key failures of a batch.

<!-- ds:block id=batch-qkwf94nk -->
**batch** · [`examples/concepts-errors/main.go:116-130`](main.go#L116-L130)

```go
func batch(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	for _, k := range []string{"tmp/a", "tmp/b"} {
		_, _ = disk.WriteBytes(ctx, k, []byte(k), filesystem.WriteOptions{})
	}
	err := disk.DeleteKeys(ctx, []string{"tmp/a", "../escape", "tmp/b"})
	var be *filesystem.BatchError
	if errors.As(err, &be) {
		for _, f := range be.Failures {
			fmt.Fprintf(out, "batch failure: %q %s\n", f.Key, filesystem.CodeOf(f.Err))
		}
	}
	fmt.Fprintf(out, "batch errors.Is ErrInvalidKey: %t\n", errors.Is(err, filesystem.ErrInvalidKey))
	left, _ := disk.Exists(ctx, "tmp/a")
	fmt.Fprintf(out, "tmp/a still there: %t\n", left)
}
```
<!-- /ds:block hash=6042d1 -->

<!-- ds:block id=statuses-hn9je66t -->
**statuses** · [`examples/concepts-errors/main.go:135-140`](main.go#L135-L140)

```go
func statuses(out io.Writer) {
	for _, c := range filesystem.CodeValues {
		s := fshttp.StatusOf(filesystem.Errorf(c, "example"))
		fmt.Fprintf(out, "%s -> %d %s\n", c, s, http.StatusText(s))
	}
}
```
<!-- /ds:block hash=560d97 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: branching, sentinels, error fields, the native cause, batch failures, HTTP statuses |
| `envelope.go` | A problem+json error writer on the upload and signed-URL handlers; code: [`envelope`](ds:block?id=envelope-r5t9x3mc) |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Errors](../../docs/concepts/errors.md), and [reference/errors](../../docs/reference/errors.md) for every code, sentinel and status in one table.
- [Interceptors](../../docs/concepts/interceptors.md): retrying `unavailable` and how a timeout is classified; example [concepts-interceptors](../concepts-interceptors).
- [Capabilities](../../docs/concepts/capabilities.md) for `unsupported`; example [concepts-capabilities](../concepts-capabilities).
