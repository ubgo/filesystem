# Resumable uploads

A 400 MB video from a phone on a train will not arrive in one request. When the connection drops at 80%, a plain upload starts over; a resumable one asks the server how much arrived and sends the rest. This recipe is for apps whose users upload large files over networks you do not control: video and podcast platforms, field-service and inspection apps, anything mobile. It uses the tus protocol, which Uppy, tus-js-client and the official tus clients for iOS, Android and Flutter already speak, through `contrib/tus`, which embeds tusd rather than reimplementing it.

The program is [examples/recipe-resumable-uploads](../../examples/recipe-resumable-uploads). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `POST /uploads/` creates an upload of a declared length; `PATCH` sends chunks at an offset; `HEAD` says how much has arrived, so a client resumes after any failure.
- Anonymous uploads, refused types and oversized lengths are refused at creation, before any byte is sent.
- When the last byte arrives, and before the client hears it succeeded, the file goes through the same `upload.Policy` as a form upload (type detected from the bytes, size counted), lands on the disk, and is recorded and attached to the user.
- If any of that fails, the client's last `PATCH` fails with the matching status, nothing stays on the disk, and the session is deleted.
- Sessions a client abandons are swept from the temporary directory by age.

## The design

| Piece | Why |
|---|---|
| `tus.New(tus.Config{…})` | The protocol: creation, offsets, resumption, termination, all from tusd. |
| `Config.TempDir` | tusd keeps unfinished sessions on local disk, and locks each one while a request writes it; the target disk (S3, GCS…) only ever sees finished files that passed the policy. `Config.Store` and `Config.Locker` replace both (see For production). |
| `Config.Cors` | Optional: which browser origins may upload. The default allows every origin and the `Upload-Session` header an [upload session](../concepts/uploads.md#upload-sessions-a-token-valid-for-a-window) sends. |
| `Config.Policy` | Checked at creation from what the client declares (length, `filetype`), and enforced on the bytes when the upload finishes. |
| `Config.Resolve` | Optional: the policy of each upload, from `Policy`, at creation and at the final `PATCH`; `upload.Sessions.Resolver()` applies an [upload session](../concepts/uploads.md#upload-sessions-a-token-valid-for-a-window)'s key prefix and limits to tus uploads. |
| `Config.Authorize` | Runs on every request (the creation, and the `PATCH`, `HEAD` and `DELETE` requests that continue, resume or cancel an upload) with its context, where the authentication middleware put the user. |
| `Config.OnStored` | Records and attaches the file. Its error deletes the stored object and fails the final request, so "done" always means "recorded". |
| `sweepSessions` | tusd never expires sessions by itself; an app sweeps its temp directory with `tus.Sweep`, session by session. |

## Step 1: The policy

<!-- ds:block id=policy-cf24neuq -->
**policy** · [`examples/recipe-resumable-uploads/main.go:66-70`](../../examples/recipe-resumable-uploads/main.go#L66-L70)

```go
var policy = upload.Policy{
	MaxSize: maxUploadBytes,
	Allowed: []string{"video/mp4", "video/quicktime", "image/jpeg", "image/png", "application/pdf"},
	Key:     upload.RandomKey("uploads"),
}
```
<!-- /ds:block hash=f907f0 -->

`MaxSize` is passed to tusd as its maximum, so an `Upload-Length` over it is answered `413` at creation. `Allowed` is checked twice: against the `filetype` the client declares in `Upload-Metadata` (when it declares one), and against the type detected from the finished bytes.

## Step 2: Who is uploading

tus requests reach the hooks with the request's context, so the user goes there in the usual middleware:

<!-- ds:block id=withuser-cqj5cewu -->
**withUser** · [`examples/recipe-resumable-uploads/main.go:79-86`](../../examples/recipe-resumable-uploads/main.go#L79-L86)

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
**userFrom** · [`examples/recipe-resumable-uploads/main.go:90-95`](../../examples/recipe-resumable-uploads/main.go#L90-L95)

```go
func userFrom(ctx context.Context) (string, error) {
	if u, ok := ctx.Value(userKey{}).(string); ok {
		return u, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "sign in first")
}
```
<!-- /ds:block hash=aa9ec4 -->

## Step 3: The handler

<!-- ds:block id=app-newtus-g2wn6ycj -->
**App.newTus** · [`examples/recipe-resumable-uploads/main.go:134-146`](../../examples/recipe-resumable-uploads/main.go#L134-L146)

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

<!-- ds:block id=new-sgs7z583 -->
**New** · [`examples/recipe-resumable-uploads/main.go:173-188`](../../examples/recipe-resumable-uploads/main.go#L173-L188)

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

`BasePath` must be the path the handler is mounted at, with the prefix stripped by `http.StripPrefix`: tusd builds the `Location` of each upload from it.

### Only the owner continues an upload

`Config.Authorize` checks who is calling; it does not know which upload a `PATCH`, `HEAD` or `DELETE` is about, so the unguessable upload URL is what ties a session to its creator. To check ownership as well, stamp the owner at creation and compare on every later request:

```go
cfg.StampMetadata = func(ctx context.Context, _ http.Header) (map[string]string, error) {
	user, err := userFrom(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"owner": user}, nil
}
cfg.AuthorizeUpload = func(ctx context.Context, r tus.UploadRequest) error {
	if r.UploadID == "" {
		return nil // creation: StampMetadata sets the owner
	}
	user, err := userFrom(ctx)
	if err != nil || r.Metadata["owner"] != user {
		return errors.New("not your upload")
	}
	return nil
}
```

`StampMetadata` writes over any key of the same name the client sent, so a client cannot claim another owner; stamp the same keys every time. `AuthorizeUpload` sees the stored metadata; on creation `UploadID` is empty and `Metadata` is what the client declared. A refusal, a stamp error, or an upload whose metadata cannot be read is 403.

## Step 4: Record what finished

<!-- ds:block id=app-record-gyqamstd -->
**App.record** · [`examples/recipe-resumable-uploads/main.go:153-169`](../../examples/recipe-resumable-uploads/main.go#L153-L169)

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

`res` is the `upload.Result` of the stored file: its key, the safe file name from the client's `filename` metadata, the detected type, the size and the SHA-256. `meta` is the client's whole `Upload-Metadata`, for anything else your client sends (an album ID, a caption); treat it as untrusted input.

`Authorize` runs on every request but CORS preflights, so an anonymous `PATCH`, `HEAD` or `DELETE` is refused with `403` like an anonymous creation; the refusal carries tusd's CORS headers, so a browser client reads the `403` instead of a network error. It checks who is calling, not that the caller created that upload: the upload's URL, a random 128-bit ID, is still what ties a session to its creator, the way presigned URLs are. `record` attaches the file to the user of the request that sent the last chunk, the signed-in user.

## Step 5: Sweep abandoned sessions

<!-- ds:block id=sweepsessions-g7wfbt4s -->
**sweepSessions** · [`examples/recipe-resumable-uploads/main.go:227-229`](../../examples/recipe-resumable-uploads/main.go#L227-L229)

```go
func sweepSessions(ctx context.Context, dir string, cutoff time.Time) (int, error) {
	return tus.Sweep(ctx, dir, cutoff)
}
```
<!-- /ds:block hash=c07848 -->

<!-- ds:block id=runsweeper-z2xq8a3z -->
**runSweeper** · [`examples/recipe-resumable-uploads/main.go:233-245`](../../examples/recipe-resumable-uploads/main.go#L233-L245)

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

`sweepSessions` is a call to `tus.Sweep(ctx, dir, olderThan)` from `contrib/tus`, which works per session rather than per file: tusd appends every chunk to the session's data file (`<id>`) but writes its `.info` file only when the session is created, so the time of last progress is the later of the two files' modification times, and a file-by-file sweep would delete the `.info` of an upload that is slow but alive. `Sweep` also touches only files named by a tusd session ID, so anything else in the directory is left alone, and returns how many sessions it deleted. Sweeping after 24 hours of silence keeps uploads that are slow but alive. Choose the age from how long your clients realistically pause (an app in the background, a phone offline overnight).

## Try it

```sh
cd examples
go run ./recipe-resumable-uploads
```

```text
listening on http://localhost:8106 data in /var/folders/sk/.../T/recipe-resumable-uploads-1515711677
```

Create an upload of a 337,756-byte photo. `Upload-Metadata` is comma-separated `key base64(value)` pairs:

```sh
META="filename $(printf 'holiday.jpg' | base64),filetype $(printf 'image/jpeg' | base64)"
curl -s -i -X POST localhost:8106/uploads/ -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0' \
  -H 'Upload-Length: 337756' -H "Upload-Metadata: $META"
```

```text
HTTP/1.1 201 Created
Location: http://localhost:8106/uploads/dc894320b6487a4810c6bc119de5353a
Tus-Resumable: 1.0.0
X-Content-Type-Options: nosniff
Content-Length: 0
```

Send the first 200,000 bytes:

```sh
U=http://localhost:8106/uploads/dc894320b6487a4810c6bc119de5353a
head -c 200000 big.jpg > part1; tail -c +200001 big.jpg > part2
curl -s -i -X PATCH $U -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0' -H 'Upload-Offset: 0' \
  -H 'Content-Type: application/offset+octet-stream' --data-binary @part1
```

```text
HTTP/1.1 204 No Content
Tus-Resumable: 1.0.0
Upload-Offset: 200000
X-Content-Type-Options: nosniff
```

The connection drops here. Later, the client asks where to continue:

```sh
curl -s -I $U -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0'
```

```text
HTTP/1.1 200 OK
Cache-Control: no-store
Content-Length: 337756
Tus-Resumable: 1.0.0
Upload-Length: 337756
Upload-Metadata: filename aG9saWRheS5qcGc=,filetype aW1hZ2UvanBlZw==
Upload-Offset: 200000
X-Content-Type-Options: nosniff
```

And sends the rest from offset 200,000. The answer to the last chunk carries the stored key:

```sh
curl -s -i -X PATCH $U -H 'X-User: u1' -H 'Tus-Resumable: 1.0.0' -H 'Upload-Offset: 200000' \
  -H 'Content-Type: application/offset+octet-stream' --data-binary @part2
curl -s localhost:8106/files -H 'X-User: u1'
curl -s -I $U -H 'Tus-Resumable: 1.0.0' -H 'X-User: u1' | head -1
```

```text
HTTP/1.1 204 No Content
Filesystem-Key: uploads/ar7mjkog7ejjyo3cvctfqfz4o7.jpg
Tus-Resumable: 1.0.0
Upload-Offset: 337756
X-Content-Type-Options: nosniff

[{"file_id":"fil_lzfzxuftc4kcjwyxpv7bmgg2ca","key":"uploads/ar7mjkog7ejjyo3cvctfqfz4o7.jpg","filename":"holiday.jpg","content_type":"image/jpeg","size":337756}]
HTTP/1.1 404 Not Found
```

Check: the file is recorded under its name from the metadata, and the session is gone (`404`): its temporary files were deleted when it finished.

What is refused, and when. tusd answers errors as `text/plain` with a tus error code; the library's codes appear as `ERR_<CODE>`:

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

A client that declares a JPEG and sends HTML gets as far as the last chunk. A chunk at the wrong offset is refused by the protocol; the right one finishes the upload, and the policy refuses the bytes:

```sh
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

Check: the temporary directory is empty and the disk holds only the photo. An abandoned session, by contrast, stays: after one chunk of an upload nobody continues, the temp directory holds its two files until the sweep removes them.

```text
200000 e89ea7cf55c2cab58e363485f9b9c656
509 e89ea7cf55c2cab58e363485f9b9c656.info
```

## The client side

With Uppy:

```js
import Uppy from '@uppy/core';
import Tus from '@uppy/tus';

new Uppy().use(Tus, {
  endpoint: '/uploads/',
  chunkSize: 5 * 1024 * 1024,      // smaller chunks lose less on a drop
  retryDelays: [0, 1000, 3000, 5000],
  headers: { Authorization: `Bearer ${token}` },
});
```

Uppy sends `filename` and `filetype` metadata by default; tus-js-client needs them set in `metadata`. Both store the upload URL in local storage and resume it when the same file is added again.

## Edge cases this handles

- A dropped connection at any point: `HEAD` returns the offset, the client continues from there.
- A chunk sent twice or out of order: `409 ERR_MISMATCHED_OFFSET`, nothing written.
- A declared length over the limit: `413` at creation, no byte accepted.
- Bytes that do not match the declared type: refused on the last chunk with `415`, nothing stored, session deleted.
- Recording fails: the object is deleted and the last request fails with the status of the error's code (`507` for `quota_exceeded`, `500` for an unclassified failure such as the database being down); the client can start over.
- An abandoned upload: its session files are swept by age.

## For production

- The temporary directory is local to an instance. Behind a load balancer, either route all requests of an upload to the same instance (sticky sessions on the upload ID), put `TempDir` on storage every instance mounts, or give every instance one shared session store and locker. Otherwise a chunk that lands on another instance gets `404`. A shared store is any tusd store that can terminate uploads, with a locker all instances share (tusd's S3 store with a Redis locker, say):

  ```go
  h, err := tus.New(tus.Config{
  	Disk: disk, Policy: policy, BasePath: "/uploads/",
  	Store:  s3store.New(bucket, s3Client),
  	Locker: redisLocker, // any handler.Locker every instance shares
  })
  ```

  `New` refuses a `Store` without a `Locker`, since a lock local to one instance would not stop two instances writing one upload. `Sweep` reads only a `TempDir`; expire a shared store's sessions with the backend's own lifecycle rules.
- Restrict the origins a browser may upload from with `Config.Cors`. Start from `tus.DefaultCors()`, which keeps tusd's headers and `Upload-Session`, and set its `AllowOrigin`:

  ```go
  cors := tus.DefaultCors()
  cors.AllowOrigin = regexp.MustCompile(`^https://app\.example\.com$`)
  cfg.Cors = cors
  ```
- Size the temporary directory for the number of concurrent uploads times `MaxSize`, plus the finished file being copied to the disk: each byte is written twice, once to the session and once to the target disk.
- `Policy.Process` runs on tus uploads (it is part of `upload.Receive`), so photos can be sanitized here ([strip photo metadata](strip-photo-metadata.md)); `Policy.Inspect` does too ([virus scanning](virus-scanning.md)).
- Check the `Location` behind a proxy. tusd builds it from the request's host and scheme, so behind a TLS-terminating proxy it can say `http://` or name an internal host. Set `Config.RespectForwardedHeaders` to build it from `X-Forwarded-Proto`, `X-Forwarded-Host` and `Forwarded` instead, but only when the proxy sets or strips those headers: otherwise a client chooses the host its upload URL points at.
- tusd logs a line per request at Info. `Config.Logger` takes the `*slog.Logger` it writes to (nil means `slog.Default()`); give it a handler at `slog.LevelWarn` to keep only failures.
- Add `Upload-Metadata` keys your app needs (an album, a caption) in the client and read them in `OnStored`; validate them like any other input.

Try it: [`examples/recipe-resumable-uploads`](../../examples/recipe-resumable-uploads) runs this section in `shared.go` ([`sharedInstances`](ds:block?id=sharedinstances-p5t9c2xh)): `go run ./recipe-resumable-uploads shared` from `examples/`.

## Related

- [Browser direct uploads](browser-direct-uploads.md): uploads that skip the server entirely, with presigned URLs or Uppy multipart.
- [Mobile uploads](mobile-uploads.md): when to choose tus, presigned or form uploads from an app.
- [Uploads](../concepts/uploads.md): the policy every path shares.
- [Files](../records/files.md): `CreateFile` and the object-first rule.
