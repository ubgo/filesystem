# fiber: every storage endpoint on a fiber app

Every HTTP handler the library ships, mounted on fiber v3: form uploads, direct-upload tickets, Uppy multipart, tus, signed links, public files and the records endpoints. `adaptor.HTTPHandler` turns each `http.Handler` into a fiber handler, except tus, which goes through `fsfiber.Handler` from `contrib/fiber` because it keeps a context after the request (see the [fiber guide](../../../docs/integrations/fiber.md#pitfalls)), so this program is routing plus one setting fiber needs: a body limit large enough for the upload policy, because fiber reads every request body into memory before any handler runs. The storage wiring is shared with the other framework examples in [`internal/app`](../internal/app/app.go). It uses `github.com/gofiber/fiber/v3` and `contrib/fiber` with the library's `upload`, `fshttp`, `contrib/tus` and `records/recordshttp` handlers. The guide is [fiber](../../../docs/integrations/fiber.md).

## Run it

From the framework module, not from `examples/`:

```sh
cd examples/frameworks
go run ./fiber
go test ./fiber
```

It listens on `http://localhost:8093` until you press Ctrl+C. The disks are in memory, the S3 bucket for tickets and Uppy is a fake S3 server (gofakes3) on a random loopback port, and the records tables are in an in-memory SQLite database, so nothing survives a restart. tus keeps unfinished uploads in a `tus-sessions-*` directory under the system temp directory. Ctrl+C or SIGTERM stops the server cleanly through fiber's `ShutdownWithTimeout`: requests in flight get up to five seconds to finish, the fake S3 stops, the tus directory is removed, and the process exits 0. The `X-User` header stands in for real authentication. The test serves the app on a free port, drives every route through `internal/apptest`, and uploads a file one byte over fiber's default 4 MiB body limit to prove the raised limit is in effect.

Because fiber buffers whole bodies, every upload costs its size in memory while it is in flight, up to the body limit (five files of 10 MiB plus form overhead here). For large files, use tus with a client that sends chunks, or direct uploads that go straight to the bucket, so no single request through fiber is large.

## Walkthrough

`report.txt` holds `quarterly numbers` and a newline (18 bytes); `page.html` is a small HTML page.

1. A form upload is stored with a type read from the bytes; HTML is refused. A wrong method is fiber's plain-text 405:

```sh
curl -s -F file=@report.txt localhost:8093/upload
curl -s -w '%{http_code}\n' -F file=@page.html localhost:8093/upload
curl -s -i -X DELETE localhost:8093/upload
```

```text
{"files":[{"key":"incoming/56yiqgqr5btssrgzzkkfdkosgr.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
HTTP/1.1 405 Method Not Allowed
Date: Tue, 06 Oct 2026 06:31:17 GMT
Content-Type: text/plain; charset=utf-8
Content-Length: 18
Allow: POST

Method Not Allowed
```

2. Alice uploads and attaches a file through the records endpoints; the file route redirects to a signed link:

```sh
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8093/media/owners/user/alice/docs
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-User: alice' localhost:8093/media/files/fil_idrju245z4dn2t5pxbina4svip
```

```text
{"files":[{"attachment_id":"att_wroqvad6v5evbrxyot65xdrko3","file_id":"fil_idrju245z4dn2t5pxbina4svip","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
302 http://localhost:8093/files/media/incoming/iyjaflyuea2v6yjwar2glytye5.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791268877&kid=k1&m=GET&sig=6f2c1d33aa947e3c5e8b0e8ecf809ae71b5e6ae166468ff93975a3055637ae9f
```

3. The signed link serves ranges, with the headers that stop user content from running script:

```sh
curl -s -i -H 'Range: bytes=0-8' "$LINK"
```

```text
HTTP/1.1 206 Partial Content
Date: Tue, 06 Oct 2026 06:31:17 GMT
Content-Type: text/plain; charset=utf-8
Content-Length: 9
Content-Security-Policy: default-src 'none'; sandbox
X-Content-Type-Options: nosniff
Cache-Control: private, no-store
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
Last-Modified: Tue, 06 Oct 2026 06:31:17 GMT
Content-Range: bytes 0-8/18
Accept-Ranges: bytes

quarterly
```

4. A tus upload: create, then one `PATCH`; the stored key comes back in `Filesystem-Key`:

```sh
curl -s -i -X POST localhost:8093/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf notes.txt | base64),filetype $(printf text/plain | base64)"
curl -s -i -X PATCH http://localhost:8093/tus/5a21e4f16e8bf7fe5e936a19288309c5 -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @report.txt
```

```text
HTTP/1.1 201 Created
Date: Tue, 06 Oct 2026 06:31:17 GMT
Content-Length: 0
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Location: http://localhost:8093/tus/5a21e4f16e8bf7fe5e936a19288309c5

HTTP/1.1 204 No Content
Date: Tue, 06 Oct 2026 06:31:17 GMT
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Filesystem-Key: incoming/gsnpw6zgufjyjlc5bz2hrsezo4.txt
Upload-Offset: 18
```

Tickets and Uppy work the same way as in [integrations-nethttp](../../integrations-nethttp). Stop the server with Ctrl+C; it shuts down cleanly and removes its tus directory.

## The code

The body limit must admit the largest request the upload routes accept: up to five files of `app.MaxUploadBytes` each, plus multipart overhead. Fiber's default of 4 MiB would refuse a legal 10 MiB upload with 413 before the library sees it.

<!-- ds:block id=maxformfiles-6e3v4vpk -->
**maxFormFiles** · [`examples/frameworks/fiber/main.go:50-54`](main.go#L50-L54)

```go
const (
	maxFormFiles   = 5
	formOverhead   = 64 << 10
	fiberBodyLimit = maxFormFiles*app.MaxUploadBytes + formOverhead
)
```
<!-- /ds:block hash=9f63d0 -->

`newRouter` sets the body limit and a read timeout long enough for the largest upload, adapts each handler with `adaptor.HTTPHandler` (tus with `fsfiber.Handler`), and strips each catch-all route's prefix with `http.StripPrefix`. `All` registers every method, `PATCH` included for tus.

<!-- ds:block id=newrouter-bqey7agv -->
**newRouter** · [`examples/frameworks/fiber/main.go:93-104`](main.go#L93-L104)

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

`fileInfo` is a handler of the application's own, mounted at `GET /info/*` next to the library's routes: it calls `Disk.Stat` directly for a signed-in user, with the request's context, and answers errors with the status of their code and `fshttp.ClientMessage` (via `app.ProblemOf`), never `err.Error()`. fiber hands the wildcard over still percent-encoded, so the handler unescapes it with `url.PathUnescape`; the Disk refuses `..` itself.

<!-- ds:block id=fileinfo-tk4wrea2 -->
**fileInfo** · [`examples/frameworks/fiber/main.go:114-131`](main.go#L114-L131)

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

```sh
curl -s -H 'X-User: alice' localhost:8093/info/incoming/<key from an upload>.txt
curl -s -H 'X-User: alice' localhost:8093/info/%2E%2E%2Fsecret.txt
```

The first answers the key, size, content type, modification time and ETag as JSON; the second `{"error":"key contains a \"..\" segment","code":"invalid_key"}` with status 400. The test runs the shared cases of `apptest.InfoCases` (an object, a signed-out caller, a missing key, a key with a space, one reached through an escaped slash, keys holding a literal `%20` and a `%`, a directory, two encoded `..` keys) through the real server.

## Files

| File | What it holds |
|---|---|
| `main.go` | The body limit, `run` (fake S3, tus directory, `app.New`, the listener with graceful shutdown), `newRouter` and `fileInfo` |
| `main_test.go` | Every route through a real fiber server via `internal/apptest`, the info route, and an upload over the default body limit |

## Next

- [fiber](../../../docs/integrations/fiber.md): the guide for this program.
- [Resumable uploads](../../recipe-resumable-uploads): large files in chunks that fit any body limit.
- The same handlers on [chi](../chi), [gin](../gin) and [echo](../echo); the shared wiring is described in the [framework module README](..).
