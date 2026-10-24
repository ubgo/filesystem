# Resumable uploads over tus

An upload endpoint whose uploads survive dropped connections: the client sends a file in chunks, and after a failure asks how much arrived and continues from there. It speaks the tus protocol (the one Uppy, tus-js-client and the official iOS, Android and Flutter clients speak) through `contrib/tus`, which embeds tusd. When the last byte arrives, the file goes through the same `upload.Policy` as a form upload (type detected from the bytes, size counted), lands on the disk, and is recorded and attached to the user with `records`, all before the client hears it succeeded. It answers large uploads from phones and flaky networks: video and podcast platforms, field-service apps, anything mobile. The guide is [Resumable uploads](../../docs/recipes/resumable-uploads.md).

## Run it

```sh
cd examples
go run ./recipe-resumable-uploads
```

It creates a new temporary directory (printed at start-up) holding `files/` (the disk), `tus/` (tusd's unfinished sessions) and `app.db` (SQLite with the `fs_` tables), and listens on `http://localhost:8106`. The demo trusts an `X-User` header as the signed-in user; a real app reads its session. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory.

Run the tests with `go test ./recipe-resumable-uploads`.

## Walkthrough

The photo below is a 17,480-byte JPEG called `big.jpg`; any JPEG works if you change the length.

1. Create an upload of a declared length. `Upload-Metadata` is comma-separated `key base64(value)` pairs:

```sh
META="filename $(printf 'holiday.jpg' | base64),filetype $(printf 'image/jpeg' | base64)"
curl -s -i -X POST localhost:8106/uploads/ -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Length: 17480' -H "Upload-Metadata: $META"
```

```text
HTTP/1.1 201 Created
Location: http://localhost:8106/uploads/e99d2ca91d1be611b6677e02682a3f78
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:24:41 GMT
Content-Length: 0
```

2. Send the first 10,000 bytes:

```sh
U=http://localhost:8106/uploads/e99d2ca91d1be611b6677e02682a3f78
head -c 10000 big.jpg > part1; tail -c +10001 big.jpg > part2
curl -s -i -X PATCH $U -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0' -H 'Upload-Offset: 0' \
  -H 'Content-Type: application/offset+octet-stream' --data-binary @part1
```

```text
HTTP/1.1 204 No Content
Tus-Resumable: 1.0.0
Upload-Offset: 10000
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:24:41 GMT
```

The session now sits in the temporary directory as two files, the bytes so far and their state:

```text
-rw-r--r--@ 1 khanakia  staff  10000 Oct  6 11:54 e99d2ca91d1be611b6677e02682a3f78
-rw-r--r--@ 1 khanakia  staff    508 Oct  6 11:54 e99d2ca91d1be611b6677e02682a3f78.info
```

3. The connection drops. Later, the client asks where to continue:

```sh
curl -s -I $U -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0'
```

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Length: 17480
Tus-Resumable: 1.0.0
Upload-Length: 17480
Upload-Metadata: filename aG9saWRheS5qcGc=,filetype aW1hZ2UvanBlZw==
Upload-Offset: 10000
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:24:41 GMT
```

4. It sends the rest from offset 10,000. The answer to the last chunk carries the stored key; the file is recorded under its name from the metadata, and the finished session is gone (`404`):

```sh
curl -s -i -X PATCH $U -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0' -H 'Upload-Offset: 10000' \
  -H 'Content-Type: application/offset+octet-stream' --data-binary @part2
curl -s localhost:8106/files -H 'X-User: u1'
curl -s -I $U -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' | head -1
```

```text
HTTP/1.1 204 No Content
Filesystem-Key: uploads/bxw4yroco5xzlie3bn43l6ggcu.jpg
Tus-Resumable: 1.0.0
Upload-Offset: 17480
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 06:24:41 GMT

[{"file_id":"fil_juzytekctu545ihpx32p76rkaq","key":"uploads/bxw4yroco5xzlie3bn43l6ggcu.jpg","filename":"holiday.jpg","content_type":"image/jpeg","size":17480}]
HTTP/1.1 404 Not Found
```

5. Refused at creation, before any byte is sent: no user, a type the policy does not allow, a length over the 2 GiB limit. tusd answers errors as `text/plain`; the library's codes appear as `ERR_<CODE>`:

```sh
curl -s -X POST localhost:8106/uploads/ -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 10' -w ' %{http_code}\n'
curl -s -X POST localhost:8106/uploads/ -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' -H 'Upload-Length: 40' \
  -H "Upload-Metadata: filename $(printf a.html|base64),filetype $(printf text/html|base64)" -w ' %{http_code}\n'
curl -s -X POST localhost:8106/uploads/ -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' -H 'Upload-Length: 5000000000' -w ' %{http_code}\n'
```

```text
ERR_FORBIDDEN: upload not authorised
 403
ERR_REJECTED: content type "text/html" is not allowed
 415
ERR_MAX_SIZE_EXCEEDED: maximum size exceeded
 413
```

6. A client that declares a JPEG and sends 38 bytes of HTML gets as far as the last chunk. A chunk at the wrong offset is refused by the protocol; the right one finishes the upload, and the policy refuses the bytes:

```sh
printf '<html><body>not a photo</body></html>\n' > evil.html
LOC=$(curl -s -i -X POST localhost:8106/uploads/ -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' -H 'Upload-Length: 38' \
  -H "Upload-Metadata: filename $(printf cat.jpg|base64),filetype $(printf image/jpeg|base64)" \
  | grep -i '^Location' | tr -d '\r' | awk '{print $2}')
curl -s -X PATCH $LOC -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' -H 'Upload-Offset: 5' \
  -H 'Content-Type: application/offset+octet-stream' --data-binary @evil.html -w ' %{http_code}\n'
curl -s -X PATCH $LOC -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' -H 'Upload-Offset: 0' \
  -H 'Content-Type: application/offset+octet-stream' --data-binary @evil.html -w ' %{http_code}\n'
```

```text
ERR_MISMATCHED_OFFSET: mismatched offset
 409
ERR_REJECTED: content type "text/html; charset=utf-8" is not allowed
 415
```

Afterwards the temporary directory is empty and the disk holds only the photo from step 4.

7. Several instances behind a load balancer, without sticky sessions. Run the program with the `shared` argument instead of starting the server: it starts two instances in the process, each a tus handler built by [`newSharedTus`](ds:block?id=app-newsharedtus-r8k3w6vn) with `Config.Store` and `Config.Locker` both shared (a tusd filestore on one directory and one memorylocker stand in for tusd's s3store and a Redis locker). The upload is created and half sent on instance a, then resumed and finished on instance b:

```sh
go run ./recipe-resumable-uploads shared
```

```text
instance a: create 201, first 22 bytes 204
instance b: HEAD sees offset 22, last 22 bytes 204
recorded for u1: image/jpeg, 44 bytes
shared store without a locker: invalid_argument
```

Instance b answers the `HEAD` with the offset instance a wrote, because the session lives in the shared store, and the file is recorded once. `tus.New` refuses a shared store without a locker, since a lock local to one instance would not stop two instances writing the same upload. `Sweep` reads only a `TempDir`; a shared store's sessions expire through the backend's own lifecycle rules.

## The code

`policy` is enforced twice: at creation from what the client declares (length, `filetype`), and on the finished bytes, exactly as for a form upload.

<!-- ds:block id=policy-cf24neuq -->
**policy** · [`examples/recipe-resumable-uploads/main.go:66-70`](main.go#L66-L70)

```go
var policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"video/mp4", "video/quicktime", "image/jpeg", "image/png", "application/pdf"},
	Key:     upload.RandomKey("uploads"),
}
```
<!-- /ds:block hash=f907f0 -->

`withUser` is the demo's authentication middleware, and `userFrom` reads the user back. tus's hooks receive the request context, so they see the same user.

<!-- ds:block id=withuser-cqj5cewu -->
**withUser** · [`examples/recipe-resumable-uploads/main.go:79-86`](main.go#L79-L86)

```go
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := r.Header.Get(headerUser); u != "" {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		}
		next.ServeHTTP(w, r)
	})
}
```
<!-- /ds:block hash=2f0cd9 -->

<!-- ds:block id=userfrom-wgd9ge2c -->
**userFrom** · [`examples/recipe-resumable-uploads/main.go:90-95`](main.go#L90-L95)

```go
func userFrom(ctx context.Context) (string, error) {
	if u, ok := ctx.Value(userKey{}).(string); ok {
		return u, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "sign in first")
}
```
<!-- /ds:block hash=aa9ec4 -->

`newTus` configures the handler: sessions in a local temp directory (the target disk only ever sees finished files that passed the policy), `Authorize` on every request, and `OnStored` for the finished file.

<!-- ds:block id=app-newtus-g2wn6ycj -->
**App.newTus** · [`examples/recipe-resumable-uploads/main.go:134-146`](main.go#L134-L146)

```go
func (a *App) newTus(disk *filesystem.Disk) (http.Handler, error) {
	return tus.New(tus.Config{
		Disk:     disk,
		Policy:   policy,
		TempDir:  a.TempDir,
		BasePath: tusPath,
		Authorize: func(ctx context.Context, _ http.Header) error {
			_, err := userFrom(ctx)
			return err
		},
		OnStored: a.record,
	})
}
```
<!-- /ds:block hash=23469f -->

`record` is `OnStored`: it creates the file row and attaches it to the user in one transaction. Its error deletes the stored object and fails the client's last request, so "done" always means "recorded".

<!-- ds:block id=app-record-gyqamstd -->
**App.record** · [`examples/recipe-resumable-uploads/main.go:153-169`](main.go#L153-L169)

```go
func (a *App) record(ctx context.Context, res upload.Result, meta map[string]string) error {
	user, err := userFrom(ctx)
	if err != nil {
		return err
	}
	return a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.CreateFile(ctx, records.NewFile{
			Disk: diskName, Key: res.Key, Filename: res.Filename,
			ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256, UploadedBy: user,
		})
		if err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleUploads)
		return err
	})
}
```
<!-- /ds:block hash=a22cf5 -->

`New` mounts the handler under `/uploads/` (tus builds upload URLs from that path, so they must match) next to `GET /files`.

<!-- ds:block id=new-sgs7z583 -->
**New** · [`examples/recipe-resumable-uploads/main.go:173-188`](main.go#L173-L188)

```go
func New(ctx context.Context, dataDir string) (*App, error) {
	store, disk, err := openStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	app := &App{Store: store, TempDir: filepath.Join(dataDir, "tus")}
	h, err := app.newTus(disk)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(tusPath, http.StripPrefix(tusPath, h))
	mux.HandleFunc("GET /files", app.list)
	app.Handler = withUser(mux)
	return app, nil
}
```
<!-- /ds:block hash=73091a -->

`sweepSessions` deletes the sessions nobody resumed through `tus.Sweep`, which judges a session by the later of its two files (tusd writes `.info` only at creation, so a file-by-file sweep would delete a slow upload that is still alive), and `runSweeper` runs it hourly with a 24-hour cutoff: tusd never expires sessions by itself.

<!-- ds:block id=sweepsessions-g7wfbt4s -->
**sweepSessions** · [`examples/recipe-resumable-uploads/main.go:227-229`](main.go#L227-L229)

```go
func sweepSessions(ctx context.Context, dir string, cutoff time.Time) (int, error) {
	return tus.Sweep(ctx, dir, cutoff)
}
```
<!-- /ds:block hash=c07848 -->

<!-- ds:block id=runsweeper-z2xq8a3z -->
**runSweeper** · [`examples/recipe-resumable-uploads/main.go:233-245`](main.go#L233-L245)

```go
func runSweeper(ctx context.Context, dir string) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			n, err := sweepSessions(ctx, dir, now.Add(-sessionMaxAge))
			fmt.Fprintf(os.Stderr, "sweep: removed %d abandoned sessions (err: %v)\n", n, err)
		}
	}
}
```
<!-- /ds:block hash=3087e2 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, the demo authentication, the tus handler, recording on completion, the session sweep and the server. |
| `shared.go` | Two instances over one shared session store and locker, run by `go run ./recipe-resumable-uploads shared`. |
| `main_test.go` | An upload interrupted and resumed then recorded, the refusals (anonymous, wrong type, too large, lying type) and the sweep of abandoned sessions. |

## Next

- The guide: [Resumable uploads](../../docs/recipes/resumable-uploads.md), with the Uppy client and production notes (sticky sessions or shared temp storage behind a load balancer, `RespectForwardedHeaders` behind a proxy, sizing the temp directory).
- [Uploads](../../docs/concepts/uploads.md) for the policy every upload path shares, and [Files](../../docs/records/files.md) for `CreateFile` and the object-first rule.
- Related recipes: [recipe-browser-direct-uploads](../recipe-browser-direct-uploads), [recipe-mobile-uploads](../recipe-mobile-uploads), [recipe-strip-photo-metadata](../recipe-strip-photo-metadata), [recipe-virus-scanning](../recipe-virus-scanning).
