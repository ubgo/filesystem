# Framework adapters: gin, echo, fiber and chi

You will have the library's endpoints mounted on your web framework with its own idioms: handlers under route groups with their prefix stripped, the signed-in user and the tenant resolved once in middleware and handed to the records store, the library's errors rendered without leaking a backend's message, and zip downloads that fail loudly when they fail. Each framework has a small module: `contrib/gin` (package `fsgin`), `contrib/echo` (`fsecho`), `contrib/fiber` (`fsfiber`, fiber v3) and `contrib/chi` (`fschi`, chi v5).

## Before you start

- The library's handlers are plain `net/http` handlers, and [gin](gin.md), [echo](echo.md), [fiber](fiber.md) and [net/http and chi](net-http.md) show them mounted with nothing but the framework. The adapters add what those pages leave to you: stripping a group's prefix, carrying identity into the handlers, rendering errors, and streaming zips safely.
- The programs are in [`examples/frameworks/adapters`](../../examples/frameworks/adapters), in the `examples/frameworks` module beside the same frameworks without adapters: `adapters/gin/` (port 8191), `adapters/echo/` (8192), `adapters/fiber/` (8193) and `adapters/chi/` (8194). From `examples/frameworks`: `go run ./adapters/gin`, and `go test ./adapters/...` walks every route of all four through a real server.
- The storage side they share is [`examples/frameworks/internal/adapterapp`](../../examples/frameworks/internal/adapterapp/adapterapp.go): one memory disk behind signed links, the records tables in in-memory SQLite, `upload.Handler`, `fshttp.Signed` and `recordshttp.Handler`. The `X-User` header stands in for your session.
- Install the one you use:

```sh
go get github.com/ubgo/filesystem/contrib/gin    # or contrib/echo, contrib/fiber, contrib/chi
```

## What each adapter gives you

The four packages have the same shape, so moving between frameworks changes only the types:

| Name | What it does |
|---|---|
| `Mount(r, prefix, h)` | Serves `h` for every method at every path below `prefix` on `r` (the engine or any group), with the full matched prefix stripped, group included. A prefix ending in `/` is stripped with its slash, which is what tus expects (`/tus/`); without one the handler sees a leading `/`, which is what every other handler expects (`/media` hands recordshttp `/owners/…`). |
| `Actor(fn)` | Middleware: resolves the request's actor (a user ID, `api-key:…`) and records it with `records.WithActor`, so `File.UploadedBy`, the audit log and the `Authorize` hook see it. `""` with no error lets an anonymous request through; an error answers 401. |
| `Scope(fn)` | Middleware: resolves the tenant and stores it on the request with `recordshttp.WithScope`, the one scope key every adapter and `recordshttp.ScopeFromRequest` share. An error or `""` answers 401 before any handler runs. |
| `ScopeOf(r *http.Request)` | Has the signature of `recordshttp.Config.Scope`: returns the tenant `Scope` stored, or `ErrNoScope` (401) for a route mounted outside the middleware, so a forgotten middleware refuses instead of running in no tenant. `recordshttp.ScopeFromRequest` reads the same key and works as `Config.Scope` too; `ScopeOf` additionally refuses the empty scope. |
| `Error(…, err)` | Answers err as the library's handlers do: `fshttp.StatusOf` for the status (413 for `http.MaxBytesReader`'s cap), a JSON `{"error","code"}` whose text is `fshttp.ClientMessage`, and `Retry-After` when the backend asked for a wait. Never `err.Error()`, which names disks and keys. |
| `Zip(…, disk, filename, files, opts)` | Streams a zip through `fshttp.ServeZip`; a failure after the first byte reaches the client as a failed download, never as a truncated archive that looks complete. |
| `ErrorHandler(next)` | echo and fiber only: an error handler that renders the library's errors with `Error` and passes every other error to `next`, so handlers can `return err`. |
| `Handler(h)`, `ScopeFrom(c)` | fiber only: adapt a library handler with the identity carried across (use it instead of `adaptor.HTTPHandler`), and read the scope from a fiber handler. |

## Step 1 — Route the library

On gin, the exact upload route stays `gin.WrapH`; everything under a prefix goes through `fsgin.Mount`, on the engine or on a group. The `/me` group resolves the user and the tenant once, and the records endpoints and the app's own export route sit inside it:

<!-- ds:block id=newrouter-d2ksmg3g -->
**newRouter** · [`examples/frameworks/adapters/gin/main.go:52-64`](../../examples/frameworks/adapters/gin/main.go#L52-L64)

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

`fsgin.ScopeOf` is what `app.New` gives `recordshttp.Config.Scope`, so the records endpoints act in the tenant the middleware resolved.

Check: start it and walk the routes. The second request has no user, so `fsgin.Scope` refuses it; bob may not attach files to alice:

```sh
cd examples/frameworks && go run ./adapters/gin
printf 'quarterly numbers\n' > report.txt
curl -s -F file=@report.txt localhost:8191/upload
curl -s -F file=@report.txt localhost:8191/me/media/owners/user/alice/docs
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8191/me/media/owners/user/alice/docs
curl -s -H 'X-User: bob' -F file=@report.txt localhost:8191/me/media/owners/user/alice/docs
```

```text
{"files":[{"key":"incoming/kcblckqtljwsmrrweujyx4x6wo.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"not signed in","code":"permission"}
{"files":[{"attachment_id":"att_zdwen6dd37eqvuebqfjcsow23u","file_id":"fil_uf2isyedzglufjl3x3me7lga22","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
{"error":"forbidden","code":"permission"}
```

`"uploaded_by":"alice"` is `fsgin.Actor` at work: the store recorded the actor the middleware put on the request.

## Step 2 — Your own handlers: errors and zips

The export route is the application's own handler. It reads the tenant with `ScopeOf`, asks the store for the user's files (the actor is in the request's context), and streams them with `Zip`; any failure goes through `Error`:

<!-- ds:block id=export-d258zs7b -->
**export** · [`examples/frameworks/adapters/gin/main.go:68-81`](../../examples/frameworks/adapters/gin/main.go#L68-L81)

```go
func export(c *gin.Context, a *app.App) {
	scope, err := fsgin.ScopeOf(c.Request)
	if err != nil {
		fsgin.Error(c, err)
		return
	}
	role := c.Param(app.ParamRole)
	files, err := a.Export(c.Request.Context(), scope, role)
	if err != nil {
		fsgin.Error(c, err)
		return
	}
	fsgin.Zip(c, a.Media, app.ExportName(role), files, fshttp.ZipOptions{})
}
```
<!-- /ds:block hash=221db9 -->

The list comes from the records store; a role without files is `CodeNotFound`, which `Error` answers 404:

<!-- ds:block id=app-export-2axc7z6k -->
**App.Export** · [`examples/frameworks/internal/adapterapp/adapterapp.go:185-198`](../../examples/frameworks/internal/adapterapp/adapterapp.go#L185-L198)

```go
func (a *App) Export(ctx context.Context, scope, role string) ([]fshttp.ZipFile, error) {
	attached, err := a.Store.Attachments(ctx, scope, records.Owner{Type: ownerUser, ID: a.Store.Actor(ctx)}, role)
	if err != nil {
		return nil, err
	}
	if len(attached) == 0 {
		return nil, filesystem.Errorf(filesystem.CodeNotFound, "no %s files", role)
	}
	files := make([]fshttp.ZipFile, 0, len(attached))
	for _, af := range attached {
		files = append(files, fshttp.ZipFile{Key: af.File.Key, Name: fmt.Sprintf("%d-%s", af.Attachment.Position, upload.SafeFilename(af.File.Filename))})
	}
	return files, nil
}
```
<!-- /ds:block hash=af36c9 -->

Check:

```sh
curl -s -H 'X-User: alice' -o docs.zip -w '%{http_code} %{content_type}\n' localhost:8191/me/export/docs && unzip -l docs.zip
curl -s -i -H 'X-User: alice' localhost:8191/me/export/photos
```

```text
200 application/zip
Archive:  docs.zip
  Length      Date    Time    Name
---------  ---------- -----   ----
       18  10-06-2026 11:21   1-report.txt
---------                     -------
       18                     1 file
HTTP/1.1 404 Not Found
Content-Type: application/json; charset=utf-8
Date: Tue, 06 Oct 2026 05:51:38 GMT
Content-Length: 40

{"error":"Not Found","code":"not_found"}
```

The 404 says `Not Found`, not the store's `no photos files`: only the codes that describe the client's own input (too large, rejected, quota, invalid argument or key) show their message.

### Your own error envelope

`Actor`, `Scope`, `Error` (and `ErrorHandler` on echo and fiber) answer with the JSON `upload.ErrorResponse`. An API with its own shape (RFC 9457 problem+json, a `{data, errors}` envelope) uses a `Responder` instead: the same functions as methods, answering every error through an `fshttp.ErrorWriter`, the type every handler of the library takes. The zero `Responder` is the package-level functions, so nothing else changes:

```go
x := fsgin.Responder{Errors: problem} // problem: func(w, r, status, err), see Errors
me := r.Group("/me", x.Actor(app.User), x.Scope(app.Tenant))
me.GET("/export/:role", func(c *gin.Context) {
	scope, err := fsgin.ScopeOf(c.Request)
	if err != nil {
		x.Error(c, err)
		return
	}
	files, err := a.Export(c.Request.Context(), scope, c.Param("role"))
	if err != nil {
		x.Error(c, err) // problem+json, with Retry-After when the backend asked for a wait
		return
	}
	fsgin.Zip(c, a.Media, "export.zip", files, fshttp.ZipOptions{Errors: problem})
})
```

The 401 of `Actor` and `Scope` reaches the writer as `ErrUnauthenticated`, or as the resolver's own error with the code `permission`, for logs. On fiber the writer runs through fiber's adaptor on the request converted to `*http.Request`. Give the same writer to the handlers you mount (`upload.HandlerOptions.Errors`, `fshttp.SignedOptions.Errors`, …) so every storage error has one shape; see [Your own error envelope](../concepts/errors.md#your-own-error-envelope).

## Step 3 — The same app on echo, fiber and chi

echo handlers return errors, so the echo program installs `fsecho.ErrorHandler` once and its export handler returns the library's errors as they are:

<!-- ds:block id=newrouter-qwzddubq -->
**newRouter** · [`examples/frameworks/adapters/echo/main.go:51-65`](../../examples/frameworks/adapters/echo/main.go#L51-L65)

```go
func newRouter(a *app.App) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HTTPErrorHandler = fsecho.ErrorHandler(e.DefaultHTTPErrorHandler)
	e.Use(middleware.Recover())
	e.POST(app.PathUpload, echo.WrapHandler(a.Upload))
	fsecho.Mount(e, app.PathFiles, a.Files)
	me := e.Group(app.GroupMe,
		fsecho.Actor(func(c echo.Context) (string, error) { return app.User(c.Request()) }),
		fsecho.Scope(func(c echo.Context) (string, error) { return app.Tenant(c.Request()) }),
	)
	fsecho.Mount(me, app.MountMedia, a.Records)
	me.GET(app.PathExport+"/:"+app.ParamRole, func(c echo.Context) error { return export(c, a) })
	return e
}
```
<!-- /ds:block hash=c730d3 -->

<!-- ds:block id=export-mqkcmukz -->
**export** · [`examples/frameworks/adapters/echo/main.go:69-80`](../../examples/frameworks/adapters/echo/main.go#L69-L80)

```go
func export(c echo.Context, a *app.App) error {
	scope, err := fsecho.ScopeOf(c.Request())
	if err != nil {
		return err
	}
	role := c.Param(app.ParamRole)
	files, err := a.Export(c.Request().Context(), scope, role)
	if err != nil {
		return err
	}
	return fsecho.Zip(c, a.Media, app.ExportName(role), files, fshttp.ZipOptions{})
}
```
<!-- /ds:block hash=1c45c1 -->

fiber is not `net/http`, so a value its middleware stores never reaches an adapted handler's `*http.Request` by itself. Mount every library handler through `fsfiber.Mount` or `fsfiber.Handler`, which carry the actor and the scope across; fiber's own `adaptor.HTTPHandler` would leave `uploaded_by` empty:

<!-- ds:block id=newapp-tmz72tk7 -->
**newApp** · [`examples/frameworks/adapters/fiber/main.go:64-80`](../../examples/frameworks/adapters/fiber/main.go#L64-L80)

```go
func newApp(a *app.App) *fiber.App {
	f := fiber.New(fiber.Config{
		BodyLimit:    bodyLimit,
		ReadTimeout:  readTimeout,
		ErrorHandler: fsfiber.ErrorHandler(fiber.DefaultErrorHandler),
	})
	f.Use(recover.New())
	f.Post(app.PathUpload, fsfiber.Handler(a.Upload))
	fsfiber.Mount(f, app.PathFiles, a.Files)
	me := f.Group(app.GroupMe,
		fsfiber.Actor(func(c fiber.Ctx) (string, error) { return c.Get(app.HeaderUser), nil }),
		fsfiber.Scope(tenant),
	)
	fsfiber.Mount(me, app.MountMedia, a.Records)
	me.Get(app.PathExport+"/:"+app.ParamRole, func(c fiber.Ctx) error { return export(c, a) })
	return f
}
```
<!-- /ds:block hash=b66d8d -->

chi is `net/http` throughout, so `fschi.Actor` and `fschi.Scope` are ordinary `func(http.Handler) http.Handler` middleware and take the same functions the other programs wrap:

<!-- ds:block id=newrouter-h2hv55wp -->
**newRouter** · [`examples/frameworks/adapters/chi/main.go:51-62`](../../examples/frameworks/adapters/chi/main.go#L51-L62)

```go
func newRouter(a *app.App) chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Method(http.MethodPost, app.PathUpload, a.Upload)
	fschi.Mount(r, app.PathFiles, a.Files)
	r.Route(app.GroupMe, func(me chi.Router) {
		me.Use(fschi.Actor(app.User), fschi.Scope(app.Tenant))
		fschi.Mount(me, app.MountMedia, a.Records)
		me.Get(app.PathExport+"/{"+app.ParamRole+"}", func(w http.ResponseWriter, r *http.Request) { export(w, r, a) })
	})
	return r
}
```
<!-- /ds:block hash=562b46 -->

Check: `go test ./adapters/...` in `examples/frameworks` passes. It runs the upload, the refused HTML file, the records 401 and 201 with `uploaded_by`, the signed download, the export zip and the 404 on all four frameworks.

## Pitfalls

- **gin.Recovery swallows the zip abort.** `fshttp.ServeZip` aborts a failed stream with `http.ErrAbortHandler`; gin's `Recovery` recovers it, and net/http would then end the response cleanly, so the client would keep a truncated archive. `fsgin.Zip` catches the abort itself and closes the connection. Over HTTP/2 a connection cannot be hijacked, so there it re-panics: serve zip routes over HTTP/2 only without `gin.Recovery` in front of them. echo's `Recover` and chi's `Recoverer` pass the abort on, so `fsecho.Zip` and `fschi.Zip` need nothing extra.
- **fiber's request context is pooled.** fiber's adaptor hands a handler fasthttp's `RequestCtx` as its context, which is reused for later requests and breaks the context contract at shutdown (its done channel is closed, then set to nil). A handler that keeps a derived context past the request, as tusd does, then panics the process in `context.propagateCancel`. `fsfiber.Handler` and `fsfiber.Mount` give each handler a context of its own instead: it carries the actor and scope, is cancelled when the server shuts down while the handler runs, and is cancelled once the handler returns. Never mount a library handler with the plain `adaptor.HTTPHandler`.
- **fiber buffers.** fiber reads every request body before a handler runs: set `fiber.Config.BodyLimit` to the policy's `MaxSize` plus form overhead (the default is 4 MiB), and `ReadTimeout` for the slowest upload. Responses through fiber's adaptor are buffered too; `fsfiber.Zip` bypasses the adaptor and streams through fasthttp's body stream, so a large archive is never held in memory and a failure closes the connection.
- **Scope is not optional.** `ScopeOf` refuses a request that did not pass through `Scope`, so a records route mounted outside the group answers 401 rather than reading another tenant's files. Resolve the tenant from the session, never from the URL or a header the client chooses (the examples' `X-User` is a stand-in).
- **Do not read the body before the library does.** `c.FormFile`, `c.Bind`, `ParseMultipartForm` or a body-logging middleware in front of the upload routes consume the body, and the library's handlers then find no file.
- **gin and fiber answer a wrong method with 404 by default.** The library's handlers answer 405 with `Allow`, but only for requests that reach them; `Mount` registers every method so that they do.

## Next

- [HTTP endpoints](../records/http-endpoints.md): every route of `recordshttp`.
- [net/http, gorilla and chi](net-http.md): the handlers themselves in detail.
- [Private downloads](../recipes/private-downloads.md): signed links and zips without a framework.
