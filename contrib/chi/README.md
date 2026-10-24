# contrib/chi

`github.com/ubgo/filesystem/contrib/chi` (package `fschi`) mounts the library's HTTP handlers on [chi](https://github.com/go-chi/chi) v5: `upload.Handler`, `upload.Uppy`, `fshttp.Signed`, `fshttp.Public`, `recordshttp.Handler` and contrib/tus under routes and sub-routers with their prefix stripped (chi's own `Mount` does not strip it), the signed-in user and the tenant resolved once in plain `net/http` middleware, the library's errors rendered safely, and zip downloads. It is its own module, so an application without chi never downloads it.

```sh
go get github.com/ubgo/filesystem/contrib/chi
```

The guide, with output from a run: [framework adapters](../../docs/integrations/framework-adapters.md). The routing of [`examples/frameworks/adapters/chi`](../../examples/frameworks/adapters/chi/main.go):

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

## API

| Name | What it is |
|---|---|
| `Mount(r chi.Router, prefix string, h http.Handler)` | serves `h` for every method below `prefix`, with the full matched route pattern (every enclosing `Route` and `Mount`) stripped; a prefix ending in `/` is stripped with its slash (tus) |
| `Actor(fn func(*http.Request) (string, error)) func(http.Handler) http.Handler` | records the actor with `records.WithActor`; `""` lets an anonymous request through, an error answers 401 |
| `Scope(fn func(*http.Request) (string, error)) func(http.Handler) http.Handler` | stores the tenant on the request with `recordshttp.WithScope`, the key `recordshttp.ScopeFromRequest` reads too; an error or `""` answers 401 |
| `ScopeOf(r *http.Request) (string, error)` | `recordshttp.Config.Scope`: the tenant `Scope` stored, or `ErrNoScope` |
| `Error(w http.ResponseWriter, err error)` | answers err as the library does (status of `fshttp.StatusOf`, JSON `upload.ErrorResponse` with `fshttp.ClientMessage`, `Retry-After`) |
| `Zip(w, r, disk, filename, files, opts)` | `fshttp.ServeZip`; chi's `Recoverer` passes its abort on, so a failed stream resets the connection |
| `ErrNoScope`, `MsgUnauthenticated` | the error of a request outside `Scope`; the 401's text, `not signed in` |
| `Responder{Errors fshttp.ErrorWriter}` | the same `Actor`, `Scope` and `Error` as methods (`x.Actor(fn)`, `x.Scope(fn)`, `x.Error(w, r, err)`), answering every error through `Errors`: the 401 with `ErrUnauthenticated` (or the resolver's own error as `permission`), the rest with `fshttp.StatusOf`'s status, `Retry-After` already set. The zero `Responder` is the package-level functions, so `fschi.Responder{Errors: problem}` plugs an application's own envelope (problem+json, its API's shape) into the middleware without changing anything else |
| `ErrUnauthenticated` | the error `Errors` gets for a 401 whose resolver returned no error of its own |

`Actor`, `Scope`, `ScopeOf` and `Error` use nothing of chi and work with any `net/http` router.
