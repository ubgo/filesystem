# echo: every storage endpoint on an echo server

Every HTTP handler the library ships, mounted on echo: form uploads, direct-upload tickets, Uppy multipart, tus, signed links, public files and the records endpoints. `echo.WrapHandler` adapts each `http.Handler`, so this program is only routing; the storage wiring is shared with the other framework examples in [`internal/app`](../internal/app/app.go). It uses `github.com/labstack/echo/v4` with the library's `upload`, `fshttp`, `contrib/tus` and `records/recordshttp` handlers. The guide is [echo](../../../docs/integrations/echo.md).

## Run it

From the framework module, not from `examples/`:

```sh
cd examples/frameworks
go run ./echo
go test ./echo
```

It listens on `http://localhost:8092` until you press Ctrl+C. The disks are in memory, the S3 bucket for tickets and Uppy is a fake S3 server (gofakes3) on a random loopback port, and the records tables are in an in-memory SQLite database, so nothing survives a restart. tus keeps unfinished uploads in a `tus-sessions-*` directory under the system temp directory. Ctrl+C or SIGTERM stops the server cleanly: requests in flight get up to five seconds to finish, the fake S3 stops, the tus directory is removed, and the process exits 0. The `X-User` header stands in for real authentication. The test serves the router on a free port and drives every route, including a full Uppy and tus upload.

## Walkthrough

`report.txt` holds `quarterly numbers` and a newline (18 bytes); `page.html` is a small HTML page.

1. A form upload is stored with a type read from the bytes; HTML is refused. A wrong method is echo's own JSON 405, not the library's error JSON:

```sh
curl -s -F file=@report.txt localhost:8092/upload
curl -s -w '%{http_code}\n' -F file=@page.html localhost:8092/upload
curl -s -i -X DELETE localhost:8092/upload
```

```text
{"files":[{"key":"incoming/dqirj3uvkfjeblvarejt7442qs.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
HTTP/1.1 405 Method Not Allowed
Allow: OPTIONS, POST
Content-Type: application/json
Date: Tue, 06 Oct 2026 06:09:39 GMT
Content-Length: 33

{"message":"Method Not Allowed"}
```

2. Alice uploads and attaches a file through the records endpoints; the file route redirects to a signed link:

```sh
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8092/media/owners/user/alice/docs
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-User: alice' localhost:8092/media/files/fil_ifqrfwvhiprl5high2v5xjddzh
```

```text
{"files":[{"attachment_id":"att_ewrn2wkdxbnt4ojn3xifk6mf7l","file_id":"fil_ifqrfwvhiprl5high2v5xjddzh","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
302 http://localhost:8092/files/media/incoming/72vhegpyz56j7gpahq4rgldtvg.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791267579&kid=k1&m=GET&sig=880da25b312603d40be9664e266ae3e423eaee06656e265ed5f74a8f120efe0d
```

3. The signed link serves ranges, with the headers that stop user content from running script:

```sh
curl -s -i -H 'Range: bytes=0-8' "$LINK"
```

```text
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 9
Content-Range: bytes 0-8/18
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/plain; charset=utf-8
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
Last-Modified: Tue, 06 Oct 2026 06:09:39 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:39 GMT

quarterly
```

4. A tus upload: create, then one `PATCH`; the stored key comes back in `Filesystem-Key`:

```sh
curl -s -i -X POST localhost:8092/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf notes.txt | base64),filetype $(printf text/plain | base64)"
curl -s -i -X PATCH http://localhost:8092/tus/8e3c0f458c37335cad2fa9dd358e87e9 -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @report.txt
```

```text
HTTP/1.1 201 Created
Location: http://localhost:8092/tus/8e3c0f458c37335cad2fa9dd358e87e9
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:39 GMT
Content-Length: 0

HTTP/1.1 204 No Content
Filesystem-Key: incoming/gaaxkm7aomy4om5zjcghrml7mw.txt
Tus-Resumable: 1.0.0
Upload-Offset: 18
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:39 GMT
```

Tickets and Uppy work the same way as in [integrations-nethttp](../../integrations-nethttp). The routes are `/media/*` and so on: echo does not redirect `/media` without a slash, so clients call the full routes. Stop the server with Ctrl+C; it shuts down cleanly and removes its tus directory.

## The code

`newRouter` is the whole echo integration. `echo.WrapHandler` adapts each handler; each catch-all route strips its prefix with `http.StripPrefix`, because the handlers route on the path below their mount point. `Any` registers every method, `PATCH` included for tus.

<!-- ds:block id=newrouter-kbd8hm3u -->
**newRouter** · [`examples/frameworks/echo/main.go:75-86`](main.go#L75-L86)

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

`fileInfo` is a handler of the application's own, mounted at `GET /info/*` next to the library's routes: it calls `Disk.Stat` directly for a signed-in user, with the request's context, and answers errors with the status of their code and `fshttp.ClientMessage` (via `app.ProblemOf`), never `err.Error()`. echo decodes the wildcard itself unless it routed on the escaped path (`URL.RawPath`, set when the URL holds an escape such as `%2F`), so the handler unescapes it with `url.PathUnescape` only in that case: unescaping a value echo already decoded would turn a literal `%20` in a key into a space. The Disk refuses `..` itself.

<!-- ds:block id=fileinfo-6u4e3d28 -->
**fileInfo** · [`examples/frameworks/echo/main.go:96-119`](main.go#L96-L119)

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

```sh
curl -s -H 'X-User: alice' localhost:8092/info/incoming/<key from an upload>.txt
curl -s -H 'X-User: alice' localhost:8092/info/%2E%2E%2Fsecret.txt
```

The first answers the key, size, content type, modification time and ETag as JSON; the second `{"error":"key contains a \"..\" segment","code":"invalid_key"}` with status 400. The test runs the shared cases of `apptest.InfoCases` (an object, a signed-out caller, a missing key, a key with a space, one reached through an escaped slash, keys holding a literal `%20` and a `%`, a directory, two encoded `..` keys) through the real server.

## Files

| File | What it holds |
|---|---|
| `main.go` | `run` (fake S3, tus directory, `app.New`, the server under `app.ServeUntilDone`), `newRouter` and `fileInfo` |
| `main_test.go` | Every route through a real echo server, plus the info route, via `internal/apptest` |

## Next

- [echo](../../../docs/integrations/echo.md): the guide for this program.
- [net/http, gorilla and chi](../../../docs/integrations/net-http.md): every handler and its transcript in detail.
- The same handlers on [chi](../chi), [gin](../gin) and [fiber](../fiber); the shared wiring is described in the [framework module README](..).
