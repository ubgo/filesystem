# Browser direct uploads

When files are large (videos, design files, scans) or many people upload at once, streaming every byte through the application server costs bandwidth, memory, request time and instance count. The usual answer is to let the browser send the file straight to the bucket with a presigned URL, and have the server check the result afterwards. This recipe is for any web app on S3 or an S3-compatible store (R2, B2, Wasabi, Spaces, SeaweedFS, Garage, MinIO) that wants that, with the same guarantees as a server-side upload: the type is detected from the stored bytes, the size is checked on the stored object, a refused file is deleted, and nothing a user uploads is recorded until the server has seen it.

The program is [examples/recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `POST /uploads/tickets` checks the declared name, type and size and returns a presigned `PUT` valid for 15 minutes.
- The browser `PUT`s the file to the bucket. The application server never sees the bytes.
- `POST /uploads/confirm` runs `upload.Confirm` on the stored object (size, type detected from content, SHA-256), moves it out of the pending area, and records and attaches it with the records module.
- `/uploads/s3/multipart…` serves Uppy's AWS S3 multipart protocol for files too big for one `PUT`, with every route bound to the signed-in user.
- Uploads nobody confirmed are deleted after 24 hours by `upload.Sweep`.

## The design

| Piece | Why |
|---|---|
| `upload.Presign` | Issues the ticket. It validates what the client declares and signs a `PUT` that pins the declared `Content-Type`, so S3 refuses a request with another one. |
| A per-user key prefix, `pending/<user>/` | A `KeyFunc` sees only the file's name and type, not the request. Building the policy per user puts the user in the key, which is how `confirm` and the Uppy routes can tell whose upload a key is. |
| `upload.Confirm` | The declared type and size are the client's word. Confirm reads the stored object's first bytes and size, and deletes it when the policy refuses it. |
| `Disk.Move` to `files/<user>/` | Confirmed objects leave `pending/`, so the sweep can delete everything left there by age without checking the database. |
| `Store.CreateFile` + `Store.Attach` in one `Tx` | Object first, row second: the row is written only for an object that exists and passed the policy. |
| `upload.Uppy` with `UppyOptions.Authorize` | The upload ID and key in every Uppy request come from the client; `Authorize` refuses keys outside the user's prefix. |
| `upload.Sweep` on `pending/` | Tickets that were used but never confirmed (a closed tab, a crash, a refused confirm whose delete failed) would cost storage forever. |

## Step 1: The policy, bound to a user

<!-- ds:block id=allowedtypes-3jfr2jzm -->
**allowedTypes** · [`examples/recipe-browser-direct-uploads/main.go:91`](../../examples/recipe-browser-direct-uploads/main.go#L91-L91)

```go
var allowedTypes = []string{"image/jpeg", "image/png", "image/webp", "application/pdf", "video/mp4"}
```
<!-- /ds:block hash=c48fd3 -->

<!-- ds:block id=policyfor-4j7a9dpb -->
**policyFor** · [`examples/recipe-browser-direct-uploads/main.go:98-104`](../../examples/recipe-browser-direct-uploads/main.go#L98-L104)

```go
func policyFor(user string) upload.Policy {
	return upload.Policy{
		MaxSize: maxUploadBytes,
		Allowed: allowedTypes,
		Key:     upload.RandomKey(path.Join(pendingDir, user)),
	}
}
```
<!-- /ds:block hash=aeafd3 -->

`upload.RandomKey(dir)` names each object `<dir>/<random>.<ext>`, the extension taken from the declared type. The client's file name never reaches the key.

## Step 2: The disk

The program runs on an in-process fake S3 unless `STORAGE_DSN` names a real bucket. Presigned URLs from the fake point at its local port, so curl on the same machine can use them.

<!-- ds:block id=openfakes3-pyu59jvs -->
**openFakeS3** · [`examples/recipe-browser-direct-uploads/main.go:110-133`](../../examples/recipe-browser-direct-uploads/main.go#L110-L133)

```go
func openFakeS3(ctx context.Context) (*filesystem.Disk, func(), error) {
	srv := httptest.NewServer(gofakes3.New(s3mem.New()).Server())
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Region:       fakeRegion,
		Credentials:  credentials.NewStaticCredentialsProvider("fake", "fake", ""),
	})
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(fakeBucket)}); err != nil {
		srv.Close()
		return nil, nil, err
	}
	drv, err := fss3.New(fss3.Config{Client: client, Bucket: fakeBucket})
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		srv.Close()
		return nil, nil, err
	}
	return disk, srv.Close, nil
}
```
<!-- /ds:block hash=bc6089 -->

<!-- ds:block id=opendisk-ztr3zg6t -->
**openDisk** · [`examples/recipe-browser-direct-uploads/main.go:137-151`](../../examples/recipe-browser-direct-uploads/main.go#L137-L151)

```go
func openDisk(ctx context.Context) (*filesystem.Disk, func(), error) {
	dsn, ok := os.LookupEnv(envDSN)
	if !ok {
		return openFakeS3(ctx)
	}
	mux := filesystem.NewMux()
	if err := mux.Register(fss3.Scheme, fss3.Opener); err != nil {
		return nil, nil, err
	}
	disk, err := mux.Open(ctx, diskName, dsn)
	if err != nil {
		return nil, nil, err
	}
	return disk, func() { _ = disk.Close() }, nil
}
```
<!-- /ds:block hash=3b2068 -->

```sh
STORAGE_DSN='s3://my-uploads?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com' go run ./recipe-browser-direct-uploads
```

Credentials come from the AWS default chain (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, shared files, instance roles), never from the DSN. [S3-compatible stores](../adapters/s3-compatible.md) lists the DSN of each provider.

## Step 3: Issue a ticket

<!-- ds:block id=app-ticket-ungdkkjy -->
**App.ticket** · [`examples/recipe-browser-direct-uploads/main.go:201-218`](../../examples/recipe-browser-direct-uploads/main.go#L201-L218)

```go
func (a *App) ticket(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req upload.TicketRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	t, err := upload.Presign(r.Context(), a.Disk, req, policyFor(user), ticketLifetime)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
```
<!-- /ds:block hash=9893ed -->

`upload.PresignHandler(disk, policy, expires)` is the same thing as a ready-made handler. Use it when keys do not need the user, for example behind an endpoint only one trusted service can reach; here the handler is written out because the policy is built per user.

## Step 4: Confirm and record

<!-- ds:block id=app-confirm-tjbds98v -->
**App.confirm** · [`examples/recipe-browser-direct-uploads/main.go:239-260`](../../examples/recipe-browser-direct-uploads/main.go#L239-L260)

```go
func (a *App) confirm(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	var req confirmRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if !filesystem.KeyInDir(req.Key, path.Join(pendingDir, user)) {
		writeError(w, filesystem.Errorf(filesystem.CodeNotFound, "no pending upload %q", req.Key))
		return
	}
	f, err := a.finish(r.Context(), user, req.Key, req.Filename)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(f))
}
```
<!-- /ds:block hash=170c16 -->

<!-- ds:block id=app-finish-cxhgemb8 -->
**App.finish** · [`examples/recipe-browser-direct-uploads/main.go:268-293`](../../examples/recipe-browser-direct-uploads/main.go#L268-L293)

```go
func (a *App) finish(ctx context.Context, user, key, filename string) (records.File, error) {
	res, err := upload.Confirm(ctx, a.Disk, key, filename, policyFor(user))
	if err != nil {
		return records.File{}, err
	}
	final := path.Join(filesDir, user, path.Base(res.Key))
	if _, err := a.Disk.Move(ctx, res.Key, final); err != nil {
		return records.File{}, err
	}
	var f records.File
	err = a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err = a.Store.CreateFile(ctx, records.NewFile{
			Disk: diskName, Key: final, Filename: res.Filename,
			ContentType: res.ContentType, Size: res.Size, SHA256: res.SHA256, UploadedBy: user,
		})
		if err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleUploads)
		return err
	})
	if err != nil {
		return records.File{}, errors.Join(err, a.Disk.Delete(context.WithoutCancel(ctx), final))
	}
	return f, nil
}
```
<!-- /ds:block hash=d61c67 -->

Order matters. `Confirm` runs first, on the object as stored, and deletes it if refused. The move takes it out of the swept prefix. The rows come last, and a failure there deletes the moved object. If the process dies between the move and the commit, the object sits under `files/` with no row: an orphan, never a row without bytes ([consistency](../concepts/consistency.md)).

`Confirm` needs the SHA-256 of the stored bytes. S3 does not report one, so Confirm reads the whole object once, which the size check has already bounded. For very large files that is a full download from the bucket to the server; budget for it, or skip the hash by recording with `CreateFile` after your own checks.

## Step 5: Multipart uploads for Uppy

A single `PUT` is limited to 5 GiB on S3 and cannot resume. For large files, Uppy's `@uppy/aws-s3` plugin uploads in parts, asking the server to create the upload, sign each part, and complete it. `upload.Uppy` speaks that protocol.

<!-- ds:block id=app-uppy-p728nqjx -->
**App.uppy** · [`examples/recipe-browser-direct-uploads/main.go:300-314`](../../examples/recipe-browser-direct-uploads/main.go#L300-L314)

```go
func (a *App) uppy(w http.ResponseWriter, r *http.Request) {
	user, err := userOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	mine := path.Join(pendingDir, user)
	opts := upload.UppyOptions{Authorize: func(_ *http.Request, key string) error {
		if !filesystem.KeyInDir(key, mine) {
			return fmt.Errorf("key %q is not this user's upload", key)
		}
		return nil
	}}
	upload.Uppy(a.Disk, policyFor(user), opts).ServeHTTP(w, r)
}
```
<!-- /ds:block hash=ad567f -->

The type the browser declares is checked at create, and the assembled object is checked with `Confirm` at complete (and deleted if refused). To record it in the same request instead, set `UppyOptions.OnStored`: it runs once the assembled file passed `Confirm`, typically to create its row, and if it fails the file is deleted and the complete request fails, so a stored file never lacks its row. Without it, the client calls `/uploads/confirm` afterwards to record it; Confirm runs again there, which is cheap for the header and costs one read for the hash. The disk needs the `Multipart` and `PresignPart` capabilities, which only the S3 driver has; on any other disk every Uppy route answers `501`.

## Step 6: Sweep what nobody confirmed

<!-- ds:block id=sweeppending-675fbb6c -->
**sweepPending** · [`examples/recipe-browser-direct-uploads/main.go:339-341`](../../examples/recipe-browser-direct-uploads/main.go#L339-L341)

```go
func sweepPending(ctx context.Context, disk *filesystem.Disk, now time.Time) (int, error) {
	return upload.Sweep(ctx, disk, pendingDir, now.Add(-pendingMaxAge))
}
```
<!-- /ds:block hash=4bc3fd -->

<!-- ds:block id=runsweeper-px4wnzxq -->
**runSweeper** · [`examples/recipe-browser-direct-uploads/main.go:345-357`](../../examples/recipe-browser-direct-uploads/main.go#L345-L357)

```go
func runSweeper(ctx context.Context, disk *filesystem.Disk) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			n, err := sweepPending(ctx, disk, now)
			fmt.Fprintf(os.Stderr, "sweep: deleted %d unconfirmed uploads (err: %v)\n", n, err)
		}
	}
}
```
<!-- /ds:block hash=1be60a -->

Sweep deletes as it lists, so a failure part-way returns the count so far and a rerun continues. With several instances, run it from one place (a cron job, a leader) or accept that two instances may race to delete the same key, which is harmless. A bucket lifecycle rule on the `pending/` prefix ("expire after 1 day", "abort incomplete multipart uploads after 1 day") does the same without code, and is the only thing that cleans up multipart uploads that were never completed or aborted, since those are not objects and do not appear in a listing.

Errors answer with the code, and with the message only when it describes the client's own input:

<!-- ds:block id=writeerror-56yg78mg -->
**writeError** · [`examples/recipe-browser-direct-uploads/main.go:386-389`](../../examples/recipe-browser-direct-uploads/main.go#L386-L389)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=e4052c -->

## The browser side

A plain `fetch` flow, for files up to a few hundred megabytes:

```js
async function uploadDirect(file) {
  const ticket = await fetch('/uploads/tickets', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename: file.name, content_type: file.type, size: file.size }),
  }).then(r => r.json());
  if (ticket.code) throw new Error(ticket.error);

  // Send exactly the headers the ticket lists: the signature covers Content-Type.
  const put = await fetch(ticket.url, { method: ticket.method, headers: ticket.headers, body: file });
  if (!put.ok) throw new Error(`upload failed: ${put.status}`);

  const res = await fetch('/uploads/confirm', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ key: ticket.key, filename: file.name }),
  });
  return res.json(); // {file_id, key, filename, content_type, size, sha256} or {error, code}
}
```

`file.type` is empty for types the browser does not know; ask the user to pick another file, or let the server refuse it.

With Uppy, multipart for everything:

```js
import Uppy from '@uppy/core';
import AwsS3 from '@uppy/aws-s3';

const uppy = new Uppy({ restrictions: { maxFileSize: 100 * 1024 * 1024 } })
  .use(AwsS3, {
    endpoint: '/uploads',          // Uppy calls /uploads/s3/multipart…
    shouldUseMultipart: () => true, // this server does not serve the single-PUT params route
  });

uppy.on('upload-success', async (file) => {
  await fetch('/uploads/confirm', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ key: file.s3Multipart.key, filename: file.name }),
  });
});
```

The bucket needs a CORS rule that allows `PUT` from your app's origin with the `Content-Type` header, and exposes `ETag`: Uppy reads each part's ETag from the response to complete the upload. Without `ExposeHeaders: ["ETag"]` multipart uploads fail at complete.

## Try it

```sh
cd examples
go run ./recipe-browser-direct-uploads
```

```text
listening on http://localhost:8105
```

Ask for a ticket (the demo trusts `X-User`):

```sh
curl -s -X POST localhost:8105/uploads/tickets -H 'X-User: u1' \
  -d '{"filename":"me.jpg","content_type":"image/jpeg","size":51566}'
```

```json
{"key":"pending/u1/tomnwllpmyv7wy5qqkkgczvdii.jpg","method":"PUT","url":"http://127.0.0.1:52552/uploads/pending/u1/tomnwllpmyv7wy5qqkkgczvdii.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=fake%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T032239Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=content-type%3Bhost\u0026x-id=PutObject\u0026X-Amz-Signature=82192c961c39dee53c2c4c0f454092cddd7e3e15c6b6fc02f9ebc3245ac1ff78","headers":{"Content-Type":["image/jpeg"]},"expires_at":"2026-10-06T09:07:39.409607+05:30"}
```

Check: `X-Amz-SignedHeaders=content-type;host`, the type is part of the signature.

Upload to the bucket, then confirm, first as the wrong user:

```sh
curl -s -i -X PUT -H 'Content-Type: image/jpeg' --data-binary @me.jpg "$URL"
```

```text
HTTP/1.1 200 OK
Etag: "223b8a1ebbca2d77dcfd11e76fc93209"
```

```sh
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u2' -d "{\"key\":\"$KEY\",\"filename\":\"me.jpg\"}"
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"me.jpg\"}"
curl -s localhost:8105/uploads -H 'X-User: u1'
```

```text
{"error":"Not Found","code":"not_found"}
{"file_id":"fil_75ikxonxv77jqg2cti4dmlp5pf","key":"files/u1/tomnwllpmyv7wy5qqkkgczvdii.jpg","filename":"me.jpg","content_type":"image/jpeg","size":51566,"sha256":"87d2873be255d6ce6b05c02b57d5f04dfc608d4bd74e7c73655418947cf9e0db"}
[{"file_id":"fil_75ikxonxv77jqg2cti4dmlp5pf","key":"files/u1/tomnwllpmyv7wy5qqkkgczvdii.jpg","filename":"me.jpg","content_type":"image/jpeg","size":51566,"sha256":"87d2873be255d6ce6b05c02b57d5f04dfc608d4bd74e7c73655418947cf9e0db"}]
```

Check: the other user gets a plain `404`, the same answer as for a key that does not exist, so keys cannot be probed.

A client that declares a JPEG and uploads HTML gets a ticket, and the upload succeeds, because the bucket cannot look inside. Confirm refuses it and deletes it, so confirming again finds nothing:

```sh
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"cat.jpg\"}"
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"cat.jpg\"}"
```

```text
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
{"error":"Not Found","code":"not_found"}
```

What a ticket refuses up front:

```sh
curl -s -X POST localhost:8105/uploads/tickets -H 'X-User: u1' -d '{"filename":"page.html","content_type":"text/html","size":40}'
curl -s -X POST localhost:8105/uploads/tickets -H 'X-User: u1' -d '{"filename":"movie.mp4","content_type":"video/mp4","size":500000000}'
```

```text
{"error":"content type \"text/html\" is not allowed","code":"rejected"}
{"error":"declared size 500000000 is not in (0, 104857600]","code":"too_large"}
```

The Uppy multipart flow by hand: create, sign part 1 (as the owner, then as someone else), upload it, list parts, complete, confirm.

```sh
curl -s -X POST localhost:8105/uploads/s3/multipart -H 'X-User: u1' -d '{"filename":"big.jpg","type":"image/jpeg"}'
```

```json
{"uploadId":"1","key":"pending/u1/pv5qgv6b77v3qphriroxnxe4qw.jpg"}
```

```sh
curl -s "localhost:8105/uploads/s3/multipart/1/1?key=$KEY" -H 'X-User: u1'
curl -s "localhost:8105/uploads/s3/multipart/1/1?key=$KEY" -H 'X-User: u2'
```

```text
{"url":"http://127.0.0.1:52552/uploads/pending/u1/pv5qgv6b77v3qphriroxnxe4qw.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026...\u0026partNumber=1\u0026uploadId=1\u0026x-id=UploadPart\u0026X-Amz-Signature=b592d19765b7701f7e7beb71ae4b9c8682a27527a841c7b437fecbeac85c7646"}
{"error":"Forbidden","code":"permission"}
```

```sh
curl -s -D - -o /dev/null -X PUT --data-binary @big.jpg "$PART_URL" | grep -i etag
curl -s "localhost:8105/uploads/s3/multipart/1?key=$KEY" -H 'X-User: u1'
curl -s -X POST "localhost:8105/uploads/s3/multipart/1/complete?key=$KEY" -H 'X-User: u1' \
  -d '{"parts":[{"PartNumber":1,"ETag":"\"b37c8eceba510c43804915b9e153323b\""}]}'
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"big.jpg\"}"
```

```text
Etag: "b37c8eceba510c43804915b9e153323b"
[{"PartNumber":1,"Size":337756,"ETag":"b37c8eceba510c43804915b9e153323b"}]
{"location":"","key":"pending/u1/pv5qgv6b77v3qphriroxnxe4qw.jpg"}
{"file_id":"fil_6jwke37kdvyy7cc4ib3g35wx3k","key":"files/u1/pv5qgv6b77v3qphriroxnxe4qw.jpg","filename":"big.jpg","content_type":"image/jpeg","size":337756,"sha256":"31858283a170a337bc1b7175f5ab7bd0feb37dc18f830bc7fb1fe6f997303649"}
```

`location` is empty because this disk has no public URL; the client uses the key. A refused type at create, and an abort:

```sh
curl -s -X POST localhost:8105/uploads/s3/multipart -H 'X-User: u1' -d '{"filename":"setup.exe","type":"application/x-msdownload"}'
curl -s -i -X DELETE "localhost:8105/uploads/s3/multipart/2?key=$KEY" -H 'X-User: u1'
```

```text
{"error":"content type \"application/x-msdownload\" is not allowed","code":"rejected"}
HTTP/1.1 204 No Content
```

### What the fake does not check

The in-process fake (gofakes3) does not verify signatures: during these runs it accepted a `PUT` with a tampered signature and one with `Content-Type: text/html` against a URL signed for `image/jpeg`. A real S3, R2 or MinIO answers both with `403 SignatureDoesNotMatch`. It also sends `Access-Control-Allow-Origin: *` itself, where a real bucket needs the CORS rule above. Everything this recipe relies on for security happens on the server (`Confirm`, the key prefix, `Authorize`) and is exercised by the test against the fake; the signature checks are the bucket's, and only a real bucket shows them.

## Edge cases this handles

- A client that lies about the type or size: the ticket is issued on its word, the stored bytes are refused by `Confirm` and deleted.
- A file larger than declared: `Confirm` checks the stored size against `MaxSize` (`413`, deleted). On a real bucket the signed `PUT` does not pin the length, so this check is the one that counts.
- One user confirming, signing parts for, completing or aborting another's upload: refused, because keys carry the user and both `confirm` and `Authorize` check the prefix.
- A confirm that is retried after it succeeded: the pending key no longer exists, `404`; the file is recorded once.
- A ticket that is never used, or used and never confirmed: the pending object is swept after 24 hours.
- Recording fails after the move: the moved object is deleted.

## For production

- Replace the `X-User` header with your session lookup; everything else stays.
- Configure CORS on the bucket (`PUT` from your origin, `Content-Type` allowed, `ETag` exposed) and a lifecycle rule that aborts incomplete multipart uploads.
- Keep tickets short. 15 minutes is enough to start a `PUT`; an upload in progress is not cut off when its URL expires.
- Rate-limit ticket requests per user: each one lets the user write up to `MaxSize` bytes to your bucket until the sweep runs.
- Presigned and Uppy uploads cannot run `Policy.Process` (the bytes go to the bucket, not through the server), so photos are not sanitized. If you need EXIF stripped, process after confirm and replace the object, or use a server-side path ([strip photo metadata](strip-photo-metadata.md)). `Policy.Inspect` does run in `Confirm`, so a virus scan still applies ([virus scanning](virus-scanning.md)).
- On a local disk in development, `upload.Presign` works the same with `signedurl` and `fshttp.Signed`, which accepts the signed `PUT` and checks the pinned `Content-Type`; Uppy multipart does not, since the local driver cannot presign parts.

## Related

- [Resumable uploads](resumable-uploads.md): tus, when uploads must survive dropped connections and go through the server.
- [Mobile uploads](mobile-uploads.md): choosing between form, presigned and tus uploads for an app.
- [Uploads](../concepts/uploads.md): policies, `Presign`, `Confirm`, `Uppy` and `Sweep`.
- [Files](../records/files.md): `CreateFile`, the object-first rule.
- [S3](../adapters/s3.md): presigning, capabilities and DSN parameters.
