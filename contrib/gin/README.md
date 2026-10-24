# contrib/gin

`github.com/ubgo/filesystem/contrib/gin` (package `fsgin`) mounts the library's HTTP handlers on [gin](https://github.com/gin-gonic/gin): `upload.Handler`, `upload.Uppy`, `fshttp.Signed`, `fshttp.Public`, `recordshttp.Handler` and contrib/tus under route groups, the signed-in user and the tenant resolved once in middleware, the library's errors rendered safely, and zip downloads. It is its own module, so an application without gin never downloads it; the handlers themselves stay `net/http`.

```sh
go get github.com/ubgo/filesystem/contrib/gin
```

The guide, with output from a run: [framework adapters](../../docs/integrations/framework-adapters.md). The routing of [`examples/frameworks/adapters/gin`](../../examples/frameworks/adapters/gin/main.go):

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

## API

| Name | What it is |
|---|---|
| `Mount(r Router, prefix string, h http.Handler)` | serves `h` for every method below `prefix` on an engine or group, with the group's base path and `prefix` stripped; a prefix ending in `/` is stripped with its slash (tus) |
| `Router` | `gin.IRoutes` plus `BasePath()`: a `*gin.Engine` or a `*gin.RouterGroup` |
| `Actor(fn func(*gin.Context) (string, error)) gin.HandlerFunc` | records the actor with `records.WithActor` on the request; `""` lets an anonymous request through, an error answers 401 |
| `Scope(fn func(*gin.Context) (string, error)) gin.HandlerFunc` | stores the tenant on the request with `recordshttp.WithScope`, the key `recordshttp.ScopeFromRequest` reads too; an error or `""` answers 401 |
| `ScopeOf(r *http.Request) (string, error)` | `recordshttp.Config.Scope`: the tenant `Scope` stored, or `ErrNoScope` |
| `Error(c *gin.Context, err error)` | answers err as the library does (status of `fshttp.StatusOf`, JSON `upload.ErrorResponse` with `fshttp.ClientMessage`, `Retry-After`) and aborts the chain |
| `Zip(c, disk, filename, files, opts)` | `fshttp.ServeZip`, with the abort that `gin.Recovery` would swallow turned into a closed connection |
| `ErrNoScope`, `MsgUnauthenticated` | the error of a request outside `Scope`; the 401's text, `not signed in` |
| `Responder{Errors fshttp.ErrorWriter}` | the same `Actor`, `Scope` and `Error` as methods (`x.Actor(fn)`, `x.Scope(fn)`, `x.Error(c, err)`), answering every error through `Errors`: the 401 with `ErrUnauthenticated` (or the resolver's own error as `permission`), the rest with `fshttp.StatusOf`'s status, `Retry-After` already set. The zero `Responder` is the package-level functions, so `fsgin.Responder{Errors: problem}` plugs an application's own envelope (problem+json, its API's shape) into the middleware without changing anything else |
| `ErrUnauthenticated` | the error `Errors` gets for a 401 whose resolver returned no error of its own |

## Limits

- Over HTTP/2 a failed zip cannot close the connection itself; serve zip routes over HTTP/2 without `gin.Recovery` in front of them.
- The adapter has no opinion on authentication: `Actor` and `Scope` call your functions.
