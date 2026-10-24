# net/http, gorilla and chi

You will have every HTTP endpoint the library ships served by one Go program: multipart form uploads, direct-upload tickets, Uppy multipart uploads, tus resumable uploads, signed downloads and uploads, public files, and the records endpoints that upload, attach, list and link files. The same handlers mount on the standard library's `http.ServeMux`, on gorilla/mux and on chi; this page shows all three, and the gin, echo and fiber pages reuse the wiring.

Read this page if you are wiring the library into a web server for the first time, or if a handler answers 404 or 405 where you expect it to work: almost every such problem is a mount point or a missing prefix strip.

## Before you start

- [Your first app](../start/first-app.md) covers disks, policies and records step by step; this page assumes them and changes only the routing.
- The program is [`examples/integrations-nethttp`](../../examples/integrations-nethttp/main.go). `go run ./integrations-nethttp` in `examples/` serves it on `http://localhost:8090`; `go run ./integrations-nethttp -gorilla` serves the same routes through gorilla/mux; `go test ./integrations-nethttp` drives every route through both routers.
- chi lives in the separate framework module, [`examples/frameworks/chi`](../../examples/frameworks/chi/main.go), so the main examples module does not depend on it.
- On chi, the adapter module `github.com/ubgo/filesystem/contrib/chi` (`fschi`) adds what this page leaves to you: mounting under sub-routers with the prefix stripped, the signed-in user and the tenant resolved once in middleware, the library's errors rendered by `fschi.Error`, and zips with `fschi.Zip`. [Framework adapters](framework-adapters.md) shows it, run from [`examples/frameworks/adapters/chi`](../../examples/frameworks/adapters/chi/main.go).
- Everything runs in one process: two memory disks, an S3 disk on an in-process fake S3 server (Uppy and presigned uploads need a backend that can sign its own URLs), and the records tables in an in-memory SQLite database. The `X-User` request header stands in for your authentication.

## The handlers

Every endpoint is a plain `http.Handler`. None of them knows its own mount point: each routes on the path below it, which is why each is mounted with its prefix stripped.

| Handler | Constructor | Routes below its mount point | Needs |
|---|---|---|---|
| Form uploads | `upload.Handler(disk, policy, upload.HandlerOptions{})` | `POST` (multipart, field `file`) | any disk |
| Direct-upload tickets | `upload.PresignHandler(disk, policy, expires)` | `POST` a JSON `TicketRequest` | a disk that can sign PUTs (`Presign` capability, or a `URLSigner` and `fshttp.Signed` mounted) |
| Uppy multipart | `upload.Uppy(disk, policy, upload.UppyOptions{})` | `POST /multipart`, `GET /multipart/{id}/{part}`, `GET /multipart/{id}`, `POST /multipart/{id}/complete`, `DELETE /multipart/{id}` | `Multipart` and `PresignPart` (S3-compatible disks); any other disk answers 501 on every route |
| tus resumable uploads | `tus.New(tus.Config{…})` from `contrib/tus` | tus 1.0 `POST`, `HEAD`, `PATCH`, `DELETE`, `OPTIONS` | a local temporary directory; `BasePath` equal to the mount point |
| Signed links | `fshttp.Signed(manager, signer)` | `GET`/`HEAD` (download), `PUT` (upload) at `/{disk}/{key}` | the `signedurl.Signer` whose base URL points at this mount point |
| Public files | `fshttp.Public(disk)` | `GET`/`HEAD` at `/{key}` | a disk whose objects (or default) are public |
| Records | `recordshttp.Handler(recordshttp.Config{…})` | see [HTTP endpoints](../records/http-endpoints.md) | a `records.Store`, and the `Scope` and `Authorize` hooks |

Each handler answers errors in its package's default shape (plain status text from `fshttp`, `{"error","code"}` JSON from `upload`) unless its options carry an `fshttp.ErrorWriter` (`Errors`), which renders every error in your API's own envelope; see [Your own error envelope](../concepts/errors.md#your-own-error-envelope).

## Step 1 — Build the handlers once

One policy applies to every upload route, so the type and size rules cannot drift between them. The type is detected from the bytes, never taken from the client:

<!-- ds:block id=policy-9pjq8nta -->
**policy** · [`examples/integrations-nethttp/main.go:98-102`](../../examples/integrations-nethttp/main.go#L98-L102)

```go
var policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"image/*", "application/pdf", "text/plain"},
	Key:     upload.RandomKey("incoming"),
}
```
<!-- /ds:block hash=6fbefa -->

The handlers are built at start-up, so a missing hook or an invalid policy fails there rather than on the first request. `Handlers` is the set every router mounts:

<!-- ds:block id=handlers-85duqk3k -->
**Handlers** · [`examples/integrations-nethttp/main.go:159-167`](../../examples/integrations-nethttp/main.go#L159-L167)

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

<!-- ds:block id=newhandlers-y3r6cbah -->
**newHandlers** · [`examples/integrations-nethttp/main.go:273-291`](../../examples/integrations-nethttp/main.go#L273-L291)

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

The signed-link handler serves what the signer issues, so the signer's base URL must be the public URL of the mount point (`baseURL + "/files"` here). A memory or local disk has no signing of its own; `filesystem.WithURLSigner(signer)` gives it signed URLs that `fshttp.Signed` verifies. The public disk gets its URL base from the `public_url` DSN parameter, again pointing at its mount point:

<!-- ds:block id=newsigner-hv3c3az5 -->
**newSigner** · [`examples/integrations-nethttp/main.go:197-203`](../../examples/integrations-nethttp/main.go#L197-L203)

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

<!-- ds:block id=newdisks-2t595xpg -->
**newDisks** · [`examples/integrations-nethttp/main.go:209-231`](../../examples/integrations-nethttp/main.go#L209-L231)

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

Check: `go test ./integrations-nethttp` passes, and `go run ./integrations-nethttp` logs `listening on http://localhost:8090`.

## Step 2 — Connect the records endpoints to your users

`recordshttp.Handler` refuses to build without `Scope` and `Authorize`: the library has no notion of users, and an endpoint set without authorization would be a data leak. `Scope` maps the request to its tenant (an error answers 401); `Authorize` decides each action on each target (an error answers 403). Neither hook's error message reaches the client.

<!-- ds:block id=newrecordshandler-365kkwzk -->
**newRecordsHandler** · [`examples/integrations-nethttp/main.go:297-312`](../../examples/integrations-nethttp/main.go#L297-L312)

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

The example lets a user act on their own owner routes and on files they uploaded. A file route carries only a file ID, so the hook loads the file and compares `UploadedBy`, which the Store fills from the actor in the context:

<!-- ds:block id=authorize-u2cj265k -->
**authorize** · [`examples/integrations-nethttp/main.go:323-338`](../../examples/integrations-nethttp/main.go#L323-L338)

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
**withUser** · [`examples/integrations-nethttp/main.go:343-350`](../../examples/integrations-nethttp/main.go#L343-L350)

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

In a real app `withUser` is your authentication middleware: it already knows the user, and adding `records.WithActor` there is one line.

## Step 3 — Mount on http.ServeMux

<!-- ds:block id=newservemux-usseuwm9 -->
**newServeMux** · [`examples/integrations-nethttp/main.go:371-381`](../../examples/integrations-nethttp/main.go#L371-L381)

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

The rules every router must follow:

| Handler | Mount pattern | Strip | Why |
|---|---|---|---|
| Form upload, ticket | the exact path | nothing | they answer one route and ignore the path |
| Uppy | `/uploads/s3/` (a subtree) | `/uploads/s3` | it routes `/multipart/…` itself |
| tus | `/tus/` | `/tus/` with the trailing slash | tusd builds upload URLs from `tus.Config.BasePath` and expects the ID alone in the path |
| Signed links | `/files/` | `/files` | the path below is `/{disk}/{key}` |
| Public files | `/public/` | `/public` | the path below is `/{key}` |
| Records | `/media/` | `/media` | it routes `/owners/…`, `/files/…`, `/attachments/…` itself |

Check: a form upload answers 201 with what was stored. The content type comes from the bytes:

```sh
curl -s -F file=@report.txt localhost:8090/upload
```

```json
{"files":[{"key":"incoming/pxhe2b2kumvcbhndfr7db6dpgp.txt","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"sha256":"4c694ad7a5ea27610e73d5dca732d67b51100682543877a8a882584667371a9d"}]}
```

An HTML file is refused whatever its name says, and nothing is stored:

```sh
curl -s -w '%{http_code}\n' -F file=@page.html localhost:8090/upload
```

```
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
```

A wrong method on an exact-path route is the router's answer, not the library's: `curl -X DELETE localhost:8090/upload` gets `http.ServeMux`'s plain 405, because the pattern `POST /upload` never lets the request reach the handler.

## Step 4 — Upload, attach and link through the records endpoints

Without a user the scope hook refuses (401); another user is refused by `Authorize` (403); the owner gets 201 with the attachment:

```sh
curl -s -w '%{http_code}\n' -F file=@report.txt localhost:8090/media/owners/user/alice/docs
curl -s -w '%{http_code}\n' -H 'X-User: bob' -F file=@report.txt localhost:8090/media/owners/user/alice/docs
curl -s -H 'X-User: alice' -F file=@report.txt localhost:8090/media/owners/user/alice/docs
```

```
{"error":"not signed in","code":"permission"}
401
{"error":"forbidden","code":"permission"}
403
{"files":[{"attachment_id":"att_p7d5rqihotznvrvpsskek7k6o2","file_id":"fil_3qcbqdh4fcnf2veougcwaduovk","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","status":"ready"}]}
```

Listing the owner's files returns each with a short-lived link, and the file route redirects to one:

```sh
curl -s -H 'X-User: alice' localhost:8090/media/owners/user/alice/docs
curl -s -i -H 'X-User: alice' localhost:8090/media/files/fil_3qcbqdh4fcnf2veougcwaduovk
```

```
{"files":[{"attachment_id":"att_p7d5rqihotznvrvpsskek7k6o2","file_id":"fil_3qcbqdh4fcnf2veougcwaduovk","filename":"report.txt","content_type":"text/plain; charset=utf-8","size":18,"position":1,"uploaded_by":"alice","url":"http://localhost:8090/files/media/incoming/6zczlglx7plu4gzd2vkalredzb.txt?ct=text%2Fplain%3B+charset%3Dutf-8\u0026exp=1791267962\u0026kid=k1\u0026m=GET\u0026sig=befe179b26920a86bcbd9d8e1bf5679644626a385a30b8c35cbf1d0ba0512a96","status":"ready"}]}
HTTP/1.1 302 Found
Cache-Control: private, no-store
Content-Type: text/html; charset=utf-8
Location: http://localhost:8090/files/media/incoming/6zczlglx7plu4gzd2vkalredzb.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791267962&kid=k1&m=GET&sig=befe179b26920a86bcbd9d8e1bf5679644626a385a30b8c35cbf1d0ba0512a96
Date: Tue, 06 Oct 2026 06:16:02 GMT
```

The link is served by `fshttp.Signed`, with ranges, an ETag and the two headers that stop user content from running script on your origin:

```sh
curl -s -i -H 'Range: bytes=0-8' "$LINK"
```

```
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 9
Content-Range: bytes 0-8/18
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/plain; charset=utf-8
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
Last-Modified: Tue, 06 Oct 2026 06:16:02 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:16:02 GMT

quarterly
```

Check: change one character of the `sig` parameter and the same request answers `403` with no detail. An expired link, a wrong method and an unknown key ID answer the same way, so a forger learns nothing.

## Step 5 — Direct uploads: tickets and Uppy

A ticket authorises one PUT straight to the bucket. The declared type and size are checked against the policy, and the signed URL pins the content type:

```sh
curl -s -X POST localhost:8090/uploads/ticket -d '{"filename":"scan.pdf","content_type":"application/pdf","size":1024}'
```

```json
{"key":"incoming/463ldnspetqockgt5ywtgjiuvv.pdf","method":"PUT","url":"http://127.0.0.1:61554/uploads/incoming/463ldnspetqockgt5ywtgjiuvv.pdf?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=example%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T061602Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=content-type%3Bhost\u0026x-id=PutObject\u0026X-Amz-Signature=dd648b9e100bdebf46447cb8f5b169a28f6298f3575e72b15c290f6bfefde7ff","headers":{"Content-Type":["application/pdf"]},"expires_at":"2026-10-06T12:01:02.72569+05:30"}
```

The declared type is the client's word. After the PUT, your app must call `upload.Confirm(ctx, disk, key, filename, policy)`, which reads the stored bytes, deletes the object if the policy refuses them, and runs `Policy.Inspect`. The library ships no confirm route because confirming is where your app records the file: see [browser direct uploads](../recipes/browser-direct-uploads.md).

Uppy's AWS S3 plugin speaks to `upload.Uppy`. Create returns an upload ID and the key; each part is signed for a direct PUT to the bucket; complete assembles the object and checks it with `Confirm`:

```sh
curl -s -X POST localhost:8090/uploads/s3/multipart -H 'X-User: alice' -d '{"filename":"notes.txt","type":"text/plain"}'
curl -s -H 'X-User: alice' "localhost:8090/uploads/s3/multipart/1/1?key=incoming/tab53jzu45prh3ln45i4fnlq2i.txt"
curl -s -D - -o /dev/null -X PUT --data-binary @report.txt "$PART_URL" | grep -i etag
curl -s -H 'X-User: alice' "localhost:8090/uploads/s3/multipart/1?key=incoming/tab53jzu45prh3ln45i4fnlq2i.txt"
curl -s -H 'X-User: alice' -X POST "localhost:8090/uploads/s3/multipart/1/complete?key=incoming/tab53jzu45prh3ln45i4fnlq2i.txt" -d '{"parts":[{"PartNumber":1,"ETag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}]}'
```

```
{"uploadId":"1","key":"incoming/tab53jzu45prh3ln45i4fnlq2i.txt"}
{"url":"http://127.0.0.1:61554/uploads/incoming/tab53jzu45prh3ln45i4fnlq2i.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=example%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T061602Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=host\u0026partNumber=1\u0026uploadId=1\u0026x-id=UploadPart\u0026X-Amz-Signature=fb6fc9932d7d895abf10db781fdd5814cafbd43d8f057ca338976c94b192b7c0"}
Etag: "f2b6df39099bb4eb5d30e7e7fa0e8ba6"
[{"PartNumber":1,"Size":18,"ETag":"f2b6df39099bb4eb5d30e7e7fa0e8ba6"}]
{"location":"","key":"incoming/tab53jzu45prh3ln45i4fnlq2i.txt"}
```

`location` is empty because the bucket disk has no public URL; the client keeps the key. The upload ID and key in every request after create come from the client, so `UppyOptions.Authorize` runs on each of them; without it any user could complete or abort another's upload. The example's hook requires a user and a key under the upload area:

<!-- ds:block id=allowuppy-peqz35h9 -->
**allowUppy** · [`examples/integrations-nethttp/main.go:356-364`](../../examples/integrations-nethttp/main.go#L356-L364)

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

Check: the same list request without `X-User` answers `{"error":"Forbidden","code":"permission"}` with status 403.

## Step 6 — Resumable uploads with tus

`contrib/tus` embeds tusd. A client creates the upload, sends bytes with `PATCH` (resuming with `HEAD` after a dropped connection), and the final `PATCH` returns only after the file has passed the policy and been stored; the key comes back in `Filesystem-Key`:

```sh
curl -s -i -X POST localhost:8090/tus/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 18' \
  -H "Upload-Metadata: filename $(printf notes.txt | base64),filetype $(printf text/plain | base64)"
curl -s -i -X PATCH http://localhost:8090/tus/ed76cbec0385b3f43fc71da7b0077b13 -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Offset: 0' -H 'Content-Type: application/offset+octet-stream' --data-binary @report.txt
```

```
HTTP/1.1 201 Created
Location: http://localhost:8090/tus/ed76cbec0385b3f43fc71da7b0077b13
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:18 GMT
Content-Length: 0

HTTP/1.1 204 No Content
Filesystem-Key: incoming/sdpvicadu4qr3pp5fhmrvrbjov.txt
Tus-Resumable: 1.0.0
Upload-Offset: 18
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:18 GMT
```

A declared type the policy refuses is stopped at create, before any byte is sent (`Upload-Metadata` declaring `filetype text/html`):

```
HTTP/1.1 415 Unsupported Media Type
Content-Length: 54
Content-Type: text/plain; charset=utf-8
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:25:18 GMT

ERR_REJECTED: content type "text/html" is not allowed
```

tusd logs every request at Info. `tus.Config.Logger` takes the `*slog.Logger` it writes to (nil means `slog.Default()`); give it a handler at `slog.LevelWarn` to keep only failures. Behind a TLS-terminating proxy, set `tus.Config.RespectForwardedHeaders` so the `Location` URLs use the proxy's scheme and host.

## gorilla/mux

gorilla's `PathPrefix` plays the part of a trailing-slash pattern; everything else is the same, including `http.StripPrefix`:

<!-- ds:block id=newgorilla-et5p2jev -->
**newGorilla** · [`examples/integrations-nethttp/main.go:386-396`](../../examples/integrations-nethttp/main.go#L386-L396)

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

Check: `go run ./integrations-nethttp -gorilla` answers the transcripts above byte for byte; `TestGorilla` runs the same requests as `TestServeMux`.

## chi

chi takes `http.Handler` values as they are. Its `Mount` does not rewrite `r.URL.Path` for a handler that is not a chi router, so the prefix is stripped with `http.StripPrefix` exactly as on `http.ServeMux`:

<!-- ds:block id=newrouter-2z3crjca -->
**newRouter** · [`examples/frameworks/chi/main.go:70-80`](../../examples/frameworks/chi/main.go#L70-L80)

```go
func newRouter(h app.Handlers) chi.Router {
	r := chi.NewRouter()
	r.Method(http.MethodPost, app.PathUpload, h.Upload)
	r.Method(http.MethodPost, app.PathTicket, h.Ticket)
	r.Handle(app.PathUppy+"/*", http.StripPrefix(app.PathUppy, h.Uppy))
	r.Handle(app.PathTus+"*", http.StripPrefix(app.PathTus, h.Tus))
	r.Handle(app.PathFiles+"/*", http.StripPrefix(app.PathFiles, h.Files))
	r.Handle(app.PathPublic+"/*", http.StripPrefix(app.PathPublic, h.Public))
	r.Handle(app.PathRecords+"/*", http.StripPrefix(app.PathRecords, h.Records))
	return r
}
```
<!-- /ds:block hash=e192b1 -->

The chi program shares its wiring with the gin, echo and fiber programs ([`examples/frameworks/internal/app`](../../examples/frameworks/internal/app/app.go)); run it with `go run ./chi` from `examples/frameworks` (port 8094), test it with `go test ./chi`. A wrong method on `/upload` answers chi's 405 with `Allow: POST`.

## Pitfalls

- **A 404 from every library route** means the prefix was not stripped: the handler saw `/files/media/…` instead of `/media/…`. Wrap each subtree handler in `http.StripPrefix(mountPoint, h)`.
- **tus answers 404 or builds wrong `Location` URLs** when `tus.Config.BasePath` differs from the mount point, or when the prefix is stripped without its trailing slash. Use the same string, slash included, in all three places.
- **Signed links 404 or 403** when the signer's base URL is not where `fshttp.Signed` is mounted, or when the disk named in the link is not in the `Manager` passed to `fshttp.Signed`. A scoped disk (`disk.Scope("tenant-a", …)`) signs links with its own name, so add scoped disks to the Manager too.
- **Middleware that reads the body first** (calling `r.ParseMultipartForm` or `r.FormValue` for logging or CSRF) leaves the upload handlers an empty body; they answer `{"code":"invalid_argument"}` with no file. Let the upload routes skip it.
- **Server timeouts cut large uploads.** `http.Server.ReadTimeout` covers the whole request body. Set `ReadHeaderTimeout` as the example does and bound slow uploads per route instead; the upload handlers already cap the size with `http.MaxBytesReader`.
- **Compression middleware on signed downloads** breaks range requests and ETags. Exclude the `/files/` and `/public/` subtrees.
- **Direct uploads from a browser** go to the bucket's origin, not yours: the bucket needs a CORS rule allowing `PUT` from your origin and exposing the `ETag` header, or Uppy cannot read part ETags.

## Next

- [gin](gin.md), [echo](echo.md), [fiber](fiber.md): the same handlers on other routers.
- [HTTP endpoints](../records/http-endpoints.md): every route and response of `recordshttp`.
- [Serving files](../concepts/serving-files.md) and [Uploads](../concepts/uploads.md): what each handler does and why.
- [Errors](../reference/errors.md): the status every error code answers with.
