# Cover and banner images

A cover image is the wide picture across the top of a profile, a channel, a group, an event or a company page. It differs from an avatar in three ways that each cause a visible bug when ignored: it is shown at the full width of the screen, so a small upload is stretched into a blur; it is cropped to a fixed wide aspect ratio, so a subject near the edge is cut off; and it is the largest image on the page, so sending the desktop size to a phone wastes most of a megabyte. This recipe is for any app with one wide header image per owner.

The program is [examples/recipe-cover-images](../../examples/recipe-cover-images). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `PUT /channels/{id}/cover` takes a JPEG, PNG or WebP image up to 15 MiB, refuses one smaller than 1200 × 400, and re-encodes it, so no EXIF or GPS block is stored.
- One cover per channel, enforced by the database. Replacing it deletes the old one and its resized copies for good.
- Three widths of one 3:1 crop (600, 1200 and 1800 pixels), cut around the most detailed region of the image, generated right after the upload commits.
- `GET /channels/{id}/cover` answers a `srcset` the page drops into an `<img>`, so each screen downloads the width it needs.
- A width the source cannot fill is never invented: a 1500-pixel upload gives a 1500-pixel "1800" version, and the `srcset` says 1500w.

## Before you start

You need the examples module (or your own module requiring `github.com/ubgo/filesystem`, `github.com/ubgo/filesystem/records` and `github.com/ubgo/filesystem/contrib/imaging`), a SQL database (SQLite here), and a disk with public URLs (the local disk served by `fshttp.Public` here; a bucket behind a CDN in production). The [avatars](avatars.md) recipe builds the same skeleton for a square image; read it first if single roles and `SetAttachment` are new to you.

## The design

| Piece | Why |
|---|---|
| `imaging.Sanitize` as `Policy.Process` | Re-encodes the image, dropping EXIF and GPS. |
| `imaging.CheckDimensions` as `Policy.Inspect` | Refuses an image too small to fill the header, from its header alone, on every upload path. |
| `records.WithSingleRoles("cover")` and `SetAttachment(..., PreviousPurge)` | One cover per owner, the old one purged after the commit. |
| One `ModeFill` variant with `GravitySmart` | A fixed 3:1 crop that keeps the most detailed region instead of the centre. |
| `variants.WithDensities` | The 2x and 3x widths derived from the base, never enlarged past the source. |
| `Plugin.SrcSet` | A ready `srcset` with each version's real width. |

When to use which crop: `GravitySmart` suits user uploads where you do not know where the subject is. When the user positions the image in a cropper (as most social apps let them), store the point they chose and use `GravityFocal` with `variants.WithFocal` instead ([variants](../plugins/variants.md#focal-points-and-format-negotiation)). `GravityTop` suits portraits, where the head is at the top.

## Step 1: The policy

The policy says which files are accepted and how they are processed. The byte cap of the upload lives in `Sanitize`'s `MaxBytes`, because `Policy.MaxSize` applies to what `Process` returns.

<!-- ds:block id=coverpolicy-6xds275s -->
**coverPolicy** · [`examples/recipe-cover-images/main.go:80-86`](../../examples/recipe-cover-images/main.go#L80-L86)

```go
var coverPolicy = upload.Policy{
	MaxSize:    maxCoverBytes,
	Allowed:    []string{"image/jpeg", "image/png", "image/webp"},
	Key:        upload.RandomKey("covers"),
	Visibility: filesystem.VisibilityPublic,
	Process:    imaging.Sanitize(imaging.Options{MaxBytes: maxCoverBytes}),
}
```
<!-- /ds:block hash=d37c1e -->

The minimum size is an inspector. It runs after the upload is stored, reads only the image header through a range read, and refuses (and deletes) an image outside the bounds:

<!-- ds:block id=newpolicy-n9mrwk4d -->
**newPolicy** · [`examples/recipe-cover-images/main.go:92-100`](../../examples/recipe-cover-images/main.go#L92-L100)

```go
func newPolicy() (upload.Policy, error) {
	dims, err := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: minCoverWidth, MinHeight: minCoverHeight})
	if err != nil {
		return upload.Policy{}, err
	}
	p := coverPolicy
	p.Inspect = dims
	return p, nil
}
```
<!-- /ds:block hash=3a54dc -->

Why a minimum and not a maximum: a large upload is scaled down, which costs nothing in quality, while a small one would have to be enlarged to cover a desktop header. `Sanitize`'s `MaxPixels` (50 megapixels by default) already refuses a pixel bomb before decoding. If you prefer to accept small images, drop `MinWidth` and let the density versions stop at the source's size, as step 2 explains; the page then shows a smaller image or lets CSS scale it.

## Step 2: The widths

<!-- ds:block id=widths-xpg9ktvc -->
**widths** · [`examples/recipe-cover-images/main.go:111-113`](../../examples/recipe-cover-images/main.go#L111-L113)

```go
var widths = map[string]imaging.Transform{
	"cover": {Mode: imaging.ModeFill, Width: 600, Height: 200, Gravity: imaging.GravitySmart, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=aa9c57 -->

<!-- ds:block id=newvariants-tv3ar5nt -->
**newVariants** · [`examples/recipe-cover-images/main.go:124-131`](../../examples/recipe-cover-images/main.go#L124-L131)

```go
func newVariants() (*variants.Plugin, error) {
	logFailure := func(_ context.Context, f records.File, name string, err error) {
		fmt.Fprintf(os.Stderr, "variant %s of %s: %v\n", name, f.ID, err)
	}
	return variants.New(widths, imaging.Options{},
		variants.WithDensities("cover", 2, 3),
		variants.WithEager(logFailure, widthNames...))
}
```
<!-- /ds:block hash=eedc6a -->

`ModeFill` scales the image to cover 600 × 200 and crops the overflow; `GravitySmart` chooses the crop window with the most detail (edges, saturated colour and skin tones), so a logo near the left edge of a wide photo stays in. `WithDensities("cover", 2, 3)` declares `cover-2x` (1200 × 400) and `cover-3x` (1800 × 600) with the same crop. Each density version is capped at the source: a 1500-pixel upload gives a `cover-3x` of 1500 pixels, never an enlarged 1800. A separately declared 1800 × 600 `ModeFill` variant would enlarge it, because `ModeFill` always fills its box.

`WithEager` generates the three versions after the transaction commits, never inside it, so a slow resize holds no database connection. If generation fails, the error goes to the callback and the version is made on its first request instead.

## Step 3: Store and attach in one transaction

<!-- ds:block id=app-setcover-gcgfdwya -->
**App.setCover** · [`examples/recipe-cover-images/main.go:194-205`](../../examples/recipe-cover-images/main.go#L194-L205)

```go
func (a *App) setCover(ctx context.Context, channel string, body io.Reader, filename string) (records.File, error) {
	var stored records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if stored, err = a.Store.Upload(ctx, "", diskName, body, filename, a.policy); err != nil {
			return err
		}
		_, err = a.Store.SetAttachment(ctx, "", stored.ID, records.Owner{Type: ownerChannel, ID: channel}, roleCover, records.PreviousPurge)
		return err
	})
	return stored, err
}
```
<!-- /ds:block hash=649280 -->

What each failure leaves behind:

- The upload is refused (type, size, too small): nothing is stored, and the old cover stays.
- `SetAttachment` fails, or the process dies before the commit: the transaction rolls back, and the rollback hook `Store.Upload` registered deletes the new object.
- The commit succeeds: the old cover's row, attachment and variant rows are gone, and its objects are deleted after the commit. A failed object delete is reported to `records.WithObjectErrorHandler`.

The handler caps the body before reading the form, then answers with the `srcset`:

<!-- ds:block id=app-putcover-tdryamu3 -->
**App.putCover** · [`examples/recipe-cover-images/main.go:231-250`](../../examples/recipe-cover-images/main.go#L231-L250)

```go
func (a *App) putCover(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCoverBytes+formOverhead)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the image in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.setCover(r.Context(), r.PathValue("id"), file, header.Filename)
	if err != nil {
		writeError(w, err)
		return
	}
	resp, err := a.describe(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
```
<!-- /ds:block hash=a1a32d -->

## Step 4: The srcset

<!-- ds:block id=app-describe-8dymvrvv -->
**App.describe** · [`examples/recipe-cover-images/main.go:217-227`](../../examples/recipe-cover-images/main.go#L217-L227)

```go
func (a *App) describe(ctx context.Context, f records.File) (coverResponse, error) {
	srcset, candidates, err := a.Variants.SrcSet(ctx, a.Store, f.Scope, f.ID, linkLifetime, widthNames...)
	if err != nil {
		return coverResponse{}, err
	}
	urls := make(map[string]string, len(candidates))
	for _, c := range candidates {
		urls[c.Name] = c.URL
	}
	return coverResponse{FileID: f.ID, SrcSet: srcset, URLs: urls}, nil
}
```
<!-- /ds:block hash=12add3 -->

`SrcSet` generates any missing version, then lists them with `w` descriptors, smallest first, using each version's real width. Two versions of the same width (a 1200-pixel upload gives a `cover-2x` and a `cover-3x` of 1200 pixels each) are listed once, so the browser never downloads a larger file for the same pixels. The page renders it as:

```html
<img src="…/cover-….jpg" srcset="…600w, …1200w, …1800w" sizes="100vw" alt="Channel cover" width="1800" height="600">
```

`sizes="100vw"` tells the browser the image spans the viewport, so a 390-pixel phone at 3x picks the 1200 version and a 1440-pixel laptop at 2x the 1800 one. Set `width` and `height` to the aspect ratio so the page does not jump while the image loads.

## Try it

```sh
cd examples
go run ./recipe-cover-images
```

```text
listening on http://localhost:8121 data in /var/folders/sk/.../T/recipe-cover-images-1127932882
```

Upload a 2400 × 1000 banner:

```sh
curl -s -X PUT -F file=@banner.jpg localhost:8121/channels/c1/cover
```

```json
{"file_id":"fil_k5x5h322uw7ltaabho3jdhbty3","srcset":"http://localhost:8121/public/variants/fil_k5x5h322uw7ltaabho3jdhbty3/cover-30aec99daefb4439-sgrl3rlb4c3g5cbto7vqggftft.jpg 600w, http://localhost:8121/public/variants/fil_k5x5h322uw7ltaabho3jdhbty3/cover-2x-8b598a6860f79319-jjurjgcfdrlnujbog6bycqwh7k.jpg 1200w, http://localhost:8121/public/variants/fil_k5x5h322uw7ltaabho3jdhbty3/cover-3x-64bbaf7fee7dbbb5-w52vdipwfgkpb34wmxdo7e3yjj.jpg 1800w","urls":{"cover":"…","cover-2x":"…","cover-3x":"…"}}
```

Check: three widths in the `srcset`, which means `WithEager` generated them before the answer. Each URL is a public JPEG (the `cover-3x` one here):

```text
200 image/jpeg 116670
```

What is refused, and how:

```sh
curl -s -X PUT -F file=@small.jpg localhost:8121/channels/c1/cover -w '\n%{http_code}\n'
```

```text
{"error":"image is 800x300; it must be at least 1200 pixels wide","code":"rejected"}

415
```

```sh
printf '<html><script>alert(1)</script>' > evil.jpg
curl -s -X PUT -F file=@evil.jpg localhost:8121/channels/c1/cover -w '\n%{http_code}\n'
```

```text
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}

415
```

Delete it; the channel then has no cover:

```sh
curl -s -i -X DELETE localhost:8121/channels/c1/cover | head -1
curl -s localhost:8121/channels/c1/cover -w '\n%{http_code}\n'
```

```text
HTTP/1.1 204 No Content
{"error":"Not Found","code":"not_found"}

404
```

`go test ./recipe-cover-images` checks the same flow, plus a 1500 × 500 replacement whose `srcset` ends at 1500w and leaves exactly four objects (the cover and three versions) on the disk.

## Pitfalls

- **A separate variant per width enlarges small uploads.** `ModeFill` always fills its box, so a declared 1800 × 600 variant of a 1300-pixel upload is an upscaled blur. Declare one base and its densities, which stop at the source's size, or raise the minimum to the largest width.
- **The minimum is checked after the upload is stored.** An inspector reads the stored header, so a too-small upload costs one write and one delete. To refuse before storing, check dimensions in your own `Process` step, at the cost of decoding there.
- **Aspect ratio is not checked.** A 4000 × 400 panorama passes the minimum and is cropped to its most detailed 3:1 window. Add `Aspect` and `AspectTolerance` to `DimensionLimits` if the product should refuse such images instead.
- **Changing the crop or a width later** gives new variant keys (the transform's digest is in the key); the old objects stay until `Plugin.Prune` runs.
- **Public covers are public.** Anyone with the URL can fetch them, which is the point for a profile banner. A private group's cover belongs on a private disk, where `SrcSet` returns signed URLs valid for the lifetime you pass.

## For production

- Authenticate and authorize: the example trusts `{id}` from the path. Check the signed-in user may edit the channel before PUT and DELETE run.
- Serve the covers from a bucket behind a CDN: an S3 or R2 disk with `WithPublicURL("https://cdn.example.com")`, and drop `fshttp.Public` ([public CDN](public-cdn.md)). Variant keys change whenever the image or the transform does, so the CDN can cache them forever.
- Set `records.WithObjectErrorHandler` to log object deletes that fail after a commit.
- Let the user position the crop: store the focal point from your cropper and switch `GravitySmart` to `GravityFocal` ([variants](../plugins/variants.md#focal-points-and-format-negotiation)).

## Related

- [Avatars](avatars.md): the square, one-per-user version of this recipe, with a generated fallback.
- [Product images](product-images.md): many ordered images per owner.
- [Variants](../plugins/variants.md): densities, `SrcSet`, focal points, eager generation and pruning.
- [Dimensions](../plugins/dimensions.md): storing width and height, and placeholders while the image loads.
- [Attachments](../records/attachments.md): roles, single roles and `SetAttachment`.
