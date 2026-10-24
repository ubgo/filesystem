# chi: every storage endpoint on a chi router

Every HTTP handler the library ships, mounted on a chi router: form uploads, direct-upload tickets, Uppy multipart, tus, signed links, public files and the records endpoints. chi takes `http.Handler` values as they are, so this program is only routing; the storage wiring is shared with the other framework examples in [`internal/app`](../internal/app/app.go). It uses `github.com/go-chi/chi/v5` with the library's `upload`, `fshttp`, `contrib/tus` and `records/recordshttp` handlers. The guide is the chi section of [net/http, gorilla and chi](../../../docs/integrations/net-http.md).

## Run it

From the framework module, not from `examples/`:

```sh
cd examples/frameworks
go run ./chi
go test ./chi
```

It listens on `http://localhost:8094` until you press Ctrl+C. The disks are in memory, the S3 bucket for tickets and Uppy is a fake S3 server (gofakes3) on a random loopback port, and the records tables are in an in-memory SQLite database, so nothing survives a restart. tus keeps unfinished uploads in a `tus-sessions-*` directory under the system temp directory. Ctrl+C or SIGTERM stops the server cleanly: requests in flight get up to five seconds to finish, the fake S3 stops, the tus directory is removed, and the process exits 0. The `X-User` header stands in for real authentication. The test serves the router on a free port and drives every route, including a full Uppy and tus upload.

## Walkthrough

`report.txt` holds `quarterly numbers` and a newline (18 bytes); `page.html` is a small HTML page.

1. A form upload is stored with a type read from the bytes; HTML is refused whatever its name says. A wrong method is chi's own 405:

```sh
curl -s -F file=@report.txt localhost:8094/upload
curl -s -w '%{http_code}\n' -F file=@page.html localhost:8094/upload
curl -s -i -X DELETE localhost:8094/upload
```

```text
{"files":[{"key":"incoming/n2447ysr2iumhhexkiobvgx34d.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
HTTP/1.1 405 Method Not Allowed
Allow: POST
Date: Tue, 06 Oct 2026 06:09:42 GMT
Content-Length: 0
```

2. Alice uploads and attaches a file through the records endpoints; the file route redirects to a signed link:

```sh
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8094/media/owners/user/alice/docs
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-User: alice' localhost:8094/media/files/fil_dislrn3q5rcj7ormno2zsnu2o6
```

```text
{"files":[{"attachment_id":"att_n75br5otwv7dklelwaajq7wkg3","file_id":"fil_dislrn3q5rcj7ormno2zsnu2o6","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
302 http://localhost:8094/files/media/incoming/bf7s2o4e6kzk5vfum3iahfp2pv.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791267583&kid=k1&m=GET&sig=3d2b13a7bbf79c8e15d3ed6ce4310410ef8f02f604373f4f1b0dd8b7d41e8b69
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
Last-Modified: Tue, 06 Oct 2026 06:09:43 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:43 GMT

quarterly
```

4. A tus upload: create, then one `PATCH`; the stored key comes back in `Filesystem-Key`:

```sh
curl -s -i -X POST localhost:8094/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf notes.txt | base64),filetype $(printf text/plain | base64)"
curl -s -i -X PATCH http://localhost:8094/tus/0c1bfcb6d0be3364159cf1611c4b0790 -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @report.txt
```

```text
HTTP/1.1 201 Created
Location: http://localhost:8094/tus/0c1bfcb6d0be3364159cf1611c4b0790
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:43 GMT
Content-Length: 0

HTTP/1.1 204 No Content
Filesystem-Key: incoming/m7y3hgq7lq3vkzubzeerratc27.txt
Tus-Resumable: 1.0.0
Upload-Offset: 18
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:09:43 GMT
```

Tickets and Uppy work the same way as in [integrations-nethttp](../../integrations-nethttp). Stop the server with Ctrl+C; it shuts down cleanly and removes its tus directory.

## The code

`newRouter` is the whole chi integration. chi's `Mount` does not rewrite `r.URL.Path` for a handler that is not a chi router, so each catch-all route strips its prefix with `http.StripPrefix`, exactly as on `http.ServeMux`; tus expects its prefix stripped with the trailing slash.

<!-- ds:block id=newrouter-2z3crjca -->
**newRouter** · [`examples/frameworks/chi/main.go:70-80`](main.go#L70-L80)

```go
func newRouter(h app.Handlers) chi.Router {
	r := chi.NewRouter()
	r.Method(http.MethodPost, app.PathUpload, h.Upload)
	r.Method(http.MethodPost, app.PathTicket, h.Ticket)
	r.Handle(app.PathUppy+"/*", http.StripPrefix(app.PathUppy, h.Uppy))
	r.Handle(app.PathTus+"*", http.StripPrefix(app.PathTus, h.Tus))
	r.Handle(app.PathFiles+"/*", http.StripPrefix(app.PathFiles, h.Files))
	r.Handle(app.PathPublic+"/*", http.StripPrefix(app.PathPublic, h.Public))
	r.Handle(app.PathRecords+"/*", http.StripPrefix(app.PathRecords, h.Records))
	return r
}
```
<!-- /ds:block hash=e192b1 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | `run` (fake S3, tus directory, `app.New`, the server under `app.ServeUntilDone`) and `newRouter` |
| `main_test.go` | Every route through a real chi server, via `internal/apptest` |

## Next

- [net/http, gorilla and chi](../../../docs/integrations/net-http.md): every handler and its transcript in detail.
- The same handlers on [gin](../gin), [echo](../echo) and [fiber](../fiber); the shared wiring is described in the [framework module README](..).
