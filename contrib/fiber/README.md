# contrib/fiber

`github.com/ubgo/filesystem/contrib/fiber` (package `fsfiber`) mounts the library's HTTP handlers on [fiber](https://github.com/gofiber/fiber) v3: `upload.Handler`, `upload.Uppy`, `fshttp.Signed`, `fshttp.Public`, `recordshttp.Handler` and contrib/tus under groups, the signed-in user and the tenant resolved once in middleware and carried into the handlers, the library's errors rendered by an `ErrorHandler`, and zip downloads streamed without buffering. It is its own module, so an application without fiber never downloads it; the handlers themselves stay `net/http`.

```sh
go get github.com/ubgo/filesystem/contrib/fiber
```

fiber is not `net/http`: a value a fiber middleware stores never reaches a handler adapted with fiber's `adaptor.HTTPHandler`, so through the plain adaptor recordshttp records no uploader and finds no tenant. Mount every library handler with `fsfiber.Mount` or `fsfiber.Handler`.

The guide, with output from a run: [framework adapters](../../docs/integrations/framework-adapters.md). The routing of [`examples/frameworks/adapters/fiber`](../../examples/frameworks/adapters/fiber/main.go):

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

## API

| Name | What it is |
|---|---|
| `Handler(h http.Handler) fiber.Handler` | adapts a library handler with a context of its own (never fasthttp's pooled `RequestCtx`, which panics handlers that keep a derived context, such as tusd, at shutdown) carrying the actor and scope from `Actor` and `Scope`, cancelled at shutdown and when the handler returns; use it instead of `adaptor.HTTPHandler` |
| `Mount(r Router, prefix string, h http.Handler)` | serves `h` through `Handler` for every method below `prefix` on an app or group, with the full matched prefix stripped; a prefix ending in `/` is stripped with its slash (tus) |
| `Router` | anything with fiber's `All`: `*fiber.App`, a group |
| `Actor(fn func(fiber.Ctx) (string, error)) fiber.Handler` | records the actor for adapted handlers and in `c.Context()` with `records.WithActor`; `""` lets an anonymous request through, an error answers 401 |
| `Scope(fn func(fiber.Ctx) (string, error)) fiber.Handler` | stores the tenant with `recordshttp.WithScope` for `ScopeOf`, `ScopeFrom` and `recordshttp.ScopeFromRequest`; an error or `""` answers 401 |
| `ScopeOf(r *http.Request) (string, error)` | `recordshttp.Config.Scope` for adapted handlers, or `ErrNoScope` |
| `ScopeFrom(c fiber.Ctx) (string, error)` | the same for your own fiber handlers |
| `Error(c fiber.Ctx, err error) error` | answers err as the library does (status of `fshttp.StatusOf`, JSON `upload.ErrorResponse` with `fshttp.ClientMessage`, `Retry-After`) and returns nil |
| `ErrorHandler(next fiber.ErrorHandler) fiber.ErrorHandler` | renders the library's errors with `Error` and passes every other error (`*fiber.Error`, yours) to `next` |
| `Zip(c, disk, filename, files, opts) error` | `fshttp.ServeZip` through fasthttp's body stream rather than the adaptor: never buffered, and a failure after the headers closes the connection instead of ending the archive cleanly |
| `ErrNoScope`, `MsgUnauthenticated` | the error of a request outside `Scope`; the 401's text, `not signed in` |
| `Responder{Errors fshttp.ErrorWriter}` | the same `Actor`, `Scope`, `Error` and `ErrorHandler` as methods (`x.Actor(fn)`, `x.Scope(fn)`, `x.Error(c, err)`, `x.ErrorHandler(next)`), answering every error through `Errors` (through fiber's adaptor, on the request converted to `*http.Request`): the 401 with `ErrUnauthenticated` (or the resolver's own error as `permission`), the rest with `fshttp.StatusOf`'s status, `Retry-After` already set. The zero `Responder` is the package-level functions, so `fsfiber.Responder{Errors: problem}` plugs an application's own envelope (problem+json, its API's shape) into the middleware without changing anything else |
| `ErrUnauthenticated` | the error `Errors` gets for a 401 whose resolver returned no error of its own |

## Limits

- fiber reads every request body before a handler runs: set `fiber.Config.BodyLimit` to the upload policy's `MaxSize` plus form overhead (fiber's default is 4 MiB) and `ReadTimeout` for the slowest upload.
- Responses of handlers adapted with `Handler` or `Mount` are buffered by fiber's adaptor until they flush; the library's download handlers serve objects through `http.ServeContent`, so a very large download is best served from a signed backend URL (`Disk.SignedURL`) rather than through fiber.
