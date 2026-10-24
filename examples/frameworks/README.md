# Framework examples: chi, gin, echo and fiber, plain and with adapters

Every HTTP handler the library ships, mounted on four web frameworks: chi, gin, echo and fiber. The library's endpoints (form uploads, direct-upload tickets, Uppy multipart, tus resumable uploads, signed downloads, public files and the records endpoints) are plain `net/http` handlers, so the storage side is wired once in `internal/app` and each framework's program is only its routing. The programs under `adapters/` do the same through the adapter modules `contrib/gin` (`fsgin`), `contrib/echo` (`fsecho`), `contrib/fiber` (`fsfiber`) and `contrib/chi` (`fschi`): routes in groups with their prefix stripped, the signed-in user and the tenant resolved once in middleware, the library's errors rendered by the adapter, and an export route of the app's own that streams a zip with the adapter's `Zip`; their storage side is `internal/adapterapp`. This directory is a separate Go module (`github.com/ubgo/filesystem/examples/frameworks`) so the main examples module does not depend on four web frameworks. It uses `github.com/ubgo/filesystem`, `upload`, `fshttp`, `signedurl`, `contrib/tus`, `contrib/gin`, `contrib/echo`, `contrib/fiber`, `contrib/chi`, `records`, `records/recordshttp` and `adapters/s3`. The guides are [net/http, gorilla and chi](../../docs/integrations/net-http.md), [gin](../../docs/integrations/gin.md), [echo](../../docs/integrations/echo.md), [fiber](../../docs/integrations/fiber.md) and [framework adapters](../../docs/integrations/framework-adapters.md).

## Run it

Run every command from this directory, not from `examples/`:

```sh
cd examples/frameworks
go run ./chi     # localhost:8094
go run ./gin     # localhost:8091
go run ./echo    # localhost:8092
go run ./fiber   # localhost:8093
go run ./adapters/gin     # localhost:8191
go run ./adapters/echo    # localhost:8192
go run ./adapters/fiber   # localhost:8193
go run ./adapters/chi     # localhost:8194
go test ./...
```

Each program listens on its port until you press Ctrl+C. Everything runs in-process: two memory disks (`media` for private files behind signed links, `public` for public files), an S3 disk (`bucket`) on a fake S3 server (gofakes3) on a random loopback port, because tickets and Uppy need a backend that signs, and the records tables in an in-memory SQLite database. Nothing survives a restart, and the signing secret is random per start. tus keeps unfinished uploads in a `tus-sessions-*` directory under the system temp directory. Ctrl+C or SIGTERM stops the server cleanly: requests in flight get up to five seconds to finish, the fake S3 stops, the tus directory is removed, and the process exits 0. The `X-User` header stands in for the app's own authentication. The adapter programs need less: one memory disk (`media`) behind signed links and the records tables in in-memory SQLite, with every signed-in user in the one tenant `demo`. Each program's test serves its router on a free port and drives every route through `internal/apptest` (`Exercise` for the plain programs, `ExerciseAdapters` for the adapter ones).

## Walkthrough

The same requests work on every framework; only the port changes. With `report.txt` holding `quarterly numbers` and a newline, on chi:

```sh
curl -s -F file=@report.txt localhost:8094/upload
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8094/media/owners/user/alice/docs
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-User: alice' localhost:8094/media/files/fil_ind3q6v7be2zsjs66tapfts3lj
```

```text
{"files":[{"key":"incoming/umy67bxqmqtjnxwnhswys6uymr.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"files":[{"attachment_id":"att_3264r5eo7cpsuazjrdlqvfcf74","file_id":"fil_ind3q6v7be2zsjs66tapfts3lj","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
302 http://localhost:8094/files/media/incoming/agmxzphzlj5kh4buepccvggo7q.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791267696&kid=k1&m=GET&sig=e0e7ed497a5ac3a2a235bb1dcd8e03ec92755d1a95156ae0e7ab2a7072a98353
```

Where the frameworks differ is what happens before a request reaches the library. A wrong method on `/upload` (`curl -i -X DELETE localhost:<port>/upload`) is answered by the router, not the library:

| Framework | Answer to `DELETE /upload` |
|---|---|
| chi | `405 Method Not Allowed`, `Allow: POST`, empty body |
| gin | `404 page not found` (gin answers 405 only with `HandleMethodNotAllowed = true`) |
| echo | `405`, `Allow: OPTIONS, POST`, `{"message":"Method Not Allowed"}` |
| fiber | `405`, `Allow: POST`, `Method Not Allowed` |

Each framework's README has the full transcript for its port.

### With the adapter modules

The adapter programs answer the same requests on 8191 to 8194. With the same `report.txt`, on `adapters/gin`: a form upload, the records endpoints signed out (401), as alice (201, `uploaded_by` set by `fsgin.Actor`) and as bob on alice's owner (refused by the app's `Authorize`), alice's export zip, and an export of a role with no files, which `fsgin.Error` answers 404:

```sh
curl -s -F file=@report.txt localhost:8191/upload
curl -s -F file=@report.txt localhost:8191/me/media/owners/user/alice/docs
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8191/me/media/owners/user/alice/docs
curl -s -H 'X-User: bob' -F file=@report.txt localhost:8191/me/media/owners/user/alice/docs
curl -s -H 'X-User: alice' -o docs.zip -w '%{http_code} %{content_type}\n' localhost:8191/me/export/docs && unzip -l docs.zip
curl -s -i -H 'X-User: alice' localhost:8191/me/export/photos
```

```text
{"files":[{"key":"incoming/u5i36hhqahvi2zlszcguzckwyg.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"not signed in","code":"permission"}{"files":[{"attachment_id":"att_tj3xhtgra7djlnlyfufm2dke57","file_id":"fil_jkqtirniu2dl6uv7mclv4noifg","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
{"error":"forbidden","code":"permission"}
200 application/zip
Archive:  docs.zip
  Length      Date    Time    Name
---------  ---------- -----   ----
       18  10-06-2026 11:41   1-report.txt
---------                     -------
       18                     1 file
HTTP/1.1 404 Not Found
Content-Type: application/json; charset=utf-8
Date: Tue, 06 Oct 2026 06:11:39 GMT
Content-Length: 40

{"error":"Not Found","code":"not_found"}
```

The same requests on `adapters/echo`, `adapters/fiber` and `adapters/chi` give the same answers apart from IDs, with one difference in how the adapter writes an error: gin's and fiber's `Error` end the JSON without a newline and send `Content-Type: application/json; charset=utf-8` (so the 401 above runs into the next answer), while echo's and chi's end it with a newline and send `Content-Type: application/json`.

## The code

`Handlers` is one plain `http.Handler` per library endpoint; it is what every framework mounts.

<!-- ds:block id=handlers-4j27haag -->
**Handlers** · [`examples/frameworks/internal/app/app.go:108-116`](internal/app/app.go#L108-L116)

```go
type Handlers struct {
	Upload  http.Handler // POST multipart form uploads
	Ticket  http.Handler // POST a TicketRequest, get a presigned PUT
	Uppy    http.Handler // Uppy's AWS S3 multipart protocol
	Tus     http.Handler // tus resumable uploads
	Files   http.Handler // signed downloads and uploads
	Public  http.Handler // public files by key
	Records http.Handler // upload, attach, list, reorder, read, trash
}
```
<!-- /ds:block hash=b718bd -->

`New` wires the storage side once: the signer, the disks, the records store and the handlers. `baseURL` is where the server is reached, because signed links are built from it.

<!-- ds:block id=new-weem4qf2 -->
**New** · [`examples/frameworks/internal/app/app.go:122-140`](internal/app/app.go#L122-L140)

```go
func New(ctx context.Context, baseURL, s3URL, tusDir string) (*App, error) {
	signer, err := newSigner(baseURL + PathFiles)
	if err != nil {
		return nil, err
	}
	disks, err := newDisks(ctx, baseURL, s3URL, signer)
	if err != nil {
		return nil, err
	}
	store, err := newStore(ctx, disks.Manager)
	if err != nil {
		return nil, err
	}
	h, err := newHandlers(disks, store, signer, tusDir)
	if err != nil {
		return nil, err
	}
	return &App{Disks: disks, Store: store, Handlers: h}, nil
}
```
<!-- /ds:block hash=5d7b70 -->

`Exercise` is the shared test: it runs form upload, the records flow, public files, a ticket, a full Uppy upload and a tus upload through a real server, so each framework's test proves its routing hands the library the requests it expects.

<!-- ds:block id=exercise-u7ra8xnm -->
**Exercise** · [`examples/frameworks/internal/apptest/apptest.go:66-73`](internal/apptest/apptest.go#L66-L73)

```go
func Exercise(t *testing.T, base string, a *app.App) {
	t.Run("form upload", func(t *testing.T) { formUpload(t, base) })
	t.Run("records", func(t *testing.T) { recordsFlow(t, base) })
	t.Run("public", func(t *testing.T) { publicFile(t, a, base) })
	t.Run("ticket", func(t *testing.T) { ticket(t, base) })
	t.Run("uppy", func(t *testing.T) { uppy(t, base) })
	t.Run("tus", func(t *testing.T) { tusUpload(t, a, base) })
}
```
<!-- /ds:block hash=28053e -->

`ServeUntilDone` is how chi, gin and echo serve: until Ctrl+C or SIGTERM cancels the context from `signal.NotifyContext`, then a graceful `http.Server.Shutdown`, so each program's deferred clean-up runs. fiber has its own listener and stops through `ShutdownWithTimeout` instead.

<!-- ds:block id=serveuntildone-cgw5a9pa -->
**ServeUntilDone** · [`examples/frameworks/internal/app/app.go:318-329`](internal/app/app.go#L318-L329)

```go
func ServeUntilDone(ctx context.Context, srv *http.Server) error {
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
```
<!-- /ds:block hash=cde672 -->

The routing itself is one function per framework: [chi](chi), [gin](gin), [echo](echo), [fiber](fiber).

With an adapter, the routing also resolves identity once for a group. On gin, `fsgin.Actor` records the user for the records store and `fsgin.Scope` the tenant that recordshttp reads through `fsgin.ScopeOf`; the other three are in [adapters](adapters).

<!-- ds:block id=newrouter-d2ksmg3g -->
**newRouter** · [`examples/frameworks/adapters/gin/main.go:52-64`](adapters/gin/main.go#L52-L64)

```go
func newRouter(a *app.App) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST(app.PathUpload, gin.WrapH(a.Upload))
	fsgin.Mount(r, app.PathFiles, a.Files)
	me := r.Group(app.GroupMe,
		fsgin.Actor(func(c *gin.Context) (string, error) { return app.User(c.Request) }),
		fsgin.Scope(func(c *gin.Context) (string, error) { return app.Tenant(c.Request) }),
	)
	fsgin.Mount(me, app.MountMedia, a.Records)
	me.GET(app.PathExport+"/:"+app.ParamRole, func(c *gin.Context) { export(c, a) })
	return r
}
```
<!-- /ds:block hash=d79a82 -->

## Files

| File | What it holds |
|---|---|
| `go.mod` | The separate module, with `replace` directives back to the library's modules |
| `internal/app/app.go` | The shared wiring: policy, disks, signer, records store, handlers, auth hooks, fake S3, graceful serving |
| `internal/app/info.go` | The `/info` route's shared pieces: `PathInfo`, the `Info` and `Problem` bodies, `ProblemOf` (status text or the client-safe cause, never `err.Error()`) |
| `internal/apptest/apptest.go` | The shared test that drives every route of the plain programs over real HTTP |
| `internal/apptest/info.go` | `ExerciseInfo` and `InfoCases`, the cases every framework's own `fileInfo` handler (echo, gin, fiber) must answer alike |
| `internal/apptest/adapters.go` | `ExerciseAdapters`, the same for the adapter programs: upload, records with and without a user, signed download, export zip, empty export |
| `internal/adapterapp/adapterapp.go` | The adapter programs' storage side: one memory disk, signer, records store, handlers, the identity functions, the export list, graceful serving |
| `chi/` | chi routing, port 8094 |
| `gin/` | gin routing, port 8091 |
| `echo/` | echo routing, port 8092 |
| `fiber/` | fiber routing and body limit, port 8093 |
| `adapters/gin/`, `adapters/echo/`, `adapters/fiber/`, `adapters/chi/` | The same library on each framework through its adapter module, ports 8191 to 8194 |

## Next

- [net/http, gorilla and chi](../../docs/integrations/net-http.md): every handler and its transcript in detail.
- [Framework adapters](../../docs/integrations/framework-adapters.md): `Mount`, `Actor`, `Scope`, `Error` and `Zip` on each framework, with the adapter programs' output.
- [integrations-nethttp](../integrations-nethttp): the same handlers on `http.ServeMux` and gorilla/mux, in the main examples module.
- [HTTP endpoints](../../docs/records/http-endpoints.md): every route of `recordshttp`.
