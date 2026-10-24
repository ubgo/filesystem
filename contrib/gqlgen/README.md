# contrib/gqlgen

`github.com/ubgo/filesystem/contrib/gqlgen` (package `fsgql`) stores files sent to a [gqlgen](https://gqlgen.com) server as the `Upload` scalar of the [GraphQL multipart request specification](https://github.com/jaydenseric/graphql-multipart-request-spec) through an `upload.Policy`, exactly as a form upload is stored: type sniffed from the bytes, size capped, name made safe, `Process` and `Inspect` run, optionally as a records row. It is its own module, so an application without GraphQL never downloads gqlgen.

```sh
go get github.com/ubgo/filesystem/contrib/gqlgen
```

The guide, with output from a run: [GraphQL uploads](../../docs/integrations/graphql.md). The example is [`examples/gqlgen`](../../examples/gqlgen). Its server:

<!-- ds:block id=newgraphql-du36vw6u -->
**newGraphQL** · [`examples/gqlgen/main.go:110-116`](../../examples/gqlgen/main.go#L110-L116)

```go
func newGraphQL(r *graph.Resolver) http.Handler {
	srv := handler.New(graph.NewExecutableSchema(graph.Config{Resolvers: r}))
	srv.AddTransport(transport.POST{})
	srv.AddTransport(fsgql.MultipartForm(r.Policy, r.MaxFiles))
	srv.SetErrorPresenter(fsgql.ErrorPresenter(nil))
	return srv
}
```
<!-- /ds:block hash=a70c2b -->

## API

| Name | What it is |
|---|---|
| `Schema` | the embedded [`upload.graphqls`](upload.graphqls): `scalar Upload` and `type StoredUpload`; copy it into your schema and map `Upload` to `graphql.Upload`, `StoredUpload` to `upload.Result` |
| `Receive(ctx, disk, up graphql.Upload, p upload.Policy) (upload.Result, error)` | stores one upload through `upload.Receive`; the client's `ContentType` is ignored, a declared size over `p.MaxSize` is refused before the disk is touched |
| `ReceiveAll(ctx, disk, ups []*graphql.Upload, p, maxFiles int) ([]upload.Result, error)` | a list, all or none: a refused file deletes the ones already stored; more than `maxFiles` is `invalid_argument` |
| `ReceiveRecord(ctx, store, scope, diskName string, up, p) (records.File, error)` | one upload as a records file through `Store.Upload`; the context's actor becomes `UploadedBy` |
| `ErrorPresenter(next graphql.ErrorPresenterFunc) graphql.ErrorPresenterFunc` | presents the library's errors with `fshttp.ClientMessage` and the code under `extensions.code`; every other error goes to `next` (`graphql.DefaultErrorPresenter` when nil) |
| `MultipartForm(p upload.Policy, maxFiles int) transport.MultipartForm` | gqlgen's multipart transport capped at `maxFiles × p.MaxSize + FormOverhead`, with at most `MaxMemory` in memory |
| `Code(err *gqlerror.Error) filesystem.Code` | the code of a presented error, for tests and Go clients |
| `CodeExtension`, `FormOverhead`, `MaxMemory` | `"code"`; 64 KiB; 1 MiB |

## Limits

- Uploads arrive whole before a resolver runs (gqlgen parses the multipart request first), so the transport's cap is the first defence against a huge request; `Receive` then enforces the per-file cap from the bytes.
- GraphQL answers errors with HTTP 200; clients branch on `extensions.code`.
