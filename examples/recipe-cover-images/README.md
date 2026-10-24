# Cover images: one wide banner per owner, served as a srcset

The cover or banner image of a profile, channel, group or event page. A channel uploads one wide image; the server refuses it when it is smaller than 1200 × 400 (it would be stretched into a blur across a desktop header), re-encodes it without EXIF, keeps one cover per channel (replacing purges the old one and its resized copies), and makes a 3:1 crop around the image's most detailed region at 600, 1200 and 1800 pixels wide, never enlarged past the source. `GET` answers a ready `srcset`. It uses `records` with a single role, `contrib/imaging` (`Sanitize`, `CheckDimensions`) and the `variants` plugin with `WithDensities` and `SrcSet`. The guide is [Cover and banner images](../../docs/recipes/cover-images.md).

## Run it

```sh
cd examples
go run ./recipe-cover-images
```

It creates a temporary directory (printed at start-up) holding `app.db` (SQLite) and `files/`, and listens on `http://localhost:8121`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Run the tests with `go test ./recipe-cover-images`.

Routes:

| Route | What it does |
|---|---|
| `PUT /channels/{id}/cover` | multipart form, field `file`: stores and attaches the cover, answers its srcset |
| `GET /channels/{id}/cover` | the srcset and the URL of each width |
| `DELETE /channels/{id}/cover` | purges the cover and its variants |
| `GET /public/…` | the public files, served by `fshttp.Public` |

## Walkthrough

1. Upload a 2400 × 1000 banner. The answer lists three widths, generated before it was sent:

```sh
curl -s -X PUT -F file=@banner.jpg localhost:8121/channels/c1/cover
```

```json
{"file_id":"fil_k5x5h322uw7ltaabho3jdhbty3","srcset":"http://localhost:8121/public/variants/fil_k5x5h322uw7ltaabho3jdhbty3/cover-30aec99daefb4439-sgrl3rlb4c3g5cbto7vqggftft.jpg 600w, http://localhost:8121/public/variants/fil_k5x5h322uw7ltaabho3jdhbty3/cover-2x-8b598a6860f79319-jjurjgcfdrlnujbog6bycqwh7k.jpg 1200w, http://localhost:8121/public/variants/fil_k5x5h322uw7ltaabho3jdhbty3/cover-3x-64bbaf7fee7dbbb5-w52vdipwfgkpb34wmxdo7e3yjj.jpg 1800w","urls":{"cover":"…","cover-2x":"…","cover-3x":"…"}}
```

2. An 800 × 300 image and an HTML file named `.jpg` are refused, and nothing is stored:

```text
{"error":"image is 800x300; it must be at least 1200 pixels wide","code":"rejected"}
415
{"error":"content type \"text/html; charset=utf-8\" is not allowed","code":"rejected"}
415
```

3. `DELETE` answers `204`, and the channel then has no cover (`404`, `not_found`).

The test also replaces the cover with a 1500 × 500 image: its `srcset` ends at `1500w`, because a density version never enlarges the source, and the disk holds exactly four objects (the cover and three versions), the first cover's having been purged.

## The code

The policy, and the minimum size added as an inspector:

<!-- ds:block id=coverpolicy-6xds275s -->
**coverPolicy** · [`examples/recipe-cover-images/main.go:80-86`](main.go#L80-L86)

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

<!-- ds:block id=newpolicy-n9mrwk4d -->
**newPolicy** · [`examples/recipe-cover-images/main.go:92-100`](main.go#L92-L100)

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

One base width and its densities:

<!-- ds:block id=widths-xpg9ktvc -->
**widths** · [`examples/recipe-cover-images/main.go:111-113`](main.go#L111-L113)

```go
var widths = map[string]imaging.Transform{
	"cover": {Mode: imaging.ModeFill, Width: 600, Height: 200, Gravity: imaging.GravitySmart, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=aa9c57 -->

<!-- ds:block id=newvariants-tv3ar5nt -->
**newVariants** · [`examples/recipe-cover-images/main.go:124-131`](main.go#L124-L131)

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

Store and attach in one transaction, purging the previous cover after the commit:

<!-- ds:block id=app-setcover-gcgfdwya -->
**App.setCover** · [`examples/recipe-cover-images/main.go:194-205`](main.go#L194-L205)

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

The srcset:

<!-- ds:block id=app-describe-8dymvrvv -->
**App.describe** · [`examples/recipe-cover-images/main.go:217-227`](main.go#L217-L227)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy and dimension check, the variants, the store, the three routes and the server. |
| `main_test.go` | Upload, the srcset and a fetched variant's real size, a smaller replacement that purges the first cover, delete, and the refusals (too small, not an image). |

## Next

- The guide: [Cover and banner images](../../docs/recipes/cover-images.md).
- [recipe-avatars](../recipe-avatars): the square, one-per-user version.
- [recipe-product-images](../recipe-product-images): many ordered images per owner.
- [records-variants](../records-variants): every variants option.
