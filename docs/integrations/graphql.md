# GraphQL uploads with gqlgen

You will have a [gqlgen](https://gqlgen.com) API whose mutations take files as the `Upload` scalar of the [GraphQL multipart request specification](https://github.com/jaydenseric/graphql-multipart-request-spec), the protocol Apollo Client, urql and Relay upload with, and every one of those files stored through an `upload.Policy` exactly like a form upload: its type detected from its bytes, its size capped, its name made safe, `Process` and `Inspect` run, and a file as a records row when you want one. The module is `contrib/gqlgen` (package `fsgql`).

## Before you start

- gqlgen already parses the multipart request (`transport.MultipartForm`) and hands a resolver a `graphql.Upload`. Its `ContentType` and `Filename` are what the client claimed; `fsgql` is the part that decides what may be stored.
- The program is [`examples/gqlgen`](../../examples/gqlgen), a module of its own: `go run .` from that directory serves `http://localhost:8195/graphql`, and `go test ./...` sends real multipart requests through it. gqlgen generates code into your module, so the example carries its own generated `graph/generated.go`; `go generate ./...` regenerates it.
- [Uploads](../concepts/uploads.md) explains the policy.

```sh
go get github.com/ubgo/filesystem/contrib/gqlgen
```

## Step 1 — The schema

Copy `fsgql.Schema` (the file [`contrib/gqlgen/upload.graphqls`](../../contrib/gqlgen/upload.graphqls)) into your schema directory. It declares `scalar Upload` and `type StoredUpload` (key, filename, contentType, size, sha256). Map both in `gqlgen.yml`, then use them in your own mutations:

```yaml
models:
  Upload:
    model: github.com/99designs/gqlgen/graphql.Upload
  StoredUpload:
    model: github.com/ubgo/filesystem/upload.Result
```

```graphql
type Mutation {
  uploadFile(file: Upload!): StoredUpload!
  uploadFiles(files: [Upload!]!): [StoredUpload!]!
  setAvatar(file: Upload!): FileRecord!
}
```

`FileRecord` is the example's own type, mapped to `records.File`.

## Step 2 — The server

Add gqlgen's multipart transport sized from the policy with `fsgql.MultipartForm`, which caps the request body at the files' total plus form overhead and keeps at most `fsgql.MaxMemory` of it in memory, so large uploads spill to temporary files instead. `fsgql.ErrorPresenter` answers the library's errors with the client-safe message and the code under `extensions.code`:

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

## Step 3 — The resolvers

One file goes through `fsgql.Receive`, which runs `upload.Receive` under the policy:

<!-- ds:block id=mutationresolver-uploadfile-akjwsz6m -->
**mutationResolver.UploadFile** · [`examples/gqlgen/graph/schema.resolvers.go:20-26`](../../examples/gqlgen/graph/schema.resolvers.go#L20-L26)

```go
func (r *mutationResolver) UploadFile(ctx context.Context, file graphql.Upload) (*upload.Result, error) {
	res, err := fsgql.Receive(ctx, r.Media, file, r.Policy)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
```
<!-- /ds:block hash=fa467c -->

A list goes through `fsgql.ReceiveAll`, all or none: when one file is refused, the ones already stored are deleted:

<!-- ds:block id=mutationresolver-uploadfiles-gxxuxrd5 -->
**mutationResolver.UploadFiles** · [`examples/gqlgen/graph/schema.resolvers.go:30-40`](../../examples/gqlgen/graph/schema.resolvers.go#L30-L40)

```go
func (r *mutationResolver) UploadFiles(ctx context.Context, files []*graphql.Upload) ([]*upload.Result, error) {
	stored, err := fsgql.ReceiveAll(ctx, r.Media, files, r.Policy, r.MaxFiles)
	if err != nil {
		return nil, err
	}
	out := make([]*upload.Result, len(stored))
	for i := range stored {
		out[i] = &stored[i]
	}
	return out, nil
}
```
<!-- /ds:block hash=356a5b -->

A file as a records row goes through `fsgql.ReceiveRecord` (`Store.Upload`). Here it runs in one transaction with `SetAttachment`, so a failed attach deletes the stored object again, and the actor your authentication recorded with `records.WithActor` becomes `uploadedBy`:

<!-- ds:block id=mutationresolver-setavatar-sbbb3w2r -->
**mutationResolver.SetAvatar** · [`examples/gqlgen/graph/schema.resolvers.go:44-61`](../../examples/gqlgen/graph/schema.resolvers.go#L44-L61)

```go
func (r *mutationResolver) SetAvatar(ctx context.Context, file graphql.Upload) (*records.File, error) {
	owner, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	var f records.File
	err = r.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if f, err = fsgql.ReceiveRecord(ctx, r.Store, Tenant, r.DiskName, file, r.Policy); err != nil {
			return err
		}
		_, err = r.Store.SetAttachment(ctx, Tenant, f.ID, owner, RoleAvatar, records.PreviousPurge)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}
```
<!-- /ds:block hash=d6cbb7 -->

Check: start it, then send files the way a GraphQL client does: an `operations` field with `null` where the file goes, a `map` field naming that path, and the file:

```sh
cd examples/gqlgen && go run .
printf 'quarterly numbers\n' > report.txt
printf '<!doctype html><script>alert(1)</script>' > page.txt
Q='{"query":"mutation($file: Upload!) { uploadFile(file: $file) { key filename contentType size sha256 } }","variables":{"file":null}}'
A='{"query":"mutation($file: Upload!) { setAvatar(file: $file) { id filename uploadedBy } }","variables":{"file":null}}'
M='{"0":["variables.file"]}'
curl -s localhost:8195/graphql -F operations="$Q" -F map="$M" -F 0=@report.txt
curl -s localhost:8195/graphql -F operations="$Q" -F map="$M" -F 0=@page.txt
curl -s localhost:8195/graphql -H 'X-User: alice' -F operations="$A" -F map="$M" -F 0=@report.txt
curl -s localhost:8195/graphql -F operations="$A" -F map="$M" -F 0=@report.txt
```

```text
{"data":{"uploadFile":{"key":"incoming/y2utperh2s4rcur3sqzjj5cnvh.txt","filename":"report.txt","contentType":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}}}
{"errors":[{"message":"content type \"text/html; charset=utf-8\" is not allowed","path":["uploadFile"],"extensions":{"code":"rejected"}}],"data":null}
{"data":{"setAvatar":{"id":"fil_uu5f5hcjzuq7fi4f5yblriji7t","filename":"report.txt","uploadedBy":"alice"}}}
{"errors":[{"message":"Forbidden","path":["setAvatar"],"extensions":{"code":"permission"}}],"data":null}
```

`page.txt` is HTML whatever its name says, so the policy refuses it from its bytes. The signed-out `setAvatar` shows the presenter's rule: a permission error's text is the status text, not the resolver's words.

## Step 4 — Lists, limits and replacing a record

Three more behaviours a client meets, from a second real run against the same server (`go run .` in `examples/gqlgen`). A list mutation maps each file to its own path in `map` (`{"0":["variables.files.0"],"1":["variables.files.1"]}`); a file over the policy's `MaxSize` (1 MiB here) is refused while it streams; and `setAvatar` called twice keeps one avatar, which the `avatar` query then reads back through the Store:

```sh
QS='{"query":"mutation($files: [Upload!]!) { uploadFiles(files: $files) { key filename size } }","variables":{"files":[null,null]}}'
MS='{"0":["variables.files.0"],"1":["variables.files.1"]}'
G='{"query":"{ avatar { id filename size } }"}'
curl -s localhost:8195/graphql -F operations="$QS" -F map="$MS" -F 0=@report.txt -F 1=@notes.txt
curl -s localhost:8195/graphql -F operations="$QS" -F map="$MS" -F 0=@report.txt -F 1=@page.txt
curl -s localhost:8195/graphql -F operations="$Q" -F map="$M" -F 0=@big.txt
curl -s localhost:8195/graphql -H 'X-User: alice' -F operations="$A" -F map="$M" -F 0=@report.txt
curl -s localhost:8195/graphql -H 'X-User: alice' -F operations="$A" -F map="$M" -F 0=@notes.txt
curl -s localhost:8195/graphql -H 'X-User: alice' -H 'Content-Type: application/json' -d "$G"
```

```text
{"data":{"uploadFiles":[{"key":"incoming/5ld243zoujjygbc2q6yg25ryhe.txt","filename":"report.txt","size":18},{"key":"incoming/itywzk6p4sjvjnc2s4agk3s63v.txt","filename":"notes.txt","size":13}]}}
{"errors":[{"message":"content type \"text/html; charset=utf-8\" is not allowed","path":["uploadFiles"],"extensions":{"code":"rejected"}}],"data":null}
{"errors":[{"message":"the file is larger than 1048576 bytes","path":["uploadFile"],"extensions":{"code":"too_large"}}],"data":null}
{"data":{"setAvatar":{"id":"fil_fejwuuwbiqegtjzup2silg72cy","filename":"report.txt","uploadedBy":"alice"}}}
{"data":{"setAvatar":{"id":"fil_rtvws6jukbacndpmaziddpapu3","filename":"notes.txt","uploadedBy":"alice"}}}
{"data":{"avatar":{"id":"fil_rtvws6jukbacndpmaziddpapu3","filename":"notes.txt","size":13}}}
```

`notes.txt` is `notes for q4` and a newline (13 bytes) and `big.txt` is 1 MiB plus one byte. How each line came about:

- **The list with an HTML file fails whole.** `fsgql.ReceiveAll` stores the files in order and, when one is refused, deletes the ones it already stored, so the client gets one error and no partial result: `report.txt` of that request is not left behind.
- **The size cap is enforced from the bytes.** `too_large` comes from counting the stream against `MaxSize`, not from the client's claim; the transport's own limit (`fsgql.MultipartForm`) sits above it, sized for the files a request may carry plus form overhead.
- **The avatar is replaced, not added.** `SetAvatar` (Step 3) runs `ReceiveRecord` and `SetAttachment` with `records.PreviousPurge` in one `Store.Tx`, so the second call leaves one avatar attachment and purges the old file after the commit; the `avatar` resolver is a plain `Store.Attachments` call for the signed-in user's owner and role, so it sees exactly what the Store holds.

To choose between the three receive functions: `fsgql.Receive` for one file whose key you keep yourself, `fsgql.ReceiveAll` for a list that must succeed whole, and `fsgql.ReceiveRecord` when the file should become a records row (attachments, trash, plugins, audit).

## Pitfalls

- **Do not trust `graphql.Upload.ContentType` or `Filename`.** They are the client's claims. Store uploads only through `fsgql.Receive`, `ReceiveAll` or `ReceiveRecord` (or `upload.Receive` yourself), never by copying `up.File` to a disk.
- **Size the transport.** gqlgen's default `MultipartForm` accepts 32 MiB per request and keeps 32 MiB in memory. `fsgql.MultipartForm(policy, maxFiles)` sets both from the policy; the per-file cap is still enforced from the bytes when each file is stored.
- **Install the presenter.** Without `fsgql.ErrorPresenter`, gqlgen's default presenter sends `err.Error()`, which for a driver failure names the disk and the storage key.
- **GraphQL answers errors with 200.** Clients branch on `errors[].extensions.code` (`rejected`, `too_large`, `permission`, …), not on the HTTP status. `fsgql.Code` reads it in Go.
- **Lists are all or none.** Use separate mutations, or call `fsgql.Receive` per file and collect failures yourself, when one bad file should not sink the others.
- **Records and the object in one transaction.** Do what `SetAvatar` does: receive the record and change attachments inside `Store.Tx`, so a failure after the upload rolls the row back and deletes the stored object, instead of leaving an orphan.

## Next

- [Framework adapters](framework-adapters.md): mount the REST endpoints next to the GraphQL one.
- [Files as records](../records/README.md): attachments, trash and plugins behind `ReceiveRecord`.
