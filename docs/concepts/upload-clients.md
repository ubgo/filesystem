# Upload clients

[Uploads](uploads.md) covers the server: the policy, `Receive`, the form handler, direct uploads, imports. This page is the other side of the same endpoints: what the browser and phone libraries send, and what your server answers. It covers the JSON every upload endpoint answers with, chunked uploads as Dropzone, FilePond and resumable.js send them, progress while a file streams, folder uploads that keep their layout, and retries that never store a file twice. Read it when you wire a front end or a mobile client to an upload endpoint.

The snippets come from `examples/concepts-upload-clients`. Run it with `go run ./concepts-upload-clients` from the `examples` directory.

## Which client protocol

| The client | Use | Why |
|---|---|---|
| A plain `<form>` or `fetch` with `FormData`, files up to tens of MB | `upload.Handler` | one request per file or per batch; the simplest contract ([Uploads, step 3](uploads.md#step-3--the-form-handler)) |
| Dropzone, FilePond or resumable.js with chunking on | `upload.Chunked` (step 2 here) | the library splits the file into small POSTs that each fit under a proxy's body limit and retry on their own |
| Uppy with `@uppy/aws-s3` (multipart) | `upload.Uppy` | the bytes go straight to the bucket in parts; your server only signs ([Uploads, step 5](uploads.md#step-5--browser-multipart-uploads-with-uppy)) |
| tus-js-client, Uppy's tus plugin, or a phone that must resume after the app is killed | `contrib/tus` | an upload survives restarts on both ends and resumes from the last byte ([Resumable uploads](../recipes/resumable-uploads.md)) |
| A large file and a backend that can presign | `upload.Presign` then `upload.Confirm` | the bytes never pass through your server ([Uploads, step 4](uploads.md#step-4--direct-uploads-presign-then-confirm)) |

Whatever the protocol, the file goes through the same `Policy`: the type is detected from the bytes, the size is capped while streaming, and a refused file is never left on the disk.

## Before you start

- `import "github.com/ubgo/filesystem/upload"`, plus the core `filesystem` package and a driver.
- A `upload.Policy` with at least `MaxSize` and `Allowed`. The example's accepts images up to 1 MiB and refuses empty files with `MinSize: 1`.
- A way to name the signed-in user from a request. Chunked uploads and idempotency keys are kept per owner, so one user can never add to or replay another's upload. The example has one user:

<!-- ds:block id=owner-nkvbk4pn -->
**owner** · [`examples/concepts-upload-clients/main.go:57`](../../examples/concepts-upload-clients/main.go#L57-L57)

```go
func owner(*http.Request) (string, error) { return demoUser, nil }
```
<!-- /ds:block hash=c49227 -->

The example's test client sends one multipart POST with the form fields before the files, which is the order every browser library uses:

<!-- ds:block id=post-2ttqjbqb -->
**post** · [`examples/concepts-upload-clients/main.go:90-119`](../../examples/concepts-upload-clients/main.go#L90-L119)

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

## Step 1 — The JSON contract

Every upload endpoint answers in one of two shapes, so a client needs one parser. On success it is `upload.Response`: `{"files": [...]}`, one entry per stored file with its `key`, `filename` (made safe, for display), `content_type` (detected from the bytes), `size`, `sha256` and, for folder uploads, `path`. On failure it is `upload.ErrorResponse`: `{"error": "...", "code": "..."}`, where `code` is the `filesystem.Code`, a closed set the client can switch on without parsing English.

<!-- ds:block id=jsoncontract-7ek5apjw -->
**jsonContract** · [`examples/concepts-upload-clients/steps.go:36-53`](../../examples/concepts-upload-clients/steps.go#L36-L53)

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

Check:

```text
success 201: {"files":[{"key":"uploads/<random>.png","filename":"photo.png","content_type":"image/png","size":300,"sha256":"<hex>","path":"photo.png"}]}
failure 415: {"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
```

The status follows the code, the same way on every endpoint (`fshttp.StatusOf`):

| `code` | Status | What the client should do |
|---|---|---|
| (none, success) | 201, or 200 when a `Partial` request stored only some files | read `files`, and `failed` for a partial request |
| `rejected` | 415 | tell the user the type or name is not accepted; do not retry |
| `too_large` | 413 | tell the user the limit; do not retry the same file |
| `invalid_argument`, `invalid_key` | 400 | a client bug (a malformed form, too many files); do not retry |
| `permission`, `read_only` | 403 | sign in again or stop |
| `quota_exceeded` | 507 | the user's storage is full; do not retry until they free space |
| `exists`, `conflict` | 409 | a name collision, or an idempotent twin still running; retry the latter later |
| `unavailable` | 503, with `Retry-After` when the server set one | retry with backoff, after `Retry-After` when present |
| `unknown` | 500 | retry later; the detail is in the server's log, never in the response |

The `error` text reaches the client only for codes about the client's input; for server-side failures it is the status text alone, so a storage key or a backend's error message never leaks. A `Partial` request (`HandlerOptions.Partial`) adds `"failed": [{"filename", "error", "code"}]` beside the stored files. Uppy's own endpoints answer the shapes Uppy expects, documented in [Uploads, step 5](uploads.md#step-5--browser-multipart-uploads-with-uppy).

## Step 2 — Chunked uploads

Dropzone, FilePond and resumable.js send a large file as many small POSTs. Each carries the upload's ID, the chunk's index, the chunk count and the total size as form fields, then the chunk's bytes. `upload.Chunked(disk, policy, ChunkedOptions{Staging, Owner})` returns the handler; `Fields` names the library's fields: `upload.DropzoneFields()` (the default; FilePond configured the same way) or `upload.ResumableFields()` (resumable.js and flow.js, which count chunks from 1).

<!-- ds:block id=chunked-x4e8fmh9 -->
**chunked** · [`examples/concepts-upload-clients/steps.go:60-92`](../../examples/concepts-upload-clients/steps.go#L60-L92)

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

Check:

```text
chunk 0: 200 {"received":1,"total":3}
chunk 2: 200 {"received":2,"total":3}
chunk 2: 200 {"received":2,"total":3}
chunk 1: 201 {"files":[{"key":"uploads/<random>.png",...}]}
```

Chunks may arrive in any order and be retried: the repeated chunk 2 changes nothing. Each answers 200 with `{"received", "total"}`, and the request that completes the set assembles the chunks in order and stores the file through `Receive`, so the policy applies to the whole file exactly as for a single upload (detected type, size, name rules, `Process`, `Inspect`), and answers 201 with the step 1 success shape. `OnStored` runs then, to create your database row.

How it holds up:

- **Owners.** Chunks wait under `Staging`, in a directory per owner and upload ID, so one user can never add to or complete another's upload. `Chunked` returns `invalid_argument` at start-up when `Staging` is missing or the root, `Owner` is nil, or the staging disk lacks `ConditionalWrite` and `CompareAndSwap`.
- **Progress across instances.** Progress lives in a small manifest beside the chunks, updated by compare-and-swap after each chunk, so a chunk costs one small read and write rather than a listing, and concurrent chunks on several instances agree on when the set is complete.
- **Bounded staging.** Each chunk is cut off as it streams once it would take the upload past its declared size, and the declared size past the policy's `MaxSize`. A chunk is at most `MaxChunkBytes` (16 MiB by default), a file at most `MaxChunks` (10,000) chunks. An owner may have at most `MaxUploadsPerOwner` uploads in progress (16 by default); one more is `quota_exceeded`. That count is exact under concurrency, and an abandoned upload stops counting once a sweep has removed its manifest, so schedule `jobs.UploadSweep` (or `upload.Sweep`) on `Staging`.
- **Early refusal.** A first chunk of a refused type is refused at once, before the rest is sent; chunks that do not add up to the declared size are refused, and an assembly failure removes the chunks.
- **A crashed assembler.** An assembler that crashed or stalled is taken over after `AssemblyTimeout` (15 minutes). Each takeover increments a generation in the manifest, and the assembler renews its claim against it before settling the file and before removing the chunks, so a superseded one stops with `conflict` and the file is stored and reported once. Nothing fences after that last renewal: a settle plus `OnStored` that together run longer than `AssemblyTimeout` can still store the file twice, so keep `OnStored` short or raise the timeout. `ChunkedOptions.Now` sets the clock claims are stamped and judged by (nil means `time.Now`); every instance serving one staging directory should share a clock, and a test can move a fake one past the timeout instead of waiting.
- **A staging disk.** `StagingDisk` puts the chunks on another disk (a local one) when the destination lacks `ConditionalWrite` and `CompareAndSwap`. The local driver has both wherever the OS reports inode change times (Linux, macOS, the BSDs).

On the client, set the chunk size well under your proxy's body limit (Dropzone's `chunkSize`, resumable.js's `chunkSize`), and turn on the library's own retries: a failed chunk is safe to send again.

## Step 3 — Progress on the server

A browser knows how many bytes it has sent; the server knows how many have reached the disk, which is what matters when the disk is slow or remote. `Policy.Progress(ctx, info, received)` is called about every 64 KiB (`upload.ProgressStep`) while a file streams to the disk and once more with the total. It covers everything that goes through `Receive`: the form handler, chunked assembly, URL imports and tus.

<!-- ds:block id=progress-cy4jj5kf -->
**progress** · [`examples/concepts-upload-clients/steps.go:98-116`](../../examples/concepts-upload-clients/steps.go#L98-L116)

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

Check: `progress: 3 calls, last 196624 of 196624 bytes`.

Feed it to a WebSocket or server-sent events keyed by the user and the file's name, or log uploads that are slow. Keep the callback quick and non-blocking: it runs inside the copy loop, so a slow callback slows the upload. It is not called for presigned or Uppy uploads, whose bytes never pass through your server; there the browser's own progress is the only one.

## Step 4 — Folder uploads

When a user drops a folder, a folder-aware client sends each file with its relative path as its file name (`Trip/day1/beach.png`); with plain `FormData`, pass `file.webkitRelativePath` as the third argument of `append`. The form handler keeps that path, made safe by `upload.SafePath`: each segment cleaned like a file name, `.` and `..` segments dropped so a client cannot climb out of the folder, and at most `upload.MaxPathDepth` (32) directories. It is `Info.Path` while the file is received and `path` in the response. `upload.FolderKey(dir)` stores each file at that path under `dir`, so the folder keeps its layout:

<!-- ds:block id=folder-gns7cy6w -->
**folder** · [`examples/concepts-upload-clients/steps.go:123-149`](../../examples/concepts-upload-clients/steps.go#L123-L149)

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

Check:

```text
folder upload: 201
  users/u1/drive/Trip/day1/beach (1).png
  users/u1/drive/Trip/day1/beach.png
  users/u1/drive/Trip/day2/hike.png
  users/u1/drive/Trip/etc/cron.png
```

`Trip/../../../etc/cron.png` landed inside the folder as `Trip/etc/cron.png`. Two files with one path meet at one key, so choose `Policy.Collision`: `CollisionRename` (as here) keeps both, `CollisionFail` refuses the second with `exists`, the default overwrites. Put the user (or tenant) in `dir`, as the example does, so two users' folders never meet. `MaxFiles` must cover the folder's file count; a large folder is better sent as one request per file, which most libraries do anyway.

## Step 5 — Retries without duplicates

A phone that queues uploads while offline, or a request that timed out after the server had already stored the file, will send the same upload again. Without help, the server stores it twice. The fix is the `Idempotency-Key` header (`upload.HeaderIdempotencyKey`): the client generates one key per file, and the server records the response of the first request that carries it.

<!-- ds:block id=idempotent-hfeg247q -->
**idempotent** · [`examples/concepts-upload-clients/steps.go:156-185`](../../examples/concepts-upload-clients/steps.go#L156-L185)

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

Check:

```text
attempt 1: 201 replayed=""
attempt 2: 200 replayed="true"
attempt 3: 422 replayed=""
objects stored: 1
```

With `HandlerOptions.Idempotency` (and the same field in `PresignHandlerOptions`), the first request runs and its response is recorded; every retry gets that response back with 200 and `Idempotent-Replayed: true`, and nothing is stored again. A twin that arrives while the first is still running gets 409 `conflict`; a failed request is not recorded, so its retry runs. A key is bound to its request: its method, path and content (for a form, each part's name, file name and bytes, so a retry with a new multipart boundary still matches), and the same key sent with another file is refused with 422, as attempt 3 shows. Keys are scoped by `Idempotency.Owner(r)`, so one user's key never replays another's response.

`upload.NewMemoryIdempotency(MemoryIdempotencyOptions{TTL, MaxEntries})` keeps responses in this process, for the TTL and up to `MaxEntries` (100,000 by default; when full, the oldest key is dropped). Behind a load balancer, implement `upload.IdempotencyStore` (`Reserve`, `Complete`, `Release`) on a shared database or Redis, so a retry that lands on another instance is still recognised.

The client contract, for an offline queue:

1. When a file is queued, generate one key for it (a UUID) and store it with the queued file.
2. Send it as `Idempotency-Key` on every attempt for that file, never a fresh one per attempt.
3. Treat 200 with `Idempotent-Replayed: true` exactly like 201: the file is stored.
4. On 409, wait and retry: the first attempt is still running. On 422, the key was reused for another file, a client bug.
5. Keep retrying for less time than the server's TTL, or a late retry after the record expired stores the file again.

## Step 6 — Tell the client the limits

A client that knows the limits can resize a photo or skip a refused file before spending the user's data on it. `upload.LimitsHandler(policy, LimitsOptions{MaxWidth, MaxHeight, Resolve})` answers GET with the sizes, types, extensions and image dimensions the server enforces (`upload.Limits`), resolved per request like the upload itself, so a free and a paid user see their own limits. The dimensions describe a check your `Inspect` step makes; they do not add one. The phone side, downscaling to those limits before upload, is in [Mobile uploads](../recipes/mobile-uploads.md).

## Pitfalls

- A new `Idempotency-Key` per attempt. It defeats the point: every retry runs again. Generate the key when the file is queued, not when the request is built.
- A memory idempotency store behind a load balancer. A retry routed to another instance runs again. Use a shared store, or sticky sessions at the least.
- `MemoryIdempotency.MaxEntries` is shared by every owner. One owner sending many keys can push out others' entries, including requests still running, whose retries would then run again. Size it for your traffic or keep a store per tenant.
- No sweep on the chunk staging area. Abandoned chunked uploads stay, and an owner whose clients abandon uploads stays at `MaxUploadsPerOwner` and is refused with `quota_exceeded`. Schedule `jobs.UploadSweep`.
- A chunk size above the proxy's body limit (nginx's `client_max_body_size` is 1 MB by default). The proxy answers 413 before your handler runs, and the client sees no JSON. Keep chunks under it.
- Trusting `filename` or `path` in the response for anything but display. They are the client's names, made safe; the key is the identity.
- A slow `Progress` callback. It runs inside the copy loop; hand the number to a channel or an atomic and return.

## Next

- [Uploads](uploads.md): the policy, `Receive` and every server-side path.
- [Integrity](integrity.md): the SHA-256 in every response, and checking it end to end.
- Recipes: [Mobile uploads](../recipes/mobile-uploads.md), [Resumable uploads](../recipes/resumable-uploads.md), [Browser direct uploads](../recipes/browser-direct-uploads.md).
- Reference: [the upload package](../../upload/README.md).
