# Capabilities: what a backend can do, and what happens when it cannot

Backends differ: S3 copies on the server, a local directory cannot store metadata, a memory disk has no signer. This program shows how the library handles that without ever silently dropping an option: what the local and memory drivers declare, the portable fallbacks a Disk uses when a capability is missing (a streamed copy, one-by-one deletes), the `unsupported` error where no fallback exists, branching on a capability before calling, and `NewDisk` refusing a driver that claims a capability it does not implement. It uses the core `filesystem` package with `local` and `memory`. The guide [Capabilities](../../docs/concepts/capabilities.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-capabilities
go test ./concepts-capabilities
```

It creates a temporary directory for the local disk, removes it on exit, prints eleven lines and exits. No network, no environment variables.

## What it does

```sh
go run ./concepts-capabilities
```

```text
local:  [move,set-visibility,delete-prefix,multipart,sweep-abandoned,find-abandoned,dirs,conditional-write,append,set-mod-time,compare-and-swap]
memory: [copy,move,set-visibility,delete-prefix,delete-keys,multipart,sweep-abandoned,find-abandoned,dirs,metadata,content-type,headers,conditional-write,append,set-mod-time,tags,storage-class,compare-and-swap]
streamed copy: b.txt 11 bytes, public
deleted two keys one by one: ok
metadata: unsupported
headers: unsupported
signed URL without a signer: unsupported
public URL without a base: unsupported
m.txt stored anyway: false
write after checking Metadata: <nil>
NewDisk(liar): filesystem: invalid_argument: driver main.liarDriver declares capability Copy but does not implement its interface
```

1. Each driver declares its capabilities; the local driver lacks server-side copy, batch delete, metadata, content type and headers.
2. `Copy` and `DeleteKeys` still work on local disk: the Disk streams the object through the process (keeping its visibility) and deletes key by key.
3. Metadata, headers, a signed URL with no signer and a public URL with no base are refused with `unsupported`, and the refused write stores nothing.
4. Checking `disk.Capabilities().Metadata` first lets the app put metadata on the object only where the backend keeps it.
5. A driver that declares `Copy` without a `Copy` method is refused by `NewDisk` at start-up, not on its first copy in production.

## The code

The fallbacks: two operations the local driver has no capability for, behaving exactly as on a backend that does them natively.

<!-- ds:block id=fallbacks-njuba3a9 -->
**fallbacks** · [`examples/concepts-capabilities/main.go:68-82`](main.go#L68-L82)

```go
func fallbacks(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("public file"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	e, err := disk.Copy(ctx, "a.txt", "b.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "streamed copy: %s %d bytes, %s\n", e.Key, e.Size, e.Visibility)
	if err := disk.DeleteKeys(ctx, []string{"a.txt", "b.txt"}); err != nil {
		return err
	}
	fmt.Fprintln(out, "deleted two keys one by one: ok")
	return nil
}
```
<!-- /ds:block hash=5d7d4a -->

Options with no fallback are refused before anything is written, rather than dropped with a reported success.

<!-- ds:block id=unsupported-kra8uej8 -->
**unsupported** · [`examples/concepts-capabilities/main.go:88-103`](main.go#L88-L103)

```go
func unsupported(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	_, err := disk.WriteBytes(ctx, "m.txt", []byte("x"), filesystem.WriteOptions{Metadata: map[string]string{"owner": "u1"}})
	fmt.Fprintf(out, "metadata: %s\n", filesystem.CodeOf(err))
	_, err = disk.WriteBytes(ctx, "h.txt", []byte("x"), filesystem.WriteOptions{Headers: filesystem.HTTPHeaders{CacheControl: "no-store"}})
	fmt.Fprintf(out, "headers: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "m.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	fmt.Fprintf(out, "signed URL without a signer: %s\n", filesystem.CodeOf(err))
	_, err = disk.PublicURL(ctx, "m.txt")
	fmt.Fprintf(out, "public URL without a base: %s\n", filesystem.CodeOf(err))
	ok, err := disk.Exists(ctx, "m.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "m.txt stored anyway: %t\n", ok)
	return nil
}
```
<!-- /ds:block hash=f314a7 -->

Branching on a capability is how an application offers a feature only where the backend has it.

<!-- ds:block id=checkfirst-u4pgcann -->
**checkFirst** · [`examples/concepts-capabilities/main.go:110-117`](main.go#L110-L117)

```go
func checkFirst(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	opts := filesystem.WriteOptions{}
	if disk.Capabilities().Metadata {
		opts.Metadata = map[string]string{"owner": "u1"}
	}
	_, err := disk.WriteBytes(ctx, "report.txt", []byte("x"), opts)
	fmt.Fprintf(out, "write after checking Metadata: %v\n", err)
}
```
<!-- /ds:block hash=c8ec05 -->

A driver that forwards the six `Driver` methods to a memory driver but claims server-side copy, and `NewDisk` catching the claim.

<!-- ds:block id=liardriver-gzywh4kk -->
**liarDriver** · [`examples/concepts-capabilities/main.go:122`](main.go#L122-L122)

```go
type liarDriver struct{ inner *memory.Driver }
```
<!-- /ds:block hash=44519c -->

<!-- ds:block id=refuseliar-wra6kbkq -->
**refuseLiar** · [`examples/concepts-capabilities/main.go:150-153`](main.go#L150-L153)

```go
func refuseLiar(out io.Writer) {
	_, err := filesystem.NewDisk("liar", liarDriver{inner: memory.New()})
	fmt.Fprintf(out, "NewDisk(liar): %v\n", err)
}
```
<!-- /ds:block hash=2549b2 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: declared capabilities, fallbacks, refusals, check-first, the lying driver |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Capabilities](../../docs/concepts/capabilities.md), with the table of capabilities by backend.
- [Errors](../../docs/concepts/errors.md) for `unsupported` and every other code; example [concepts-errors](../concepts-errors).
- [Write a driver](../../docs/extending/write-a-driver.md) and its example [extending-driver](../extending-driver).
