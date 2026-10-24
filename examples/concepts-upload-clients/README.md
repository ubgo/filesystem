# Upload clients: chunks, progress, folders, retries and the JSON contract

The upload endpoints are written for the client libraries browsers and phones already use. This program drives them the way those clients do: it reads the JSON every endpoint answers with, sends one file in three chunks out of order and with a retry as Dropzone does, reports progress while a file streams, uploads a dropped folder that keeps its layout, and retries one upload under an `Idempotency-Key` without storing it twice. It uses the core `filesystem` package, `memory` and `upload`. The guide [Upload clients](../../docs/concepts/upload-clients.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-upload-clients
go test ./concepts-upload-clients
```

Every request goes to an `httptest` recorder over a memory disk: no network, no environment variables.

## What it does

```sh
go run ./concepts-upload-clients
```

```text
success 201: {"files":[{"key":"uploads/<random>.png","filename":"photo.png","content_type":"image/png","size":300,"sha256":"<hex>","path":"photo.png"}]}
failure 415: {"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
chunk 0: 200 {"received":1,"total":3}
chunk 2: 200 {"received":2,"total":3}
chunk 2: 200 {"received":2,"total":3}
chunk 1: 201 {"files":[{"key":"uploads/<random>.png","filename":"photo.png","content_type":"image/png","size":300,"sha256":"<hex>","path":"photo.png"}]}
progress: 3 calls, last 196624 of 196624 bytes
folder upload: 201
  users/u1/drive/Trip/day1/beach (1).png
  users/u1/drive/Trip/day1/beach.png
  users/u1/drive/Trip/day2/hike.png
  users/u1/drive/Trip/etc/cron.png
attempt 1: 201 replayed=""
attempt 2: 200 replayed="true"
attempt 3: 422 replayed=""
objects stored: 1
```

The random key and the digest are replaced with markers so the output is the same on every run.

1. A stored file answers 201 with `{"files": [...]}`; a refused one answers with the status for its code and `{"error", "code"}`.
2. Three chunks arrive as 0, 2, 2 (a retry) and 1. Each answers 200 with how many distinct chunks have arrived; the one that completes the set answers 201 with the stored file, exactly as a single upload would.
3. A 196,624-byte file reports progress three times, the last with the full size.
4. Four files dropped as a folder are stored at their relative paths under the user's drive; the `..` segments are dropped, and the second `beach.png` is renamed rather than overwriting the first.
5. The first attempt stores the file; the retry with the same key gets the recorded response back marked `Idempotent-Replayed: true`; the same key with another file is refused with 422. One object is stored.

## The code

The JSON contract.

<!-- ds:block id=jsoncontract-7ek5apjw -->
**jsonContract** · [`examples/concepts-upload-clients/steps.go:36-53`](steps.go#L36-L53)

```go
func jsonContract(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{})
	ok, err := post(ctx, h, nil, file("photo.png", photo))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "success %d: %s\n", ok.Code, stable(ok.Body.String()))
	bad, err := post(ctx, h, nil, file("page.png", "<!DOCTYPE html><p>hi</p>"))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "failure %d: %s\n", bad.Code, strings.TrimSpace(bad.Body.String()))
	return nil
}
```
<!-- /ds:block hash=6c8377 -->

One file in three chunks, out of order and with a retry.

<!-- ds:block id=chunked-x4e8fmh9 -->
**chunked** · [`examples/concepts-upload-clients/steps.go:60-92`](steps.go#L60-L92)

```go
func chunked(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h, err := upload.Chunked(disk, policy, upload.ChunkedOptions{Staging: staging, Owner: owner})
	if err != nil {
		return err
	}
	f := upload.DropzoneFields()
	send := func(index int) (int, string, error) {
		chunk := photo[index*chunkSize : min((index+1)*chunkSize, len(photo))]
		rec, err := post(ctx, h, nil,
			part{field: f.UploadID, value: "4f1c-photo"},
			part{field: f.Index, value: itoa(index)},
			part{field: f.Total, value: itoa((len(photo) + chunkSize - 1) / chunkSize)},
			part{field: f.TotalSize, value: itoa(len(photo))},
			part{field: f.File, file: "photo.png", value: chunk},
		)
		if err != nil {
			return 0, "", err
		}
		return rec.Code, stable(rec.Body.String()), nil
	}
	for _, index := range []int{0, 2, 2, 1} {
		code, body, err := send(index)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "chunk %d: %d %s\n", index, code, body)
	}
	return nil
}
```
<!-- /ds:block hash=f701cf -->

Progress while a file streams.

<!-- ds:block id=progress-cy4jj5kf -->
**progress** · [`examples/concepts-upload-clients/steps.go:98-116`](steps.go#L98-L116)

```go
func progress(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	var calls, last atomic.Int64
	p := policy
	p.Progress = func(_ context.Context, _ upload.Info, received int64) {
		calls.Add(1)
		last.Store(received)
	}
	big := pngHeader + strings.Repeat("x", 3*upload.ProgressStep)
	res, err := upload.Receive(ctx, disk, strings.NewReader(big), "big.png", p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "progress: %d calls, last %d of %d bytes\n", calls.Load(), last.Load(), res.Size)
	return nil
}
```
<!-- /ds:block hash=66faa7 -->

A dropped folder.

<!-- ds:block id=folder-gns7cy6w -->
**folder** · [`examples/concepts-upload-clients/steps.go:123-149`](steps.go#L123-L149)

```go
func folder(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	p := policy
	p.Key = upload.FolderKey("users/" + demoUser + "/drive")
	p.Collision = upload.CollisionRename
	h := upload.Handler(disk, p, upload.HandlerOptions{MaxFiles: 4})
	rec, err := post(ctx, h, nil,
		file("Trip/day1/beach.png", photo),
		file("Trip/day2/hike.png", photo),
		file("Trip/../../../etc/cron.png", photo),
		file("Trip/day1/beach.png", photo),
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "folder upload: %d\n", rec.Code)
	for e, err := range disk.List(ctx, "users", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "  %s\n", e.Key)
	}
	return nil
}
```
<!-- /ds:block hash=a20f98 -->

Retries under an `Idempotency-Key`.

<!-- ds:block id=idempotent-hfeg247q -->
**idempotent** · [`examples/concepts-upload-clients/steps.go:156-185`](steps.go#L156-L185)

```go
func idempotent(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	store, err := upload.NewMemoryIdempotency(upload.MemoryIdempotencyOptions{TTL: replayWindow})
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{
		Idempotency: &upload.Idempotency{Store: store, Owner: owner},
	})
	key := http.Header{upload.HeaderIdempotencyKey: {"9b2e7c0a-queued-photo-1"}}
	for attempt, body := range []string{photo, photo, photo + "changed"} {
		rec, err := post(ctx, h, key, file("photo.png", body))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "attempt %d: %d replayed=%q\n", attempt+1, rec.Code, rec.Header().Get(upload.HeaderIdempotentReplayed))
	}
	n := 0
	for _, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		n++
	}
	fmt.Fprintf(out, "objects stored: %d\n", n)
	return nil
}
```
<!-- /ds:block hash=9edadc -->

The test client: one multipart POST, fields before files, as the browser libraries send them.

<!-- ds:block id=post-2ttqjbqb -->
**post** · [`examples/concepts-upload-clients/main.go:90-119`](main.go#L90-L119)

```go
func post(ctx context.Context, h http.Handler, header http.Header, parts ...part) (*httptest.ResponseRecorder, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.file == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			w, err = mw.CreateFormFile(p.field, p.file)
		}
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(w, p.value); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, nil
}
```
<!-- /ds:block hash=ab6bd4 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, the program and the multipart test client |
| `steps.go` | One function per step |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Upload clients](../../docs/concepts/upload-clients.md).
- The server side of the same endpoints: [Uploads](../../docs/concepts/uploads.md) ([concepts-uploads](../concepts-uploads)).
- Recipes: [Mobile uploads](../../docs/recipes/mobile-uploads.md) ([recipe-mobile-uploads](../recipe-mobile-uploads)), [Resumable uploads](../../docs/recipes/resumable-uploads.md) ([recipe-resumable-uploads](../recipe-resumable-uploads)).
