# gin

You will have every HTTP endpoint the library ships served by a [gin](https://github.com/gin-gonic/gin) engine: form uploads, direct-upload tickets, Uppy multipart, tus resumable uploads, signed links, public files and the records endpoints. The library's handlers are plain `http.Handler` values, so gin mounts them with `gin.WrapH`; nothing about the storage side changes.

## Before you start

- [net/http, gorilla and chi](net-http.md) explains each handler, the policy, the signer and the records hooks. This page changes only the router.
- The program is [`examples/frameworks/gin`](../../examples/frameworks/gin/main.go), in the separate module `examples/frameworks` so the main examples module does not depend on gin. From `examples/frameworks`: `go run ./gin` serves it on `http://localhost:8091`; `go test ./gin` drives every route through a real gin server.
- The storage wiring is shared by the gin, echo, fiber and chi programs: [`examples/frameworks/internal/app`](../../examples/frameworks/internal/app/app.go). It is the net/http example's wiring with exported names (`app.Handlers`, `app.PathFiles`, …).
- This page mounts the handlers with nothing but gin. The adapter module `github.com/ubgo/filesystem/contrib/gin` (`fsgin`) adds what is left to you here: mounting under route groups with the prefix stripped, the signed-in user and the tenant resolved once in middleware, the library's errors rendered by `fsgin.Error`, and zips with `fsgin.Zip`. [Framework adapters](framework-adapters.md) shows it on the same routes, run from [`examples/frameworks/adapters/gin`](../../examples/frameworks/adapters/gin/main.go).

## Step 1 — Mount the handlers

`gin.WrapH` turns an `http.Handler` into a gin handler. Each library handler routes on the path below its mount point, so a catch-all route (`/*path`) is wrapped in `http.StripPrefix`. `Any` registers every method, which tus (`PATCH`, `HEAD`, `DELETE`, `OPTIONS`) and the signed-link handler (`GET`, `HEAD`, `PUT`) need:

<!-- ds:block id=newrouter-h32462p2 -->
**newRouter** · [`examples/frameworks/gin/main.go:75-86`](../../examples/frameworks/gin/main.go#L75-L86)

```go
func newRouter(h app.Handlers) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(app.PathUpload, gin.WrapH(h.Upload))
	r.POST(app.PathTicket, gin.WrapH(h.Ticket))
	r.Any(app.PathUppy+"/*path", gin.WrapH(http.StripPrefix(app.PathUppy, h.Uppy)))
	r.Any(app.PathTus+"*path", gin.WrapH(http.StripPrefix(app.PathTus, h.Tus)))
	r.Any(app.PathFiles+"/*path", gin.WrapH(http.StripPrefix(app.PathFiles, h.Files)))
	r.Any(app.PathPublic+"/*path", gin.WrapH(http.StripPrefix(app.PathPublic, h.Public)))
	r.Any(app.PathRecords+"/*path", gin.WrapH(http.StripPrefix(app.PathRecords, h.Records)))
	return r
}
```
<!-- /ds:block hash=870af6 -->

The tus route is `/tus/*path` (`app.PathTus` is `/tus/`): gin's catch-all matches `/tus/` itself, which is where tus clients `POST` to create an upload, and the strip removes `/tus/` with its trailing slash, as tusd expects.

Check: start it and upload a file:

```sh
curl -s -F file=@report.txt localhost:8091/upload
```

```json
{"files":[{"key":"incoming/wnqgzckaxr76hjv6eu5wrtiu6s.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
```

## Step 2 — Records, signed links and ranges

The records endpoints and the signed-link handler answer exactly as on net/http:

```sh
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8091/media/owners/user/alice/docs
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-User: alice' localhost:8091/media/files/fil_55vn5w35fzi5r2np3unbe66srl
curl -s -i -H 'Range: bytes=0-8' "$LINK"
```

```
{"files":[{"attachment_id":"att_noecz7r4er2cryhqyjehctap5h","file_id":"fil_55vn5w35fzi5r2np3unbe66srl","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
302 http://localhost:8091/files/media/incoming/x6ol4j3iw2cdp6pgxwl5wev6xz.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791268015&kid=k1&m=GET&sig=e2df2a00e35d33ecb9c404be1ca7a031c66a2a5d2dd43db0af9259e252f33df2
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 9
Content-Range: bytes 0-8/18
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/plain; charset=utf-8
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
Last-Modified: Tue, 06 Oct 2026 06:16:55 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:16:55 GMT

quarterly
```

Check: `go test ./gin` passes. It runs the form upload (and a refused HTML file), the records 401, 403 and 201, the redirect and signed download, a public file, a ticket, a full Uppy upload against the fake S3 and a tus upload, all through gin.

## Step 3 — Write your own gin handler

Everything your app does with files beyond the shipped endpoints (a metadata route, a rename, an export) is a gin handler of your own that calls the `Disk` or the `Store` directly. The program has one at `GET /info/*key`, answering the metadata of one object of the media disk to a signed-in user:

<!-- ds:block id=fileinfo-zpspbcnp -->
**fileInfo** · [`examples/frameworks/gin/main.go:97-110`](../../examples/frameworks/gin/main.go#L97-L110)

```go
func fileInfo(disk *filesystem.Disk) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(app.HeaderUser) == "" {
			c.JSON(http.StatusUnauthorized, app.Problem{Error: app.ErrSignedOutMessage, Code: filesystem.CodePermission})
			return
		}
		entry, err := disk.Stat(c.Request.Context(), c.Param("key"))
		if err != nil {
			c.JSON(fshttp.StatusOf(err), app.ProblemOf(err))
			return
		}
		c.JSON(http.StatusOK, app.InfoOf(entry))
	}
}
```
<!-- /ds:block hash=58080f -->

It is mounted after the library's routes: `r.GET(app.PathInfo+"/*key", fileInfo(a.Disks.Media))`. What it gets right:

1. **`c.Request.Context()`**, so a client that disconnects cancels the backend call (and, on the records side, the actor set by `records.WithActor` travels with it).
2. **The key comes straight from `c.Param("key")`.** gin's catch-all value is already unescaped (`read%20me.txt` arrives as `read me.txt`) and keeps its leading slash, which the Disk's key cleaning drops. fiber needs an explicit `url.PathUnescape` here, and echo needs one only when it routed on the escaped path (see the echo guide); gin does not.
3. **No traversal check of its own.** The Disk cleans every key and refuses a `..` segment before any backend sees it; `%2E%2E%2F` in the URL reaches the handler as `/../` and is refused with `invalid_key`.
4. **Errors answer with the status of their code and `fshttp.ClientMessage`** (`app.ProblemOf`), never `err.Error()`, because a driver's message can name buckets, paths or other keys:

<!-- ds:block id=problemof-kjsuu6nd -->
**ProblemOf** · [`examples/frameworks/internal/app/info.go:43-45`](../../examples/frameworks/internal/app/info.go#L43-L45)

```go
func ProblemOf(err error) Problem {
	return Problem{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}
}
```
<!-- /ds:block hash=c4bbf7 -->

From a real run (`go run ./gin`, after uploading `report.txt` through `/upload`):

```text
$ curl -s -H 'X-User: alice' localhost:8091/info/incoming/74zjrajqrbc3rkoo5ar7hp6uex.txt
{"key":"incoming/74zjrajqrbc3rkoo5ar7hp6uex.txt","size":18,"content_type":"text/plain; charset=utf-8","modified":"2026-10-08T13:21:13.860277Z","etag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}
$ curl -s -i localhost:8091/info/incoming/74zjrajqrbc3rkoo5ar7hp6uex.txt
HTTP/1.1 401 Unauthorized
Content-Type: application/json; charset=utf-8
Content-Length: 45

{"error":"sign in first","code":"permission"}
$ curl -s -H 'X-User: alice' localhost:8091/info/incoming/none.txt
{"error":"Not Found","code":"not_found"}
$ curl -s -H 'X-User: alice' localhost:8091/info/%2E%2E%2Fsecret.txt
{"error":"key contains a \"..\" segment","code":"invalid_key"}
```

Check: `go test ./gin` sends these requests and more (a key with a space, one reached through an escaped slash, keys holding a literal `%20` and a `%`, a directory, an encoded `..` behind a real directory) through the cases shared by every framework program, `apptest.InfoCases` in [`internal/apptest/info.go`](../../examples/frameworks/internal/apptest/info.go).

If your authentication runs as gin middleware and stores the user in the gin context, copy it into the request context before the records handler runs, so `records.WithActor` and the `Scope` and `Authorize` hooks can see it: the hooks receive the `*http.Request`, not the `*gin.Context`. `fsgin` from `contrib/gin` does this once in middleware ([Framework adapters](framework-adapters.md)).

## Pitfalls

- **Wrong methods answer 404, not 405.** gin only answers 405 with `engine.HandleMethodNotAllowed = true`; by default `GET /upload` is gin's `404 page not found`. The library's own handlers answer 405 with an `Allow` header only for requests that reach them.
- **Do not put `c.Request.ParseMultipartForm`, `c.FormFile` or a body-reading logger in front of the upload routes.** They consume the body, and the library's handlers then find no file.
- **`gin.Default()` adds gin's logger and recovery**; the example uses `gin.New()` with `gin.Recovery()` only. Either works.
- **`gin.Recovery` swallows a failed zip's abort.** `fshttp.ServeZip` aborts a stream that fails after its first byte by panicking with `http.ErrAbortHandler`, so that net/http closes the connection and the client sees a failed download. `gin.Recovery` recovers that panic like any other, gin then ends the response cleanly, and the client keeps a truncated archive that looks complete. Do not call plain `fshttp.ServeZip` behind `gin.Recovery`; use `fsgin.Zip` from `contrib/gin`, which catches the abort itself and closes the connection ([framework adapters](framework-adapters.md)).
- **`c.Param` of a catch-all starts with `/`.** Passing it to the Disk is fine (key cleaning drops it), but code that compares it with a stored key, or builds a prefix from it, must trim it first.
- **`context.Background()` in a handler** keeps a slow backend call running after the client left and loses the actor the records hooks and the audit log read; use `c.Request.Context()`.
- **Trailing-slash redirects.** gin redirects `/media` to `/media/` when `RedirectTrailingSlash` is on (the default); clients should call the full routes (`/media/owners/…`) anyway.
- **One catch-all per prefix.** gin panics at registration when a catch-all conflicts with another route: registering `/uploads/*path` after `/uploads/s3/*path` stops with `catch-all wildcard '*path' in new path '/uploads/*path' conflicts with existing path segment 's3' in existing prefix '/uploads/s3'`. The example keeps the ticket route as an exact path (`/uploads/ticket`) and Uppy under its own catch-all.

## Next

- [Framework adapters](framework-adapters.md): `fsgin` for groups, the user and tenant in middleware, central error rendering and zips that abort correctly.
- [net/http, gorilla and chi](net-http.md): every handler and transcript in detail.
- [echo](echo.md), [fiber](fiber.md): the same handlers on other frameworks.
- [HTTP endpoints](../records/http-endpoints.md): every route of `recordshttp`.
