# The records layer over HTTP

A JSON API over the records layer with `recordshttp`: upload files and attach them to a user, list them with links, reorder and detach them, redirect to a file, replace its content and trash it. Downloads are served by `fshttp.Signed` on the same server, so the redirect of `GET /media/files/{id}` leads to bytes. This is the backend a web or mobile client of a gallery, a profile or a ticket's attachments talks to. Authentication is out of scope: the demo trusts the `X-Tenant` and `X-User` headers, which a real app replaces with its session lookup. The authorization rule is real: a user may change only their own attachments and only the files they uploaded. It uses `github.com/ubgo/filesystem/records`, `records/recordshttp`, `fshttp`, `upload` and `signedurl` on SQLite over an in-memory disk. The guide [HTTP endpoints](../../docs/records/http-endpoints.md) explains each step.

## Run it

```sh
cd examples
go run ./records-http
go test ./records-http
```

It listens on `http://localhost:8094`, keeps the SQLite database in a temporary directory and the bytes in memory, and needs no environment variables. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes its temporary directory (`records-http…` under your system temp directory). Uploads accept images and plain text up to 64 KiB, at most 3 files per request.

```text
2026/10/03 16:18:51 listening on http://localhost:8094
```

## Walkthrough

The commands below use a tiny PNG, `plan.txt` holding `quarterly plan`, `notes.txt` holding `meeting notes`, `notes-v2.txt` holding `meeting notes, v2`, an HTML file and a 70 000-byte `big.txt`. IDs, keys and signatures differ on every run.

1. Upload two files and attach them to user `u1`'s gallery, in one transaction. The storage key is random; the client's name is kept only as `filename`.

```sh
H='-H X-Tenant:acme -H X-User:u1'
curl -s -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@photo.png -F file=@notes.txt
```

```json
{"files":[{"attachment_id":"att_d4sfjaccuhpor2tpvcggg5drqs","file_id":"fil_qq3kocfqifd6jhhh7ltsvi6gzj","filename":"photo.png","content_type":"image/png","size":74,"position":1,"uploaded_by":"user:u1","status":"ready"},{"attachment_id":"att_pbfzdlr5ts54nneostcwe7k5ml","file_id":"fil_iup4lnydd3lo22oluksdbwi343","filename":"notes.txt","content_type":"text/plain; charset=utf-8","size":14,"position":2,"uploaded_by":"user:u1","status":"ready"}]}
```

2. List them in position order, each with a signed link:

```sh
curl -s localhost:8094/media/owners/user/u1/gallery $H
```

```json
{"files":[{"attachment_id":"att_d4sfjaccuhpor2tpvcggg5drqs","file_id":"fil_qq3kocfqifd6jhhh7ltsvi6gzj","filename":"photo.png","content_type":"image/png","size":74,"position":1,"uploaded_by":"user:u1","url":"http://localhost:8094/files/media/uploads/xi6mjyjhwqyq2h6cgrygkz3glf.png?ct=image%2Fpng\u0026exp=1791267880\u0026kid=k1\u0026m=GET\u0026sig=8329db5c88463bfc4dff74bebc00332c73dff3227c7b699fcbb5227ef0f2f35d","status":"ready"},{"attachment_id":"att_pbfzdlr5ts54nneostcwe7k5ml","file_id":"fil_iup4lnydd3lo22oluksdbwi343","filename":"notes.txt","content_type":"text/plain; charset=utf-8","size":14,"position":2,"uploaded_by":"user:u1","url":"http://localhost:8094/files/media/uploads/cg7puswx76z4vcnxfa2sv6sxza.txt?ct=text%2Fplain%3B+charset%3Dutf-8\u0026exp=1791267880\u0026kid=k1\u0026m=GET\u0026sig=d0c07cbc25c06726e1dbfda761c088bcb991ba3740be7ecfbbbba7dac44000e8","status":"ready"}]}
```

3. Reorder with every attachment ID exactly once; a partial list changes nothing:

```sh
curl -s -i -X PUT localhost:8094/media/owners/user/u1/gallery/order $H -d '{"ids":["att_pbfzdlr5ts54nneostcwe7k5ml","att_d4sfjaccuhpor2tpvcggg5drqs"]}' | head -1
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/owners/user/u1/gallery/order $H -d '{"ids":["att_pbfzdlr5ts54nneostcwe7k5ml"]}'
```

```text
HTTP/1.1 204 No Content
{"error":"records: reorder must list each of the 2 attachments exactly once","code":"invalid_argument"}
 400
```

4. Detach one attachment (the file is kept). An attachment of another tenant is `404`, the same as an unknown one:

```sh
curl -s -i -X DELETE localhost:8094/media/attachments/att_d4sfjaccuhpor2tpvcggg5drqs $H | head -1
curl -s -w ' %{http_code}\n' -X DELETE localhost:8094/media/attachments/att_pbfzdlr5ts54nneostcwe7k5ml -H X-Tenant:globex -H X-User:u1
```

```text
HTTP/1.1 204 No Content
{"error":"Not Found","code":"not_found"}
 404
```

5. `GET /files/{id}` redirects to the file's link and is never cached; following it prints the bytes:

```sh
curl -s -i localhost:8094/media/files/fil_iup4lnydd3lo22oluksdbwi343 $H
curl -s -L localhost:8094/media/files/fil_iup4lnydd3lo22oluksdbwi343 $H
```

```text
HTTP/1.1 302 Found
Cache-Control: private, no-store
Content-Type: text/html; charset=utf-8
Location: http://localhost:8094/files/media/uploads/cg7puswx76z4vcnxfa2sv6sxza.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791267881&kid=k1&m=GET&sig=24bd88e92d295be279b0e3f372bd9f28927a2fb0febeebd2616f0742057e5b86
Date: Tue, 06 Oct 2026 06:19:41 GMT
Content-Length: 243

<a href="http://localhost:8094/files/media/uploads/cg7puswx76z4vcnxfa2sv6sxza.txt?ct=text%2Fplain%3B+charset%3Dutf-8&amp;exp=1791267881&amp;kid=k1&amp;m=GET&amp;sig=24bd88e92d295be279b0e3f372bd9f28927a2fb0febeebd2616f0742057e5b86">Found</a>.

meeting notes
```

6. `PUT /files/{id}` replaces the content; the file keeps its ID and attachments:

```sh
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/files/fil_iup4lnydd3lo22oluksdbwi343 $H -F file=@notes-v2.txt
curl -s -L localhost:8094/media/files/fil_iup4lnydd3lo22oluksdbwi343 $H
```

```text
{"file_id":"fil_iup4lnydd3lo22oluksdbwi343","filename":"notes-v2.txt","content_type":"text/plain; charset=utf-8","size":18}
 200
meeting notes, v2
```

7. `DELETE /files/{id}` moves the file to the trash; it is gone from every route at once:

```sh
curl -s -i -X DELETE localhost:8094/media/files/fil_iup4lnydd3lo22oluksdbwi343 $H | head -1
curl -s -w ' %{http_code}\n' localhost:8094/media/files/fil_iup4lnydd3lo22oluksdbwi343 $H
```

```text
HTTP/1.1 204 No Content
{"error":"Not Found","code":"not_found"}
 404
```

8. Refusals: no user, another user's gallery, another user's file, a type the policy refuses, and a file over 64 KiB:

```sh
curl -s -w ' %{http_code}\n' localhost:8094/media/owners/user/u1/gallery
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery -H X-Tenant:acme -H X-User:u2 -F file=@notes.txt
curl -s -w ' %{http_code}\n' -X PUT localhost:8094/media/files/fil_qq3kocfqifd6jhhh7ltsvi6gzj -H X-Tenant:acme -H X-User:u2 -F file=@notes.txt
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@page.html
curl -s -w ' %{http_code}\n' -X POST localhost:8094/media/owners/user/u1/gallery $H -F file=@big.txt
```

```text
{"error":"not signed in","code":"permission"}
 401
{"error":"forbidden","code":"permission"}
 403
{"error":"forbidden","code":"permission"}
 403
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
 415
{"error":"body exceeds 65536 bytes","code":"too_large"}
 413
```

9. `/byo` serves the same routes with `DiskFor` instead of one fixed `Disk` (`perdisk.go`): each tenant's uploads go to the disk mounted for its own bucket. Globex has one, so its upload lands on `globex-bucket`, and the link the gallery lists names that disk; Acme has none, so its upload is refused with `403` before any byte is read and nothing is stored. The refusal's message stays generic; the reason is in the server's error.

```sh
curl -s -w ' %{http_code}\n' -X POST localhost:8094/byo/owners/user/u1/gallery -H X-Tenant:globex -H X-User:u1 -F file=@plan.txt
curl -s localhost:8094/media/owners/user/u1/gallery -H X-Tenant:globex -H X-User:u1
curl -s -w ' %{http_code}\n' -X POST localhost:8094/byo/owners/user/u1/gallery -H X-Tenant:acme -H X-User:u1 -F file=@plan.txt
```

```text
{"files":[{"attachment_id":"att_uz24rv2pb6cazejgpkcyra7rts","file_id":"fil_4kd4jekiav25aoqhortqbigxew","filename":"plan.txt","content_type":"text/plain; charset=utf-8","size":14,"position":1,"uploaded_by":"user:u1","status":"ready"}]}
 201
{"files":[{"attachment_id":"att_uz24rv2pb6cazejgpkcyra7rts","file_id":"fil_4kd4jekiav25aoqhortqbigxew","filename":"plan.txt","content_type":"text/plain; charset=utf-8","size":14,"position":1,"uploaded_by":"user:u1","url":"http://localhost:8094/files/globex-bucket/uploads/skphxq7miha2c23exz3swz62e3.txt?ct=text%2Fplain%3B+charset%3Dutf-8\u0026exp=1791541176\u0026kid=k1\u0026m=GET\u0026sig=ba6137b02f0bbab47acf4f27f435f526deb11e1a6532adf394694c6066b8a19a","status":"ready"}]}
{"error":"Forbidden","code":"permission"}
 403
```

## The code

The upload policy: images and plain text, judged from the bytes, under random keys:

<!-- ds:block id=policy-wvgc7kq3 -->
**policy** · [`examples/records-http/main.go:67-71`](main.go#L67-L71)

```go
var policy = upload.Policy{
	MaxSize: maxFileSize,
	Allowed: []string{"image/*", "text/plain"},
	Key:     upload.RandomKey("uploads"),
}
```
<!-- /ds:block hash=523b37 -->

One memory disk whose signed URLs point at the download handler, and a Store that attaches files to users:

<!-- ds:block id=newdisks-y73hs4nf -->
**newDisks** · [`examples/records-http/main.go:76-87`](main.go#L76-L87)

```go
func newDisks() (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(baseURL+"/files", []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, signer, err
}
```
<!-- /ds:block hash=568c80 -->

<!-- ds:block id=newstore-peccqhpr -->
**newStore** · [`examples/records-http/main.go:103-112`](main.go#L103-L112)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerUser))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=7d9526 -->

The two hooks a real app replaces: the tenant of the request (from the session, never the URL) and the authorization rule:

<!-- ds:block id=scopeof-kgn4xeh9 -->
**scopeOf** · [`examples/records-http/main.go:123-128`](main.go#L123-L128)

```go
func scopeOf(r *http.Request) (string, error) {
	if t := r.Header.Get(headerTenant); t != "" {
		return t, nil
	}
	return "", errNoSession
}
```
<!-- /ds:block hash=c333dd -->

<!-- ds:block id=authorizer-uejc9mcn -->
**authorizer** · [`examples/records-http/main.go:138-157`](main.go#L138-L157)

```go
func authorizer() func(*http.Request, recordshttp.Action, recordshttp.Target) error {
	return func(r *http.Request, action recordshttp.Action, t recordshttp.Target) error {
		user := r.Header.Get(headerUser)
		if user == "" {
			return errNotYours
		}
		if t.Owner.Type != "" {
			if t.Owner.Type != ownerUser || t.Owner.ID != user {
				return errNotYours
			}
			return nil
		}
		// recordshttp loads the file in the request's scope before calling
		// this, so another tenant's file never reaches here (it is a 404).
		if t.File == nil || t.File.UploadedBy != actorPrefix+user {
			return errNotYours
		}
		return nil
	}
}
```
<!-- /ds:block hash=340a13 -->

The signed-in user as the actor, recorded as `uploaded_by`, and the mux with the records endpoints under `/media` and signed downloads under `/files`:

<!-- ds:block id=withactor-5rp58uu3 -->
**withActor** · [`examples/records-http/main.go:162-169`](main.go#L162-L169)

```go
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), actorPrefix+user))
		}
		next.ServeHTTP(w, r)
	})
}
```
<!-- /ds:block hash=398966 -->

<!-- ds:block id=newhandler-ytttpkep -->
**newHandler** · [`examples/records-http/main.go:174-191`](main.go#L174-L191)

```go
func newHandler(s *records.Store, disks *filesystem.Manager, signer *signedurl.Signer) (http.Handler, error) {
	media, err := recordshttp.Handler(recordshttp.Config{
		Store:        s,
		Disk:         diskName,
		Policy:       policy,
		MaxFiles:     maxFilesPerRequest,
		Scope:        scopeOf,
		Authorize:    authorizer(),
		LinkLifetime: linkLifetime,
	})
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/media/", http.StripPrefix("/media", withActor(media)))
	mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer)))
	return mux, nil
}
```
<!-- /ds:block hash=17e177 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The server: policy, disks, Store, scope and authorization hooks, the handler |
| `perdisk.go` | The `/byo` routes: `DiskFor` puts each tenant's uploads on its own bucket's disk; code: [`newBucketHandler`](ds:block?id=newbuckethandler-k6t9z4mr) |
| `main_test.go` | Drives every endpoint through `httptest`, the refusals, an upload of more than `MaxFiles` files that rolls back with its objects, and the per-tenant disk with its refusal |

## Next

- The guide: [HTTP endpoints](../../docs/records/http-endpoints.md).
- Related examples: [records-attachments](../records-attachments) for the same operations in Go, [integrations-nethttp](../integrations-nethttp) for `fshttp`, [quickstart](../quickstart) for a smaller app with its own handlers.
