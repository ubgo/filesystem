# echo

You will have every HTTP endpoint the library ships served by an [echo](https://github.com/labstack/echo) v4 server: form uploads, direct-upload tickets, Uppy multipart, tus resumable uploads, signed links, public files and the records endpoints. The library's handlers are plain `http.Handler` values, so echo mounts them with `echo.WrapHandler`; nothing about the storage side changes.

## Before you start

- [net/http, gorilla and chi](net-http.md) explains each handler, the policy, the signer and the records hooks. This page changes only the router.
- The program is [`examples/frameworks/echo`](../../examples/frameworks/echo/main.go), in the separate module `examples/frameworks`. From that directory: `go run ./echo` serves it on `http://localhost:8092`; `go test ./echo` drives every route through a real echo server.
- The storage wiring is shared with the other framework programs: [`examples/frameworks/internal/app`](../../examples/frameworks/internal/app/app.go).
- This page mounts the handlers with nothing but echo. The adapter module `github.com/ubgo/filesystem/contrib/echo` (`fsecho`) adds what is left to you here: mounting under groups with the prefix stripped, the signed-in user and the tenant resolved once in middleware, an error handler that renders the library's errors so handlers can `return err`, and zips with `fsecho.Zip`. [Framework adapters](framework-adapters.md) shows it on the same routes, run from [`examples/frameworks/adapters/echo`](../../examples/frameworks/adapters/echo/main.go).

## Step 1 — Mount the handlers

`echo.WrapHandler` turns an `http.Handler` into an `echo.HandlerFunc`. Each catch-all route (`/*`) is wrapped in `http.StripPrefix`, because the library's handlers route on the path below their mount point. `Any` registers every method, `PATCH` included for tus:

<!-- ds:block id=newrouter-kbd8hm3u -->
**newRouter** · [`examples/frameworks/echo/main.go:75-86`](../../examples/frameworks/echo/main.go#L75-L86)

```go
func newRouter(h app.Handlers) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.POST(app.PathUpload, echo.WrapHandler(h.Upload))
	e.POST(app.PathTicket, echo.WrapHandler(h.Ticket))
	e.Any(app.PathUppy+"/*", echo.WrapHandler(http.StripPrefix(app.PathUppy, h.Uppy)))
	e.Any(app.PathTus+"*", echo.WrapHandler(http.StripPrefix(app.PathTus, h.Tus)))
	e.Any(app.PathFiles+"/*", echo.WrapHandler(http.StripPrefix(app.PathFiles, h.Files)))
	e.Any(app.PathPublic+"/*", echo.WrapHandler(http.StripPrefix(app.PathPublic, h.Public)))
	e.Any(app.PathRecords+"/*", echo.WrapHandler(http.StripPrefix(app.PathRecords, h.Records)))
	return e
}
```
<!-- /ds:block hash=4f8d94 -->

`app.PathTus` is `/tus/`, so the tus route is `/tus/*`, which matches `/tus/` itself (where clients create uploads) and every upload URL below it.

Check: start it and upload a file:

```sh
curl -s -F file=@report.txt localhost:8092/upload
```

```json
{"files":[{"key":"incoming/t4bybslcucperzh5t5kb2tiprp.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
```

A signed download through echo keeps ranges and the security headers:

```sh
curl -s -i -H 'Range: bytes=0-8' "$LINK"
```

```
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 9
Content-Range: bytes 0-8/18
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/plain; charset=utf-8
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
Last-Modified: Tue, 06 Oct 2026 06:16:45 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:16:45 GMT

quarterly
```

Check: `go test ./echo` passes; it runs the form upload and a refused HTML file, the records 401, 403 and 201, the redirect and signed download, a public file, a ticket, a full Uppy upload and a tus upload, all through echo.

## Step 2 — Write your own echo handler

The library's handlers cover uploads and downloads; everything else your app does with files (a metadata endpoint, a "rename" button, an export) is an echo handler of your own that calls the `Disk` or the `Store` directly. The program has one, mounted at `GET /info/*`, that answers the metadata of one object of the media disk to a signed-in user:

<!-- ds:block id=fileinfo-6u4e3d28 -->
**fileInfo** · [`examples/frameworks/echo/main.go:96-119`](../../examples/frameworks/echo/main.go#L96-L119)

```go
func fileInfo(disk *filesystem.Disk) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(app.HeaderUser) == "" {
			return c.JSON(http.StatusUnauthorized, app.Problem{Error: app.ErrSignedOutMessage, Code: filesystem.CodePermission})
		}
		// echo routes on the escaped path (URL.RawPath) only when it holds
		// an escape Go would not write itself, such as %2F; only then is
		// the parameter still percent-encoded. Otherwise it is already
		// decoded, and unescaping it again would turn a literal "%20" in a
		// key into a space.
		key := c.Param("*")
		if c.Request().URL.RawPath != "" {
			var err error
			if key, err = url.PathUnescape(key); err != nil {
				return c.JSON(http.StatusBadRequest, app.Problem{Error: "malformed path", Code: filesystem.CodeInvalidKey})
			}
		}
		entry, err := disk.Stat(c.Request().Context(), key)
		if err != nil {
			return c.JSON(fshttp.StatusOf(err), app.ProblemOf(err))
		}
		return c.JSON(http.StatusOK, app.InfoOf(entry))
	}
}
```
<!-- /ds:block hash=9b55d5 -->

Mount it next to the library's routes: `e.GET(app.PathInfo+"/*", fileInfo(a.Disks.Media))`. Four rules make it safe, and each is tested:

1. **Use the request's context**, `c.Request().Context()`, so a client that disconnects cancels the backend call.
2. **Decode the wildcard exactly once.** echo routes on the request's escaped path (`URL.RawPath`) when it holds an escape Go would not write itself, such as `%2F`, and on the decoded path otherwise. So `c.Param("*")` arrives decoded for an ordinary URL (`notes/read%20me.txt` gives `notes/read me.txt`) and still percent-encoded for one with `%2F`. Unescape only in the second case (`c.Request().URL.RawPath != ""`): unescaping a value echo already decoded turns a literal `%20` in a key into a space, and makes a key with a `%` answer 400. Library handlers mounted with `fsecho.Mount` never see this, because Mount strips its prefix from the decoded path.
3. **Let the Disk judge the key.** It cleans the key and refuses `..` before any backend sees it, so the handler does not need its own traversal check; a `%2E%2E%2F` in the URL becomes `../` after unescaping and is refused with `invalid_key`.
4. **Answer errors with the status of their code and `fshttp.ClientMessage`**, never `err.Error()`, which names the disk and the key. `app.ProblemOf` does both:

<!-- ds:block id=problemof-kjsuu6nd -->
**ProblemOf** · [`examples/frameworks/internal/app/info.go:43-45`](../../examples/frameworks/internal/app/info.go#L43-L45)

```go
func ProblemOf(err error) Problem {
	return Problem{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)}
}
```
<!-- /ds:block hash=c4bbf7 -->

From a real run (`go run ./echo`, after uploading `report.txt` through `/upload`):

```text
$ curl -s -H 'X-User: alice' localhost:8092/info/incoming/pnraeb74w5gcphnoafdiwbf4pa.txt
{"key":"incoming/pnraeb74w5gcphnoafdiwbf4pa.txt","size":18,"content_type":"text/plain; charset=utf-8","modified":"2026-10-08T13:21:09.190966Z","etag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}
$ curl -s -i localhost:8092/info/incoming/pnraeb74w5gcphnoafdiwbf4pa.txt
HTTP/1.1 401 Unauthorized
Content-Type: application/json
Content-Length: 46

{"error":"sign in first","code":"permission"}
$ curl -s -H 'X-User: alice' localhost:8092/info/incoming/none.txt
{"error":"Not Found","code":"not_found"}
$ curl -s -H 'X-User: alice' localhost:8092/info/%2E%2E%2Fsecret.txt
{"error":"key contains a \"..\" segment","code":"invalid_key"}
```

The not-found body is the status text, not the backend's message; the traversal body is the library's own client-safe cause, which never contains the key.

Check: `go test ./echo` runs the same requests, plus the escaped key, the same key reached through an escaped slash (the case where echo leaves the parameter encoded), keys holding a literal `%20` and a `%` (which catch a second decode), a directory and an encoded `..` behind a real directory name, through the shared cases every framework program must pass:

<!-- ds:block id=infocases-9kt9dpsf -->
**InfoCases** · [`examples/frameworks/internal/apptest/info.go:44-55`](../../examples/frameworks/internal/apptest/info.go#L44-L55)

```go
var InfoCases = []InfoCase{
	{"object", "/" + InfoKey, "alice", http.StatusOK, `"size":18`},
	{"escaped key", "/notes/read%20me.txt", "alice", http.StatusOK, `"key":"notes/read me.txt"`},
	{"escaped slash", "/notes%2Fread%20me.txt", "alice", http.StatusOK, `"key":"notes/read me.txt"`},
	{"literal %20 in key", "/notes/50%2520off.txt", "alice", http.StatusOK, `"key":"notes/50%20off.txt"`},
	{"literal % in key", "/notes/100%25.txt", "alice", http.StatusOK, `"key":"notes/100%.txt"`},
	{"signed out", "/" + InfoKey, "", http.StatusUnauthorized, `"code":"permission"`},
	{"missing", "/reports/none.txt", "alice", http.StatusNotFound, `{"error":"Not Found","code":"not_found"}`},
	{"directory", "/reports", "alice", http.StatusNotFound, `"code":"not_found"`},
	{"encoded traversal", "/%2E%2E%2Fsecret.txt", "alice", http.StatusBadRequest, `"code":"invalid_key"`},
	{"encoded slash traversal", "/reports%2F..%2F..%2Fsecret.txt", "alice", http.StatusBadRequest, `"code":"invalid_key"`},
}
```
<!-- /ds:block hash=e4c86c -->

## Step 3 — Pass the signed-in user to the library

An echo `HTTPErrorHandler` that maps errors centrally can do what `fileInfo` does inline: `c.JSON(fshttp.StatusOf(err), app.ProblemOf(err))`, so handlers just `return err`.

If your authentication middleware stores the user with `c.Set`, copy it into the request context (`c.SetRequest(c.Request().WithContext(records.WithActor(ctx, user)))`) before the records handler runs: the records `Scope` and `Authorize` hooks receive the `*http.Request`, not the `echo.Context`, and the audit log records the actor from the context. The adapter module `fsecho` does exactly this once in middleware ([Framework adapters](framework-adapters.md)).

## Pitfalls

- **echo answers wrong methods itself.** `GET /upload` gets echo's JSON 405 with `Allow: OPTIONS, POST`, not the library's error JSON:

  ```
  HTTP/1.1 405 Method Not Allowed
  Allow: OPTIONS, POST
  Content-Type: application/json
  Date: Sat, 03 Oct 2026 06:16:45 GMT
  Content-Length: 33

  {"message":"Method Not Allowed"}
  ```

- **`/media` without a slash is 404.** The route is `/media/*`; echo does not redirect, so clients call the full routes (`/media/owners/…`).
- **Body-reading middleware** (`middleware.BodyDump`, a logger that reads the body, `c.FormFile` in front of the route) consumes the upload before the library sees it. Skip it on the upload routes.
- **`middleware.BodyLimit`** answers 413 itself before the library's handler runs. If you use it, set it above the upload policy's `MaxSize` times the files per request, or leave the limit to the library, which counts the bytes as they stream.
- **`middleware.Gzip`** on the `/files/` and `/public/` subtrees breaks range requests; skip it there.
- **Path parameters are decoded or not depending on the URL.** `c.Param` is decoded when echo routed on the decoded path and percent-encoded when it routed on `URL.RawPath` (a path holding `%2F` or another escape Go would not write). A handler of your own that takes a key from the path must decode it exactly once (Step 2): always unescaping breaks keys holding `%`, never unescaping breaks keys reached through an escaped slash. Routes mounted with `fsecho.Mount` always get the decoded key.
- **`c.Request().Context()`, not `context.Background()`.** A background context keeps a slow backend call running after the client left, and loses the actor the records hooks and the audit log read.

## Next

- [Framework adapters](framework-adapters.md): `fsecho` for groups, the user and tenant in middleware, central error rendering and zips.
- [net/http, gorilla and chi](net-http.md): every handler and transcript in detail.
- [gin](gin.md), [fiber](fiber.md): the same handlers on other frameworks.
- [Errors](../reference/errors.md): the status every error code answers with.
