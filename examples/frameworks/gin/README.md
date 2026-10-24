# gin: every storage endpoint on a gin engine

Every HTTP handler the library ships, mounted on a gin engine: form uploads, direct-upload tickets, Uppy multipart, tus, signed links, public files and the records endpoints. `gin.WrapH` adapts each `http.Handler`, so this program is only routing; the storage wiring is shared with the other framework examples in [`internal/app`](../internal/app/app.go). It uses `github.com/gin-gonic/gin` with the library's `upload`, `fshttp`, `contrib/tus` and `records/recordshttp` handlers. The guide is [gin](../../../docs/integrations/gin.md).

## Run it

From the framework module, not from `examples/`:

```sh
cd examples/frameworks
go run ./gin
go test ./gin
```

It listens on `http://localhost:8091` until you press Ctrl+C. gin starts in debug mode and prints its route table, one line per method for every catch-all route; set `GIN_MODE=release` to silence it. The disks are in memory, the S3 bucket for tickets and Uppy is a fake S3 server (gofakes3) on a random loopback port, and the records tables are in an in-memory SQLite database, so nothing survives a restart. tus keeps unfinished uploads in a `tus-sessions-*` directory under the system temp directory. Ctrl+C or SIGTERM stops the server cleanly: requests in flight get up to five seconds to finish, the fake S3 stops, the tus directory is removed, and the process exits 0. The `X-User` header stands in for real authentication. The test serves the engine on a free port and drives every route, including a full Uppy and tus upload.

## Walkthrough

`report.txt` holds `quarterly numbers` and a newline (18 bytes); `page.html` is a small HTML page.

1. A form upload is stored with a type read from the bytes; HTML is refused. A wrong method is gin's 404, not a 405, because gin answers 405 only with `HandleMethodNotAllowed = true`:

```sh
curl -s -F file=@report.txt localhost:8091/upload
curl -s -w '%{http_code}\n' -F file=@page.html localhost:8091/upload
curl -s -i -X DELETE localhost:8091/upload
```

```text
{"files":[{"key":"incoming/jsbatpgo3xppnu6qop5vobex3y.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
HTTP/1.1 404 Not Found
Content-Type: text/plain
Date: Tue, 06 Oct 2026 06:09:24 GMT
Content-Length: 18

404 page not found
```

2. Alice uploads and attaches a file through the records endpoints; the file route redirects to a signed link:

```sh
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8091/media/owners/user/alice/docs
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-User: alice' localhost:8091/media/files/fil_7jpvyvmbgz6v5lz2a4ulf2uuz5
```

```text
{"files":[{"attachment_id":"att_ccjeulbutlczd7is7z555bz7mz","file_id":"fil_7jpvyvmbgz6v5lz2a4ulf2uuz5","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
302 http://localhost:8091/files/media/incoming/yj242snjowld626mku3plbl6am.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791267564&kid=k1&m=GET&sig=a04507079c4756eb28a6cbc8b422d9b86359b14a610f99bb9d1a045f24b3b076
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
Last-Modified: Tue, 06 Oct 2026 06:09:24 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:24 GMT

quarterly
```

4. A tus upload: create, then one `PATCH`; the stored key comes back in `Filesystem-Key`:

```sh
curl -s -i -X POST localhost:8091/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf notes.txt | base64),filetype $(printf text/plain | base64)"
curl -s -i -X PATCH http://localhost:8091/tus/a68dc11ef92c9e8ccaddf3c8ae25f6dd -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @report.txt
```

```text
HTTP/1.1 201 Created
Location: http://localhost:8091/tus/a68dc11ef92c9e8ccaddf3c8ae25f6dd
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:24 GMT
Content-Length: 0

HTTP/1.1 204 No Content
Filesystem-Key: incoming/cvidup3c3fopckzzxz5rfgncmv.txt
Tus-Resumable: 1.0.0
Upload-Offset: 18
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:24 GMT
```

Tickets and Uppy work the same way as in [integrations-nethttp](../../integrations-nethttp). Stop the server with Ctrl+C; it shuts down cleanly and removes its tus directory.

## The code

`newRouter` is the whole gin integration. `gin.WrapH` adapts each handler; the handlers route on the path below their mount point, so each catch-all route strips its prefix with `http.StripPrefix`. `Any` registers every method, which tus (`PATCH`, `HEAD`, `DELETE`, `OPTIONS`) and the signed-link handler (`GET`, `HEAD`, `PUT`) need.

<!-- ds:block id=newrouter-h32462p2 -->
**newRouter** · [`examples/frameworks/gin/main.go:75-86`](main.go#L75-L86)

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

`fileInfo` is a handler of the application's own, mounted at `GET /info/*` next to the library's routes: it calls `Disk.Stat` directly for a signed-in user, with the request's context, and answers errors with the status of their code and `fshttp.ClientMessage` (via `app.ProblemOf`), never `err.Error()`. gin hands the catch-all over already unescaped, with a leading slash the Disk drops; the Disk refuses `..` itself.

<!-- ds:block id=fileinfo-zpspbcnp -->
**fileInfo** · [`examples/frameworks/gin/main.go:97-110`](main.go#L97-L110)

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

```sh
curl -s -H 'X-User: alice' localhost:8091/info/incoming/<key from an upload>.txt
curl -s -H 'X-User: alice' localhost:8091/info/%2E%2E%2Fsecret.txt
```

The first answers the key, size, content type, modification time and ETag as JSON; the second `{"error":"key contains a \"..\" segment","code":"invalid_key"}` with status 400. The test runs the shared cases of `apptest.InfoCases` (an object, a signed-out caller, a missing key, a key with a space, one reached through an escaped slash, keys holding a literal `%20` and a `%`, a directory, two encoded `..` keys) through the real server.

## Files

| File | What it holds |
|---|---|
| `main.go` | `run` (fake S3, tus directory, `app.New`, the server under `app.ServeUntilDone`), `newRouter` and `fileInfo` |
| `main_test.go` | Every route through a real gin server, plus the info route, via `internal/apptest` |

## Next

- [gin](../../../docs/integrations/gin.md): the guide for this program.
- [net/http, gorilla and chi](../../../docs/integrations/net-http.md): every handler and its transcript in detail.
- The same handlers on [chi](../chi), [echo](../echo) and [fiber](../fiber); the shared wiring is described in the [framework module README](..).
