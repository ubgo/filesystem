# Basics: the Disk API in one program

A tour of the storage layer on its own, with no database: two disks opened from DSNs through a Mux (a local directory and a memory store), a start-up check through a Manager, a write that sniffs the content type and hashes the bytes while streaming, a refused traversal key, a scoped disk for one tenant, a copy across disks, and a signed download link served by `fshttp`. It uses the core `filesystem` package with `local`, `memory`, `layer` (logging and retry interceptors), `signedurl` and `fshttp`. The [project README](../../README.md#examples) introduces it, and [Disks and drivers](../../docs/concepts/disks-and-drivers.md) explains the API it walks through.

## Run it

```sh
cd examples
go run ./basics
go test ./basics
```

It creates a temporary directory for the local disk and removes it on exit. The signed link is served by an in-process `httptest` server on a random port that closes when the program ends. It reads no environment variables and needs no network.

## What it does

```sh
go run ./basics
```

```text
wrote docs/hello.txt: 14 bytes, text/plain; charset=utf-8, sha256 60cf5d8443f5…
read back "hello, storage"
refused a traversal key
listed docs/hello.txt
listed tenants/42/logo.png
copied to the memory disk
downloaded "hello, storage" through a signed URL (status 200)
```

1. A Mux holds the two drivers this program imports (`file://` and `mem://`); nothing registers itself globally. The local disk gets a URL signer and two interceptors, and `Manager.Check` verifies both disks at start-up.
2. `Write` streams the bytes, detects `text/plain` from their content and computes the SHA-256 on the way; `ReadAll` reads them back.
3. `Stat("../../etc/passwd")` fails with `CodeInvalidKey` before any driver sees the key.
4. `Scope("tenant-42", "tenants/42")` gives a disk that sees only that prefix; its `logo.png` lands at `tenants/42/logo.png` on the parent disk, as the recursive listing shows.
5. `CopyAcross` streams a file from the local disk to the memory disk.
6. `SignedURL` returns a one-minute link to a private file, and `fshttp.Signed` serves it after checking the signature and expiry.

## The code

The whole program is one function, `run`, shown here in four parts in the order of the steps above.

Step 1 opens the disks from DSNs through a `Mux` and gives the local disk a signer, with an `httptest` server standing in for the app that serves its links.

<!-- ds:block id=run-sk7bhtyn lines=1-35 -->
**run** · [`examples/basics/main.go:43-135`](main.go#L43-L135)

```go
func run(ctx context.Context, out io.Writer, dir string) error {
	// 1. Open disks from DSNs. The Mux holds the drivers this program
	// imports; nothing registers itself globally.
	mux := filesystem.NewMux()
	_ = mux.Register(local.Scheme, local.Opener)
	_ = mux.Register(memory.Scheme, memory.Opener)

	// Signed URLs for a local disk are served by the application itself.
	srv := httptest.NewServer(nil)
	defer srv.Close()
	signer, err := signedurl.New(srv.URL+"/files", []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}})
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	uploads, err := mux.Open(ctx, "uploads", "file://"+dir,
		filesystem.WithURLSigner(signer),
		filesystem.WithInterceptors(layer.Log(logger), layer.Retry(layer.RetryOptions{})),
	)
	if err != nil {
		return err
	}
	defer func() { _ = uploads.Close() }()
	cache, err := mux.Open(ctx, "cache", "mem://")
	if err != nil {
		return err
	}
	mgr, err := filesystem.NewManager(uploads, cache)
	if err != nil {
		return err
	}
	if err := mgr.Check(ctx); err != nil {
		return fmt.Errorf("start-up check: %w", err)
	}

```
<!-- /ds:block hash=26f03a -->

Steps 2 and 3 write and read a file, then show keys being cleaned and a traversal refused.

<!-- ds:block id=run-sk7bhtyn lines=36-54 -->
**run** · [`examples/basics/main.go:43-135`](main.go#L43-L135)

```go
	// 2. Write and read. The Disk detects the content type and hashes the
	// bytes while streaming.
	e, err := uploads.Write(ctx, "docs/hello.txt", strings.NewReader("hello, storage"), filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s: %d bytes, %s, sha256 %s…\n", e.Key, e.Size, e.ContentType, e.SHA256[:12])
	data, err := uploads.ReadAll(ctx, "docs/hello.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read back %q\n", data)

	// 3. Keys are normalised and traversal is refused before any driver
	// sees them.
	if _, err := uploads.Stat(ctx, "../../etc/passwd"); filesystem.CodeOf(err) == filesystem.CodeInvalidKey {
		fmt.Fprintln(out, "refused a traversal key")
	}

```
<!-- /ds:block hash=26f03a -->

Steps 4 and 5 scope a disk to one tenant and copy across disks.

<!-- ds:block id=run-sk7bhtyn lines=55-75 -->
**run** · [`examples/basics/main.go:43-135`](main.go#L43-L135)

```go
	// 4. A scoped disk sees only one tenant's keys.
	tenant, err := uploads.Scope("tenant-42", "tenants/42")
	if err != nil {
		return err
	}
	if _, err := tenant.WriteBytes(ctx, "logo.png", []byte("\x89PNG\r\n\x1a\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	for entry, err := range uploads.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed %s\n", entry.Key)
	}

	// 5. Copy across disks streams the bytes.
	if _, err := filesystem.CopyAcross(ctx, uploads, "docs/hello.txt", cache, "hello-copy.txt"); err != nil {
		return err
	}
	fmt.Fprintln(out, "copied to the memory disk")

```
<!-- /ds:block hash=26f03a -->

Step 6 signs a link to a private file and fetches it through `fshttp.Signed`.

<!-- ds:block id=run-sk7bhtyn lines=76-93 -->
**run** · [`examples/basics/main.go:43-135`](main.go#L43-L135)

```go
	// 6. A signed link to a private file, served by fshttp.
	srv.Config.Handler = http.StripPrefix("/files", fshttp.Signed(mgr, signer))
	link, err := uploads.SignedURL(ctx, "docs/hello.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		return err
	}
	resp, err := http.Get(link.URL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "downloaded %q through a signed URL (status %d)\n", body, resp.StatusCode)
	return nil
}
```
<!-- /ds:block hash=26f03a -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: `run` performs every step and prints what it saw |
| `main_test.go` | Runs the program and checks its output |

## Next

- The concepts behind each step: [disks and drivers](../../docs/concepts/disks-and-drivers.md), [keys](../../docs/concepts/keys.md), [multi-tenancy](../../docs/concepts/multi-tenancy.md), [serving files](../../docs/concepts/serving-files.md), [interceptors](../../docs/concepts/interceptors.md).
- One step each, in depth: [concepts-disks](../concepts-disks), [concepts-keys](../concepts-keys), [concepts-tenancy](../concepts-tenancy), [concepts-serving](../concepts-serving).
- With files stored as database rows: [mediaserver](../mediaserver) and [drive](../drive).
