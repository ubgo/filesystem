# fiber

You will have every HTTP endpoint the library ships served by a [fiber](https://github.com/gofiber/fiber) v3 app: form uploads, direct-upload tickets, Uppy multipart, tus resumable uploads, signed links, public files and the records endpoints. Fiber runs on fasthttp, not net/http, so the library's handlers go through fiber's own `adaptor` middleware. That works, with two costs this page states plainly: request bodies are buffered in memory, and so are responses.

## Before you start

- [net/http, gorilla and chi](net-http.md) explains each handler, the policy, the signer and the records hooks. This page changes only the router.
- The program is [`examples/frameworks/fiber`](../../examples/frameworks/fiber/main.go), in the separate module `examples/frameworks`. From that directory: `go run ./fiber` serves it on `http://localhost:8093`; `go test ./fiber` drives every route through a real fiber listener, including an upload larger than fiber's default body limit.
- The storage wiring is shared with the other framework programs: [`examples/frameworks/internal/app`](../../examples/frameworks/internal/app/app.go).
- The bridge on this page is `github.com/gofiber/fiber/v3/middleware/adaptor`, part of fiber itself. The adapter module `github.com/ubgo/filesystem/contrib/fiber` (`fsfiber`) replaces it for the library's handlers: `fsfiber.Handler` and `fsfiber.Mount` carry the signed-in user and the tenant from fiber's middleware into the handler's `*http.Request` (the plain adaptor drops them), `fsfiber.ErrorHandler` renders the library's errors, and `fsfiber.Zip` streams zips. [Framework adapters](framework-adapters.md) shows it on the same routes, run from [`examples/frameworks/adapters/fiber`](../../examples/frameworks/adapters/fiber/main.go).

## Step 1 — Raise the body limit

Fiber reads a whole request body into memory before any handler runs, and refuses a body over `fiber.Config.BodyLimit` (4 MiB by default) with its own 413. The limit must admit the largest request your upload routes accept, or fiber refuses uploads the policy allows:

<!-- ds:block id=maxformfiles-6e3v4vpk -->
**maxFormFiles** · [`examples/frameworks/fiber/main.go:50-54`](../../examples/frameworks/fiber/main.go#L50-L54)

```go
const (
	maxFormFiles   = 5
	formOverhead   = 64 << 10
	fiberBodyLimit = maxFormFiles*app.MaxUploadBytes + formOverhead
)
```
<!-- /ds:block hash=9f63d0 -->

With the default limit, a 4 MiB + 1 byte upload never reaches the library: fiber answers `413` with the plain-text body `Request Entity Too Large` instead of the library's JSON error. The library still enforces the policy's `MaxSize` on what it receives; fiber's limit only decides whether it receives anything.

## Step 2 — Mount the handlers

`adaptor.HTTPHandler` turns an `http.Handler` into a fiber handler; tus goes through `fsfiber.Handler` instead, for the reason under [Pitfalls](#pitfalls). Each catch-all route (`/*`) is wrapped in `http.StripPrefix`, because the library's handlers route on the path below their mount point. `All` registers every method, `PATCH` included for tus:

<!-- ds:block id=newrouter-bqey7agv -->
**newRouter** · [`examples/frameworks/fiber/main.go:93-104`](../../examples/frameworks/fiber/main.go#L93-L104)

```go
func newRouter(h app.Handlers) *fiber.App {
	f := fiber.New(fiber.Config{BodyLimit: fiberBodyLimit, ReadTimeout: readTimeout})
	f.Post(app.PathUpload, adaptor.HTTPHandler(h.Upload))
	f.Post(app.PathTicket, adaptor.HTTPHandler(h.Ticket))
	f.All(app.PathUppy+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathUppy, h.Uppy)))
	// fsfiber, not the raw adaptor: tusd keeps a context past the request, and fasthttp's pooled RequestCtx then panics at shutdown.
	f.All(app.PathTus+"*", fsfiber.Handler(http.StripPrefix(app.PathTus, h.Tus)))
	f.All(app.PathFiles+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathFiles, h.Files)))
	f.All(app.PathPublic+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathPublic, h.Public)))
	f.All(app.PathRecords+"/*", adaptor.HTTPHandler(http.StripPrefix(app.PathRecords, h.Records)))
	return f
}
```
<!-- /ds:block hash=cc40cc -->

Check: start it and upload a file:

```sh
curl -s -F file=@report.txt localhost:8093/upload
```

```json
{"files":[{"key":"incoming/f74vkwe624gdfen5do7h3xa3ug.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
```

A signed download through the adaptor keeps ranges and the security headers; fasthttp orders the headers differently:

```sh
curl -s -i -H 'Range: bytes=0-8' "$LINK"
```

```
HTTP/1.1 206 Partial Content
Date: Tue, 06 Oct 2026 06:16:46 GMT
Content-Type: text/plain; charset=utf-8
Content-Length: 9
Cache-Control: private, no-store
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
Last-Modified: Tue, 06 Oct 2026 06:16:46 GMT
Content-Range: bytes 0-8/18
Accept-Ranges: bytes
X-Content-Type-Options: nosniff
Content-Security-Policy: default-src 'none'; sandbox

quarterly
```

Check: `go test ./fiber` passes; it runs the form upload and a refused HTML file, the records 401, 403 and 201, the redirect and signed download, a public file, a ticket, a full Uppy upload, a tus upload, and a 4 MiB + 1 byte upload that the raised limit lets through.

## Step 3 — Write your own fiber handler

Everything your app does with files beyond the shipped endpoints (a metadata route, a rename, an export) is a native fiber handler that calls the `Disk` or the `Store` directly; it needs no adaptor. The program has one at `GET /info/*`, answering the metadata of one object of the media disk to a signed-in user:

<!-- ds:block id=fileinfo-tk4wrea2 -->
**fileInfo** · [`examples/frameworks/fiber/main.go:114-131`](../../examples/frameworks/fiber/main.go#L114-L131)

```go
func fileInfo(disk *filesystem.Disk) fiber.Handler {
	return func(c fiber.Ctx) error {
		if c.Get(app.HeaderUser) == "" {
			return c.Status(http.StatusUnauthorized).JSON(app.Problem{Error: app.ErrSignedOutMessage, Code: filesystem.CodePermission})
		}
		// fiber hands parameters over still percent-encoded (unless
		// Config.UnescapePath is set for the whole app).
		key, err := url.PathUnescape(c.Params("*"))
		if err != nil {
			return c.Status(http.StatusBadRequest).JSON(app.Problem{Error: "malformed path", Code: filesystem.CodeInvalidKey})
		}
		entry, err := disk.Stat(c.Context(), key)
		if err != nil {
			return c.Status(fshttp.StatusOf(err)).JSON(app.ProblemOf(err))
		}
		return c.Status(http.StatusOK).JSON(app.InfoOf(entry))
	}
}
```
<!-- /ds:block hash=55e8bf -->

It is mounted after the library's routes: `r.Get(app.PathInfo+"/*", fileInfo(a.Disks.Media))`. What it gets right:

1. **`c.Context()`**, fiber v3's request context, goes to every library call.
2. **The wildcard is unescaped.** fiber hands `c.Params("*")` over still percent-encoded unless the whole app sets `fiber.Config{UnescapePath: true}`; without `url.PathUnescape`, `notes/read%20me.txt` is looked up literally and answers 404.
3. **No traversal check of its own.** The Disk cleans every key and refuses a `..` segment before any backend sees it; `%2E%2E%2F` becomes `../` after unescaping and is refused with `invalid_key`.
4. **Errors answer with the status of their code and `fshttp.ClientMessage`** (`app.ProblemOf`), never `err.Error()`, which can name buckets or other keys:

<!-- ds:block id=problemof-kjsuu6nd -->
**ProblemOf** · [`examples/frameworks/internal/app/info.go:43-45`](../../examples/frameworks/internal/app/info.go#L43-L45)

```go
func ProblemOf(err error) Problem {
	return Problem{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}
}
```
<!-- /ds:block hash=c4bbf7 -->

From a real run (`go run ./fiber`, after uploading `report.txt` through `/upload`):

```text
$ curl -s -H 'X-User: alice' localhost:8093/info/incoming/ogml5666luzvhw45pdgzytj5nc.txt
{"key":"incoming/ogml5666luzvhw45pdgzytj5nc.txt","size":18,"content_type":"text/plain; charset=utf-8","modified":"2026-10-08T13:21:17.86473Z","etag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}
$ curl -s -i localhost:8093/info/incoming/ogml5666luzvhw45pdgzytj5nc.txt
HTTP/1.1 401 Unauthorized
Content-Type: application/json; charset=utf-8
Content-Length: 45

{"error":"sign in first","code":"permission"}
$ curl -s -H 'X-User: alice' localhost:8093/info/incoming/none.txt
{"error":"Not Found","code":"not_found"}
$ curl -s -H 'X-User: alice' localhost:8093/info/%2E%2E%2Fsecret.txt
{"error":"key contains a \"..\" segment","code":"invalid_key"}
```

Check: `go test ./fiber` sends these requests and more (a key with a space, one reached through an escaped slash, keys holding a literal `%20` and a `%`, a directory, an encoded `..` behind a real directory) through the cases shared by every framework program, `apptest.InfoCases` in [`internal/apptest/info.go`](../../examples/frameworks/internal/apptest/info.go).

Writing from a fiber handler: `c.Body()` is the buffered body, not a stream, so hand it to `disk.Write` as `bytes.NewReader(c.Body())`, which also makes the write retryable by `layer.Retry`.

The records hooks receive the `*http.Request` the adaptor builds, not the `fiber.Ctx`. Pass authentication through request headers (as the example's `X-User` does), or use `adaptor.HTTPHandlerWithContext` and read fiber's context in the hook with `adaptor.LocalContextFromHTTPRequest(r)`. `fsfiber` from `contrib/fiber` resolves the user and tenant once in middleware ([Framework adapters](framework-adapters.md)).

## Costs of running on fasthttp

- **Uploads are buffered in memory.** Every concurrent upload holds its whole body in RAM before the library sees a byte, so the library's streaming (nothing larger than `MaxSize` is ever buffered) does not hold on fiber. Size `BodyLimit` and your instance's memory together, or send large files around the app server: [browser direct uploads](../recipes/browser-direct-uploads.md) put them straight into the bucket, and [tus](../recipes/resumable-uploads.md) sends them in chunks.
- **Downloads are buffered too.** fasthttp's adaptor holds a net/http handler's response in memory unless the handler calls `Flush`, and `fshttp` does not. For large private files on an S3, GCS or Azure disk, redirect to the backend's own signed URL (`records.Store.URL` does this) instead of serving through `fshttp.Signed`.
- **A zip through the adaptor cannot fail safely.** The adaptor either buffers the whole archive or, once the handler flushes, ends the stream cleanly when `fshttp.ServeZip` aborts a failed archive with `http.ErrAbortHandler`, so the client keeps a truncated archive that looks complete. Use `fsfiber.Zip` from `contrib/fiber`: it bypasses the adaptor and streams through fasthttp's body stream, so a large archive is never held in memory and a failure after the first byte closes the connection ([framework adapters](framework-adapters.md)).
- **Timeouts cover the body.** `fiber.Config.ReadTimeout` bounds reading the whole request; the example sets five minutes. Set it for the slowest upload you accept.

## Pitfalls

- **Wrong methods** get fiber's plain-text 405 with an `Allow` header (`GET /upload` answers `Method Not Allowed`, `Allow: POST`).
- **A 413 in plain text** on an upload the policy allows is fiber's `BodyLimit`, not the library.
- **Path parameters arrive percent-encoded.** A handler of your own that takes a key from the path must unescape it (Step 3), or every key with a space or non-ASCII character answers 404. Library handlers mounted with `fsfiber.Mount` get the decoded key either way, because Mount strips its prefix from the decoded path of the net/http request the adaptor builds; with `UnescapePath` set, stop unescaping in your own handlers, or a key holding `%` is decoded twice.
- **A handler that outlives its request can crash the server at shutdown.** fiber's adaptor hands the handler fasthttp's pooled `RequestCtx` as its request context. A handler that keeps a context derived from it after the request (tus does, and so does background work started from a request) holds a context whose `RequestCtx` has since been reused, and at shutdown the process can panic with `context: internal error: missing cancel error`. The example did: one tus upload, then Ctrl+C, exited 2. Mount such handlers with `fsfiber.Handler` from `contrib/fiber`, which gives each one a context of its own that is cancelled at shutdown and when the handler returns.

## Next

- [Framework adapters](framework-adapters.md): `fsfiber` for the user and tenant in middleware, central error rendering, `fsfiber.Handler` and zips that stream.
- [net/http, gorilla and chi](net-http.md): every handler and transcript in detail.
- [gin](gin.md), [echo](echo.md): the same handlers on other frameworks.
- [Resumable uploads](../recipes/resumable-uploads.md): large files in chunks that fit any body limit.
