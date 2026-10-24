# Browser direct uploads: presigned PUTs and Uppy multipart straight to S3

An HTTP server that lets a browser send files straight to an S3 bucket, so large uploads never pass through the application server: the server hands out a presigned PUT (or, for big files, signs the parts of a multipart upload for Uppy), the browser uploads, and the server confirms the stored bytes against the policy before recording the file. It answers the need of any product with large uploads (video, design files, datasets) that should not tie up app servers. It uses `upload.Presign`, `upload.Confirm`, `upload.Uppy` and `upload.Sweep` over `adapters/s3`, and `records` (`CreateFile`, `Attach`) over SQLite. The guide is [Browser direct uploads](../../docs/recipes/browser-direct-uploads.md).

## Run it

```sh
cd examples
go run ./recipe-browser-direct-uploads
```

It listens on `http://localhost:8105`. Without `STORAGE_DSN` the bucket is an in-process fake S3 (gofakes3) on a random local port, which the presigned URLs point at, so it works offline; with `STORAGE_DSN='s3://bucket?region=…'` it uses a real bucket (credentials from the AWS default chain, and the bucket needs the CORS rule the guide gives). The SQLite database `app.db` goes in a new temporary directory, removed when Ctrl-C or SIGTERM stops the server. An hourly sweeper deletes unconfirmed uploads older than 24 hours. Stop it with Ctrl-C.

Run the tests with `go test ./recipe-browser-direct-uploads`. The demo trusts an `X-User` header as the signed-in user; a real app reads its session.

The fake does not verify signatures, so a tampered signature or a `Content-Type` other than the signed one is accepted here, where a real S3, R2 or MinIO answers `403 SignatureDoesNotMatch`. Everything this recipe relies on for security happens on the server (`Confirm`, the per-user key prefix, `Authorize`) and is exercised against the fake.

## Walkthrough

1. Ask for a ticket for a 6334-byte JPEG. The declared type is part of the signature (`X-Amz-SignedHeaders=content-type;host`) and the key sits under the user's `pending/` prefix:

```sh
T=$(curl -s -X POST localhost:8105/uploads/tickets -H 'X-User: u1' -d '{"filename":"me.jpg","content_type":"image/jpeg","size":6334}')
echo "$T"; URL=$(echo "$T" | jq -r .url); KEY=$(echo "$T" | jq -r .key)
```

```json
{"key":"pending/u1/ajfflnyjl4bhx5xloyi6gy4and.jpg","method":"PUT","url":"http://127.0.0.1:52758/uploads/pending/u1/ajfflnyjl4bhx5xloyi6gy4and.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=fake%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T032257Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=content-type%3Bhost\u0026x-id=PutObject\u0026X-Amz-Signature=5f1a34022f8181662d9f3ac74e8323171f6b5033fb5bb8a953b06ccb50529b54","headers":{"Content-Type":["image/jpeg"]},"expires_at":"2026-10-06T09:07:57.864982+05:30"}
```

2. Upload to the bucket, then confirm, first as another user (a plain `404`, the same as for a key that does not exist, so keys cannot be probed), then as the owner. The object moves to `files/u1/` and is recorded:

```sh
curl -s -i -X PUT -H 'Content-Type: image/jpeg' --data-binary @me.jpg "$URL" | grep -i -E '^HTTP|^etag'
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u2' -d "{\"key\":\"$KEY\",\"filename\":\"me.jpg\"}"
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"me.jpg\"}"
curl -s localhost:8105/uploads -H 'X-User: u1'
```

```text
HTTP/1.1 200 OK
Etag: "4abe0173614826fc8bd0cca3c3c465fd"
{"error":"Not Found","code":"not_found"}
{"file_id":"fil_idzxixcxpa2oljylrhz5v6pdzf","key":"files/u1/ajfflnyjl4bhx5xloyi6gy4and.jpg","filename":"me.jpg","content_type":"image/jpeg","size":6334,"sha256":"920c18840310bfd89c28f6e1b1090d5fb479facd4a1c324c6b66da3605c70dce"}
[{"file_id":"fil_idzxixcxpa2oljylrhz5v6pdzf","key":"files/u1/ajfflnyjl4bhx5xloyi6gy4and.jpg","filename":"me.jpg","content_type":"image/jpeg","size":6334,"sha256":"920c18840310bfd89c28f6e1b1090d5fb479facd4a1c324c6b66da3605c70dce"}]
```

3. A client that declares a JPEG and uploads HTML gets a ticket and its PUT succeeds (the bucket cannot look inside), but confirm detects the type from the bytes, refuses it and deletes it, so a second confirm finds nothing:

```sh
printf '<html><body>not a cat</body></html>' > cat.jpg
# ticket for {"filename":"cat.jpg","content_type":"image/jpeg","size":35}, then PUT cat.jpg to its URL
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"cat.jpg\"}"
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"cat.jpg\"}"
```

```text
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
{"error":"Not Found","code":"not_found"}
```

4. What a ticket refuses up front, from the client's declaration:

```sh
curl -s -X POST localhost:8105/uploads/tickets -H 'X-User: u1' -d '{"filename":"page.html","content_type":"text/html","size":40}'
curl -s -X POST localhost:8105/uploads/tickets -H 'X-User: u1' -d '{"filename":"movie.mp4","content_type":"video/mp4","size":500000000}'
```

```text
{"error":"content type \"text/html\" is not allowed","code":"rejected"}
{"error":"declared size 500000000 is not in (0, 104857600]","code":"too_large"}
```

5. The Uppy multipart protocol by hand: create, sign part 1 (as the owner, then as someone else, who is refused), upload the part, list parts, complete, confirm:

```sh
curl -s -X POST localhost:8105/uploads/s3/multipart -H 'X-User: u1' -d '{"filename":"big.jpg","type":"image/jpeg"}'
# KEY is the returned key, upload ID 1
curl -s "localhost:8105/uploads/s3/multipart/1/1?key=$KEY" -H 'X-User: u1'
curl -s "localhost:8105/uploads/s3/multipart/1/1?key=$KEY" -H 'X-User: u2'
curl -s -D - -o /dev/null -X PUT --data-binary @big.jpg "$PART_URL" | grep -i etag
curl -s "localhost:8105/uploads/s3/multipart/1?key=$KEY" -H 'X-User: u1'
curl -s -X POST "localhost:8105/uploads/s3/multipart/1/complete?key=$KEY" -H 'X-User: u1' -d '{"parts":[{"PartNumber":1,"ETag":"\"065d611d3b903475751fd4e7ba3cc347\""}]}'
curl -s -X POST localhost:8105/uploads/confirm -H 'X-User: u1' -d "{\"key\":\"$KEY\",\"filename\":\"big.jpg\"}"
```

```text
{"uploadId":"1","key":"pending/u1/yruc3u5htk6wb4czfigy5fdhdq.jpg"}
{"url":"http://127.0.0.1:52758/uploads/pending/u1/yruc3u5htk6wb4czfigy5fdhdq.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256\u0026X-Amz-Credential=fake%2F20261006%2Fus-east-1%2Fs3%2Faws4_request\u0026X-Amz-Date=20261006T032258Z\u0026X-Amz-Expires=900\u0026X-Amz-SignedHeaders=host\u0026partNumber=1\u0026uploadId=1\u0026x-id=UploadPart\u0026X-Amz-Signature=8e2bb454cc49152f7e7be7724752b6aed7aae32b750a0a10b481d91d8d48c4d9"}
{"error":"Forbidden","code":"permission"}
Etag: "065d611d3b903475751fd4e7ba3cc347"
[{"PartNumber":1,"Size":724281,"ETag":"065d611d3b903475751fd4e7ba3cc347"}]
{"location":"","key":"pending/u1/yruc3u5htk6wb4czfigy5fdhdq.jpg"}
{"file_id":"fil_oz2s4jedyib73tsmsesm57qmq5","key":"files/u1/yruc3u5htk6wb4czfigy5fdhdq.jpg","filename":"big.jpg","content_type":"image/jpeg","size":724281,"sha256":"9ca7aed3d2995674295af15c0843198b4adc27eb1476ad96336ec4feef1116f5"}
```

`location` is empty because this disk has no public URL; the client uses the key.

6. A refused type at create, and an abort of a second upload:

```sh
curl -s -X POST localhost:8105/uploads/s3/multipart -H 'X-User: u1' -d '{"filename":"setup.exe","type":"application/x-msdownload"}'
curl -s -i -X DELETE "localhost:8105/uploads/s3/multipart/2?key=$KEY" -H 'X-User: u1' | head -1
```

```text
{"error":"content type \"application/x-msdownload\" is not allowed","code":"rejected"}
HTTP/1.1 204 No Content
```

7. Record the file in the complete request itself. The routes under `/uploads/s3r/` are the same Uppy protocol with `UppyOptions.OnStored`, which runs once the completed file has passed `Confirm` and creates its row and attachment; the key is under `files/<user>/` from the start, so there is no confirm call and nothing to move. The complete request sends `"filename"` beside the parts (a custom `completeMultipartUpload` in Uppy), which the policy's name rules check again and which becomes the recorded name:

```sh
curl -s -X POST localhost:8105/uploads/s3r/multipart -H 'X-User: u1' -d '{"filename":"IMG_0042.jpg","type":"image/jpeg"}'
# sign part 1 and PUT it as in step 5, then:
curl -s -X POST "localhost:8105/uploads/s3r/multipart/1/complete?key=$KEY" -H 'X-User: u1' \
  -d '{"filename":"Holiday photo.jpg","parts":[{"PartNumber":1,"ETag":"\"781fd1dca845f5432923526e76a69657\""}]}'
curl -s localhost:8105/uploads -H 'X-User: u1'
```

```text
{"uploadId":"1","key":"files/u1/7gwpk66wwipbedbop4jz6sjy4h.jpg"}
{"location":"","key":"files/u1/7gwpk66wwipbedbop4jz6sjy4h.jpg"}
[{"file_id":"fil_gvzxy6kneb5rhiegs73xxatjcz","key":"files/u1/7gwpk66wwipbedbop4jz6sjy4h.jpg","filename":"Holiday photo.jpg","content_type":"image/jpeg","size":23,"sha256":"8e3faef8c60a6acca2e0eddf0095cca597caba4a2598dede60c7cfd5625e1cc1"}]
```

If `OnStored` fails, the file is deleted and the complete request answers the error, so a stored file never lacks its row.

## The code

`allowedTypes` are checked twice: from the client's declaration when a ticket is issued, and from the stored bytes at confirm. `policyFor` binds the user into the key prefix, which is how confirm and the Uppy routes know an upload is the caller's.

<!-- ds:block id=allowedtypes-3jfr2jzm -->
**allowedTypes** · [`examples/recipe-browser-direct-uploads/main.go:91`](main.go#L91-L91)

```go
var allowedTypes = []string{"image/jpeg", "image/png", "image/webp", "application/pdf", "video/mp4"}
```
<!-- /ds:block hash=c48fd3 -->

<!-- ds:block id=policyfor-4j7a9dpb -->
**policyFor** · [`examples/recipe-browser-direct-uploads/main.go:98-104`](main.go#L98-L104)

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

`openDisk` opens the bucket `STORAGE_DSN` names, or `openFakeS3`, an in-process gofakes3 server whose URLs a browser or curl on this machine can reach.

<!-- ds:block id=opendisk-ztr3zg6t -->
**openDisk** · [`examples/recipe-browser-direct-uploads/main.go:137-151`](main.go#L137-L151)

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

<!-- ds:block id=openfakes3-pyu59jvs -->
**openFakeS3** · [`examples/recipe-browser-direct-uploads/main.go:110-133`](main.go#L110-L133)

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

`ticket` issues the presigned PUT; `Presign` checks the declared type, name and size, which are only the client's word, and pins the declared `Content-Type` in the signature.

<!-- ds:block id=app-ticket-ungdkkjy -->
**App.ticket** · [`examples/recipe-browser-direct-uploads/main.go:201-218`](main.go#L201-L218)

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

`confirm` checks the key is the caller's own pending upload, and `finish` runs `upload.Confirm` on the stored bytes (a refused object is deleted), moves the object out of `pending/` so the sweep leaves it alone, and records and attaches it in one transaction. If recording fails the moved object is deleted, so a failure leaves neither bytes nor a row.

<!-- ds:block id=app-confirm-tjbds98v -->
**App.confirm** · [`examples/recipe-browser-direct-uploads/main.go:239-260`](main.go#L239-L260)

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
**App.finish** · [`examples/recipe-browser-direct-uploads/main.go:268-293`](main.go#L268-L293)

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

`uppy` serves Uppy's AWS S3 multipart protocol per request, with an `Authorize` hook that refuses any key outside the caller's prefix, so one user cannot sign parts for, complete or abort another's upload.

<!-- ds:block id=app-uppy-p728nqjx -->
**App.uppy** · [`examples/recipe-browser-direct-uploads/main.go:300-314`](main.go#L300-L314)

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

`sweepPending` deletes unconfirmed uploads older than 24 hours (longer than any ticket), and `runSweeper` runs it hourly. The library starts no background work.

<!-- ds:block id=sweeppending-675fbb6c -->
**sweepPending** · [`examples/recipe-browser-direct-uploads/main.go:339-341`](main.go#L339-L341)

```go
func sweepPending(ctx context.Context, disk *filesystem.Disk, now time.Time) (int, error) {
	return upload.Sweep(ctx, disk, pendingDir, now.Add(-pendingMaxAge))
}
```
<!-- /ds:block hash=4bc3fd -->

<!-- ds:block id=runsweeper-px4wnzxq -->
**runSweeper** · [`examples/recipe-browser-direct-uploads/main.go:345-357`](main.go#L345-L357)

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

`writeError` shows the message only for codes that describe the client's own input; any other error is answered with the status text, so backend messages and key existence do not leak.

<!-- ds:block id=writeerror-56yg78mg -->
**writeError** · [`examples/recipe-browser-direct-uploads/main.go:386-389`](main.go#L386-L389)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=e4052c -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, the fake or real bucket, tickets, confirm and record, the Uppy routes, the sweeper and the error mapping. |
| `recorded.go` | The Uppy routes that record the file in the complete request with `OnStored`, under the name sent beside the parts. |
| `main_test.go` | The presigned flow, confirm deleting what the policy refuses, the Uppy multipart flow with another user refused, a file recorded by the complete request, and the sweep. |

## Next

- The guide: [Browser direct uploads](../../docs/recipes/browser-direct-uploads.md), with the browser-side `fetch` and Uppy code, the bucket's CORS rule, edge cases and production notes.
- [Uploads](../../docs/concepts/uploads.md), [Files](../../docs/records/files.md) and [S3](../../docs/adapters/s3.md).
- Related recipes: [recipe-resumable-uploads](../recipe-resumable-uploads) (tus through the server), [recipe-mobile-uploads](../recipe-mobile-uploads) (choosing an upload path for an app), [recipe-virus-scanning](../recipe-virus-scanning) (scanning in `Confirm`).
