# Mobile uploads

A phone app uploads differently from a browser form: many photos at once from the camera roll, over networks that drop, from a client that retries on its own when it did not hear back, with files that carry the GPS position of the user's home. This recipe is for the backend of an iOS, Android, React Native or Flutter app. It first helps you pick the upload path, then builds the most common one, "back up these 20 photos", with per-photo failures, idempotent retries and metadata stripping.

The program is [examples/recipe-mobile-uploads](../../examples/recipe-mobile-uploads). Every code block below is copied from it, and every transcript is from running it.

## Pick the path

| Situation | Path | Recipe |
|---|---|---|
| Photos and documents up to a few tens of MB, the common case | A multipart `POST` to your server, one or many files per request | this page |
| Large files (video), and your storage is S3-compatible | A presigned `PUT` straight to the bucket, then a confirm call | [browser direct uploads](browser-direct-uploads.md) |
| Large files over unreliable networks, or uploads that continue after the app is suspended | tus: chunked, resumable, through your server | [resumable uploads](resumable-uploads.md) |

All three enforce the same `upload.Policy` on the server. Two differences matter for phones. A presigned upload goes straight to the bucket, so `Policy.Process` cannot run and photos keep their EXIF unless you process them after confirming. A multipart `POST` restarts from zero when the connection drops, so keep each request small (one batch, not the whole roll) and let tus handle anything that takes minutes.

## What you get

- `POST /photos` takes up to 20 photos in one multipart request, each streamed to storage as it arrives, never buffered whole.
- Each photo is checked on its own (type from its bytes, size) and re-encoded with `imaging.Sanitize`, so the GPS position, camera serial numbers and anything appended after the image never reach storage.
- A bad photo (HEIC, a corrupt file, something that is not an image) is listed with its error code; the others are stored.
- A batch retried after a lost response returns the same file IDs, marked as duplicates, and stores nothing twice.
- `GET /photos/limits` tells the app what a photo may be (types, bytes, largest side), so it downscales before uploading; the server refuses a photo over 4096 px on its longest side whatever the client did.

## The design

| Piece | Why |
|---|---|
| `imaging.Sanitize` as `Policy.Process` | Strips metadata by re-encoding. Its output is deterministic, so the same photo always produces the same bytes and the same SHA-256. |
| `imaging.Options.MaxBytes` | Caps the photo as sent. `Policy.MaxSize` applies to what `Process` returns. |
| One `Store.Tx` per photo, around `Store.Upload` | One photo's failure cannot undo another's. `Store.Upload` registers a rollback hook that deletes the object, so rolling back a duplicate leaves no bytes. |
| `Store.Files` with a `FileSHA256` filter | Finds a live file of the user's with the same content, which is what makes a retry idempotent. |
| The user ID as the records scope | Every query of `fs_files` filters by scope, so one user's photos are never visible to, or deduplicated against, another's. |

Why not `upload.Handler` with `HandlerOptions{Partial: true}`? It does exactly the per-file part (store what it can, list the rest with codes), and is the right choice when the response may list storage keys. This app answers with file IDs and duplicate flags, which needs each photo recorded as it is stored, so the loop is written out with `Store.Upload`.

## Step 1: The policy

<!-- ds:block id=photopolicy-sccupwen -->
**photoPolicy** · [`examples/recipe-mobile-uploads/main.go:76-81`](../../examples/recipe-mobile-uploads/main.go#L76-L81)

```go
var photoPolicy = upload.Policy{
	MaxSize: maxPhotoBytes,
	Allowed: []string{"image/jpeg", "image/png", "image/webp"},
	Key:     upload.RandomKey("photos"),
	Process: imaging.Sanitize(imaging.Options{MaxBytes: maxPhotoBytes}),
}
```
<!-- /ds:block hash=890b74 -->

### HEIC

iPhones shoot HEIC by default. The library detects it from the bytes (`image/heic`, from the ISO `ftyp` brand), but no pure-Go decoder exists, so `imaging` cannot resize it and `Sanitize` passes it through untouched, with its metadata. Your choices:

- Refuse it, as this example does, and have the app export JPEG. On iOS, `PHPickerConfiguration.preferredAssetRepresentationMode = .compatible` makes the picker hand over JPEG; re-encoding with `UIImage.jpegData(compressionQuality:)` works too.
- Accept it untouched (add `image/heic` to `Allowed`) and accept that its EXIF, including GPS, is stored. Serve it only to its owner.
- Convert it on the server with a native tool (libheif, libvips) in your own `Processor`; the library ships none.

### Downscale on the phone

A 48-megapixel photo is 10 to 20 MB. If your product shows photos at screen size, resize on the device to the largest size you display (2048 or 4096 px on the long side) before upload: the upload is ten times faster, uses a tenth of the user's data plan, and the server-side re-encode is cheaper. The contract has three parts, and all three come from one place in the server so they cannot drift apart:

1. **The server enforces a largest side.** `imaging.CheckDimensions` is an `upload.Inspector`: it reads only the image header of the stored upload and refuses a photo over the bound with `CodeRejected`, deleting the object. An inspector runs on the server paths and on the presigned confirm alike, so a client that skips the downscale is refused, not trusted.
2. **The server publishes its limits.** `upload.LimitsHandler` answers `GET` with `upload.Limits` as JSON: the byte cap, the allowed and denied types, and the `MaxWidth`/`MaxHeight` you pass. Those two fields describe the check in step 1; they do not add one, which is why the example builds both from the same constant.
3. **The client downscales to fit.** It fetches the limits once per session, and before each upload scales any photo larger than `max_width` × `max_height` down to fit, keeping the aspect ratio.

<!-- ds:block id=photolimits-se3qfup2 -->
**photoLimits** · [`examples/recipe-mobile-uploads/main.go:99-107`](../../examples/recipe-mobile-uploads/main.go#L99-L107)

```go
func photoLimits() (upload.Policy, http.Handler, error) {
	dims, err := imaging.CheckDimensions(imaging.DimensionLimits{MaxWidth: maxPhotoSide, MaxHeight: maxPhotoSide})
	if err != nil {
		return upload.Policy{}, nil, err
	}
	p := photoPolicy
	p.Inspect = dims
	return p, upload.LimitsHandler(p, upload.LimitsOptions{MaxWidth: maxPhotoSide, MaxHeight: maxPhotoSide}), nil
}
```
<!-- /ds:block hash=097d8b -->

`New` mounts the handler beside the upload route (`mux.Handle("GET /photos/limits", limits)`) and every photo is stored with the completed policy. When limits depend on the user's plan, set `LimitsOptions.Resolve` to the same resolver the upload uses, and put the endpoint behind the same authentication.

The client side, in Go for a desktop or command-line uploader (the test uses it too). On iOS the resize is `UIGraphicsImageRenderer` plus `jpegData(compressionQuality:)`; on Android `Bitmap.createScaledBitmap` plus `compress(Bitmap.CompressFormat.JPEG, …)`; in Flutter the `image` package's `copyResize`.

<!-- ds:block id=fetchlimits-vzux344u -->
**fetchLimits** · [`examples/recipe-mobile-uploads/client.go:18-34`](../../examples/recipe-mobile-uploads/client.go#L18-L34)

```go
func fetchLimits(ctx context.Context, client *http.Client, base string) (upload.Limits, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/photos/limits", nil)
	if err != nil {
		return upload.Limits{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return upload.Limits{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return upload.Limits{}, fmt.Errorf("limits: %s", resp.Status)
	}
	var l upload.Limits
	err = json.NewDecoder(resp.Body).Decode(&l)
	return l, err
}
```
<!-- /ds:block hash=ce897c -->

<!-- ds:block id=downscale-jyrutuxr -->
**downscale** · [`examples/recipe-mobile-uploads/client.go:43-58`](../../examples/recipe-mobile-uploads/client.go#L43-L58)

```go
func downscale(photo []byte, l upload.Limits) ([]byte, error) {
	info, err := imaging.Probe(bytes.NewReader(photo))
	if err != nil {
		return nil, err
	}
	if (l.MaxWidth == 0 || info.Width <= l.MaxWidth) && (l.MaxHeight == 0 || info.Height <= l.MaxHeight) {
		return photo, nil
	}
	res, err := imaging.Process(bytes.NewReader(photo), imaging.Transform{
		Mode: imaging.ModeFit, Width: l.MaxWidth, Height: l.MaxHeight, Format: imaging.FormatJPEG,
	}, imaging.Options{MaxBytes: l.MaxBytes})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}
```
<!-- /ds:block hash=5df300 -->

What the endpoint answers (the `denied` list, the types refused whatever `allowed` says, is shortened here):

```sh
curl -s localhost:8107/photos/limits
```

```json
{"max_bytes":26214400,"allowed":["image/jpeg","image/png","image/webp"],"denied":["text/html","application/xhtml+xml","image/svg+xml",...],"max_width":4096,"max_height":4096}
```

A 6000 × 4000 photo sent without downscaling is refused, and nothing is stored:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' -F file=@IMG_0100.jpg -w '\n%{http_code}\n'
```

```text
{"files":[],"failed":[{"filename":"IMG_0100.jpg","error":"image is 6000x4000; it must be at most 4096 pixels wide","code":"rejected"}]}

415
```

`TestDownscaleContract` in the example checks the round trip: the oversized photo is refused, the same photo after `downscale` with the fetched limits is stored, and a photo that already fits is sent unchanged.

- The dimensions are as displayed: `CheckDimensions` and `imaging.Probe` apply the EXIF orientation, so a portrait photo a phone stored sideways is measured upright. Downscale on the client after rotating, as the platform image APIs do.
- Refusing is a choice. A server that would rather accept large photos can resize them itself in `Policy.Process` (`imaging.Process` with `ModeFit`), at the CPU cost the client-side resize was meant to save.
- Keep the server's limits even when every client downscales: a client is not a security boundary, and an old app version will not know about a new limit.

## Step 2: Store one photo, or recognize it

<!-- ds:block id=app-storeone-cd7dtawe -->
**App.storeOne** · [`examples/recipe-mobile-uploads/main.go:168-187`](../../examples/recipe-mobile-uploads/main.go#L168-L187)

```go
func (a *App) storeOne(ctx context.Context, user string, body io.Reader, name string) (stored, error) {
	out := stored{Filename: upload.SafeFilename(name)}
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.Upload(ctx, user, diskName, body, name, a.policy)
		if err != nil {
			return err
		}
		if existing, ok, err := a.findBySHA(ctx, user, f); err != nil || ok {
			out.FileID, out.Duplicate = existing, ok
			return errors.Join(err, errDuplicate)
		}
		out.FileID = f.ID
		_, err = a.Store.Attach(ctx, user, f.ID, records.Owner{Type: ownerUser, ID: user}, roleCameraRoll)
		return err
	})
	if out.Duplicate && errors.Is(err, errDuplicate) {
		return out, nil
	}
	return out, err
}
```
<!-- /ds:block hash=666140 -->

<!-- ds:block id=app-findbysha-e7ny7uwv -->
**App.findBySHA** · [`examples/recipe-mobile-uploads/main.go:193-202`](../../examples/recipe-mobile-uploads/main.go#L193-L202)

```go
func (a *App) findBySHA(ctx context.Context, user string, f records.File) (string, bool, error) {
	page, err := a.Store.Files(ctx, records.FileQuery{
		Scope: user, Limit: 1,
		Filters: []sqlb.Predicate{sqlb.Eq(records.FileSHA256, f.SHA256), sqlb.NotEq(records.FileID, f.ID)},
	})
	if err != nil || len(page.Files) == 0 {
		return "", false, err
	}
	return page.Files[0].ID, true, nil
}
```
<!-- /ds:block hash=7d2d49 -->

The duplicate check runs after the photo is stored and sanitized, because the server cannot know the stored hash earlier: the client's hash is of the original, and `Sanitize` changes the bytes. The cost is that a retried photo is uploaded again. To save the bandwidth as well, have the client send the SHA-256 of each original in a first request, store it in `NewFile.Metadata`, and skip the photos the server already has; that check is in your app, not in the library.

Two concurrent retries of the same photo can both pass the check before either commits and store it twice. For backups that is harmless; if it is not for you, put a unique index on `(scope, sha256)` in your own migration and treat the violation (`records.IsUniqueViolation`) as a duplicate.

## Step 3: The batch

<!-- ds:block id=batchresponse-xwtcum37 -->
**batchResponse** · [`examples/recipe-mobile-uploads/main.go:157-160`](../../examples/recipe-mobile-uploads/main.go#L157-L160)

```go
type batchResponse struct {
	Files  []stored             `json:"files"`
	Failed []upload.FileFailure `json:"failed,omitempty"`
}
```
<!-- /ds:block hash=e5bd73 -->

<!-- ds:block id=app-receive-mqtjrgav -->
**App.receive** · [`examples/recipe-mobile-uploads/main.go:209-239`](../../examples/recipe-mobile-uploads/main.go#L209-L239)

```go
func (a *App) receive(ctx context.Context, user string, mr *multipart.Reader) (batchResponse, error) {
	resp := batchResponse{Files: []stored{}}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return resp, nil
		}
		if err != nil {
			return resp, requestError(err)
		}
		if part.FormName() != formField || part.FileName() == "" {
			_ = part.Close()
			continue
		}
		if len(resp.Files)+len(resp.Failed) == maxFilesPerRequest {
			_ = part.Close()
			return resp, filesystem.Errorf(filesystem.CodeInvalidArgument, "more than %d files", maxFilesPerRequest)
		}
		s, err := a.storeOne(ctx, user, part, part.FileName())
		_ = part.Close()
		var mbe *http.MaxBytesError
		switch {
		case err == nil:
			resp.Files = append(resp.Files, s)
		case errors.As(err, &mbe):
			return resp, requestError(err)
		default:
			resp.Failed = append(resp.Failed, upload.FileFailure{Filename: s.Filename, Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
		}
	}
}
```
<!-- /ds:block hash=023b5b -->

<!-- ds:block id=app-upload-2ekk2k73 -->
**App.upload** · [`examples/recipe-mobile-uploads/main.go:244-267`](../../examples/recipe-mobile-uploads/main.go#L244-L267)

```go
func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFilesPerRequest*maxPhotoBytes+formOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, filesystem.NewError(filesystem.CodeInvalidArgument, err))
		return
	}
	resp, err := a.receive(r.Context(), user, mr)
	switch {
	case err != nil:
		writeError(w, err)
	case len(resp.Files) == 0 && len(resp.Failed) > 0:
		writeJSON(w, fshttp.StatusOf(filesystem.NewError(resp.Failed[0].Code, nil)), resp)
	case len(resp.Failed) > 0:
		writeJSON(w, http.StatusOK, resp)
	default:
		writeJSON(w, http.StatusCreated, resp)
	}
}
```
<!-- /ds:block hash=ef9589 -->

The status follows `upload.Handler`'s convention: `201` when everything is stored, `200` when some photos failed (read `failed`), and the first failure's status (`415`, `413`) when nothing could be stored. Request-level failures (more than 20 files, the whole body over 20 × 25 MiB, a malformed body) end the request but keep the photos already committed, which a retry will then report as duplicates.

## Try it

```sh
cd examples
go run ./recipe-mobile-uploads
```

```text
listening on http://localhost:8107 data in /var/folders/sk/.../T/recipe-mobile-uploads-3622032633
```

A batch of four: two JPEGs, a HEIC, and an HTML file named like a photo:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' \
  -F file=@IMG_0001.jpg -F file=@IMG_0002.jpg -F file=@IMG_0003.HEIC -F file=@IMG_0004.jpg -w '\n%{http_code}\n'
```

```text
{"files":[{"filename":"IMG_0001.jpg","file_id":"fil_kczudnkhjobzi5cimd3nnnayux"},{"filename":"IMG_0002.jpg","file_id":"fil_ebdsc226bt7jugiz72wcuiezlq"}],"failed":[{"filename":"IMG_0003.HEIC","error":"content type \"image/heic\" is not allowed","code":"rejected"},{"filename":"IMG_0004.jpg","error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}]}

200
```

The app did not hear the answer and sends the two photos again:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' -F file=@IMG_0001.jpg -F file=@IMG_0002.jpg -w '\n%{http_code}\n'
curl -s localhost:8107/photos -H 'X-User: u1'
```

```text
{"files":[{"filename":"IMG_0001.jpg","file_id":"fil_kczudnkhjobzi5cimd3nnnayux","duplicate":true},{"filename":"IMG_0002.jpg","file_id":"fil_ebdsc226bt7jugiz72wcuiezlq","duplicate":true}]}

201
[{"file_id":"fil_kczudnkhjobzi5cimd3nnnayux","filename":"IMG_0001.jpg","content_type":"image/jpeg","size":31858,"sha256":"054af3a10ff5e30b8dbd661160f84d776bdf153970f6baeb8d6e278f8fdf6ff9"},{"file_id":"fil_ebdsc226bt7jugiz72wcuiezlq","filename":"IMG_0002.jpg","content_type":"image/jpeg","size":70838,"sha256":"7fc5d8291cc57d034486aa0445daf10654d10e2ccb9099f5da1979aa937d58e9"}]
```

Check: the same IDs come back, and the disk holds two objects, not four.

The stored sizes (31,858 and 70,838 bytes) are larger than the uploads (18,806 and 41,258): these test images were written at a low JPEG quality, and `Sanitize` re-encodes at quality 90 (`imaging.SanitizeQuality`). A photo saved at a higher quality than 90 shrinks, one saved lower grows; if storage size matters, pass `imaging.WithQuality(q)` to `Sanitize` with a lower quality (for example 80), or resize in your own `Processor` with `imaging.Process`.

A batch where nothing can be stored answers with that failure's status:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' -F file=@IMG_0003.HEIC -w '\n%{http_code}\n'
```

```text
{"files":[],"failed":[{"filename":"IMG_0003.HEIC","error":"content type \"image/heic\" is not allowed","code":"rejected"}]}

415
```

## Edge cases this handles

- A retried batch after a lost response: same file IDs, `duplicate: true`, no second copy.
- One bad photo in twenty: the other nineteen are stored; the client retries or skips the one listed in `failed`.
- The same photo from two users: two files, each in its owner's scope.
- A photo over 25 MiB: refused by `Sanitize`'s `MaxBytes` (`too_large`), nothing stored.
- A photo over 4096 px on its longest side: refused by `CheckDimensions` (`rejected`, `415`) after its header is read, and its object deleted.
- More than 20 files: `400`, the ones before the 21st stay stored.
- A connection that drops mid-batch: the photos fully received are committed; the client's retry reports them as duplicates and stores the rest.

## For production

- Authenticate with your session or token middleware instead of `X-User`.
- Store photos on an object store and serve them with signed URLs ([private downloads](private-downloads.md)) or, for thumbnails, add the variants plugin ([product images](product-images.md) shows the wiring).
- Image work is CPU-bound and a camera-roll backup is a burst: limit concurrent batches per instance, and let clients send batches one after another rather than in parallel.
- Add a unique index on `(scope, sha256)` if duplicates under concurrent retries matter.
- Give the request a deadline that fits 20 photos on a slow link (the server's `WriteTimeout`, a proxy's body timeout), or move large backups to tus.

## Related

- [Strip photo metadata](strip-photo-metadata.md): what `Sanitize` removes and how to prove it.
- [Resumable uploads](resumable-uploads.md): tus for large files and bad networks.
- [Browser direct uploads](browser-direct-uploads.md): presigned uploads straight to S3.
- [Uploads](../concepts/uploads.md): policies, processors, `Handler` and `Partial`.
- [Files](../records/files.md): `Store.Upload`, `Files` and filters.
