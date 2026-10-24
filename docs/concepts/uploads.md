# Uploads

Bytes arrive from browsers and phones in several ways: a form post through your server, a direct upload to the bucket with a signed URL, a browser multipart upload of a large video, a resumable upload over a flaky mobile link, an import from a URL the user pasted. The `upload` package handles each of them under one `Policy`, and every path checks the bytes actually stored, never the client's word. This page explains the policy, each path end to end with its guarantees, the Process and Inspect hooks, and sweeping uploads nobody finished.

The snippets come from `examples/concepts-uploads`. Run it with `go run ./concepts-uploads` from the `examples` directory.

## The guarantees

Every path below keeps these, whatever the client sends:

- The type is detected from the content's first bytes (`sniff.Detect`), never taken from the `Content-Type` header or the file name.
- HTML, SVG, XML, JavaScript and executables (`upload.DefaultDenied`), and file names with a script or executable extension in any position (`upload.DefaultDeniedExtensions`, which catches `photo.php.jpg`), are refused unless the policy sets `AllowActiveContent`.
- The size is counted while streaming, never taken from a header; nothing larger than `MaxSize` is stored.
- A refused file leaves nothing on the disk: it is either never written or deleted before the error is returned.
- The client's file name never reaches the key. It is reduced to a safe display name (`upload.SafeFilename`: base name only, no control characters, at most 255 bytes) and returned for your database.

| Path | Function | Bytes pass through your server | Process runs | Inspect runs | Checked when |
|---|---|---|---|---|---|
| A stream you hold | `upload.Receive` | yes | yes | yes | before and while storing |
| Form post | `upload.Handler` | yes | yes | yes | before and while storing |
| Direct upload | `upload.Presign`, then `upload.Confirm` | no | no | yes | at Presign (declared) and at Confirm (stored bytes) |
| Browser multipart (Uppy) | `upload.Uppy` | no | no | yes | at create (declared) and at complete (stored bytes) |
| Resumable (tus) | `contrib/tus` | yes | yes | yes | when the upload finishes |
| Import from a URL | `upload.FromURL` | yes | yes | yes | before and while storing |
| Files as records | `records.Store.Upload` | yes | yes | yes | as `Receive` |

## Step 1 — The policy

<!-- ds:block id=policy-e9b5m967 -->
**policy** · [`examples/concepts-uploads/main.go:43-47`](../../examples/concepts-uploads/main.go#L43-L47)

```go
var policy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"image/*", "application/pdf"},
	Key:     upload.RandomKey(uploadDir),
}
```
<!-- /ds:block hash=c35128 -->

| Field | Meaning |
|---|---|
| `MaxSize` | the largest accepted file in bytes; required |
| `MinSize` | the smallest accepted file in bytes; `1` refuses empty files (usually a client bug), `0` accepts any size; refused with `rejected` and deleted |
| `Allowed` | accepted MIME patterns (`image/*`, `application/pdf`, `*/*`); required, there is no accept-everything default |
| `Denied` | refused patterns, checked after `Allowed`; `DefaultDenied` is always added |
| `AllowActiveContent` | lifts `DefaultDenied` and `DefaultDeniedExtensions`, for an endpoint that must accept HTML or scripts and serves them from another origin |
| `Detector` | a `sniff.Detector` labelling formats the built-in detection does not know (DICOM, STL, Parquet), so `Allowed` can name them instead of `application/octet-stream`; consulted only when the bytes come back octet-stream, its answer still goes through `Allowed`, `Denied` and the active-content rule; see [Security](security.md#formats-the-detection-does-not-know) |
| `Key` | a `KeyFunc` choosing the storage key from `Info{Filename, ContentType}`; nil means `RandomKey("")` |
| `Visibility` | the stored files' visibility; empty means the disk's default |
| `Collision` | what happens when the key holds an object: overwrite (default), fail with `exists`, or rename to the first free `name (1).ext`; see below |
| `Headers` | a `HeadersFunc` choosing the HTTP headers stored with each file (`Cache-Control`, `Content-Disposition`, `Content-Encoding`); nil stores none |
| `Extensions` | when set, the only file-name extensions accepted (`.pdf`, `.jpg`), without case; a name without an extension passes and its type is still checked |
| `MatchExtension` | refuse a file whose name's extension means another type than its bytes (a PNG named `invoice.pdf`); unknown extensions pass |
| `Process` | transform each accepted file before it is stored |
| `Inspect` | examine each file once stored and before success is reported; an error deletes it |

`Policy.Validate()` checks the policy itself (a size, at least one type, well-formed extensions, a known visibility). Every path calls it, so a misconfigured endpoint fails with `CodeInvalidArgument` instead of accepting everything; call it at start-up to fail earlier. `upload.RandomKey(dir)` names files with 128 random bits in lowercase base32 and the extension of the detected type (`uploads/3qx…7a.png`); a `KeyFunc` of your own sees only the file's `Info`, never the request, so to place keys per user build the `Policy` per request with a `KeyFunc` that closes over the user (`p.Key = upload.RandomKey("users/" + id)`); it must not trust `Info.Filename` beyond cosmetics.

### Key layouts and name collisions

`upload.RandomKey(dir)` is the default and never collides. `upload.ShardedKey(dir, depth)` is the same random name split into `depth` levels of two-character directories (`photos/q3/7a/q37a….png`, 1,024 directories per level), so a local disk holding millions of files never has one huge directory. `upload.NameKey(dir)` keeps the client's name made safe (`drive/Q3 report.pdf`), for products where users see and choose names; two uploads of one name then meet at one key, and `Policy.Collision` decides what happens:

| `Collision` | Second upload of `photo.png` |
|---|---|
| `CollisionOverwrite` (default) | replaces the first |
| `CollisionFail` | refused with `exists`; the first is kept |
| `CollisionRename` | stored as `photo (1).png`, then `photo (2).png`, … (up to `MaxRenameAttempts`, then `exists`) |

Fail and rename write with `IfNotExists`, so they need the `ConditionalWrite` capability, and a racing upload that takes the chosen name first gets `exists` rather than overwriting it. `Receive` (and so the form handler, imports and tus) and the Uppy handler apply the policy; `Presign` refuses anything but overwrite with `unsupported`, because a presigned PUT replaces whatever is at its key. `upload.FreeKey(ctx, disk, key)` is the renaming step on its own.

A key that depends on who uploads (the tenant an authentication middleware put on the request's context) needs the context: set `Policy.KeyContext`, a `KeyContextFunc` that gets the upload's context beside the `Info`, instead of `Key`. The handlers pass the request's context, so one policy serves every tenant without a `Resolver` building a policy per request. Set `Key` or `KeyContext`, not both: `Validate` refuses a policy with both.

```go
policy.KeyContext = func(ctx context.Context, info upload.Info) (string, error) {
	tenant, ok := tenantFrom(ctx) // your middleware's value
	if !ok {
		return "", filesystem.Errorf(filesystem.CodePermission, "no tenant")
	}
	return upload.RandomKey("tenants/" + tenant)(info)
}
```

The program stores one file under a sharded key, then uploads `photo.png` twice under `NameKey` with rename and with fail:

<!-- ds:block id=layouts-fecj8ewm -->
**layouts** · [`examples/concepts-uploads/more.go:57-85`](../../examples/concepts-uploads/more.go#L57-L85)

```go
func layouts(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	sharded := policy
	sharded.Key = upload.ShardedKey("photos", shardDepth)
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "a.png", sharded)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sharded: %d directory levels under photos/\n", strings.Count(path.Dir(res.Key), "/"))
	for _, c := range []upload.Collision{upload.CollisionRename, upload.CollisionFail} {
		named := policy
		named.Key = upload.NameKey("drive/" + string(c))
		named.Collision = c
		var got []string
		for range 2 {
			res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "photo.png", named)
			if err != nil {
				got = append(got, string(filesystem.CodeOf(err)))
				continue
			}
			got = append(got, res.Key)
		}
		fmt.Fprintf(out, "collision %s: %s\n", c, strings.Join(got, ", "))
	}
	return nil
}
```
<!-- /ds:block hash=8ca20f -->

Check: `sharded: 2 directory levels under photos/`, `collision rename: drive/rename/photo.png, drive/rename/photo (1).png` and `collision fail: drive/fail/photo.png, exists`. Which layout: `RandomKey` when users never see keys (avatars, attachments, anything served by a signed link); `ShardedKey` when the disk is local or SFTP and will hold more than about 100,000 files; `NameKey` with a collision policy when users browse files by name (a drive, a document library); `FolderKey` for folder uploads ([Upload clients](upload-clients.md#step-4--folder-uploads)).

Try it: [`examples/concepts-uploads`](../../examples/concepts-uploads) runs this section in `tenants.go` ([`tenantPolicy`](ds:block?id=tenantpolicy-w6k2r9md)): `go run ./concepts-uploads` from `examples/`.

### Stored HTTP headers

`Policy.Headers` stores headers with each upload, which the backend's own URLs and `fshttp` then send with the file. `upload.StaticHeaders(h)` stores the same headers with every file, such as a year-long `Cache-Control` for random keys that are never reused; `upload.Attachment(cacheControl)` makes every file download under the name the client gave it (`attachment; filename=report.pdf`, encoded per RFC 2231 when the name is not plain ASCII), which is how a random key gets the original name back. A function of your own sees the file's `Info` after `Process` has run, so the name and type are the stored ones; its error refuses the upload.

```go
p := policy
p.Headers = upload.Attachment("public, max-age=31536000, immutable")
```

The disk must store headers (`Capabilities.Headers`: memory, S3, GCS and Azure do; local and SFTP do not), otherwise every upload fails with `unsupported` rather than storing the file without them. `Receive` (and so the form handler, URL imports and tus) and the Uppy handler apply them. `Presign` pins them in the signed PUT: the ticket's `Headers` list them next to `Content-Type`, the client must send them unchanged, and the backend refuses an upload without them and stores them. That works where the disk can guarantee it: a driver that declares `Capabilities.PresignHeaders`, or a disk whose driver stores headers and whose `URLSigner` pins them (`signedurl.Signer` with `fshttp.Signed`, so memory in development). Anywhere else `Presign` fails with `unsupported` rather than issue a ticket whose upload would land without them; local disk, which stores no headers, is one such place.

## Step 2 — Receive: the core of every server-side path

`upload.Receive(ctx, disk, body, filename, policy)` reads the first 512 bytes, detects the type, checks it and the name against the policy, runs `Process`, chooses the key, streams the rest to the disk with the size cap, and runs `Inspect`. It returns a `Result` with `Key`, `Filename`, `ContentType`, `Size` and `SHA256`.

<!-- ds:block id=receive-eszpqjvz -->
**receive** · [`examples/concepts-uploads/main.go:91-108`](../../examples/concepts-uploads/main.go#L91-L108)

```go
func receive(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), `..\..\Holiday Photo.PNG`, policy)
	if err != nil {
		return err
	}
	shape := path.Dir(res.Key) + "/<random>" + path.Ext(res.Key)
	fmt.Fprintf(out, "receive: key %s name %q type %s size %d\n", shape, res.Filename, res.ContentType, res.Size)
	_, err = upload.Receive(ctx, disk, strings.NewReader(htmlFile), "photo.png", policy)
	fmt.Fprintf(out, "receive html named photo.png: %s\n", filesystem.CodeOf(err))
	big := strings.NewReader(pngFile + strings.Repeat("x", maxUpload))
	_, err = upload.Receive(ctx, disk, big, "big.png", policy)
	fmt.Fprintf(out, "receive over MaxSize: %s, objects stored: %d\n", filesystem.CodeOf(err), count(ctx, disk))
	return nil
}
```
<!-- /ds:block hash=a2ec4e -->

Check: `receive: key uploads/<random>.png name "Holiday Photo.PNG" type image/png size 29`, then `receive html named photo.png: rejected` and `receive over MaxSize: too_large, objects stored: 1` (only the first PNG).

Errors: `CodeRejected` for a refused type, name or inspection, `CodeTooLarge` over `MaxSize`, the disk's own codes for storage failures.

## Step 3 — The form handler

`upload.Handler(disk, policy, HandlerOptions{})` accepts `multipart/form-data` POSTs. Files stream straight to the disk: no temporary copy, no buffering of the whole body. The whole request is capped at `MaxFiles × MaxSize` plus 64 KiB for form overhead.

| Option | Meaning |
|---|---|
| `Field` | the form field holding files; empty means `"file"` (`DefaultField`) |
| `MaxFiles` | files per request; 0 means 1; more is `invalid_argument` |
| `Partial` | store every file it can and list the others, instead of all or nothing |
| `MaxTotal` | caps the bytes of all the files of one request together: each file may use only what the earlier ones left, and one that does not fit is `too_large` (`the files of this request exceed N bytes in total`); the request body is capped at `MaxTotal` plus form overhead when that is lower than `MaxFiles × MaxSize` |
| `Resolve` | a `Resolver` choosing the policy for each request, before any byte of the body is read; see below |
| `OnStored` | runs after the request's files are stored, to create your database rows; if it fails, the stored files are deleted and the request fails |
| `Errors` | an `fshttp.ErrorWriter` for every failed request; nil is the `{error, code}` JSON (see [Errors](errors.md#your-own-error-envelope)) |
| `Respond` | an `upload.SuccessWriter` for the success body; nil is the `{files, failed}` JSON, byte for byte as before (see below) |

### Your own success envelope

The success bodies are defaults too. `HandlerOptions.Respond`, and the same field on `PresignHandlerOptions`, `ChunkedOptions` and `LimitsOptions`, takes an `upload.SuccessWriter`, `func(w, r, status, body upload.SuccessBody)`. The body is one of a closed set, `upload.Response`, `upload.ChunkProgress`, `upload.Ticket` or `upload.Limits`, so the writer can switch on it; `status` is the one the default would send. A nil writer is `upload.JSONSuccess`, whose output is exactly what the handlers answered before the option existed, so Uppy, Dropzone and clients built on the upload JSON contract keep working. The Uppy and tus bodies have no such option, because their clients parse them as the protocols define.

```go
// Every success as {"data": …}, the envelope the rest of the API uses.
wrap := func(w http.ResponseWriter, r *http.Request, status int, body upload.SuccessBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Data upload.SuccessBody `json:"data"`
	}{body})
}
uploads := upload.Handler(disk, policy, upload.HandlerOptions{Respond: wrap, Errors: problem})
tickets := upload.PresignHandlerWith(disk, policy, time.Minute, upload.PresignHandlerOptions{Respond: wrap})
```

With `Idempotency`, a replayed response sends the recorded body under the `Content-Type` the first response set (`Recorded.ContentType`), so an envelope in another media type (`application/vnd.api+json`, a protobuf) replays as itself; only the status becomes 200. A record without a type, such as one a store wrote before the field existed, replays as `application/json`. A store of your own that persists `Recorded` must keep `ContentType` beside `Body`.

Try it: [`examples/concepts-uploads`](../../examples/concepts-uploads) runs this section in `envelope.go` ([`dataEnvelope`](ds:block?id=dataenvelope-q7w3n8kd)): `go run ./concepts-uploads` from `examples/`, then look for `envelope respond=true: upload 201 data.files[1], records 201 data.files[1], share listing 200 data[1]`.

### Sizes: empty files, a request's total, and the body cap

Three size checks besides `MaxSize`, each closing a different gap:

- **`Policy.MinSize`.** `1` refuses an empty file with `rejected`, which is almost always a client bug (a file picker that lost its file, a mobile upload cut off before the first byte). A form whose files have a known structure can set a higher floor.
- **`HandlerOptions.MaxTotal`.** Caps the bytes of all the files of one request together: each file may use only what the earlier ones left, and one that does not fit is `too_large`. Without it, a request may carry `MaxFiles × MaxSize`.
- **The body cap.** The handler wraps the request body in `http.MaxBytesReader`, at `MaxFiles × MaxSize` plus 64 KiB of form overhead (or `MaxTotal` plus overhead when that is lower). An oversized body is refused once that much has been read, never read to its end, so a client cannot tie up a connection streaming gigabytes at a 1 MB endpoint. A proxy in front (nginx's `client_max_body_size`) should cap it again, lower than any client needs to go.

<!-- ds:block id=sizelimits-hkgs2nwg -->
**sizeLimits** · [`examples/concepts-uploads/more.go:118-146`](../../examples/concepts-uploads/more.go#L118-L146)

```go
func sizeLimits(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	nonEmpty := policy
	nonEmpty.MinSize = 1
	_, err = upload.Receive(ctx, disk, strings.NewReader(""), "empty.png", nonEmpty)
	fmt.Fprintf(out, "empty file: %s\n", filesystem.CodeOf(err))
	two := upload.Handler(disk, policy, upload.HandlerOptions{MaxFiles: 2, MaxTotal: requestTotal})
	status, _, err := postForm(ctx, two, formFile{"a.png", pngFile}, formFile{"b.png", pngFile + pngFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "two files over MaxTotal %d: %d\n", requestTotal, status)
	small := policy
	small.MaxSize = freeLimit
	one := upload.Handler(disk, small, upload.HandlerOptions{})
	req := formRequest(ctx, "", formFile{"big.png", pngFile + strings.Repeat("x", hugeBody)})
	body := &countingReader{r: req.Body}
	req.Body = io.NopCloser(body)
	rec := httptest.NewRecorder()
	one.ServeHTTP(rec, req)
	fmt.Fprintf(out, "body of %d bytes: %d after reading under %d\n", hugeBody, rec.Code, readBound)
	if body.n >= readBound {
		return fmt.Errorf("the handler read %d bytes of an oversized body", body.n)
	}
	return nil
}
```
<!-- /ds:block hash=64c05e -->

Check: `empty file: rejected`, `two files over MaxTotal 60: 413`, and `body of 8388608 bytes: 413 after reading under 131072`: of an 8 MiB body, the handler read one buffer before refusing it.

### Limits per user, plan or request

Limits often follow the user: a free plan takes 5 MB files, a paid one 100 MB; a team's uploads go under its own directory. `HandlerOptions.Resolve` (and the same field in `UppyOptions` and `PresignHandlerOptions`, for `upload.PresignHandlerWith`) is a `Resolver`, `func(r *http.Request, base Policy) (Policy, error)`: it gets the handler's policy and returns the one for this request, which is validated like any other; a resolved policy that fails validation is your server's misconfiguration, so the request is answered 500 with the status text only (code `unknown`), never the configuration detail. It runs before any byte of the request body is read, so it is also where to refuse early: an unauthenticated request, a quota already used up, or a rate limit. A refusal is answered with its code's status; return `unavailable` with `WithRetryAfter(d)` for a rate limit, and the client gets 503 with `Retry-After`. Its message reaches the client only for the codes about the client's input, as with every error here.

The program serves three callers from one handler: a free plan limited to 64 bytes, a paid plan with the full limit, and a caller over its rate limit:

<!-- ds:block id=perrequest-jrzefwkt -->
**perRequest** · [`examples/concepts-uploads/more.go:166-191`](../../examples/concepts-uploads/more.go#L166-L191)

```go
func perRequest(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	limited := map[string]bool{planSpammer: true}
	resolve := func(r *http.Request, p upload.Policy) (upload.Policy, error) {
		if limited[r.Header.Get(planHeader)] {
			slow := filesystem.NewError(filesystem.CodeUnavailable, errors.New("too many uploads"))
			return p, slow.WithRetryAfter(retryAfter)
		}
		p.MaxSize = freeLimit
		if r.Header.Get(planHeader) == planPaid {
			p.MaxSize = paidLimit
		}
		return p, nil
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{Resolve: resolve})
	body := pngFile + strings.Repeat("x", freeLimit)
	for _, plan := range []string{planFree, planPaid, planSpammer} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, formRequest(ctx, plan, formFile{"photo.png", body}))
		fmt.Fprintf(out, "plan %s: %d retry-after=%q\n", plan, rec.Code, rec.Header().Get("Retry-After"))
	}
	return nil
}
```
<!-- /ds:block hash=d23ec6 -->

Check: `plan free: 413`, `plan paid: 201`, and `plan spammer: 503 retry-after="60"`. The rate limiter itself is yours (`golang.org/x/time/rate` per user, or a Redis counter shared by every instance); the resolver is the place it runs, before a byte of the body is read, so a refused client costs you one request header. Build the key layout here too (`p.Key = upload.RandomKey("users/" + id)`), since a `KeyFunc` never sees the request.

The Uppy handler resolves every request of an upload separately (create, then complete, whose `Confirm` checks the assembled file), so the resolver must give one upload the same limits each time.

### Upload sessions: a token valid for a window

For the full step-by-step guide, with sessions kept in the database so they survive restarts and work across instances, see [Upload sessions](../plugins/upload-sessions.md) and its example `examples/records-upload-sessions`.

Sometimes the client that uploads is not the one your API authenticates: a phone app handed a one-off upload slot, a third party sending a document, a background uploader that outlives a login. An upload session is what Supabase's signed uploads and Uploadcare's signed uploads are: your server issues a token for one key or one key prefix, with a size cap and types no wider than the endpoint's policy, valid until it expires; the upload carries only the token, in the `Upload-Session` header (`upload.HeaderUploadSession`).

```go
sessions, _ := upload.NewSessions(&upload.MemorySessions{}, upload.SessionsOptions{})
h := upload.Handler(disk, policy, upload.HandlerOptions{Resolve: sessions.Resolver()})

// In an authenticated API call:
is, err := sessions.Issue(ctx, policy, upload.SessionRequest{
	Prefix: "users/" + user.ID, MaxSize: 10 << 20, Allowed: []string{"image/png", "image/jpeg"}, TTL: 15 * time.Minute,
})
// Hand is.Token to the client; it sends "Upload-Session: <token>" with each upload.
```

- `Issue` refuses a request that would widen the policy (a bigger `MaxSize`, a type outside `Allowed`), needs exactly one of `Key` (every upload stored at that key) and `Prefix` (the policy's key chosen inside it), and a TTL up to `upload.MaxSessionLifetime` (7 days).
- The token is the session ID and a 256-bit secret; the store keeps only the secret's SHA-256. Every redemption checks the secret in constant time and the expiry by the `Sessions` clock, never the store's word, and judges the session's types against the endpoint's policy as it is now, so narrowing the policy narrows sessions already issued. A missing, malformed, unknown, revoked or expired token is one answer: 403, code `permission`.
- The narrowed policy then goes through the same checks as any upload: the size is counted while streaming and the type detected from the bytes.
- `Resolver()` plugs into every handler's `Resolve`: `Handler`, `Chunked`, `Uppy`, `PresignHandlerWith` and `LimitsHandler` (which then shows the client the session's own limits). Each request of a chunked or multipart upload carries the token, so a session that expires mid-upload stops it at the next request.
- `Revoke(ctx, id)` ends a session at once; `Sweep(ctx)` deletes the expired ones (schedule it; an expired session is refused whether or not it was swept).
- `upload.MemorySessions` keeps sessions in this process. Behind a load balancer, or to survive restarts, use the records-backed store, [`records/uploadsessions`](../../records/uploadsessions/README.md), or implement `upload.SessionStore` (four methods) on your own database.
- tus: `contrib/tus` takes the same `Resolver` as `Config.Resolve`. It is resolved at creation (against the declared size and type) and again at the final `PATCH` (the policy the bytes are stored under), so the session's prefix and limits apply and a session that expires mid-upload stops it there.

<!-- ds:block id=form-bav4dakx -->
**form** · [`examples/concepts-uploads/form.go:67-93`](../../examples/concepts-uploads/form.go#L67-L93)

```go
func form(ctx context.Context, out io.Writer) error {
	files := []formFile{{"a.png", pngFile}, {"page.html", htmlFile}, {"b.pdf", pdfFile}}
	for _, partial := range []bool{false, true} {
		disk, err := newDisk()
		if err != nil {
			return err
		}
		recorded := 0
		h := upload.Handler(disk, policy, upload.HandlerOptions{
			MaxFiles: maxFilesPerRequest,
			Partial:  partial,
			OnStored: func(_ context.Context, _ *http.Request, stored []upload.Result) error {
				recorded = len(stored) // a real app inserts rows here
				return nil
			},
		})
		status, body, err := postForm(ctx, h, files...)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "form partial=%t: %d, stored %d, recorded %d\n", partial, status, count(ctx, disk), recorded)
		if !partial {
			fmt.Fprintf(out, "  body %s\n", body)
		}
	}
	return nil
}
```
<!-- /ds:block hash=925491 -->

By default a request stores all its files or none, which is right for a form whose files belong together: one refused file fails the request (with that file's status) and the files already stored are deleted. With `Partial`, a bulk upload stores what it can, answers 200 instead of 201 when some failed, and lists each failure with its own code. Request-level failures (not multipart, over the request cap, malformed) still fail the whole request after deleting what it stored.

```text
form partial=false: 415, stored 0, recorded 0
  body {"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
form partial=true: 200, stored 2, recorded 2
```

<!-- ds:block id=formfailures-8rzt54v2 -->
**formFailures** · [`examples/concepts-uploads/form.go:98-110`](../../examples/concepts-uploads/form.go#L98-L110)

```go
func formFailures(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Handler(disk, policy, upload.HandlerOptions{MaxFiles: maxFilesPerRequest, Partial: true})
	_, body, err := postForm(ctx, h, formFile{"page.html", htmlFile}, formFile{"b.pdf", pdfFile})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  partial body: %s\n", body)
	return nil
}
```
<!-- /ds:block hash=7e3ba7 -->

```json
{"files":[{"key":"uploads/ayhaezujn3eamoosxfpzu5ro4o.pdf","filename":"b.pdf","content_type":"application/pdf","size":24,"sha256":"9cc0121057382813f19c17ac79edbe548c4d06b0f374754e1565c2f3185d8f13"}],"failed":[{"filename":"page.html","error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}]}
```

Responses: 201 with `Response{Files}` when everything was stored; 200 with `Files` and `Failed` for a partial success; the failing file's status when nothing was stored; otherwise `ErrorResponse{Error, Code}` with the status of the code (413, 415, 400, 507, …). Error messages are sent only for codes about the client's own input; anything else (a backend failure, an `OnStored` error) is reported by status text alone.

The handler does no authentication: mount it behind your own middleware. Uploads through it are not retried by `layer.Retry`, because a request body cannot be replayed.

## Step 4 — Direct uploads: Presign, then Confirm

Large files should not pass through your application. `upload.Presign(ctx, disk, TicketRequest, policy, expires)` checks what the client declares (file name, content type, size) against the policy and returns a `Ticket`: a key, a method, a signed URL, the headers to send and an expiry. The client uploads straight to the bucket, then tells your server the key, and `upload.Confirm(ctx, disk, key, filename, policy)` checks what was actually stored: the size, the type detected from the first bytes, the name, that the stored `Content-Type` names that same type, then `Inspect`. An object that fails is deleted and the error returned, so nothing the policy refuses stays on the disk.

<!-- ds:block id=direct-gtjfeh86 -->
**direct** · [`examples/concepts-uploads/direct.go:83-106`](../../examples/concepts-uploads/direct.go#L83-L106)

```go
func direct(ctx context.Context, out io.Writer) error {
	b, err := newBucket()
	if err != nil {
		return err
	}
	defer b.srv.Close()
	for _, body := range []string{pngFile, htmlFile} {
		req := upload.TicketRequest{Filename: "photo.png", ContentType: "image/png", Size: int64(len(body))}
		t, err := upload.Presign(ctx, b.disk, req, policy, ticketLifetime)
		if err != nil {
			return err
		}
		status, err := put(ctx, t, body)
		if err != nil {
			return err
		}
		res, err := upload.Confirm(ctx, b.disk, t.Key, req.Filename, policy)
		left, _ := b.disk.Exists(ctx, t.Key)
		fmt.Fprintf(out, "direct: PUT %d, confirm %q type %q, object kept: %t\n", status, filesystem.CodeOf(err), res.ContentType, left)
	}
	_, err = upload.Presign(ctx, b.disk, upload.TicketRequest{Filename: "page.html", ContentType: "text/html", Size: 1}, policy, ticketLifetime)
	fmt.Fprintf(out, "presign declared text/html: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=a7b318 -->

Check: `direct: PUT 200, confirm "" type "image/png", object kept: true` for the honest client, then `direct: PUT 200, confirm "rejected" type "", object kept: false` for the client that declared a PNG and sent HTML, and `presign declared text/html: rejected`.

The two steps are not optional. Between Presign and Confirm the bucket holds whatever the client sent; only Confirm sees the bytes. The client must send the `Content-Type` the ticket's `Headers` carry. On S3, GCS and `fshttp.Signed` the signed URL pins it, so the backend refuses a request with another `Content-Type` header; an Azure SAS cannot pin it, so there the backend stores whatever the client sent. Either way the bytes themselves are not checked until Confirm, and Confirm closes the gap on every backend: when the declared type stored with the object would make a browser render the bytes as something they are not, the object is deleted and refused with `rejected` (`the upload was declared as "text/html" but the content is "image/png"`). That is an active type (HTML, SVG, XML, JavaScript) the content is not, or a text or markup type (`text/*`, any `+xml` or `/xml`, scripts) on content that is not text. Without that check, PNG bytes declared as `text/html` would be served as HTML. Every other difference passes, because it is an alias or a generic label rather than a danger: `audio/wav` for `audio/wave`, `application/x-zip-compressed` for `application/zip`, `application/octet-stream` for anything. Parameters such as `charset`, and case, are ignored. A disk without the `ContentType` capability is not compared, because the type its `Stat` reports is derived (local disk guesses it from the extension), not declared. Your application records the file only after Confirm succeeds.

### Declared size and checksum

`Confirm` checks what was stored against the policy; it cannot know what the client meant to send. A connection dropped near the end leaves a truncated object that may still detect as the right type, and a buggy client can upload the wrong file under the right ticket. Let the client declare both when it asks for the ticket: `TicketRequest.Size` is already required, and `TicketRequest.SHA256` is the hex SHA-256 it computed (browsers have `crypto.subtle.digest`). `Presign` checks only the digest's form; keep both with the ticket, and confirm with `upload.ConfirmExpected(ctx, disk, key, filename, policy, upload.Expected{Size, SHA256})`, which refuses, and deletes, an object whose stored size or SHA-256 differs:

<!-- ds:block id=declared-n3nu9wxw -->
**declared** · [`examples/concepts-uploads/more.go:198-224`](../../examples/concepts-uploads/more.go#L198-L224)

```go
func declared(ctx context.Context, out io.Writer) error {
	b, err := newBucket()
	if err != nil {
		return err
	}
	defer b.srv.Close()
	sum := sha256.Sum256([]byte(pngFile))
	req := upload.TicketRequest{Filename: "photo.png", ContentType: "image/png", Size: int64(len(pngFile)), SHA256: hex.EncodeToString(sum[:])}
	want := upload.Expected{Size: req.Size, SHA256: req.SHA256}
	for _, sent := range []string{pngFile, pngFile[:len(pngFile)-1] + "!"} {
		t, err := upload.Presign(ctx, b.disk, req, policy, ticketLifetime)
		if err != nil {
			return err
		}
		if _, err := put(ctx, t, sent); err != nil {
			return err
		}
		_, err = upload.ConfirmExpected(ctx, b.disk, t.Key, req.Filename, policy, want)
		kept, _ := b.disk.Exists(ctx, t.Key)
		fmt.Fprintf(out, "declared sha256, sent %d bytes: confirm %q, kept %t\n", len(sent), filesystem.CodeOf(err), kept)
	}
	_, err = upload.Presign(ctx, b.disk, req, policy, slowMobileLink)
	fmt.Fprintf(out, "ticket for %s: %v\n", slowMobileLink, err)
	_, err = upload.Presign(ctx, b.disk, req, policy, filesystem.MaxPresignExpiry+time.Hour)
	fmt.Fprintf(out, "ticket past MaxPresignExpiry: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=4aeaf2 -->

Check: `declared sha256, sent 29 bytes: confirm "", kept true` for the honest upload, then `confirm "rejected", kept false` for the same length with one byte changed, which only the digest catches. The presign handler only issues tickets, so keeping the declaration until the client confirms is your application's job: store it with the pending upload's row, never take it from the confirm request, which a client could change. Where the backend can verify the checksum itself as the bytes arrive (S3), see [Integrity](integrity.md).

**Ticket lifetimes.** `Presign`'s `expires` is per call: a phone on a slow network may need a ticket that lives for hours, a browser on broadband a few minutes. The bound is `filesystem.MaxPresignExpiry`, seven days (the S3 SigV4 limit); anything longer is refused with `invalid_argument` rather than shortened, as the program's last two lines show (`ticket for 24h0m0s: <nil>`, `ticket past MaxPresignExpiry: invalid_argument`). Give the long lifetime only to the endpoint that needs it, and remember that the ticket is a credential until it expires: anyone holding it can upload to its key.

### More digests than SHA-256

Every upload path reports the SHA-256 it computed. `Policy.Checksums` asks for more, in the same `filesystem.Checksum` shape `Disk.Write` takes: MD5 for an existing checksum column, CRC32C to compare with what GCS reports, BLAKE3 for content addressing. You bring the `hash.Hash`; the library names no algorithm. Each digest comes back in `Result.Checksums` by name, in lowercase hex, and in the JSON response as `"checksums"`; without the field nothing more is computed and the response is unchanged.

```go
policy := upload.Policy{
	MaxSize: 50 << 20,
	Allowed: []string{"application/pdf", "image/*"},
	Checksums: []filesystem.Checksum{
		{Name: "md5", New: md5.New},
		{Name: "crc32c", New: func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }},
	},
}
res, err := upload.Receive(ctx, disk, body, filename, policy)
// res.Checksums["md5"], res.Checksums["crc32c"]
```

`Receive`, and so the form handler, `FromURL`, tus and the chunked handler, has `Disk.Write` compute them while it streams. `Confirm` and `ConfirmExpected`, and so presigned tickets and Uppy's multipart uploads, read the stored object once to compute them, since those bytes never passed through the server; the read is bounded by the size the policy already checked. A multipart upload cannot carry them at `Disk.CreateMultipart` (`unsupported`): parts arrive separately and may go straight to the bucket, so nothing could verify a caller-chosen hash across them, which is why the assembled object is hashed at confirm time instead. A `Checksum.Want`, which a per-request `Resolver` can set from what the client declared (a `Content-MD5`, say), is verified on both paths: `Receive` fails with `invalid_argument` before anything is committed, and `Confirm` deletes the object and answers `rejected`. A name that is not 1 to 32 lowercase letters, digits or dashes, a name listed twice or a missing `New` makes `Validate` answer `invalid_argument`. The digests describe the bytes as stored, after `Process`. Records keeps them on the file row; see [Integrity](integrity.md).

### Public direct uploads

A ticket signs no visibility by default: the object gets whatever the backend gives an upload that names none, which on S3 and GCS is private. An avatar or a public attachment that should be readable by URL needs the visibility signed into the upload itself, so the client cannot change it afterwards and nothing has to call `SetVisibility` later. The client asks with `TicketRequest.Visibility` (`"visibility":"public"` in the JSON), and the server allows it per policy with `Policy.AllowPublic`:

```go
avatars := upload.Policy{
	MaxSize:     2 << 20,
	Allowed:     []string{"image/png", "image/jpeg", "image/webp"},
	Key:         upload.RandomKey("avatars"),
	AllowPublic: true, // a ticket for this policy may ask for a public object
}
ticket, err := upload.Presign(ctx, disk, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 48213, Visibility: filesystem.VisibilityPublic}, avatars, time.Minute)
```

Whoever holds a ticket creates the object, so a ticket that may be public lets any client publish a file under your bucket; that is why it is a separate switch the server sets, never something the client can turn on. Without `AllowPublic`, asking for public is refused with `permission` (403) before anything is signed; asking for private, or for nothing, needs no allow.

The disk must be able to pin the visibility, which is the `PresignVisibility` capability: S3 and GCS in ACL mode (`acl=true`), where the ticket's PUT signs `x-amz-acl` or `x-goog-acl` (the client must send the header from `Ticket.Headers`) and a form pins an `acl` field. A bucket with ACLs off (S3's bucket owner enforced, GCS's uniform access) refuses with `unsupported`, as does a disk signing through a `URLSigner`; on a disk without per-object visibility the disk's default visibility is accepted and signs nothing, as for a `Write`. `Policy.Visibility` keeps applying to `Receive` and the Uppy handler only, so a policy shared with those never changes what existing tickets do. The same field exists one level down for direct callers: `filesystem.PresignOptions.Visibility` and `filesystem.PostPolicy.Visibility`.

When the server, not the client, decides (every avatar is public, every gallery upload too), set `Policy.SignVisibility`: `Presign` then signs `Policy.Visibility` into every ticket, so the client's JSON need not carry `"visibility"` and cannot change it. A request asking for another visibility than the policy's is refused with `permission` before anything is signed, rather than silently overridden. A public `Visibility` still needs `AllowPublic`, and `SignVisibility` without a `Visibility` is a misconfigured policy: `Validate` refuses both with `invalid_argument`. Off, the default, keeps the rule above.

```go
avatars := upload.Policy{
	MaxSize:        5 << 20,
	Allowed:        []string{"image/png", "image/jpeg", "image/webp"},
	Key:            upload.RandomKey("avatars"),
	Visibility:     filesystem.VisibilityPublic,
	AllowPublic:    true, // still required for a public ticket
	SignVisibility: true, // every ticket signs public; the client cannot ask otherwise
}
ticket, err := upload.Presign(ctx, disk, upload.TicketRequest{Filename: "me.png", ContentType: "image/png", Size: 48213}, avatars, time.Minute)
```

Try it: [`examples/concepts-uploads`](../../examples/concepts-uploads) runs this section in `public.go` ([`publicTicket`](ds:block?id=publicticket-n6w9t4hb)): `go run ./concepts-uploads` from `examples/`, then look for `public ticket without AllowPublic: permission`.

### Form uploads the bucket checks

A presigned PUT lets the client send any number of bytes; only `Confirm` finds out afterwards. A presigned POST form is the one way to make the bucket itself refuse a wrong upload: `disk.PresignPost(ctx, filesystem.PostPolicy{…})` returns a URL, form fields and an expiry, and the backend refuses any POST whose size is outside `[MinSize, MaxSize]`, whose key is not `Key` (or under `KeyPrefix`), whose `Content-Type` is not `ContentType`, or that arrives late. It needs the `PresignPost` capability (S3 and GCS); other disks answer `unsupported`. The client posts a multipart form with every field, in any order, then the file in the field named `file` (`filesystem.PostFileField`), last; field names are the same on every backend (`key`, `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`). With `KeyPrefix`, the key field is `<prefix>/${filename}` and the backend substitutes the uploaded file's name.

`upload.Presign` issues such a form when the policy sets `PostForm: true`: the ticket has `Method: "POST"` and `Fields` instead of `Headers`, and the backend accepts exactly the declared size and type under the policy's key, with the policy's stored headers. Run `Confirm` afterwards all the same: the bytes are still the client's.

The program [`examples/recipe-browser-form-uploads`](../../examples/recipe-browser-form-uploads/) is the whole flow: a policy with `PostForm`, a ticket endpoint, a page whose script posts the form to the bucket, and a confirm endpoint. The policy and the ticket:

<!-- ds:block id=avatarpolicy-4w4dsk4r -->
**avatarPolicy** · [`examples/recipe-browser-form-uploads/main.go:145-153`](../../examples/recipe-browser-form-uploads/main.go#L145-L153)

```go
func avatarPolicy() upload.Policy {
	return upload.Policy{
		MaxSize:  avatarMaxSize,
		MinSize:  1,
		Allowed:  []string{avatarType},
		Key:      upload.RandomKey("avatars"),
		PostForm: true,
	}
}
```
<!-- /ds:block hash=0717e3 -->

<!-- ds:block id=issueticket-8tsaxjxf -->
**issueTicket** · [`examples/recipe-browser-form-uploads/main.go:172-184`](../../examples/recipe-browser-form-uploads/main.go#L172-L184)

```go
func issueTicket(w http.ResponseWriter, r *http.Request, disk *filesystem.Disk) {
	var req upload.TicketRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad ticket request", http.StatusBadRequest)
		return
	}
	ticket, err := upload.Presign(r.Context(), disk, req, avatarPolicy(), ticketLifetime)
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	writeJSON(w, ticket)
}
```
<!-- /ds:block hash=66b9ba -->

and the confirmation, which holds the stored object to the size the browser declared:

<!-- ds:block id=confirmupload-nxz3g54m -->
**confirmUpload** · [`examples/recipe-browser-form-uploads/main.go:198-210`](../../examples/recipe-browser-form-uploads/main.go#L198-L210)

```go
func confirmUpload(w http.ResponseWriter, r *http.Request, disk *filesystem.Disk) {
	var req confirmRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad confirm request", http.StatusBadRequest)
		return
	}
	res, err := upload.ConfirmExpected(r.Context(), disk, req.Key, req.Filename, avatarPolicy(), upload.Expected{Size: req.Size})
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	writeJSON(w, res)
}
```
<!-- /ds:block hash=003e02 -->

Against RustFS the bucket refuses a file larger than declared (`oversize upload: refused by the bucket (400)`); against the in-process fake, which does not check policies, `Confirm` catches it and deletes it.

```json
{"key":"uploads/3qx….png","method":"POST","url":"https://media.s3.amazonaws.com/","fields":{"key":"uploads/3qx….png","Content-Type":"image/png","policy":"…","x-amz-signature":"…"},"expires_at":"…"}
```

What the client declared can be held to the bytes as well. `TicketRequest.SHA256` is an optional hex SHA-256 the client computed; `Presign` checks its form, your application keeps it with the ticket, and `upload.ConfirmExpected(ctx, disk, key, filename, policy, upload.Expected{Size, SHA256})` refuses (with `rejected`, deleting the object) a stored size other than the declared one, which catches a truncated or padded upload, and bytes whose SHA-256 differs, which catches corruption and a swapped file. `Confirm` is `ConfirmExpected` with nothing declared.

`upload.PresignHandler(disk, policy, expires)` answers POSTed `TicketRequest` JSON (`{"filename":…,"content_type":…,"size":…}`) with the `Ticket`. Mount it behind authentication. A disk needs `Presign` or a `URLSigner`; a mirrored disk refuses signed PUTs, because an upload straight to the primary would bypass its mirrors. Uploads that are presigned and never confirmed stay in the bucket: sweep their prefix (Step 9).

## Step 5 — Browser multipart uploads with Uppy

For very large files a browser uploads parts in parallel straight to the bucket. `upload.Uppy(disk, policy, UppyOptions{})` serves the endpoints Uppy's AWS S3 plugin calls, mounted with the prefix stripped:

| Route | Does |
|---|---|
| `POST /multipart` | checks the declared type and name, creates the upload, returns `{"uploadId","key"}` |
| `GET /multipart/{id}/{part}?key=` | signs one part's PUT |
| `GET /multipart/{id}?key=` | lists uploaded parts, for resuming |
| `POST /multipart/{id}/complete?key=` | assembles the parts, then runs `Confirm` on the result and deletes it if refused |
| `DELETE /multipart/{id}?key=` | aborts the upload |

`UppyOptions.Authorize(r, key)` runs before every route but create. The upload ID and key come from the client, so without it one user could complete or abort another's upload; nil allows everything and is safe only when the endpoints are unreachable by untrusted users. `Expires` is the lifetime of each part URL (default 15 minutes). The disk needs the `Multipart` and `PresignPart` capabilities, which only the S3 driver has; on any other disk every route answers 501:

<!-- ds:block id=uppy-gknjx429 -->
**uppy** · [`examples/concepts-uploads/direct.go:112-125`](../../examples/concepts-uploads/direct.go#L112-L125)

```go
func uppy(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	h := upload.Uppy(disk, policy, upload.UppyOptions{
		Authorize: func(*http.Request, string) error { return nil }, // check the key belongs to the user
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/multipart", bytes.NewReader([]byte(`{"filename":"a.png","type":"image/png"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Fprintf(out, "uppy on memory: %d %s", rec.Code, rec.Body.String())
	return nil
}
```
<!-- /ds:block hash=84dd36 -->

Check: `uppy on memory: 501 {"error":"Not Implemented","code":"unsupported"}`. A complete browser set-up against S3 is in [Browser direct uploads](../recipes/browser-direct-uploads.md).

`UppyOptions.OnStored(ctx, r, file)` runs once the completed file has passed `Confirm`, as `HandlerOptions.OnStored` does for form uploads: create its record there, and if that fails the file is deleted and the complete request fails with your error, so no stored file lacks its row. Uppy's own complete request carries only the parts; a custom `completeMultipartUpload` may add `"filename"` beside them, which is checked by the policy's name rules again and reported as `Result.Filename`.

Try it: [`examples/recipe-browser-direct-uploads`](../../examples/recipe-browser-direct-uploads) runs this section in `recorded.go` ([`uppyRecorded`](ds:block?id=app-uppyrecorded-d3k7v9qw)): `go run ./recipe-browser-direct-uploads` from `examples/`, then use the `/uploads/s3r/` routes.

## Step 6 — Resumable uploads (tus)

Mobile clients on unreliable links resume where they stopped with the tus protocol. `contrib/tus` embeds tusd and promotes each finished upload through `Receive`, so the policy, `Process` and `Inspect` apply as on a form post. See [the tus reference](../../contrib/tus/README.md) and [Resumable uploads](../recipes/resumable-uploads.md).

## Step 7 — Imports from a URL

`upload.FromURL(ctx, disk, rawURL, policy, FetchOptions{})` fetches a URL and stores it through `Receive`. The type is detected from the bytes, never from the server's `Content-Type`; the file name comes from `Content-Disposition`, else the URL's last segment, else `download`; an announced `Content-Length` over `MaxSize` is refused before any byte is read, and the body is capped while streaming.

The fetch refuses any connection to an address outside the public internet: loopback, private networks, link-local (cloud metadata at 169.254.169.254), CGNAT, multicast, reserved, NAT64, and IPv4-mapped forms of all of them. The check runs at dial time, after DNS resolution, for every connection including each redirect, so a hostname that resolves (or re-resolves) to an internal address is caught. Only `http` and `https` URLs without credentials are fetched, at most five redirects are followed (`DefaultMaxRedirects`), no proxy is used (a proxy would be dialled instead of the target), and the whole fetch is bounded by `DefaultFetchTimeout` (one minute). `FetchOptions.AllowAddr` lets a known internal server through on purpose.

`FetchOptions.Header` is sent with every request of the fetch: a `User-Agent` for sites that refuse Go's default, an `Authorization` for an internal source (Go drops it on a redirect to another host). Where egress must go through a proxy (Smokescreen, Squid, an egress gateway), `FetchOptions.Proxy` takes an `http://host:port` CONNECT proxy without giving up the guard: the fetch still resolves the name and checks the address, then asks the proxy to `CONNECT` to that address, never to the name, so the proxy cannot resolve it to an internal one:

```go
proxy, _ := url.Parse("http://smokescreen.internal:4750")
res, err := upload.FromURL(ctx, disk, link, policy, upload.FetchOptions{
	Header: http.Header{"User-Agent": {"acme-importer/1.0 (+https://acme.example/bot)"}},
	Proxy:  proxy,
})
```

<!-- ds:block id=fromurl-bxrwf6up -->
**fromURL** · [`examples/concepts-uploads/remote.go:23-46`](../../examples/concepts-uploads/remote.go#L23-L46)

```go
func fromURL(ctx context.Context, out io.Writer) error {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html") // ignored: the bytes decide
		w.Header().Set("Content-Disposition", `attachment; filename="scan.pdf"`)
		_, _ = io.WriteString(w, pdfFile)
	}))
	defer remote.Close()
	disk, err := newDisk()
	if err != nil {
		return err
	}
	_, err = upload.FromURL(ctx, disk, remote.URL+"/files/7", policy, upload.FetchOptions{})
	fmt.Fprintf(out, "from loopback: %v\n", err)
	loopback := func(a netip.Addr) bool { return a.IsLoopback() }
	res, err := upload.FromURL(ctx, disk, remote.URL+"/files/7", policy, upload.FetchOptions{AllowAddr: loopback})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "from allowed server: %q %s %d bytes\n", res.Filename, res.ContentType, res.Size)
	fmt.Fprintf(out, "PublicAddr(%s): %t\n", metadataService, upload.PublicAddr(metadataService))
	_, err = upload.FromURL(ctx, disk, "file:///etc/passwd", policy, upload.FetchOptions{})
	fmt.Fprintf(out, "file URL: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=b2c752 -->

Check: `from loopback: filesystem: invalid_argument: address is not on the public internet`, then `from allowed server: "scan.pdf" application/pdf 24 bytes` (the server claimed `text/html`), `PublicAddr(169.254.169.254): false` and `file URL: invalid_argument`.

Errors: `invalid_argument` for a refused URL, address, redirect or 4xx answer other than 404, 410 and 429; `not_found` for 404 and 410; `too_large` for an announced size over the limit; `unavailable` for network failures, timeouts, 429 and 5xx. `upload.Fetch` is the same fetch without storing, and `upload.PublicAddr` the address check, for webhooks and link previews.

Try it: [`examples/concepts-uploads`](../../examples/concepts-uploads) runs this section in `proxied.go` ([`throughProxy`](ds:block?id=throughproxy-n9q4v7hs)): `go run ./concepts-uploads` from `examples/`.

### Base64 bodies and data URIs

JSON APIs and mobile clients often send small files as base64 text, and a canvas export or a pasted image arrives as a data URI. `upload.FromBase64(ctx, disk, encoded, filename, policy)` and `upload.FromDataURI(ctx, disk, "data:image/png;base64,…", filename, policy)` decode while streaming and are `Receive` in every other respect: the type is detected from the decoded bytes (a data URI's declared type is ignored, like any client's), the policy's size cap counts decoded bytes, and nothing refused is stored. Text that is not standard padded base64 (line breaks are ignored) and data URIs that are not base64 are `invalid_argument`.

<!-- ds:block id=encoded-kb86vpvx -->
**encoded** · [`examples/concepts-uploads/more.go:92-111`](../../examples/concepts-uploads/more.go#L92-L111)

```go
func encoded(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(pngFile))
	res, err := upload.FromBase64(ctx, disk, b64, "avatar.png", policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "base64: %s, %d bytes\n", res.ContentType, res.Size)
	res, err = upload.FromDataURI(ctx, disk, "data:text/plain;base64,"+b64, "paste.png", policy)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "data URI declared text/plain: stored as %s\n", res.ContentType)
	_, err = upload.FromBase64(ctx, disk, "not base64!", "x.png", policy)
	fmt.Fprintf(out, "bad base64: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=ccf2c3 -->

Check: `base64: image/png, 29 bytes`, `data URI declared text/plain: stored as image/png` (the declaration is ignored, the bytes decide), and `bad base64: invalid_argument`. Base64 is a third larger than the file and the whole text sits in your request body, so keep it to small files (avatars, signatures, screenshots) and cap the JSON body with `http.MaxBytesReader` at about four thirds of `MaxSize`; send anything larger as multipart or direct to the bucket.

### Accept fast, store later

When the durable store is slow or far away (S3 in another region, a provider with high write latency), the user waits for it on every upload. Accept fast, store later splits the two: the handler stores the upload on a fast disk next to the application (a local disk) under a staging directory and answers at once, and a background job moves each file to the durable store under the same key without the staging prefix. `jobs.Promote(staging, dir, store, minAge)` is that job: it moves only objects at least `minAge` old (so an upload still being written is left alone), copies before it deletes (so a crash leaves the file on both disks and the next run finishes it), and reports one error per object it could not move without stopping the others. Until a file is moved, readers must look on the staging disk: stat the store first and fall back to staging. The schedule, `RunNow` right after an upload, and a complete program are in [Maintenance jobs](maintenance-jobs.md#accept-fast-store-later).

Use it when upload latency matters more than having every file in the durable store the moment the request returns. Do not use it when the application runs on several machines without a shared staging disk: a file accepted on one machine is invisible on the others until promoted.

### Cache, then promote

Some forms need the file before the record is valid: the user uploads an avatar, the form fails validation on another field, and the page is shown again with the avatar still attached. Store the file in a cache area as soon as it arrives (`Receive` with `upload.RandomKey("cache")`), send its key back to the page, and when the record is finally saved move it to its permanent key with `upload.Promote(ctx, cacheDisk, "cache", key, storeDisk, "avatars/u1.png")`, on the same disk (a move) or another one (a copy, then the cached object is deleted). The key comes back from the client, so `Promote` refuses, with `permission` and without moving anything, any key that is not under the cache area: a client cannot promote, and so take over, another user's file. The cache area cannot be the root of the disk. Run `upload.Sweep` on the cache area for the uploads nobody promoted.

<!-- ds:block id=cachethenpromote-ywrmuhf4 -->
**cacheThenPromote** · [`examples/concepts-uploads/more.go:230-250`](../../examples/concepts-uploads/more.go#L230-L250)

```go
func cacheThenPromote(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	cached := policy
	cached.Key = upload.RandomKey(cacheDir)
	res, err := upload.Receive(ctx, disk, strings.NewReader(pngFile), "avatar.png", cached)
	if err != nil {
		return err
	}
	e, err := upload.Promote(ctx, disk, cacheDir, res.Key, disk, "avatars/u1.png")
	if err != nil {
		return err
	}
	stillCached, _ := disk.Exists(ctx, res.Key)
	fmt.Fprintf(out, "promoted to %s, still in cache: %t\n", e.Key, stillCached)
	_, err = upload.Promote(ctx, disk, cacheDir, "avatars/u1.png", disk, "avatars/u2.png")
	fmt.Fprintf(out, "promote a key outside the cache: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=30674c -->

Check: `promoted to avatars/u1.png, still in cache: false`, then `promote a key outside the cache: permission`.

## Step 8 — Process and Inspect

`Policy.Process` is a `Processor`: `func(ctx, Info, io.Reader) (io.Reader, Info, error)`. It transforms each accepted file before it is stored (strip a photo's metadata, convert a format, compress) and returns the new body and what it now is. The returned type is checked against the policy again, `MaxSize` applies to what it returns, and an error refuses the upload. `Receive` runs it, so it covers form uploads, imports, tus and records; presigned and Uppy uploads go straight to the bucket and cannot be processed. `imaging.Sanitize` is the built-in processor that re-encodes photos; see [Security](security.md).

`Policy.Inspect` is an `Inspector`: `func(ctx, *filesystem.Disk, Result) error`. It examines each file once stored, reading it from the disk, before the upload reports success: a virus scan (`scan.Inspect`), a full image decode, a moderation call. An error deletes the object and becomes the upload's error, so a file that fails inspection is never reported as stored. `Receive` and `Confirm` run it, which covers every path above. Return `CodeRejected` for a refused file and `CodeUnavailable` when the inspection itself could not run. `upload.Inspectors(a, b, c)` runs several in order and stops at the first error.

<!-- ds:block id=trimtext-5f5xcut2 -->
**trimText** · [`examples/concepts-uploads/hooks.go:27-36`](../../examples/concepts-uploads/hooks.go#L27-L36)

```go
func trimText(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
	if !strings.HasPrefix(in.ContentType, "text/plain") {
		return body, in, nil
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return nil, upload.Info{}, err
	}
	return bytes.NewReader(bytes.TrimSpace(b)), in, nil
}
```
<!-- /ds:block hash=95c293 -->

<!-- ds:block id=refusemarked-3nej68ju -->
**refuseMarked** · [`examples/concepts-uploads/hooks.go:41-50`](../../examples/concepts-uploads/hooks.go#L41-L50)

```go
func refuseMarked(ctx context.Context, disk *filesystem.Disk, r upload.Result) error {
	b, err := disk.ReadAll(ctx, r.Key)
	if err != nil {
		return err
	}
	if bytes.Contains(b, []byte(blockedMarker)) {
		return filesystem.Errorf(filesystem.CodeRejected, "inspection refused %s", r.Filename)
	}
	return nil
}
```
<!-- /ds:block hash=0d9074 -->

<!-- ds:block id=processandinspect-2ryw5smr -->
**processAndInspect** · [`examples/concepts-uploads/hooks.go:55-69`](../../examples/concepts-uploads/hooks.go#L55-L69)

```go
func processAndInspect(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	p := upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Process: trimText, Inspect: upload.Inspectors(refuseMarked)}
	res, err := upload.Receive(ctx, disk, strings.NewReader("  notes  \n"), "notes.txt", p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "processed: %d bytes\n", res.Size)
	_, err = upload.Receive(ctx, disk, strings.NewReader("this is blocked"), "bad.txt", p)
	fmt.Fprintf(out, "inspected: %s, objects stored: %d\n", filesystem.CodeOf(err), count(ctx, disk))
	return nil
}
```
<!-- /ds:block hash=27418c -->

Check: `processed: 5 bytes` (the surrounding spaces were trimmed before storing) and `inspected: rejected, objects stored: 1`.

## Step 9 — Sweep what nobody finished

Presigned uploads that were never confirmed, and server uploads that were never attached to anything, cost storage forever. `upload.Sweep(ctx, disk, dir, cutoff)` deletes every object under `dir` last modified before `cutoff` and returns how many it deleted. It deletes as it lists, so a failure part-way returns the count so far and a rerun continues. The root of a disk is refused. The library starts no background goroutine: schedule it yourself (a cron job, a ticker in a worker).

<!-- ds:block id=sweep-52zp4ugj -->
**sweep** · [`examples/concepts-uploads/hooks.go:75-97`](../../examples/concepts-uploads/hooks.go#L75-L97)

```go
func sweep(ctx context.Context, out io.Writer) error {
	now := time.Now()
	clock := now.Add(-2 * abandonedAfter)
	disk, err := filesystem.NewDisk("uploads", memory.New(memory.WithClock(func() time.Time { return clock })))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "incoming/old.png", []byte(pngFile), filesystem.WriteOptions{}); err != nil {
		return err
	}
	clock = now
	if _, err := disk.WriteBytes(ctx, "incoming/new.png", []byte(pngFile), filesystem.WriteOptions{}); err != nil {
		return err
	}
	n, err := upload.Sweep(ctx, disk, "incoming", now.Add(-abandonedAfter))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept %d, left %d\n", n, count(ctx, disk))
	_, err = upload.Sweep(ctx, disk, "", now)
	fmt.Fprintf(out, "sweep the root: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=3e3c8e -->

Check: `swept 1, left 1` and `sweep the root: invalid_argument`.

Sweep deletes by age only. Point it at a prefix that holds nothing but unconfirmed uploads, never at a prefix where recorded files live, or it deletes them too. Finding objects that no database row points at is shown in [Consistency](consistency.md). A bucket lifecycle rule on the same prefix does the same job without code.

### Leftovers that are not objects

`upload.Sweep` sees only objects. Unfinished work also leaves state that `List` never shows: multipart uploads that were neither completed nor aborted (a browser closed half-way through an Uppy upload), and on local disk and SFTP the staging files of writes a crash interrupted. `disk.SweepAbandoned(ctx, before)` removes those whose last activity is before `before` and returns how many it removed; stored objects are never touched. Schedule it next to `upload.Sweep`:

```go
n, err := disk.SweepAbandoned(ctx, time.Now().Add(-24*time.Hour))
```

Choose `before` well beyond the longest upload you allow: a recent cutoff aborts uploads still in progress. It needs the `SweepAbandoned` capability (local, memory, Azure and SFTP have it); any other disk answers `unsupported`, so check `disk.Capabilities().SweepAbandoned` in a job that sweeps every disk. A scoped or mirrored disk never has it, because the leftovers belong to the whole backend: sweep the disk it was made from. On S3, a bucket lifecycle rule (`AbortIncompleteMultipartUpload`) does the same job without code.

To see what a sweep would remove before running it, `disk.FindAbandoned(ctx, before)` is its dry run: it returns each leftover (`Kind` multipart or staging, the upload ID or staging file name, the key an upload was started for, its `LastActivity`), oldest first, and removes nothing. Each driver answers it from the same scan its sweep uses, so the preview and the sweep apply one rule; work that starts, finishes or is aborted between the two calls still makes them differ. It needs the `FindAbandoned` capability (local, memory, S3 with `sweep_uploads=true`, Azure, SFTP, FTP and WebDAV), never forwarded by a scoped or mirrored disk; a read-only disk can run it, since it changes nothing.

<!-- ds:block id=abandoned-hkvv4ks7 -->
**abandoned** · [`examples/concepts-uploads/more.go:255-277`](../../examples/concepts-uploads/more.go#L255-L277)

```go
func abandoned(ctx context.Context, out io.Writer) error {
	disk, err := newDisk()
	if err != nil {
		return err
	}
	if _, err := disk.CreateMultipart(ctx, "videos/big.mp4", filesystem.WriteOptions{}); err != nil {
		return err
	}
	cutoff := time.Now().Add(time.Second) // in production: a day or more ago
	found, err := disk.FindAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, a := range found {
		fmt.Fprintf(out, "would remove: %s upload of %s\n", a.Kind, a.Key)
	}
	n, err := disk.SweepAbandoned(ctx, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "swept %d abandoned upload(s)\n", n)
	return nil
}
```
<!-- /ds:block hash=f34cc8 -->

Check: `would remove: multipart upload of videos/big.mp4`, then `swept 1 abandoned upload(s)`. The program sets the cutoff a second in the future so the upload it just started counts as abandoned; a real job passes a time a day or more in the past.

## Step 10 — Client protocols: chunks, retries, folders, progress

What the browser and phone libraries send, and what the endpoints answer, has its own page: [Upload clients](upload-clients.md). In short:

- **The JSON contract.** Every endpoint answers `{"files": [...]}` on success and `{"error", "code"}` on failure, with the status following the code ([step 1 there](upload-clients.md#step-1--the-json-contract)).
- **Chunked uploads.** `upload.Chunked(disk, policy, ChunkedOptions{Staging, Owner})` accepts Dropzone, FilePond and resumable.js chunks in any order and with retries, and stores the assembled file through `Receive` ([step 2](upload-clients.md#step-2--chunked-uploads)).
- **Progress.** `Policy.Progress` is told how many bytes have reached the disk, about every 64 KiB ([step 3](upload-clients.md#step-3--progress-on-the-server)).
- **Folders.** `upload.SafePath` keeps a dropped folder's relative paths, made safe, and `upload.FolderKey(dir)` stores each file at its path ([step 4](upload-clients.md#step-4--folder-uploads)).
- **Retries without duplicates.** `HandlerOptions.Idempotency` replays the recorded response to a retry that carries the same `Idempotency-Key`, so an offline queue never stores a file twice ([step 5](upload-clients.md#step-5--retries-without-duplicates)).
- **Limits for clients.** `upload.LimitsHandler` tells a client the limits before it sends anything ([step 6](upload-clients.md#step-6--tell-the-client-the-limits)).

**Content-addressed keys.** `p.Key, p.Settle = upload.ContentAddressed("blobs", upload.ContentAddressing{Owner: tenantFromContext})` names each file by its content: identical uploads share one object and a key proves its content. By default sharing stays within one owner, `blobs/<owner digest>/<sha256>.<ext>`, the owner coming from `Owner(ctx)`, which reads what your authentication put in the request's context; without an `Owner` every upload is refused. `ContentAddressing{Shared: true}` shares across owners, `blobs/<sha256>.<ext>`, as a deliberate choice: a duplicate stored with other settings answers `conflict`, which tells one owner that someone else holds identical content, and deleting one owner's file then deletes the object everyone's identical file uses (count references first, as records' `FileBySHA256` allows). Either way an upload is only deduplicated against an object stored with the same visibility and headers, so a public upload can never make a private file public (a duplicate with other settings is `conflict`). The file is written under `blobs/.incoming/` while the Disk hashes it, then moved to its content key; a duplicate's incoming copy is deleted and the existing object reused. `Policy.Settle` is the hook it uses: it runs after `Inspect` and may move the upload. See [Integrity](integrity.md) for deduplication end to end.

## Pitfalls

- Trusting `TicketRequest.ContentType` or `Size`: they are what the client says. Record a direct upload only after `Confirm`.
- Using the client's file name in the key: use `RandomKey` or your own random or content-addressed key, and keep the name for display.
- Allowing `*/*` without thinking about what you serve: active content is still refused, but archives, office documents and executables in disguise are not. Allow the types the feature needs.
- `AllowActiveContent` on an endpoint whose files are served from your application's origin: it re-opens stored XSS. Serve such files from another domain.
- Forgetting `UppyOptions.Authorize`: any user who learns an upload ID and key can complete or abort it.
- Expecting `Process` on direct uploads: the bytes never pass through your server. Process them afterwards, or upload through the server.

## Next

- [Security](security.md): every default with its reason.
- [Files as records](../records/README.md): `Store.Upload` records each file as a row and attaches it to your models.
- Recipes: [Avatars](../recipes/avatars.md), [Browser direct uploads](../recipes/browser-direct-uploads.md), [Mobile uploads](../recipes/mobile-uploads.md), [Import from a URL](../recipes/import-from-url.md), [Virus scanning](../recipes/virus-scanning.md).
- Reference: [the upload package](../../upload/README.md).
