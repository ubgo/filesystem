# Integrations: every HTTP handler on net/http and gorilla/mux

Every HTTP handler the library ships, mounted on a standard library `http.ServeMux`, and the same handler values on a gorilla/mux router: form uploads (`upload.Handler`), direct-upload tickets (`upload.PresignHandler`), Uppy multipart (`upload.Uppy`), tus resumable uploads (`contrib/tus`), signed downloads (`fshttp.Signed`), public files (`fshttp.Public`) and the records endpoints (`recordshttp`). It answers "how do I put file storage behind my API" for an app on the standard library or any router that takes an `http.Handler`. It uses `github.com/ubgo/filesystem`, `upload`, `fshttp`, `signedurl`, `contrib/tus`, `records`, `records/recordshttp` and `adapters/s3`. The guide is [net/http, gorilla and chi](../../docs/integrations/net-http.md).

## Run it

```sh
cd examples
go run ./integrations-nethttp            # http.ServeMux on localhost:8090
go run ./integrations-nethttp -gorilla   # the same routes on gorilla/mux
go test ./integrations-nethttp
```

It listens on `http://localhost:8090` until you press Ctrl+C. Everything runs in-process: two memory disks (`media` for private files behind signed links, `public` for public files), an S3 disk (`bucket`) on a fake S3 server (gofakes3) on a random loopback port, because tickets and Uppy need a backend that signs, and the records tables in an in-memory SQLite database. Nothing survives a restart, and the signing secret is random per start, so links die with the process. tus keeps unfinished uploads in a `tus-sessions-*` directory under the system temp directory. Ctrl+C or SIGTERM stops the server cleanly: requests in flight get up to five seconds to finish, the fake S3 stops, the tus directory is removed, and the process exits 0. The `X-User` header stands in for the app's own authentication. The tests run every step below on both routers.

## Walkthrough

The examples use `report.txt` holding `quarterly numbers` and a newline (18 bytes), and `page.html` holding a small HTML page.

1. A form upload answers 201 with what was stored; the content type comes from the bytes. An HTML file is refused whatever its name says, and nothing is stored:

```sh
curl -s -F file=@report.txt localhost:8090/upload
curl -s -w '%{http_code}\n' -F file=@page.html localhost:8090/upload
```

```text
{"files":[{"key":"incoming/t4qdi2xfavpq4hhupf4rmotsck.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
```

A wrong method is the router's answer: `curl -X DELETE localhost:8090/upload` gets a plain `Method Not Allowed` (405) because the route is `POST /upload`.

2. The records endpoints: without a user the scope hook refuses (401), another user is refused by `Authorize` (403), and the owner gets the attachment:

```sh
curl -s -w '%{http_code}\n' -F file=@report.txt localhost:8090/media/owners/user/alice/docs
curl -s -w '%{http_code}\n' -H 'X-User: bob' -F file=@report.txt localhost:8090/media/owners/user/alice/docs
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8090/media/owners/user/alice/docs
```

```text
{"error":"not signed in","code":"permission"}
401
{"error":"forbidden","code":"permission"}
403
{"files":[{"attachment_id":"att_mf4bn6mthbbak5tydyeebyeegm","file_id":"fil_z2zyneay7ln6pqq6jdx4ylup5u","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
```

3. Listing the owner's files returns each with a short-lived signed link, and the file route redirects to a fresh one:

```sh
curl -s -H 'X-User: alice' localhost:8090/media/owners/user/alice/docs
curl -s -i -H 'X-User: alice' localhost:8090/media/files/fil_z2zyneay7ln6pqq6jdx4ylup5u
```

```text
{"files":[{"attachment_id":"att_mf4bn6mthbbak5tydyeebyeegm","file_id":"fil_z2zyneay7ln6pqq6jdx4ylup5u","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","url":"http://localhost:8090/files/media/incoming/dy2nkbfh5j432zabwy2ji4plsa.txt?ct=text%2Fplain%3B+charset%3Dutf-8\u0026exp=1791268518\u0026kid=k1\u0026m=GET\u0026sig=2e2c1350996f11cd93758a752ea19dcb34c2f999c0987b864c34811a3ed53fbd","status":"ready"}]}
HTTP/1.1 302 Found
Cache-Control: private, no-store
Content-Type: text/html; charset=utf-8
Location: http://localhost:8090/files/media/incoming/dy2nkbfh5j432zabwy2ji4plsa.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791268518&kid=k1&m=GET&sig=2e2c1350996f11cd93758a752ea19dcb34c2f999c0987b864c34811a3ed53fbd
Date: Tue, 06 Oct 2026 06:25:18 GMT
Content-Length: 244
```

4. `fshttp.Signed` serves the link with ranges, an ETag, and the two headers that stop user content from running script on your origin. Change one character of `sig` and the answer is a bare 403:

```sh
curl -s -i -H 'Range: bytes=0-8' "$LINK"
curl -s -w '%{http_code}\n' "$TAMPERED_LINK"
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
Last-Modified: Tue, 06 Oct 2026 06:25:18 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:18 GMT

quarterlyForbidden
403
```

5. A ticket authorises one PUT straight to the bucket; the declared type and size are checked against the policy and the signed URL pins the content type. The declared type is the client's word: after the PUT the app must call `upload.Confirm`, which this program does not route (see [browser direct uploads](../recipe-browser-direct-uploads)).

```sh
curl -s -X POST localhost:8090/uploads/ticket -d '{"filename":"scan.pdf","content_type":"application/pdf","size":1024}'
```

```json
{"key":"incoming/no3stj2sjnifmnloirbxgysnst.pdf","method":"PUT","url":"http://127.0.0.1:52771/uploads/incoming/no3stj2sjnifmnloirbxgysnst.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=example%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T062518Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=content-type%3Bhost\u0026x-id=PutObject\u0026X-Amz-Signature=84fed94247efe377079603ba84ae6735c24174e9ad2d9e5caa1fe70941d09686","headers":{"Content-Type":["application/pdf"]},"expires_at":"2026-10-06T12:10:18.958176+05:30"}
```

6. Uppy's AWS S3 multipart protocol: create, sign a part, PUT the part straight to the bucket, list the parts, complete. Without `X-User` the `Authorize` hook refuses:

```sh
curl -s -X POST localhost:8090/uploads/s3/multipart -H 'X-User: alice' -d '{"filename":"notes.txt","type":"text/plain"}'
curl -s -H 'X-User: alice' "localhost:8090/uploads/s3/multipart/1/1?key=incoming/xc6kt23y3dam3oe3s37fthihxt.txt"
curl -s -D - -o /dev/null -X PUT --data-binary @report.txt "$PART_URL" | grep -i etag
curl -s -H 'X-User: alice' "localhost:8090/uploads/s3/multipart/1?key=incoming/xc6kt23y3dam3oe3s37fthihxt.txt"
curl -s -H 'X-User: alice' -X POST "localhost:8090/uploads/s3/multipart/1/complete?key=incoming/xc6kt23y3dam3oe3s37fthihxt.txt" -d '{"parts":[{"PartNumber":1,"ETag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}]}'
curl -s -w '%{http_code}\n' "localhost:8090/uploads/s3/multipart/1?key=incoming/xc6kt23y3dam3oe3s37fthihxt.txt"
```

```text
{"uploadId":"1","key":"incoming/xc6kt23y3dam3oe3s37fthihxt.txt"}
{"url":"http://127.0.0.1:52771/uploads/incoming/xc6kt23y3dam3oe3s37fthihxt.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=example%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T062519Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=host\u0026partNumber=1\u0026uploadId=1\u0026x-id=UploadPart\u0026X-Amz-Signature=d8874946b1ea95ab7282945494d24012aa11e706c543aa378520ebab1f9df73c"}
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
[{"PartNumber":1,"Size":18,"ETag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}]
{"location":"","key":"incoming/xc6kt23y3dam3oe3s37fthihxt.txt"}
{"error":"Forbidden","code":"permission"}
403
```

`location` is empty because the bucket disk has no public URL; the client keeps the key.

7. tus: create the upload, then send the bytes. The final `PATCH` returns only after the file passed the policy and was stored, with the key in `Filesystem-Key`. A declared type the policy refuses is stopped at create, before any byte is sent:

```sh
curl -s -i -X POST localhost:8090/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf notes.txt | base64),filetype $(printf text/plain | base64)"
curl -s -i -X PATCH http://localhost:8090/tus/54d7f4877025092d48a026d5c0b2c9fc -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @report.txt
curl -s -i -X POST localhost:8090/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf page.html | base64),filetype $(printf text/html | base64)"
```

```text
HTTP/1.1 201 Created
Location: http://localhost:8090/tus/54d7f4877025092d48a026d5c0b2c9fc
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:19 GMT
Content-Length: 0

HTTP/1.1 204 No Content
Filesystem-Key: incoming/meodpactlw34qfaw43mrkkaokv.txt
Tus-Resumable: 1.0.0
Upload-Offset: 18
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:19 GMT

HTTP/1.1 415 Unsupported Media Type
Content-Length: 54
Content-Type: text/plain; charset=utf-8
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:19 GMT

ERR_REJECTED: content type "text/html" is not allowed
```

Stop the server with Ctrl+C; it shuts down cleanly and removes its tus directory.

## The code

`policy` is the one upload policy every route enforces: images, PDFs and plain text up to 10 MiB, checked from the bytes, stored under `incoming/` with random names.

<!-- ds:block id=policy-9pjq8nta -->
**policy** · [`examples/integrations-nethttp/main.go:98-102`](main.go#L98-L102)

```go
var policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey("incoming"),
}
```
<!-- /ds:block hash=6fbefa -->

`Handlers` holds one plain `http.Handler` per library endpoint, which is why the same values mount on net/http, gorilla, chi, gin, echo or fiber.

<!-- ds:block id=handlers-85duqk3k -->
**Handlers** · [`examples/integrations-nethttp/main.go:159-167`](main.go#L159-L167)

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

`newSigner` makes the HMAC signer for the memory disks, whose backend cannot sign. Production loads the secret from a secret store and keeps old keys listed while links signed with them are alive.

<!-- ds:block id=newsigner-hv3c3az5 -->
**newSigner** · [`examples/integrations-nethttp/main.go:197-203`](main.go#L197-L203)

```go
func newSigner(base string) (*signedurl.Signer, error) {
	secret := make([]byte, signingKeySize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return signedurl.New(base, []signedurl.Key{{ID: signingKeyID, Secret: secret}})
}
```
<!-- /ds:block hash=7a5285 -->

`newDisks` opens the memory disks from DSNs, so the same code takes `s3://…` or `file:///…` in production; the S3 disk is built from a client because its endpoint is the in-process fake.

<!-- ds:block id=newdisks-2t595xpg -->
**newDisks** · [`examples/integrations-nethttp/main.go:209-231`](main.go#L209-L231)

```go
func newDisks(ctx context.Context, baseURL, s3URL string, signer *signedurl.Signer) (Disks, error) {
	m := filesystem.NewMux()
	if err := m.Register(memory.Scheme, memory.Opener); err != nil {
		return Disks{}, err
	}
	media, err := m.Open(ctx, diskMedia, "mem://media", filesystem.WithURLSigner(signer))
	if err != nil {
		return Disks{}, err
	}
	public, err := m.Open(ctx, diskPublic, "mem://public?visibility=public&public_url="+url.QueryEscape(baseURL+pathPublic))
	if err != nil {
		return Disks{}, err
	}
	bucket, err := newBucket(ctx, s3URL)
	if err != nil {
		return Disks{}, err
	}
	manager, err := filesystem.NewManager(media, public, bucket)
	if err != nil {
		return Disks{}, err
	}
	return Disks{Manager: manager, Media: media, Public: public, Bucket: bucket}, nil
}
```
<!-- /ds:block hash=fa1893 -->

`newHandlers` builds every endpoint once, so construction errors (a missing hook, an invalid policy) surface at start-up.

<!-- ds:block id=newhandlers-y3r6cbah -->
**newHandlers** · [`examples/integrations-nethttp/main.go:273-291`](main.go#L273-L291)

```go
func newHandlers(disks Disks, store *records.Store, signer *signedurl.Signer, tusDir string) (Handlers, error) {
	tusHandler, err := tus.New(tus.Config{Disk: disks.Media, Policy: policy, TempDir: tusDir, BasePath: pathTus})
	if err != nil {
		return Handlers{}, err
	}
	recordsHandler, err := newRecordsHandler(store)
	if err != nil {
		return Handlers{}, err
	}
	return Handlers{
		Upload:  upload.Handler(disks.Media, policy, upload.HandlerOptions{MaxFiles: maxFormFiles}),
		Ticket:  upload.PresignHandler(disks.Bucket, policy, ticketLifetime),
		Uppy:    upload.Uppy(disks.Bucket, policy, upload.UppyOptions{Authorize: allowUppy}),
		Tus:     tusHandler,
		Files:   fshttp.Signed(disks.Manager, signer),
		Public:  fshttp.Public(disks.Public),
		Records: withUser(recordsHandler),
	}, nil
}
```
<!-- /ds:block hash=bc1093 -->

`newRecordsHandler` configures `recordshttp`. `Scope` and `Authorize` are required: the library knows nothing about users, so these two hooks are where the app's authentication decides. `authorize` lets a user act on their own owner routes and on files they uploaded, and `withUser` records the user as the actor of every change.

<!-- ds:block id=newrecordshandler-365kkwzk -->
**newRecordsHandler** · [`examples/integrations-nethttp/main.go:297-312`](main.go#L297-L312)

```go
func newRecordsHandler(store *records.Store) (http.Handler, error) {
	return recordshttp.Handler(recordshttp.Config{
		Store:  store,
		Disk:   diskMedia,
		Policy: policy,
		Scope: func(r *http.Request) (string, error) {
			if r.Header.Get(headerUser) == "" {
				return "", errSignedOut
			}
			return tenant, nil
		},
		Authorize: func(r *http.Request, _ recordshttp.Action, t recordshttp.Target) error {
			return authorize(r.Context(), store, r.Header.Get(headerUser), t)
		},
	})
}
```
<!-- /ds:block hash=6afa34 -->

<!-- ds:block id=authorize-u2cj265k -->
**authorize** · [`examples/integrations-nethttp/main.go:323-338`](main.go#L323-L338)

```go
func authorize(ctx context.Context, store *records.Store, user string, t recordshttp.Target) error {
	if t.FileID == "" {
		if t.Owner.Type == ownerUser && t.Owner.ID == user {
			return nil
		}
		return errNotYours
	}
	f, err := store.File(ctx, t.Scope, t.FileID)
	if err != nil {
		return err
	}
	if f.UploadedBy != user {
		return errNotYours
	}
	return nil
}
```
<!-- /ds:block hash=d443cc -->

<!-- ds:block id=withuser-gs93hupd -->
**withUser** · [`examples/integrations-nethttp/main.go:343-350`](main.go#L343-L350)

```go
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := r.Header.Get(headerUser); user != "" {
			r = r.WithContext(records.WithActor(r.Context(), user))
		}
		next.ServeHTTP(w, r)
	})
}
```
<!-- /ds:block hash=015523 -->

`allowUppy` runs on every Uppy request after create, because the upload ID and key come from the client; without it any user could complete or abort another's upload.

<!-- ds:block id=allowuppy-peqz35h9 -->
**allowUppy** · [`examples/integrations-nethttp/main.go:356-364`](main.go#L356-L364)

```go
func allowUppy(r *http.Request, key string) error {
	if r.Header.Get(headerUser) == "" {
		return errSignedOut
	}
	if !filesystem.KeyInDir(key, "incoming") {
		return errNotYours
	}
	return nil
}
```
<!-- /ds:block hash=9253bc -->

`newServeMux` and `newGorilla` mount the same handlers. Each handler routes on the path below its mount point, so each is wrapped in `http.StripPrefix`; tus expects its prefix stripped with the trailing slash.

<!-- ds:block id=newservemux-usseuwm9 -->
**newServeMux** · [`examples/integrations-nethttp/main.go:371-381`](main.go#L371-L381)

```go
func newServeMux(h Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST "+pathUpload, h.Upload)
	mux.Handle("POST "+pathTicket, h.Ticket)
	mux.Handle(pathUppy+"/", http.StripPrefix(pathUppy, h.Uppy))
	mux.Handle(pathTus, http.StripPrefix(pathTus, h.Tus))
	mux.Handle(pathFiles+"/", http.StripPrefix(pathFiles, h.Files))
	mux.Handle(pathPublic+"/", http.StripPrefix(pathPublic, h.Public))
	mux.Handle(pathRecords+"/", http.StripPrefix(pathRecords, h.Records))
	return mux
}
```
<!-- /ds:block hash=e08e36 -->

<!-- ds:block id=newgorilla-et5p2jev -->
**newGorilla** · [`examples/integrations-nethttp/main.go:386-396`](main.go#L386-L396)

```go
func newGorilla(h Handlers) *mux.Router {
	r := mux.NewRouter()
	r.Handle(pathUpload, h.Upload).Methods(http.MethodPost)
	r.Handle(pathTicket, h.Ticket).Methods(http.MethodPost)
	r.PathPrefix(pathUppy + "/").Handler(http.StripPrefix(pathUppy, h.Uppy))
	r.PathPrefix(pathTus).Handler(http.StripPrefix(pathTus, h.Tus))
	r.PathPrefix(pathFiles + "/").Handler(http.StripPrefix(pathFiles, h.Files))
	r.PathPrefix(pathPublic + "/").Handler(http.StripPrefix(pathPublic, h.Public))
	r.PathPrefix(pathRecords + "/").Handler(http.StripPrefix(pathRecords, h.Records))
	return r
}
```
<!-- /ds:block hash=43cf53 -->

`serveUntilDone` serves until `main`'s `signal.NotifyContext` is cancelled by Ctrl+C or SIGTERM, then shuts the server down gracefully and returns, so the deferred clean-up (the fake S3 and the tus directory) runs instead of being skipped by a killed process.

<!-- ds:block id=serveuntildone-wvuc3w29 -->
**serveUntilDone** · [`examples/integrations-nethttp/main.go:419-430`](main.go#L419-L430)

```go
func serveUntilDone(ctx context.Context, srv *http.Server) error {
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
<!-- /ds:block hash=f21579 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, disks, signer, records store, handlers, the auth hooks, both routers and the fake S3 server |
| `main_test.go` | Every walkthrough step against `httptest` servers on both `http.ServeMux` and gorilla/mux |

## Next

- [net/http, gorilla and chi](../../docs/integrations/net-http.md): the guide for this program.
- The same handlers on other routers: [chi](../frameworks/chi), [gin](../frameworks/gin), [echo](../frameworks/echo), [fiber](../frameworks/fiber).
- [HTTP endpoints](../../docs/records/http-endpoints.md): every route and response of `recordshttp`.
- Recipes built on these handlers: [private downloads](../recipe-private-downloads), [resumable uploads](../recipe-resumable-uploads), [browser direct uploads](../recipe-browser-direct-uploads).
