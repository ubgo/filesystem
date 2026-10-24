# Avatars: one profile picture per user, cropped, cleaned and resized

The profile-picture feature of a social or SaaS app: a user uploads a photo, optionally with the square they chose in a cropper; the server re-encodes it (dropping EXIF and GPS), keeps exactly one avatar per user, generates the 64 and 256 px sizes right after the upload commits, and serves them as public URLs. It uses `upload.Policy` with `imaging.Sanitize` or a crop processor from `contrib/imaging`, `records` with `WithSingleRoles` and `SetAttachment(..., PreviousPurge)`, the `contrib/imaging/variants` plugin with `WithEager`, and `fshttp.Public` over a local disk with `WithPublicURL`. The guide is [Avatars](../../docs/recipes/avatars.md).

## Run it

```sh
cd examples
go run ./recipe-avatars
```

It creates a new temporary directory (printed at start-up) holding `app.db` (SQLite with the `fs_` tables) and the stored files under `files/`, and listens on `http://localhost:8101`, where `/public/…` serves the public files. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory.

Run the tests with `go test ./recipe-avatars`. Authentication is out of scope: a real app checks that the signed-in user is `{id}` before these routes run.

## Walkthrough

The photo here is a 1200 × 900 JPEG (`magick -size 1200x900 gradient:steelblue-orange me.jpg`); any photo works.

1. Upload it with the square the user chose (700 × 700 from x=300, y=100). Both variant URLs are in the answer because `WithEager` already generated them:

```sh
curl -s -X PUT -F file=@me.jpg 'localhost:8101/users/u1/avatar?crop=300,100,700,700'
```

```json
{"file_id":"fil_2s3zgnwxiwao2pk6tvvggu6nco","content_type":"image/jpeg","size":9373,"urls":{"256":"http://localhost:8101/public/variants/fil_2s3zgnwxiwao2pk6tvvggu6nco/avatar-256-a49dd20dfc4a10d7-6ll3mmyiz7z3i2xxgzpu5uzf7n.jpg","64":"http://localhost:8101/public/variants/fil_2s3zgnwxiwao2pk6tvvggu6nco/avatar-64-9cb641cf4231aac5-gftcwnfiwdtlvgs4lhxaxhxeqz.jpg"}}
```

2. Ask for a size: a redirect to the public URL, which serves the JPEG:

```sh
curl -s -i 'localhost:8101/users/u1/avatar?size=64' | head -3
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -L 'localhost:8101/users/u1/avatar?size=64'
```

```text
HTTP/1.1 302 Found
Content-Type: text/html; charset=utf-8
Location: http://localhost:8101/public/variants/fil_2s3zgnwxiwao2pk6tvvggu6nco/avatar-64-9cb641cf4231aac5-gftcwnfiwdtlvgs4lhxaxhxeqz.jpg
200 image/jpeg 807
```

3. What is refused: HTML named `.jpg` (the type comes from the bytes), a crop outside the photo, and a size that was never declared:

```sh
printf '<html><script>alert(1)</script>' > evil.jpg
curl -s -w '%{http_code}\n' -X PUT -F file=@evil.jpg localhost:8101/users/u1/avatar
curl -s -w '%{http_code}\n' -X PUT -F file=@me.jpg 'localhost:8101/users/u1/avatar?crop=1000,800,400,400'
curl -s -w '%{http_code}\n' 'localhost:8101/users/u1/avatar?size=1024'
```

```text
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
{"error":"imaging: crop (1000,800)-(1400,1200) is outside the 1200x900 image","code":"rejected"}
415
{"error":"size must be 64 or 256","code":"invalid_argument"}
400
```

4. Replace it without a crop (`Sanitize` only, so the whole photo is re-encoded at its own size), then delete it:

```sh
curl -s -X PUT -F file=@me.jpg localhost:8101/users/u1/avatar
curl -s -i -X DELETE localhost:8101/users/u1/avatar | head -1
curl -s -w '%{http_code}\n' localhost:8101/users/u1/avatar
```

```text
{"file_id":"fil_ld2wyh4u24pugq6xglecz67wzc","content_type":"image/jpeg","size":34446,"urls":{"256":"http://localhost:8101/public/variants/fil_ld2wyh4u24pugq6xglecz67wzc/avatar-256-a49dd20dfc4a10d7-wwcfqqbgibxhjvgzh3loazdjfi.jpg","64":"http://localhost:8101/public/variants/fil_ld2wyh4u24pugq6xglecz67wzc/avatar-64-9cb641cf4231aac5-dsrj2c6yf6wstbxuoz3k42jxlt.jpg"}}
HTTP/1.1 204 No Content
<a href="/avatars/fallback/u1.png?size=256&amp;v=1">Found</a>.

302
```

A user without an avatar is redirected to a generated fallback (an identicon seeded by the user ID, since the demo has no display names), served by `contrib/imaging/placeholder` with a year-long cache:

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -L 'localhost:8101/users/u1/avatar?size=64'
```

```text
200 image/png 205
```

Afterwards the data directory's `files/` holds no object: the replacement purged the first avatar and its two variants, and the delete purged the second.

## The code

`inputLimits` caps the bytes a processor reads, and `avatarPolicy` accepts JPEG, PNG and WebP (detected from the bytes), stores them public under random keys, and re-encodes them with `Sanitize` so EXIF and GPS never reach storage.

<!-- ds:block id=inputlimits-sybzgm7e -->
**inputLimits** · [`examples/recipe-avatars/main.go:86`](main.go#L86-L86)

```go
var inputLimits = imaging.Options{MaxBytes: maxAvatarBytes}
```
<!-- /ds:block hash=c682c3 -->

<!-- ds:block id=avatarpolicy-nqds67xq -->
**avatarPolicy** · [`examples/recipe-avatars/main.go:93-99`](main.go#L93-L99)

```go
var avatarPolicy = upload.Policy{
	MaxSize:    maxAvatarBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp"},
	Key:        upload.RandomKey("avatars"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(inputLimits),
}
```
<!-- /ds:block hash=9d223b -->

`sizes` declares the two variants once; only these names can be generated, so a client cannot ask for an arbitrary size. `newVariants` generates both right after an avatar is recorded.

<!-- ds:block id=sizes-nnfzky83 -->
**sizes** · [`examples/recipe-avatars/main.go:104-107`](main.go#L104-L107)

```go
var sizes = map[string]imaging.Transform{
	"avatar-" + sizeSmall: {Mode: imaging.ModeFill, Width: 64, Height: 64, Format: imaging.FormatJPEG},
	"avatar-" + sizeLarge: {Mode: imaging.ModeFill, Width: 256, Height: 256, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=61045d -->

<!-- ds:block id=newvariants-8s2acvgm -->
**newVariants** · [`examples/recipe-avatars/main.go:136-141`](main.go#L136-L141)

```go
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(sizes, imaging.Options{}, variants.WithEager(logFailure, "avatar-"+sizeSmall, "avatar-"+sizeLarge))
}
```
<!-- /ds:block hash=d1aa25 -->

`openDisk` makes `PublicURL` return URLs that `fshttp.Public` serves, and `openStore` uses `WithSingleRoles` so the database holds at most one avatar per user, even under concurrent uploads.

<!-- ds:block id=opendisk-r8e7pzhe -->
**openDisk** · [`examples/recipe-avatars/main.go:119-130`](main.go#L119-L130)

```go
func openDisk(dataDir, publicBase string) (*filesystem.Manager, *filesystem.Disk, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithPublicURL(publicBase+publicPrefix))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, disk, err
}
```
<!-- /ds:block hash=f158bc -->

<!-- ds:block id=openstore-h5yb7sge -->
**openStore** · [`examples/recipe-avatars/main.go:146-161`](main.go#L146-L161)

```go
func openStore(ctx context.Context, dataDir string, disks *filesystem.Manager, v *variants.Plugin) (*records.Store, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser),
		records.WithSingleRoles(roleAvatar),
		records.WithPlugins(v))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}
```
<!-- /ds:block hash=b9d381 -->

`cropTo` cuts the chosen region out of the upright photo and scales it to a 512 px square; `policyFor` swaps it in for `Sanitize` when the request carries `?crop=`.

<!-- ds:block id=cropto-t875xnwf -->
**cropTo** · [`examples/recipe-avatars/main.go:217-227`](main.go#L217-L227)

```go
func cropTo(region image.Rectangle) upload.Processor {
	t := imaging.Transform{Mode: imaging.ModeFill, Width: avatarSide, Height: avatarSide, Format: imaging.FormatJPEG, Quality: avatarQuality, Crop: region}
	return func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		res, err := imaging.Process(body, t, inputLimits)
		if err != nil {
			return nil, upload.Info{}, err
		}
		name := strings.TrimSuffix(in.Filename, filepath.Ext(in.Filename)) + t.Extension()
		return bytes.NewReader(res.Data), upload.Info{Filename: name, ContentType: res.ContentType}, nil
	}
}
```
<!-- /ds:block hash=f34577 -->

<!-- ds:block id=policyfor-dy8uujc8 -->
**policyFor** · [`examples/recipe-avatars/main.go:232-242`](main.go#L232-L242)

```go
func policyFor(r *http.Request) (upload.Policy, error) {
	region, err := parseCrop(r.URL.Query().Get("crop"))
	if err != nil {
		return upload.Policy{}, err
	}
	p := avatarPolicy
	if region != (image.Rectangle{}) {
		p.Process = cropTo(region)
	}
	return p, nil
}
```
<!-- /ds:block hash=967c1e -->

`setAvatar` stores the photo and makes it the user's only avatar in one transaction. `PreviousPurge` deletes the old avatar for good (its object and variants after the commit); a failure anywhere rolls back and deletes the new upload, so the user keeps the old avatar.

<!-- ds:block id=app-setavatar-2bfqagmp -->
**App.setAvatar** · [`examples/recipe-avatars/main.go:249-260`](main.go#L249-L260)

```go
func (a *App) setAvatar(ctx context.Context, userID string, body io.Reader, filename string, p upload.Policy) (records.File, error) {
	var stored records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if stored, err = a.Store.Upload(ctx, "", diskName, body, filename, p); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, "", stored.ID, records.Owner{Type: ownerUser, ID: userID}, roleAvatar, records.PreviousPurge)
		return err
	})
	return stored, err
}
```
<!-- /ds:block hash=a08bc0 -->

`putAvatar` is the handler around it: cap the body, read the field, choose the policy, answer with every size's URL.

<!-- ds:block id=app-putavatar-ac86bchk -->
**App.putAvatar** · [`examples/recipe-avatars/main.go:273-297`](main.go#L273-L297)

```go
func (a *App) putAvatar(w http.ResponseWriter, r *http.Request) {
	p, err := policyFor(r)
	if err != nil {
		writeError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+formOverhead)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the photo in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.setAvatar(r.Context(), r.PathValue("id"), file, header.Filename, p)
	if err != nil {
		writeError(w, err)
		return
	}
	urls, err := a.urls(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, avatarResponse{FileID: f.ID, Type: f.ContentType, Size: f.Size, URLs: urls})
}
```
<!-- /ds:block hash=6b9b66 -->

`getAvatar` redirects to a declared size and refuses any other, and sends a user without an avatar to the generated fallback; `deleteAvatar` purges the row and variant rows in one transaction, then the objects.

<!-- ds:block id=app-getavatar-6n4vcnet -->
**App.getAvatar** · [`examples/recipe-avatars/main.go:330-355`](main.go#L330-L355)

```go
func (a *App) getAvatar(w http.ResponseWriter, r *http.Request) {
	size := r.URL.Query().Get("size")
	if size == "" {
		size = sizeLarge
	}
	if _, ok := sizes["avatar-"+size]; !ok {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "size must be %s or %s", sizeSmall, sizeLarge))
		return
	}
	f, err := a.current(r.Context(), r.PathValue("id"))
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		q := url.Values{"size": {size}, "v": {placeholder.Version}}
		http.Redirect(w, r, fallbackPrefix+url.PathEscape(r.PathValue("id"))+".png?"+q.Encode(), http.StatusFound)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	u, err := a.Variants.URL(r.Context(), a.Store, f.Scope, f.ID, "avatar-"+size, time.Hour)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}
```
<!-- /ds:block hash=7848cb -->

<!-- ds:block id=app-deleteavatar-vgd2gujt -->
**App.deleteAvatar** · [`examples/recipe-avatars/main.go:360-371`](main.go#L360-L371)

```go
func (a *App) deleteAvatar(w http.ResponseWriter, r *http.Request) {
	f, err := a.current(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.Store.Purge(r.Context(), "", f.ID); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=c50f7d -->

`writeError` maps the error code to a status and names the code in the body, so a client can tell "too large" from "not an image".

<!-- ds:block id=writeerror-6eybqhn9 -->
**writeError** · [`examples/recipe-avatars/main.go:376-379`](main.go#L376-L379)

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
| `main.go` | The policy, the declared sizes, the disk and store wiring, the crop processor, and the three avatar routes plus `/public/`. |
| `main_test.go` | The full lifecycle (upload, crop, replace, delete, no objects left), the refusals, and crop parsing. |

## Next

- The guide: [Avatars](../../docs/recipes/avatars.md), with edge cases and production notes.
- [Attachments](../../docs/records/attachments.md), [Variants](../../docs/plugins/variants.md) and [Uploads](../../docs/concepts/uploads.md).
- Related recipes: [recipe-product-images](../recipe-product-images) (many ordered images per owner), [recipe-strip-photo-metadata](../recipe-strip-photo-metadata) (`Sanitize` on every photo), and the [records-variants](../records-variants) example.
