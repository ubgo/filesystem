# Image variants: thumbnails and cards

The variants plugin: named image sizes declared by the application (`thumb`, a 128×128 crop, and `card`, fitted inside 400×400), generated on first request or eagerly after an upload, served through a redirect handler, dropped when the original is replaced or changes visibility, pruned after a transform changes, and deleted with their file. A client can only ask for a declared name, never an arbitrary size. This is what a photo feed, a product grid or a profile page needs. It uses `github.com/ubgo/filesystem/records`, `contrib/imaging` and `contrib/imaging/variants` on SQLite over an in-memory disk. The guide [Image variants](../../docs/plugins/variants.md) explains each step.

## Run it

```sh
cd examples
go run ./records-variants
go test ./records-variants
```

It needs no network or environment variables. The photos are generated in memory, the SQLite database lives in a temporary directory that is deleted when the program exits, and the variant handler is called in-process, so no port is opened.

## What it does

```sh
go run ./records-variants
```

```text
uploaded 1200x800 photo; variant objects after upload: 1 (eager thumb)
card: 400x267 image/jpeg 13459 bytes at variants/…
GET thumb: 302 to https://app.example.com/files…
GET huge: 404
variant objects after replace: 1 (old ones dropped, eager thumb remade)
card after replace: 267x400
public thumb: https://cdn.example.com/variants/…
pruned after the card transform changed: 1
variant objects after purge: 0
focal point set: banner remade true
catch-up: generated 2, complete 0, failed 0
events: variants.ready [avatar,avatar-2x,avatar-3x,banner], variants.ready [avatar,avatar-2x,avatar-3x,banner], variants.ready [avatar,avatar-2x,avatar-3x,banner]
density srcset descriptors: 1x 2x 3x
GET banner (Accept "image/png,image/*;q=0.8"): 302 banner….png, Vary "Accept, Sec-CH-DPR, DPR"
GET banner (Accept "image/*"): 302 banner….jpg, Vary "Accept, Sec-CH-DPR, DPR"
GET avatar?dpr=2.5 (Accept ""): 302 avatar-3x….jpg, Vary "Accept, Sec-CH-DPR, DPR"
located thumb: disk thumbs, key under thumbs/acme/{file}/: true
variant objects: 0 on media, 1 on thumbs
thumb link: https://app.example.com/files/thumbs/thumbs/…
after purge: 0 variant objects on thumbs
proxy GET thumb: 200 image/jpeg, 4008 bytes, Location "", nosniff true, CSP set true, Cache-Control "public, max-age=86400"
proxy GET thumb (Range bytes=0-99): 206, 100 bytes, Content-Range bytes 0-99/4008, same bytes as the full body true
proxy GET thumb (If-None-Match its ETag): 304, 0 bytes
handler with mode "stream": 500
```

1. A 1200×800 photo is uploaded. `thumb` is eager, so it is made right after the upload's transaction commits: one variant object.
2. The first request for `card` generates it: 400×267, keeping the ratio, as JPEG.
3. The handler answers `GET /variants/{fileID}/thumb` with a redirect to a signed link, and an undeclared name (`huge`) with 404.
4. Replacing the photo with an 800×1200 one drops the old variants and remakes the eager thumb; the new card is 267×400.
5. Once the file is public, the thumb's link is a CDN URL.
6. Opening the same tables with a changed `card` transform and pruning removes the one variant made by the old transform.
7. Purging the file deletes its remaining variants.

The second half (`more.go`) runs on a database of its own with the focal plugin and a variants plugin that reads its points and declares a 48-pixel avatar with 2x and 3x versions (`WithDensities`). Nothing is eager:

8. A job worker calls `Generate` for the uploaded photo; setting a focal point drops its variants, and the banner, cropped around the new point, is made again under a new key.
9. A second photo is uploaded with no job. `GenerateMissing` makes the variants both photos lack (the first lost its avatars with the focal change).
10. Each run emits `variants.ready` with the names it made.
11. `DensitySrcSet` describes the avatar as `1x 2x 3x`.
12. A handler with `Negotiate` serves the banner as PNG to a request whose `Accept` names `image/png`, and as its declared JPEG to `image/*`. With `Density`, `?dpr=2.5` gets the 3x avatar. Every response says `Vary: Accept, Sec-CH-DPR, DPR`.
13. A third Store puts derived files on another disk (`location.go`): `variants.WithLocation` sends every variant to the `thumbs` disk under `thumbs/<scope>/<fileID>/`, while the original stays on `media`. The variant row keeps its disk, so the signed link names `thumbs` and the purge deletes the object there. A location naming a disk the Manager lacks fails the request with nothing written (`TestLocationOnUnknownDiskFails`).
14. A fourth Store serves variants with `variants.ModeProxy` (`proxy.go`): the handler streams the thumbnail itself instead of redirecting, through `fshttp.ServeEntry`, so the whole thumb comes back with 200, no `Location`, `X-Content-Type-Options: nosniff`, fshttp's sandbox Content-Security-Policy and the `CacheControl` the app chose. A resumed download asking for `Range: bytes=0-99` gets 206 with exactly those 100 bytes and `Content-Range: bytes 0-99/4008`; a cache revalidating with the variant's `ETag` gets 304 and no body. A handler given a mode it does not know (`"stream"`) answers 500, so a typo never serves the other way. The byte counts are those of this run's generated photo.

## The code

The only variants that exist, and the plugin that declares them with `thumb` eager (a failed eager variant is reported and made on first request instead):

<!-- ds:block id=transforms-n65mkxr9 -->
**transforms** · [`examples/records-variants/main.go:68-71`](main.go#L68-L71)

```go
var transforms = map[string]imaging.Transform{
	nameThumb: {Mode: imaging.ModeFill, Width: 128, Height: 128, Format: imaging.FormatJPEG},
	nameCard:  {Mode: imaging.ModeFit, Width: 400, Height: 400, Format: imaging.FormatJPEG, Quality: 80},
}
```
<!-- /ds:block hash=28ade7 -->

<!-- ds:block id=newplugin-k2uvchpd -->
**newPlugin** · [`examples/records-variants/main.go:110-112`](main.go#L110-L112)

```go
func newPlugin(t map[string]imaging.Transform, onEager func(context.Context, records.File, string, error)) (*variants.Plugin, error) {
	return variants.New(t, imaging.Options{}, variants.WithEager(onEager, nameThumb))
}
```
<!-- /ds:block hash=366b96 -->

<!-- ds:block id=logeager-dtsbqr6p -->
**logEager** · [`examples/records-variants/main.go:116-118`](main.go#L116-L118)

```go
func logEager(_ context.Context, f records.File, name string, err error) {
	log.Printf("variant %s of %s: %v", name, f.ID, err)
}
```
<!-- /ds:block hash=56c97c -->

Installing the plugin; `Migrate` creates `fs_variants`:

<!-- ds:block id=newstore-5u24ynfu -->
**newStore** · [`examples/records-variants/main.go:123-132`](main.go#L123-L132)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, p *variants.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=86cc0d -->

Uploading, and fetching a variant on demand:

<!-- ds:block id=uploadphoto-vphdmbu3 -->
**uploadPhoto** · [`examples/records-variants/main.go:150-156`](main.go#L150-L156)

```go
func uploadPhoto(ctx context.Context, s *records.Store, w, h int) (records.File, error) {
	data, err := makePNG(w, h)
	if err != nil {
		return records.File{}, err
	}
	return s.Upload(ctx, tenant, diskName, bytes.NewReader(data), "beach.png", photoPolicy)
}
```
<!-- /ds:block hash=ac5f23 -->

<!-- ds:block id=card-ky3dmcrb -->
**card** · [`examples/records-variants/main.go:160-162`](main.go#L160-L162)

```go
func card(ctx context.Context, s *records.Store, p *variants.Plugin, fileID string) (variants.Variant, error) {
	return p.Get(ctx, s, tenant, fileID, nameCard)
}
```
<!-- /ds:block hash=93b8a9 -->

The redirect handler, with the tenant it may read (from the session in a real app, never from the URL):

<!-- ds:block id=scopeof-wrkqp89b -->
**scopeOf** · [`examples/records-variants/main.go:167`](main.go#L167-L167)

```go
func scopeOf(*http.Request) (string, error) { return tenant, nil }
```
<!-- /ds:block hash=f6a5fd -->

<!-- ds:block id=newhandler-7bqv9349 -->
**newHandler** · [`examples/records-variants/main.go:172-177`](main.go#L172-L177)

```go
func newHandler(s *records.Store, p *variants.Plugin) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/variants/", http.StripPrefix("/variants/",
		variants.Handler(s, p, scopeOf, variants.HandlerOptions{LinkLifetime: linkTTL})))
	return mux
}
```
<!-- /ds:block hash=38cbde -->

Replacement and pruning after a transform changes:

<!-- ds:block id=replacephoto-nnpjgrgs -->
**replacePhoto** · [`examples/records-variants/main.go:205-212`](main.go#L205-L212)

```go
func replacePhoto(ctx context.Context, s *records.Store, fileID string, w, h int) error {
	data, err := makePNG(w, h)
	if err != nil {
		return err
	}
	_, err = s.Replace(ctx, tenant, fileID, bytes.NewReader(data), "", photoPolicy)
	return err
}
```
<!-- /ds:block hash=1c61d2 -->

<!-- ds:block id=prunechanged-224hztec -->
**pruneChanged** · [`examples/records-variants/main.go:217-231`](main.go#L217-L231)

```go
func pruneChanged(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (int, error) {
	changed := map[string]imaging.Transform{
		nameThumb: transforms[nameThumb],
		nameCard:  {Mode: imaging.ModeFit, Width: 600, Height: 600, Format: imaging.FormatJPEG},
	}
	p, err := newPlugin(changed, logEager)
	if err != nil {
		return 0, err
	}
	s, err := newStore(ctx, db, disks, p)
	if err != nil {
		return 0, err
	}
	return p.Prune(ctx, s)
}
```
<!-- /ds:block hash=312aaf -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: transforms, the plugin, the handler, upload, replace, publish, prune and purge |
| `more.go` | Focal points, a job worker, `GenerateMissing`, the ready events, densities and format negotiation |
| `location.go` | Variants on another disk with `WithLocation`; code: [`newLocatedPlugin`](ds:block?id=newlocatedplugin-p9w2k6xe) |
| `proxy.go` | Streaming variants with `ModeProxy`: ranges, conditional requests and the security headers; code: [`newProxyHandler`](ds:block?id=newproxyhandler-z6k2w8mr), [`proxied`](ds:block?id=proxied-j3t9n5xa) |
| `main_test.go` | Runs the same steps, plus caching of a generated variant, a file that is not an image, undeclared names refused by the handler, no new variants for a trashed file, a location on an unknown disk, and the proxy's ranges and 304 |

## Next

- The guide: [Image variants](../../docs/plugins/variants.md).
- Related examples: [records-dimensions](../records-dimensions) for the original's size, [recipe-product-images](../recipe-product-images) and [recipe-avatars](../recipe-avatars) for variants in an app, [mediaserver](../mediaserver).
