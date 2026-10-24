# contrib/echo

`github.com/ubgo/filesystem/contrib/echo` (package `fsecho`) mounts the library's HTTP handlers on [echo](https://github.com/labstack/echo) v4: `upload.Handler`, `upload.Uppy`, `fshttp.Signed`, `fshttp.Public`, `recordshttp.Handler` and contrib/tus under groups, the signed-in user and the tenant resolved once in middleware, the library's errors rendered by an `HTTPErrorHandler`, and zip downloads. It is its own module, so an application without echo never downloads it; the handlers themselves stay `net/http`.

```sh
go get github.com/ubgo/filesystem/contrib/echo
```

The guide, with output from a run: [framework adapters](../../docs/integrations/framework-adapters.md). The routing of [`examples/frameworks/adapters/echo`](../../examples/frameworks/adapters/echo/main.go):

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

## API

| Name | What it is |
|---|---|
| `Mount(r Router, prefix string, h http.Handler)` | serves `h` for every method below `prefix` on an `*echo.Echo` or `*echo.Group`, with the full matched prefix stripped; a prefix ending in `/` is stripped with its slash (tus) |
| `Router` | anything with echo's `Any`: `*echo.Echo`, `*echo.Group` |
| `Actor(fn func(echo.Context) (string, error)) echo.MiddlewareFunc` | records the actor with `records.WithActor` on the request; `""` lets an anonymous request through, an error answers 401 |
| `Scope(fn func(echo.Context) (string, error)) echo.MiddlewareFunc` | stores the tenant on the request with `recordshttp.WithScope`, the key `recordshttp.ScopeFromRequest` reads too; an error or `""` answers 401 |
| `ScopeOf(r *http.Request) (string, error)` | `recordshttp.Config.Scope`: the tenant `Scope` stored, or `ErrNoScope` |
| `Error(c echo.Context, err error) error` | answers err as the library does (status of `fshttp.StatusOf`, JSON `upload.ErrorResponse` with `fshttp.ClientMessage`, `Retry-After`) and returns nil |
| `ErrorHandler(next echo.HTTPErrorHandler) echo.HTTPErrorHandler` | renders the library's errors with `Error` and passes every other error (`*echo.HTTPError`, yours) to `next` |
| `Zip(c, disk, filename, files, opts) error` | `fshttp.ServeZip`; echo's `Recover` passes its abort on, so a failed stream resets the connection |
| `ErrNoScope`, `MsgUnauthenticated` | the error of a request outside `Scope`; the 401's text, `not signed in` |
| `Responder{Errors fshttp.ErrorWriter}` | the same `Actor`, `Scope`, `Error` and `ErrorHandler` as methods (`x.Actor(fn)`, `x.Scope(fn)`, `x.Error(c, err)`, `x.ErrorHandler(next)`), answering every error through `Errors`: the 401 with `ErrUnauthenticated` (or the resolver's own error as `permission`), the rest with `fshttp.StatusOf`'s status, `Retry-After` already set. The zero `Responder` is the package-level functions, so `fsecho.Responder{Errors: problem}` plugs an application's own envelope (problem+json, its API's shape) into the middleware without changing anything else |
| `ErrUnauthenticated` | the error `Errors` gets for a 401 whose resolver returned no error of its own |
