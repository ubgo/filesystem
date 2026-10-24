# Avatars

A profile picture is the first file feature most products ship, and the one where the small mistakes show: two avatars after a double-click, the old photo still stored forever, a 12 MB phone photo sent to every comment thread, the user's home GPS position published in the EXIF block. This recipe is for any social, SaaS or community app where a user (or a team, or an organization) has exactly one picture.

The program is [examples/recipe-avatars](../../examples/recipe-avatars). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `PUT /users/{id}/avatar` takes a photo (JPEG, PNG or WebP, up to 8 MiB) and, optionally, the square the user chose in a cropper (`?crop=x,y,w,h`).
- The stored file is always a re-encoded image: no EXIF, no GPS, no camera serial, no payload appended after the image data.
- A user has at most one avatar, enforced by the database, even when two uploads race. The previous one is deleted for good, with its resized copies.
- A 64 px and a 256 px square are generated right after the upload commits, so no profile view waits for a resize.
- `GET /users/{id}/avatar?size=64` redirects to a public URL that a browser or CDN can cache.
- A user without an avatar gets a generated fallback (initials or identicon) instead of a broken image ([When there is no avatar](#when-there-is-no-avatar)).

## The design

| Piece | Why |
|---|---|
| `records.WithSingleRoles("avatar")` | The role holds one file per owner. The attachment ID is derived from the owner and role, so a second concurrent insert hits a unique index instead of creating a second avatar. |
| `Store.SetAttachment(..., records.PreviousPurge)` | Replaces whatever was attached and deletes the old file for good, objects after the commit. `PreviousTrash` would keep it restorable; for avatars nobody restores, purge saves the bytes. |
| `imaging.Sanitize` as `Policy.Process` | Re-encodes the photo before it is stored. Without a crop this is the only processing. |
| A crop processor built on `imaging.Process` | When the client sends the region it chose, the server cuts that region out of the upright photo and scales it to a 512 px square. It re-encodes too, so it strips metadata like Sanitize. |
| `variants` plugin with `WithEager` | Declares the sizes once; only declared names can be generated, and both are made after the commit. |
| `WithPublicURL` + `fshttp.Public` | Avatars are public. The local disk in this demo is served by `fshttp.Public`; on S3 or R2 the same `WithPublicURL` points at the bucket's CDN domain instead (see [public CDN](public-cdn.md)). |

## Step 1: The policy

The upload policy is the contract of the endpoint: which types, how large, where stored, and how processed.

<!-- ds:block id=inputlimits-sybzgm7e -->
**inputLimits** · [`examples/recipe-avatars/main.go:86`](../../examples/recipe-avatars/main.go#L86-L86)

```go
var inputLimits = imaging.Options{MaxBytes: maxAvatarBytes}
```
<!-- /ds:block hash=c682c3 -->

<!-- ds:block id=avatarpolicy-nqds67xq -->
**avatarPolicy** · [`examples/recipe-avatars/main.go:93-99`](../../examples/recipe-avatars/main.go#L93-L99)

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

`Allowed` lists exact types, not `image/*`: GIF and TIFF are images too, but nobody needs an animated or 50 MB avatar. The type is detected from the bytes, so a file named `me.jpg` that contains HTML is refused whatever its name or `Content-Type` header says. `upload.RandomKey("avatars")` names the object `avatars/<random>.jpg`; the user's file name never reaches the key.

The input cap matters. `Policy.MaxSize` applies to what `Process` returns, which is the re-encoded image, usually far smaller than the upload. The bound on what the client may send is therefore `imaging.Options.MaxBytes` (refused with `CodeTooLarge`) plus the `http.MaxBytesReader` in the handler. `imaging.Options.MaxPixels` (50 megapixels by default) refuses a pixel bomb, a tiny file that declares a huge image, from its header before anything is decoded.

## Step 2: The sizes

<!-- ds:block id=sizes-nnfzky83 -->
**sizes** · [`examples/recipe-avatars/main.go:104-107`](../../examples/recipe-avatars/main.go#L104-L107)

```go
var sizes = map[string]imaging.Transform{
	"avatar-" + sizeSmall: {Mode: imaging.ModeFill, Width: 64, Height: 64, Format: imaging.FormatJPEG},
	"avatar-" + sizeLarge: {Mode: imaging.ModeFill, Width: 256, Height: 256, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=61045d -->

<!-- ds:block id=newvariants-8s2acvgm -->
**newVariants** · [`examples/recipe-avatars/main.go:136-141`](../../examples/recipe-avatars/main.go#L136-L141)

```go
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(sizes, imaging.Options{}, variants.WithEager(logFailure, "avatar-"+sizeSmall, "avatar-"+sizeLarge))
}
```
<!-- /ds:block hash=d1aa25 -->

`ModeFill` scales to cover the box and crops the overflow, so the output is exactly 64 × 64 even when the source is not square. Each variant is stored as `variants/<file id>/<name>-<digest>-<id>.jpg`, where `<id>` is the random part of the variant's own ID, so every generation writes a key no other generation writes and a key never shows two different images. The digest identifies the transform, so changing a size later produces new objects on the next request, and `Plugin.Prune` removes the old ones.

`WithEager` runs after the transaction commits, never inside it, so a slow resize does not hold a database connection. When it fails, the error goes to the handler and the variant is generated on its first request instead.

## Step 3: The disk and the store

<!-- ds:block id=opendisk-r8e7pzhe -->
**openDisk** · [`examples/recipe-avatars/main.go:119-130`](../../examples/recipe-avatars/main.go#L119-L130)

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
**openStore** · [`examples/recipe-avatars/main.go:146-161`](../../examples/recipe-avatars/main.go#L146-L161)

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

`WithOwnerTypes("user")` makes an attachment to any other owner type an error, so a typo cannot attach a file to nothing. `Migrate` creates `fs_files`, `fs_attachments` and the plugin's `fs_variants`.

## Step 4: The crop

A cropper widget in the browser or app reports a rectangle in pixels of the photo as the user sees it. `imaging.Transform.Crop` is defined in exactly those coordinates: the EXIF orientation is applied first, so a portrait photo a phone stored sideways is cropped where the user pointed.

<!-- ds:block id=cropto-t875xnwf -->
**cropTo** · [`examples/recipe-avatars/main.go:217-227`](../../examples/recipe-avatars/main.go#L217-L227)

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
**policyFor** · [`examples/recipe-avatars/main.go:232-242`](../../examples/recipe-avatars/main.go#L232-L242)

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

The crop is applied once, at upload, and the stored file is the cropped square. The alternative, storing the original and the rectangle, lets the user re-crop later without uploading again, but every size must then be derived from the rectangle, which the variants plugin does not do (its transforms are fixed per name). Store the original separately, in another role, if you need re-cropping.

## Step 5: Store and attach in one transaction

<!-- ds:block id=app-setavatar-2bfqagmp -->
**App.setAvatar** · [`examples/recipe-avatars/main.go:249-260`](../../examples/recipe-avatars/main.go#L249-L260)

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

What each failure leaves behind:

- The upload is refused (type, size, crop outside the photo): nothing is stored, the old avatar stays.
- `SetAttachment` fails, or the process dies before the commit: the transaction rolls back, and `Store.Upload` registered a rollback hook that deletes the new object. The old avatar stays.
- Two uploads for the same user race: both store their objects, and the single role's unique index lets only one attachment exist. Where the transactions overlap (Postgres, MySQL), one wins and the other fails, typically with `CodeExists` (`409`), and its object is deleted on rollback; retrying it makes it the avatar. Where the database serializes writers (SQLite), the later upload replaces the earlier one.
- The commit succeeds but deleting the old object fails: the row is already gone, so nothing points at it. The failure goes to `records.WithObjectErrorHandler` when one is set; [consistency](../concepts/consistency.md) covers sweeping such orphans.

The handler around it caps the body and reads the form field:

<!-- ds:block id=app-putavatar-ac86bchk -->
**App.putAvatar** · [`examples/recipe-avatars/main.go:273-297`](../../examples/recipe-avatars/main.go#L273-L297)

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

## Step 6: Serve, and delete

`getAvatar` redirects to a declared size and refuses any other; a user without an avatar goes to the generated fallback ([When there is no avatar](#when-there-is-no-avatar)).

<!-- ds:block id=app-getavatar-6n4vcnet -->
**App.getAvatar** · [`examples/recipe-avatars/main.go:330-355`](../../examples/recipe-avatars/main.go#L330-L355)

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

`Plugin.URL` returns the public URL because the file is public and the disk can build one; for a private file it would return a signed URL valid for the given lifetime. The size is checked against the declared names before anything else, so `?size=4096` is a 400, not a resize.

<!-- ds:block id=app-deleteavatar-vgd2gujt -->
**App.deleteAvatar** · [`examples/recipe-avatars/main.go:360-371`](../../examples/recipe-avatars/main.go#L360-L371)

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

`Store.Purge` deletes the attachment, the file row and the variant rows in one transaction, then the objects once it commits. When a whole account is deleted, use `Store.PurgeOwner` instead, inside the transaction that deletes the user ([account deletion](account-deletion.md)).

Errors are answered with the status `fshttp.StatusOf` maps the code to, and the code itself in the body, so a client can tell "too large" from "not an image":

<!-- ds:block id=writeerror-6eybqhn9 -->
**writeError** · [`examples/recipe-avatars/main.go:376-379`](../../examples/recipe-avatars/main.go#L376-L379)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=e4052c -->

## Try it

```sh
cd examples
go run ./recipe-avatars
```

```text
listening on http://localhost:8101 data in /var/folders/sk/.../T/recipe-avatars-3858795084
```

Upload a 1200 × 900 photo with the square the user chose (700 × 700 from x=300, y=100):

```sh
curl -s -X PUT -F file=@me.jpg 'localhost:8101/users/u1/avatar?crop=300,100,700,700'
```

```json
{"file_id":"fil_qiqamm6v62galcw6feasvnieiq","content_type":"image/jpeg","size":48984,"urls":{"256":"http://localhost:8101/public/variants/fil_qiqamm6v62galcw6feasvnieiq/avatar-256-a49dd20dfc4a10d7-ria36z62n2xp6ph4lrshmn3ak7.jpg","64":"http://localhost:8101/public/variants/fil_qiqamm6v62galcw6feasvnieiq/avatar-64-9cb641cf4231aac5-lmzzxfmkda7mbcyuv5f75svhv4.jpg"}}
```

Check: both variant URLs are in the answer, which means `WithEager` already generated them.

```sh
curl -s -i 'localhost:8101/users/u1/avatar?size=64'
```

```text
HTTP/1.1 302 Found
Location: http://localhost:8101/public/variants/fil_qiqamm6v62galcw6feasvnieiq/avatar-64-9cb641cf4231aac5-lmzzxfmkda7mbcyuv5f75svhv4.jpg
```

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -L 'localhost:8101/users/u1/avatar?size=64'
```

```text
200 image/jpeg 792
```

What is refused, and how:

```sh
printf '<html><script>alert(1)</script>' > evil.jpg
curl -s -X PUT -F file=@evil.jpg localhost:8101/users/u1/avatar
```

```json
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
```

```sh
curl -s -X PUT -F file=@me.jpg 'localhost:8101/users/u1/avatar?crop=1000,800,400,400'
```

```json
{"error":"imaging: crop (1000,800)-(1400,1200) is outside the 1200x900 image","code":"rejected"}
```

```sh
curl -s 'localhost:8101/users/u1/avatar?size=1024'
```

```json
{"error":"size must be 64 or 256","code":"invalid_argument"}
```

Replace it without a crop (Sanitize only, the 1200 × 900 photo is kept whole), then delete it:

```sh
curl -s -X PUT -F file=@me.jpg localhost:8101/users/u1/avatar
curl -s -i -X DELETE localhost:8101/users/u1/avatar
curl -s localhost:8101/users/u1/avatar
```

```text
{"file_id":"fil_qssxwtn6osbnglo7swtprvuxgc","content_type":"image/jpeg","size":372128,"urls":{...}}
HTTP/1.1 204 No Content
<a href="/avatars/fallback/u1.png?size=256&amp;v=1">Found</a>.
```

Check: the data directory's `files/` holds no object after the delete. The replacement purged the first avatar and its two variants; the delete purged the second. A user without an avatar is redirected to a generated fallback ([When there is no avatar](#when-there-is-no-avatar)), which is a real image:

```sh
curl -s -o /dev/null -w '%{http_code} %{content_type} %{size_download}\n' -L 'localhost:8101/users/u1/avatar?size=64'
curl -s -I 'localhost:8101/avatars/fallback/u1.png?size=64&v=1' | grep -iE '^HTTP|cache-control|etag'
```

```text
200 image/png 205
HTTP/1.1 200 OK
Cache-Control: public, max-age=31536000, immutable
Etag: "43d3f0746335b6154adb4738822076e6"
```

## When there is no avatar

A user who never uploaded a photo still needs a picture. `contrib/imaging/placeholder` draws one without storing anything: the user's initials on a coloured square, or an identicon (a mirrored 5×5 pattern) when the name has no initials the font can draw (CJK, emoji, no name at all). The colour and the pattern come from a seed, the user ID, so a user keeps the same fallback on every device and every deployment, and the same request always produces the same PNG bytes.

Mount the handler once, with its prefix stripped: `mux.Handle("/avatars/fallback/", http.StripPrefix("/avatars/fallback/", placeholder.Handler(placeholder.HandlerOptions{})))`. It answers `GET /avatars/fallback/{seed}.png?name=Ada+Lovelace&size=64`, with `size` from `placeholder.MinSize` to `placeholder.MaxSize` (16 to 512) and `style=identicon` to skip the initials. Then, where `getAvatar` finds no avatar (`a.current` returns `not_found`), redirect to that URL instead of answering 404, and do the same wherever the app renders an avatar URL for a user without one. The example has no display names, so its fallback is the identicon; an app adds `name=<display name>` to get initials (`/avatars/fallback/usr_42.png?name=Ada+Lovelace&size=64&v=1`). `getAvatar`, shown above, does exactly this with `url.Values` for the query.

The fallback is cached hard. Everything it depends on is in its URL, so the handler sends `Cache-Control: public, max-age=31536000, immutable` and an `ETag` computed without drawing (a revalidation is a `304` that costs a hash). Put `placeholder.Version` in the URL (the `v=1` above): it changes when the drawing does, and a cache that never revalidates then fetches the new look. When the user uploads a photo, `getAvatar` starts redirecting to the variant instead, so nothing cached has to be purged.

If display names should not appear in URLs, or a client must not be able to put arbitrary text on another user's fallback, set `HandlerOptions.Name` to a lookup from seed to name: the `name` parameter is then ignored, the URL is `/avatars/fallback/usr_42.png?size=64`, and because a rename changes the image under the same URL, responses carry `Cache-Control: public, max-age=3600` and are revalidated by `ETag`. A lookup error is answered with its code's status, so an unknown user can be a `404`.

## Edge cases this handles

- Concurrent uploads for one user: never two avatars. A loser fails (`409` when it hits the unique index) and leaves nothing behind.
- A crop rectangle outside the photo: `415` with `rejected`, nothing stored.
- A photo that is not an image, or HTML named `.jpg`: `415`, detected from the bytes.
- A huge upload: `413`, refused while streaming; nothing larger than the cap is read into memory by the processor.
- A pixel bomb: refused from the header by `MaxPixels` before decoding.
- Changing the sizes later: new transforms get new digests; old variant objects stay until `Plugin.Prune`.

## For production

- Authenticate and authorize: the handlers trust `{id}` from the path. Check that the signed-in user is `{id}` (or may manage it) before they run.
- Put the avatars on an object store behind a CDN: an S3 or R2 bucket as the disk, `WithPublicURL("https://cdn.example.com")`, and drop `fshttp.Public` ([public CDN](public-cdn.md)). With `records.WithVisibilityDisks` the public files can live in their own bucket ([public and private](../concepts/public-and-private.md)).
- Set `records.WithObjectErrorHandler` to log or queue object deletions that failed after a commit.
- Image work is CPU-bound: a burst of uploads resizes in parallel. Limit concurrent uploads per instance (a semaphore in front of the handler) if your instances are small.
- The client should send already-reasonable sizes. Mobile apps can downscale before upload ([mobile uploads](mobile-uploads.md)), which also makes the upload faster.

## Related

- [Product images](product-images.md): many ordered images per owner, the gallery version of this recipe.
- [Strip photo metadata](strip-photo-metadata.md): Sanitize on every photo upload, not only avatars.
- [Attachments](../records/attachments.md): roles, single roles, `SetAttachment` and `Previous`.
- [Variants](../plugins/variants.md): declared sizes, lazy and eager generation, pruning.
- [Uploads](../concepts/uploads.md): policies, processors and inspectors.
