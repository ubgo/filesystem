# Mobile uploads: a camera-roll backup endpoint

The backend of "back up my camera roll": a phone sends up to 20 photos in one multipart request, each photo is checked and stripped of its GPS position and camera details on its own, a bad photo does not sink the others (it is listed under `failed`), and a batch retried after a lost response does not store the same photo twice. It walks the multipart body part by part, stores each photo in its own `records.Store.Tx` with `Store.Upload` under an `upload.Policy` whose `Process` is `contrib/imaging`'s `Sanitize`, and recognises a repeat by the SHA-256 of the sanitized bytes (sanitizing is deterministic). The guide is [Mobile uploads](../../docs/recipes/mobile-uploads.md).

## Run it

```sh
cd examples
go run ./recipe-mobile-uploads
```

It creates a new temporary directory (printed at start-up) holding `app.db` (SQLite, the `fs_` tables) and `files/`, and listens on `http://localhost:8107`. The user is the `X-User` header, a demo shortcut: a real app reads its session. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Run the tests with `go test ./recipe-mobile-uploads`.

HEIC is refused on purpose: it is detected (`image/heic`) but cannot be decoded in pure Go, so it could not be sanitized; the app asks the phone for JPEG instead.

## Walkthrough

The files: two JPEGs carrying EXIF with a GPS position and a camera model (`IMG_0001.jpg`, 800×600, and `IMG_0002.jpg`, 1200×900), a real HEIC (`IMG_0003.HEIC`), and an HTML file named like a photo (`IMG_0004.jpg`). Any JPEGs from a phone work the same way.

1. A batch of four: the two JPEGs are stored, the other two are listed with their reasons, and the answer is `200` because some failed:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' \
  -F file=@IMG_0001.jpg -F file=@IMG_0002.jpg -F file=@IMG_0003.HEIC -F file=@IMG_0004.jpg -w '\n%{http_code}\n'
```

```text
{"files":[{"filename":"IMG_0001.jpg","file_id":"fil_razmw6gnxrr6cquynvfsygztne"},{"filename":"IMG_0002.jpg","file_id":"fil_53ocq4avrhu6ssv77tpuxmsx4n"}],"failed":[{"filename":"IMG_0003.HEIC","error":"content type \"image/heic\" is not allowed","code":"rejected"},{"filename":"IMG_0004.jpg","error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}]}

200
```

2. The phone did not hear the answer and sends the two photos again. The same IDs come back marked `duplicate`, and the camera roll still holds two photos:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' -F file=@IMG_0001.jpg -F file=@IMG_0002.jpg -w '\n%{http_code}\n'
curl -s localhost:8107/photos -H 'X-User: u1'
```

```text
{"files":[{"filename":"IMG_0001.jpg","file_id":"fil_razmw6gnxrr6cquynvfsygztne","duplicate":true},{"filename":"IMG_0002.jpg","file_id":"fil_53ocq4avrhu6ssv77tpuxmsx4n","duplicate":true}]}

201
[{"file_id":"fil_razmw6gnxrr6cquynvfsygztne","filename":"IMG_0001.jpg","content_type":"image/jpeg","size":70105,"sha256":"2a390899eda2e7ddf0682ccc2dc9810c31471b8e39cb6ba4b5e15f9ba45bf46b"},{"file_id":"fil_53ocq4avrhu6ssv77tpuxmsx4n","filename":"IMG_0002.jpg","content_type":"image/jpeg","size":155919,"sha256":"ae096f6ab666a1cede0481efe40bb26b12a2bcb96920e87d2285adc7f9076dd2"}]
```

After both requests the data directory's `files/` holds two objects, not four, and reading their EXIF finds no tags at all: the GPS position and camera model are gone.

The stored sizes (70,105 and 155,919 bytes) are larger than the uploads (57,587 and 127,913) because these test JPEGs were written at quality 85 and `Sanitize` re-encodes at quality 90 (`imaging.SanitizeQuality`). A photo saved at a higher quality shrinks; see the guide for choosing a lower quality.

3. A batch where nothing can be stored answers with that failure's status, and a request without a user is refused:

```sh
curl -s -X POST localhost:8107/photos -H 'X-User: u1' -F file=@IMG_0003.HEIC -w '\n%{http_code}\n'
curl -s localhost:8107/photos -w '\n%{http_code}\n'
```

```text
{"files":[],"failed":[{"filename":"IMG_0003.HEIC","error":"content type \"image/heic\" is not allowed","code":"rejected"}]}

415
{"error":"Forbidden","code":"permission"}

403
```

4. The phone asks what a photo may be before it uploads, and a 6000 × 4000 photo sent without downscaling is refused by the server's dimension check (the `denied` list is shortened here):

```sh
curl -s localhost:8107/photos/limits
curl -s -X POST localhost:8107/photos -H 'X-User: u1' -F file=@IMG_0100.jpg -w '\n%{http_code}\n'
```

```text
{"max_bytes":26214400,"allowed":["image/jpeg","image/png","image/webp"],"denied":["text/html","application/xhtml+xml","image/svg+xml",...],"max_width":4096,"max_height":4096}
{"files":[],"failed":[{"filename":"IMG_0100.jpg","error":"image is 6000x4000; it must be at most 4096 pixels wide","code":"rejected"}]}

415
```

`client.go` holds the client half of that contract (`fetchLimits` and `downscale`), and `TestDownscaleContract` checks that the downscaled photo is then accepted.

## The code

`photoPolicy` accepts JPEG, PNG and WebP, re-encodes each with `imaging.Sanitize` to drop EXIF, and caps the photo as sent through `Sanitize`'s `MaxBytes` (because `Policy.MaxSize` applies to what `Process` returns).

<!-- ds:block id=photopolicy-sccupwen -->
**photoPolicy** · [`examples/recipe-mobile-uploads/main.go:76-81`](main.go#L76-L81)

```go
var photoPolicy = upload.Policy{
	MaxSize: maxPhotoBytes,
	Allowed: []string{"image/jpeg", "image/png", "image/webp"},
	Key:     upload.RandomKey("photos"),
	Process: imaging.Sanitize(imaging.Options{MaxBytes: maxPhotoBytes}),
}
```
<!-- /ds:block hash=890b74 -->

`batchResponse` is the answer: what is stored (new or recognised) and what failed and why.

<!-- ds:block id=batchresponse-xwtcum37 -->
**batchResponse** · [`examples/recipe-mobile-uploads/main.go:157-160`](main.go#L157-L160)

```go
type batchResponse struct {
	Files  []stored             `json:"files"`
	Failed []upload.FileFailure `json:"failed,omitempty"`
}
```
<!-- /ds:block hash=e5bd73 -->

`storeOne` uploads and attaches one photo in its own transaction. A photo the user already has is rolled back, which makes `Store.Upload` delete the new object, and the existing file's ID is returned: retrying a batch is safe.

<!-- ds:block id=app-storeone-cd7dtawe -->
**App.storeOne** · [`examples/recipe-mobile-uploads/main.go:168-187`](main.go#L168-L187)

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

`findBySHA` looks for another live file of the user's with the same SHA-256. `fs_files` has no index on `sha256`; add one in your own migration if users keep tens of thousands of photos.

<!-- ds:block id=app-findbysha-e7ny7uwv -->
**App.findBySHA** · [`examples/recipe-mobile-uploads/main.go:193-202`](main.go#L193-L202)

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

`receive` streams each multipart part to `storeOne`. A photo's own failure is listed and the batch goes on; a failure of the request itself (malformed, over the size cap, more than 20 files) ends it, keeping the photos already stored.

<!-- ds:block id=app-receive-mqtjrgav -->
**App.receive** · [`examples/recipe-mobile-uploads/main.go:209-239`](main.go#L209-L239)

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

`upload` is the route: `201` when every photo is stored, `200` when some failed, and the first failure's status when none could be stored.

<!-- ds:block id=app-upload-2ekk2k73 -->
**App.upload** · [`examples/recipe-mobile-uploads/main.go:244-267`](main.go#L244-L267)

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

`photoLimits` adds the dimension check to the policy and builds the endpoint that publishes the same limits, so the two cannot drift apart.

<!-- ds:block id=photolimits-se3qfup2 -->
**photoLimits** · [`examples/recipe-mobile-uploads/main.go:99-107`](main.go#L99-L107)

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

The client side: fetch the limits once, then scale any photo larger than them down to fit.

<!-- ds:block id=fetchlimits-vzux344u -->
**fetchLimits** · [`examples/recipe-mobile-uploads/client.go:18-34`](client.go#L18-L34)

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
**downscale** · [`examples/recipe-mobile-uploads/client.go:43-58`](client.go#L43-L58)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy and its dimension check, the limits endpoint, the per-photo transaction with duplicate detection, the batch walker, the upload and list routes, and the server. |
| `client.go` | The client half of the downscale contract: fetch the limits, scale a photo down to fit them. |
| `main_test.go` | A partial batch with a retry that stores nothing twice, request-level failures (a batch where every photo fails, more than 20 files, no signed-in user), and the downscale contract (an oversized photo refused, the same photo downscaled and accepted). |

## Next

- The guide: [Mobile uploads](../../docs/recipes/mobile-uploads.md), including which path to pick for large files and production notes.
- [Uploads](../../docs/concepts/uploads.md) for policies, processors, `Handler` and `Partial`, and [Files](../../docs/records/files.md) for `Store.Upload` and `Files` filters.
- Related recipes: [recipe-strip-photo-metadata](../recipe-strip-photo-metadata) (what `Sanitize` removes and how to prove it), [recipe-resumable-uploads](../recipe-resumable-uploads) (tus for large files and bad networks) and [recipe-browser-direct-uploads](../recipe-browser-direct-uploads) (presigned uploads straight to S3).
