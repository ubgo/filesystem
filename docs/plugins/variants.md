# Image variants

You will serve resized copies of uploaded images (a square thumbnail for a list, a card-sized image for a feed) under names your application declares, generated once and then served from storage, regenerated when the original changes, and deleted with it. Use it for avatars, product photos, galleries and chat images; skip it if a CDN image service resizes for you.

## Before you start

- A [records Store](../records/README.md). The plugin works on recorded files (`records.File`), not on raw keys.
- The plugin lives in the `contrib/imaging` module, which brings the image decoders: `go get github.com/ubgo/filesystem/contrib/imaging`. It decodes JPEG, PNG, GIF, WebP, BMP and TIFF, and writes JPEG or PNG.
- The example is [`examples/records-variants`](../../examples/records-variants): a SQLite Store over a memory disk with two declared variants. `go run ./records-variants` in `examples/` prints every step below; `go test ./records-variants` runs them.
- Only declared names exist. A client cannot ask for `?w=5000`, so there is no open transform endpoint to abuse for denial of service.

## Step 1 — Declare the variants

A variant is a name and an `imaging.Transform`: a mode (`ModeFit` keeps the whole image inside the box, `ModeFill` covers the box and crops, `ModePad` fits and pads), a size up to 8192 pixels, a format (`FormatJPEG`, `FormatPNG`), a JPEG quality, a gravity for `ModeFill` and an optional crop region. Names match `[a-z0-9][a-z0-9_-]{0,63}`; they appear in storage keys and URLs.

<!-- ds:block id=transforms-n65mkxr9 -->
**transforms** · [`examples/records-variants/main.go:68-71`](../../examples/records-variants/main.go#L68-L71)

```go
var transforms = map[string]imaging.Transform{
	nameThumb: {Mode: imaging.ModeFill, Width: 128, Height: 128, Format: imaging.FormatJPEG},
	nameCard:  {Mode: imaging.ModeFit, Width: 400, Height: 400, Format: imaging.FormatJPEG, Quality: 80},
}
```
<!-- /ds:block hash=28ade7 -->

`variants.New(transforms, opts, options...)` checks every name and transform, so a mistake fails at start-up. `imaging.Options` bound what is decoded: zero means 50 megapixels and 50 MiB, so a decompression bomb is refused rather than allocated. `WithEager(onError, names...)` generates the named variants right after an image is recorded, replaced, restored from the trash or changes visibility, so the first visitor does not wait for the resize; it runs after the transaction commits, so a slow resize never holds the database. A name it lists that is not declared is an error.

`WithProcessor(proc)` makes the variants with another [processor](image-processing.md): a Pipeline with the WebP or AVIF codec module for those output formats, or another backend. A declared transform the processor cannot write is refused by `New` with `unsupported`.

`WithHeaders(h, names...)` stores HTTP headers on the named variants (every declared one when no name is given), served by the disk, by `fshttp` and by a CDN in front of a public disk. A common value is `filesystem.HTTPHeaders{CacheControl: "public, max-age=31536000, immutable"}`. Two limits come with a long cache. A cache never sees a key change hands, because no key is ever written twice (see step 3), so a cached copy never shows a different picture. But a copy a browser or CDN already holds keeps being served until its entry expires, after the file is made private, trashed or purged: the plugin deletes the object, not the cached copies. `immutable` with a year-long `max-age` is therefore right only for variants of files that stay public (product photos, published avatars). For files that can be made private or deleted, use a short `max-age`, purge your CDN on `file.visibility_changed`, `file.trashed` and `file.purged` (an `records.EventHooks` plugin of your own), or serve them private and signed. Variants never inherit the original's headers, so an original stored with `Content-Disposition: attachment` still has thumbnails that display inline. The disk must have the `Headers` capability (memory, S3, GCS and Azure do; local and SFTP do not): on a disk without it every generation of those variants fails with `unsupported`, because `Disk.Write` refuses headers it cannot store rather than dropping them, so declare headers only for disks that keep them.

<!-- ds:block id=newplugin-k2uvchpd -->
**newPlugin** · [`examples/records-variants/main.go:110-112`](../../examples/records-variants/main.go#L110-L112)

```go
func newPlugin(t map[string]imaging.Transform, onEager func(context.Context, records.File, string, error)) (*variants.Plugin, error) {
	return variants.New(t, imaging.Options{}, variants.WithEager(onEager, nameThumb))
}
```
<!-- /ds:block hash=366b96 -->

<!-- ds:block id=logeager-dtsbqr6p -->
**logEager** · [`examples/records-variants/main.go:116-118`](../../examples/records-variants/main.go#L116-L118)

```go
func logEager(_ context.Context, f records.File, name string, err error) {
	log.Printf("variant %s of %s: %v", name, f.ID, err)
}
```
<!-- /ds:block hash=56c97c -->

Install it like any plugin; `Migrate` creates `fs_variants`:

<!-- ds:block id=newstore-5u24ynfu -->
**newStore** · [`examples/records-variants/main.go:123-132`](../../examples/records-variants/main.go#L123-L132)

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

Check: `Migrate` returns `variants/0001_variants` among the migrations it applied.

### Variants on another disk

By default a variant is written on its file's disk under `variants/<fileID>/`. `WithLocation(fn)` puts it elsewhere: thumbnails on a public CDN disk while the originals stay private, a cheaper bucket or storage class, or a path your CDN rule matches. `fn` gets the file and the variant's name and returns a disk name of the Store's Manager (`""` for the file's own) and a key prefix; the plugin appends `<name>-<digest>-<id><ext>`, so every generation still writes a key never written before:

```go
p, err := variants.New(transforms, imaging.Options{},
	variants.WithLocation(func(f records.File, name string) (string, string, error) {
		return "cdn", "thumbs/" + f.Scope + "/" + f.ID + "/", nil
	}))
```

The disk is stored on the variant's row, so serving, `Prune`, the purge and `SweepOrphans` find the object there. An error from `fn` fails the request with nothing written, and an eager run marks the file `failed`.

Try it: [`examples/records-variants`](../../examples/records-variants) runs this section in `location.go` ([`thumbsLocation`](ds:block?id=thumbslocation-v3n8c5qa)): `go run ./records-variants` from `examples/`.

## Step 2 — Upload, and let the eager variant appear

<!-- ds:block id=uploadphoto-vphdmbu3 -->
**uploadPhoto** · [`examples/records-variants/main.go:150-156`](../../examples/records-variants/main.go#L150-L156)

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

Check: the program prints `uploaded 1200x800 photo; variant objects after upload: 1 (eager thumb)`. The thumbnail exists before anyone asked for it. Files that are not images (by their recorded content type) are skipped by the eager pass.

## Step 3 — Get a variant

`Get(ctx, s, scope, fileID, name)` returns the variant, generating it on first request: it reads the original, applies the transform, writes the result next to the original on the same disk under `variants/<fileID>/<name>-<digest>-<id><ext>`, and records a row. `<digest>` identifies the transform and `<id>` is the random part of the variant's own ID, so every generation writes a key no other generation ever writes: after a replacement, after a visibility change, even when `ReplaceObject` returns the file to an object key it had before. That is what keeps deferred deletions safe (a drop deletes, after its commit, only the objects of the rows it dropped, so a variant generated in between is never lost) and why one key never shows two different pictures. Later calls return the row without touching the image. Two concurrent first requests may both generate; one row wins and the other returns it. A view-once or expired file has no variants through `Get`, `URL`, `SrcSet` or `DensitySrcSet` (`not_found`), because the returned key could be streamed without spending the view.

The generation runs outside any transaction (a resize must not hold the database), so the file can change while it works. The row is therefore recorded in a short transaction that first locks the file's row and reads it again: if the file was trashed or purged meanwhile, `Get` fails with `not_found` and deletes the object it wrote, so no row outlives the trash and no thumbnail stays reachable; if it was replaced or made public or private, the variant is stale, so `Get` deletes it and generates again from the file as it now is, up to three attempts in all (`unavailable` when the file keeps changing). A `Trash`, `Replace` or `SetVisibility` that starts after the lock waits for it and then drops the new row with the others. A stale or losing attempt deletes the object it wrote; no row can name it, since its key is its own.

<!-- ds:block id=card-ky3dmcrb -->
**card** · [`examples/records-variants/main.go:160-162`](../../examples/records-variants/main.go#L160-L162)

```go
func card(ctx context.Context, s *records.Store, p *variants.Plugin, fileID string) (variants.Variant, error) {
	return p.Get(ctx, s, tenant, fileID, nameCard)
}
```
<!-- /ds:block hash=93b8a9 -->

Check: `card: 400x267 image/jpeg 13459 bytes at variants/…`. The 1200×800 photo fits inside 400×400 keeping its ratio.

`URL(ctx, s, scope, fileID, name, ttl)` does the same and returns a link: the public URL when the file is public and the disk has one, otherwise a signed GET valid for `ttl`. A variant takes its file's visibility when it is written, on disks that support per-object visibility.

## Step 4 — Serve variants over HTTP

`variants.Handler(s, p, scope, opts)` serves `GET /{fileID}/{name}` as a `302` to the variant's URL, generating it first if needed. The scope comes from your `ScopeFunc` (the signed-in tenant), never from the URL; an error answers `403`. An undeclared name is `404`. The redirect carries `Cache-Control: private, max-age=60`, and generation continues if the client disconnects, so the work is kept for the next request.

<!-- ds:block id=scopeof-wrkqp89b -->
**scopeOf** · [`examples/records-variants/main.go:167`](../../examples/records-variants/main.go#L167-L167)

```go
func scopeOf(*http.Request) (string, error) { return tenant, nil }
```
<!-- /ds:block hash=f6a5fd -->

<!-- ds:block id=newhandler-7bqv9349 -->
**newHandler** · [`examples/records-variants/main.go:172-177`](../../examples/records-variants/main.go#L172-L177)

```go
func newHandler(s *records.Store, p *variants.Plugin) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/variants/", http.StripPrefix("/variants/",
		variants.Handler(s, p, scopeOf, variants.HandlerOptions{LinkLifetime: linkTTL})))
	return mux
}
```
<!-- /ds:block hash=38cbde -->

Check: the program prints `GET thumb: 302 to https://app.example.com/files…` and `GET huge: 404`.

Use it as `<img src="/variants/fil_…/thumb">`. `HandlerOptions.LinkLifetime` sets the lifetime of the signed URL it redirects to, `variants.DefaultLinkLifetime` (10 minutes) when zero.

### Proxy instead of redirect

By default the handler answers `302` to the variant's signed or public URL. Behind a CDN, or to keep the bucket unreachable, stream the bytes instead:

```go
h := variants.Handler(store, plugin, scopeOf, variants.HandlerOptions{
	Mode:         variants.ModeProxy,
	CacheControl: "public, max-age=86400", // only when every file served is public
})
```

A proxied response goes through `fshttp.ServeEntry`, the serve path of fshttp's own handlers, so it answers `Range` with 206 (a video player or a resumed download fetches only the bytes it needs), `If-None-Match` and `If-Modified-Since` with 304 from the variant's `ETag` and modification time, and carries `X-Content-Type-Options: nosniff` and fshttp's sandbox Content-Security-Policy. A plain GET still gets the whole variant with 200. The read is reported with `Store.Served` before a byte is written, so egress metering sees it; a 304 or a range is reported at the variant's full size, an upper bound. `CacheControl` applies in both modes and defaults to `private, max-age=60`; the handler takes the scope from the request while a shared cache keys by URL alone, so a public value is safe only for public files. An unknown `Mode` answers 500.

Two more options fit the handler into an application that already has conventions. `Errors` takes the same `fshttp.ErrorWriter` as fshttp's and upload's handlers, so a 405, a 404 for a path naming no variant, a 403 from your `ScopeFunc` and every store or disk failure answer in your envelope rather than plain text; nil keeps the plain status text. `Sendfile`, in proxy mode, hands the variant's body to nginx (`X-Accel-Redirect`) or Apache (`X-Sendfile`) once the handler has checked the request and set every header, as `fshttp.Public` does; nil streams the bytes, and redirect mode ignores it.

```go
h := variants.Handler(store, plugin, scopeOf, variants.HandlerOptions{
	Mode: variants.ModeProxy,
	Errors: func(w http.ResponseWriter, _ *http.Request, status int, err error) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "title": fshttp.ClientMessage(err)})
	},
	Sendfile: &fshttp.Sendfile{Path: func(disk, key string) (string, bool) {
		return "/protected/" + disk + "/" + key, disk == "local" // only the disk nginx can read
	}},
})
```

Try it: [`examples/records-variants`](../../examples/records-variants) runs this section in `proxy.go` ([`newProxyHandler`](ds:block?id=newproxyhandler-z6k2w8mr), [`proxied`](ds:block?id=proxied-j3t9n5xa)): `go run ./records-variants` from `examples/`, then look for `proxy GET thumb (Range bytes=0-99): 206, 100 bytes, Content-Range bytes 0-99/4008, same bytes as the full body true`.

## Processing status and events

Eager generation reports its progress through the file's processing status (`records.File.Status`, served by `GET /files/{id}/status` in recordshttp):

| When | Status | Detail |
|---|---|---|
| in the transaction that records, replaces, restores or changes the visibility of an image, with `WithEager` | `processing` | `variants 0/N` |
| after the variants are made | `ready` | empty |
| after one could not be made | `failed` | `<variant>: <code>`, such as `square: unavailable` |

The status is written in the change's own transaction, so a client polling the file never sees it `ready` before its eager variants exist. The detail carries the error code, never the message, because clients read it. Only images are marked; a file of another type keeps its status.

With the final status, in the same transaction, the plugin emits one of two events through `records.EventHooks`, with `Event.File` set to the file:

| Event | When | `Event.Detail` |
|---|---|---|
| `variants.ready` (`EventReady`) | every requested variant exists | `variants`: the names, comma separated |
| `variants.failed` (`EventFailed`) | at least one failed | `variants`: those made; `failed`: the first that failed; `code`: its error code |

The `WithEager` handler still hears every failure, with the full error, for logging. To push the result to a client (a websocket, a notification), listen for the two events in an `EventHooks` plugin; the second half of [`examples/records-variants`](../../examples/records-variants) (`more.go`) records them and prints `events: variants.ready [avatar,avatar-2x,avatar-3x,banner], …`, one per generation run.

The processing status is one column per file. When another plugin also uses it (a video transcoder), the last writer wins. The variants plugin only ever clears a `processing` status whose detail starts with `variants`.

## Generating from a job

`Generate(ctx, s, scope, fileID, names...)` makes the named variants of one file now (every declared variant when no name is given), with the same status and events as eager generation. Call it from a job runner instead of `WithEager` when an upload request must not wait for resizing:

<!-- ds:block id=variantjob-k2n7w4tr -->
**variantJob** · [`examples/records-variants/more.go:65-67`](../../examples/records-variants/more.go#L65-L67)

```go
func variantJob(ctx context.Context, s *records.Store, vp *variants.Plugin, scope, fileID string) error {
	return vp.Generate(ctx, s, scope, fileID)
}
```
<!-- /ds:block hash=c36f1b -->

- **Per upload.** Enqueue one job per recorded image (a River or Asynq job whose arguments are the scope and file ID) and call `Generate` in its worker.
- **Retries.** It returns the first variant's error, so the queue retries the run. Variants that already exist are found rather than made again, so a retry finishes what a failed run left.
- **Nothing to do.** An unknown or trashed file is `not_found`; a file that is not an image returns nil and changes nothing.

`GenerateMissing(ctx, s, MissingOptions{Scope, Names, Cursor, MaxFiles, PageSize})` is the scheduled companion:

<!-- ds:block id=catchup-p4c8m3vh -->
**catchUp** · [`examples/records-variants/more.go:73-75`](../../examples/records-variants/more.go#L73-L75)

```go
func catchUp(ctx context.Context, s *records.Store, vp *variants.Plugin, maxFiles int) (variants.MissingResult, error) {
	return vp.GenerateMissing(ctx, s, variants.MissingOptions{Scope: tenant, MaxFiles: maxFiles})
}
```
<!-- /ds:block hash=ef0e71 -->

Check: the example uploads a second photo with no job and prints `catch-up: generated 2, complete 0, failed 0`. The first photo counts too, because the focal change below dropped its variants and only its banner was requested since.

 It walks a scope's images newest first and runs `Generate` for every one that lacks a named variant. It catches uploads whose job was lost (a crash, a full queue) and makes a newly declared variant for older files. Files that already have every variant are only looked up. `MaxFiles` bounds one run, and `NextCursor` resumes it. A failing file is listed in `Failed` and the run carries on. Schedule it with `jobs.Func` (see [Maintenance jobs](../concepts/maintenance-jobs.md#image-variants-from-a-job)).

## Focal points and format negotiation

**Focal points.** A variant declared with `Gravity: imaging.GravityFocal` crops around its file's focal point. `variants.WithFocal(src)` names where the points come from: a `FocalSource` with `Focal(f records.File)`, the point, and `FocalChanged()`, the event kind that announces a change. The `contrib/imaging/focal` plugin is one source; an application's face detector can be another.

The example declares a banner cropped around the point and an avatar with densities:

<!-- ds:block id=moretransforms-c5w9k2nq -->
**moreTransforms** · [`examples/records-variants/more.go:28-31`](../../examples/records-variants/more.go#L28-L31)

```go
var moreTransforms = map[string]imaging.Transform{
	nameBanner: {Mode: imaging.ModeFill, Width: 120, Height: 40, Format: imaging.FormatJPEG, Gravity: imaging.GravityFocal},
	nameAvatar: {Mode: imaging.ModeFill, Width: 48, Height: 48, Format: imaging.FormatJPEG},
}
```
<!-- /ds:block hash=c95b03 -->

and installs the focal plugin before the variants plugin that reads it:

<!-- ds:block id=newmoreplugins-h8t3v6mc -->
**newMorePlugins** · [`examples/records-variants/more.go:51-58`](../../examples/records-variants/more.go#L51-L58)

```go
func newMorePlugins() (*focal.Plugin, *variants.Plugin, error) {
	fp := focal.New()
	vp, err := variants.New(moreTransforms, imaging.Options{},
		variants.WithFocal(fp),
		variants.WithDensities(nameAvatar),
	)
	return fp, vp, err
}
```
<!-- /ds:block hash=dfeabc -->

`fp.Set(ctx, store, scope, fileID, &imaging.Focal{X: 0.9, Y: 0.5})` moves the point to the right edge. Check: `focal point set: banner remade true`: the banner after the change has a new key.

`focal.Set` stores the point in the file's metadata (`focal_x`, `focal_y`, also settable through `NewFile.Metadata` at upload). It locks the file's row, and emits `focal.changed` with the file in the same transaction; on that event the variants plugin drops the file's variants, so they are made again around the new point. A variant being generated while the point changes is made again rather than recorded. The point is per file, so it is not part of the variant's digest. Without a point a `GravityFocal` variant is centred.

**Format negotiation.** `HandlerOptions{Negotiate: []imaging.Format{imaging.FormatAVIF, imaging.FormatWebP}}` makes the handler serve a variant in the first listed format that the request's `Accept` header names explicitly with a non-zero quality, and in its declared format otherwise. Wildcards do not count: every browser sends `image/*`, and only one that names `image/webp` can display it. Only formats the plugin's processor can be shown to write (it has an `Encodes` method, as `*imaging.Pipeline` does) stay in the list; a processor that cannot say gets none, so a request never fails for a format it cannot write. Each negotiated format is a sibling variant, generated on its first request like any variant and kept by `Prune`. Every response then carries `Vary: Accept`, so a cache keeps one per format.

<!-- ds:block id=newnegotiatinghandler-w6r2t9qk -->
**newNegotiatingHandler** · [`examples/records-variants/more.go:82-88`](../../examples/records-variants/more.go#L82-L88)

```go
func newNegotiatingHandler(s *records.Store, vp *variants.Plugin) http.Handler {
	return variants.Handler(s, vp, scopeOf, variants.HandlerOptions{
		LinkLifetime: linkTTL,
		Negotiate:    []imaging.Format{imaging.FormatPNG},
		Density:      true,
	})
}
```
<!-- /ds:block hash=709403 -->

The example negotiates PNG so it runs with the built-in codecs; in production list `imaging.FormatAVIF, imaging.FormatWebP` and give the plugin a processor with their codec modules (`variants.WithProcessor`, see [imaging-codecs](../../examples/imaging-codecs)). Check:

```text
GET banner (Accept "image/png,image/*;q=0.8"): 302 banner….png, Vary "Accept, Sec-CH-DPR, DPR"
GET banner (Accept "image/*"): 302 banner….jpg, Vary "Accept, Sec-CH-DPR, DPR"
GET avatar?dpr=2.5 (Accept ""): 302 avatar-3x….jpg, Vary "Accept, Sec-CH-DPR, DPR"
```

The wildcard alone gets the declared JPEG. The same handler has `Density: true`, so a device pixel ratio of 2.5 is rounded up to the 3x avatar (see [device densities](#device-densities-1x-2x-3x)).

### A format of your own

A transform may name a format outside `imaging.FormatValues` when the plugin's processor encodes it: register an `imaging.DescribedEncoder` on a Pipeline and pass it with `WithProcessor`. `New` checks every transform with `ValidateFor` once the processor is known, the stored key ends in the encoder's extension, `Accept` negotiation matches its content type, and `Prune` keeps those variants. Without the encoder, `New` refuses the transform.

With `jxlEncoder` from [image processing](image-processing.md#formats-outside-the-built-in-set):

```go
proc := imaging.NewPipeline(imaging.WithEncoder(jxlEncoder{}))
plugin, err := variants.New(map[string]imaging.Transform{
	"hero": {Mode: imaging.ModeFit, Width: 1600, Height: 1600, Format: "jxl"},
}, imaging.Options{}, variants.WithProcessor(proc))
```

## Responsive images: srcset

`SrcSet(ctx, s, scope, fileID, ttl, names...)` returns the value of an `<img srcset>` attribute for the named variants, such as `https://cdn.example.com/variants/…/w320-….jpg 320w, https://cdn.example.com/variants/…/w640-….jpg 640w`, and the `[]Candidate{Name, URL, Width}` it was built from. Each variant is generated if needed, exactly as `Get` would, and linked as `URL` links it: public for a public file on a disk with public URLs, otherwise signed for `ttl`.

Declare a family of one aspect ratio and growing widths for it (`w320`, `w640`, `w1280` with `ModeFit` and a generous height): the browser chooses by width alone, using the `sizes` attribute you write next to it.

- **Real widths.** The `w` descriptors are the variants' real widths, smallest first. `ModeFit` never enlarges, so for a 900-pixel original `w1280` is 900 pixels wide and is described as `900w`.
- **No duplicates.** Two variants that come out the same width give one candidate, the first named, so a small original never makes the browser download a bigger file for the same pixels.
- **All or nothing.** Passing no names is `invalid_argument`. Any variant that cannot be generated or linked fails the whole call, so a page never gets a srcset that silently lacks a size.

## Device densities: 1x, 2x, 3x

A fixed-size image (an avatar, an icon, a product tile) is shown at the same CSS size on every screen, but a phone with a 3x display needs three times the pixels to look sharp. Declare the variant once at its 1x size and let `WithDensities` derive the others:

```go
transforms := map[string]imaging.Transform{
	"avatar": {Mode: imaging.ModeFill, Width: 48, Height: 48, Format: imaging.FormatJPEG},
}
vp, err := variants.New(transforms, imaging.Options{},
	variants.WithDensities("avatar"),                 // avatar-2x, avatar-3x (iOS @2x/@3x)
	variants.WithEager(onEager, "avatar", "avatar-2x"), // after WithDensities, which declares the names
)
```

- **Names.** Each factor becomes an ordinary declared variant named `variants.DensityName(base, factor)`: `avatar-2x`, `avatar-3x`, and `avatar-1_5x` for a fractional factor (names have no dot). The base itself is the 1x. No factors means 2 and 3; pass a list for Android's buckets, such as `WithDensities("avatar", 1.5, 2, 3, 4)` for hdpi to xxxhdpi. Factors run from 1 to `variants.MaxDensity` (4). The transform of each version is the base's with `Width` and `Height` multiplied and rounded down.
- **Everything else is a variant.** A density version is generated on first request, stored under `variants/<fileID>/avatar-2x-<digest>-<id>.jpg`, served by `Get`, `URL` and the handler, kept by `Prune`, made by `Generate`, and can be eager. An iOS or Android app can request `/variants/<fileID>/avatar-3x` directly.
- **Never upscaled.** A version is never made from pixels the source does not have. When the original (or the transform's `Crop`) is too small for a factor, the version is made at the largest factor the source fills, and never below 1x. For the 48-pixel avatar above, a 120-pixel photo gives a 96-pixel 2x and a 120-pixel "3x" (2.5x); a 40-pixel photo gives a 2x and a 3x that are the 1x size. The versions still exist, so a request for `avatar-3x` never fails because the photo is small. The cap reads only the source's header (`Processor.Probe`). `ModeFill` is capped by the smaller of the source's two ratios to the box; `ModeFit` and `ModePad` by the larger, past which their image stops growing (`ModePad` would otherwise pad a small image onto a bigger canvas, which the screen then shrinks). The 1x itself behaves as any variant: `ModeFill` enlarges a source smaller than its box.
- **Headers.** A density version takes its base's `WithHeaders`, whichever option comes first, unless headers are given for its own name.
- **Option order.** `WithEager` and a `WithHeaders` that names a density version only see variants declared before them, so list `WithDensities` first; naming an undeclared version is `invalid_argument` at start-up, never a silent miss.

Check: the example's 48-pixel avatar of a 400×200 photo prints `density srcset descriptors: 1x 2x 3x` from `DensitySrcSet`.

**srcset with x descriptors.** `DensitySrcSet(ctx, s, scope, fileID, base, ttl)` returns `https://…/avatar-….jpg 1x, https://…/avatar-2x-….jpg 2x, https://…/avatar-3x-….jpg 3x` for an `<img width="48" height="48">`, and the candidates, each with its `Density`. The descriptors are the real densities (each version's width over the 1x width, rounded to two decimals), so a capped version is advertised as what it is (`2.5x`, not `3x`) and the browser shows it at the right size. A version no denser than the one before it is left out, so a small photo gives just `1x`. An undeclared base is `not_found`, a base without densities `invalid_argument`, and any version's failure fails the call, as with `SrcSet`.

**Picking by dpr in the handler.** `HandlerOptions{Density: true}` serves a base with densities in the version the request's device pixel ratio needs, as Cloudinary's `dpr` does: the `dpr` query parameter (`/variants/<fileID>/avatar?dpr=2`), else the `Sec-CH-DPR` client hint, else the older `DPR` hint. The ratio is rounded up to the next declared factor (2.6 gets the 3x), and one above every factor gets the densest. No ratio, a ratio of 1 or less, or one that is not a positive number gets the base. Every response then carries `Vary: Sec-CH-DPR, DPR`, so a cache keeps one per ratio. Browsers send the hint only after the page's response includes `Accept-CH: Sec-CH-DPR`; the query parameter needs no setup. A density version requested by its own name is served as it is. Without the option a `dpr` changes nothing.

## Step 5 — Replace the original

The plugin implements `records.EventHooks`. On `file.replaced` and `file.visibility_changed` it deletes the file's variant rows inside the change's transaction and their objects after the commit, so nobody is served a thumbnail of the old picture, or a public thumbnail of a file made private. Eager variants are made again after the commit; the others on their next request. On `file.trashed` it deletes them the same way, so a trashed image's thumbnails stop answering at their URLs (a public thumbnail would otherwise stay reachable); `file.restored` makes the eager ones again, the others on request. `file.released` (a quarantined image whose scan passed) makes the eager ones for the first time.

The plugin also implements `records.ObjectOwner`: no `fs_files` row names a variant object, so `Store.SweepOrphans` asks the plugin before deleting one, and variants are never swept as orphans.

<!-- ds:block id=replacephoto-nnpjgrgs -->
**replacePhoto** · [`examples/records-variants/main.go:205-212`](../../examples/records-variants/main.go#L205-L212)

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

Check: `variant objects after replace: 1 (old ones dropped, eager thumb remade)` and `card after replace: 267x400`: the replacement is 800×1200, and the card was generated from it.

After `SetVisibility(…, VisibilityPublic)` the regenerated thumbnail is public, and `URL` returns its CDN address: `public thumb: https://cdn.example.com/variants/…`. With [`WithVisibilityDisks`](../records/replace-and-visibility.md) the variants are regenerated on the file's new disk, because they always live on the file's disk.

## Step 6 — Change a transform, then prune

Each variant row records the digest of the transform that made it. Change a transform and the next request generates a new variant under a new key; the old row and object stay until `Prune(ctx, s)` deletes every variant whose digest no longer matches its declared transform, or whose name is no longer declared, and returns how many it removed.

<!-- ds:block id=prunechanged-224hztec -->
**pruneChanged** · [`examples/records-variants/main.go:217-231`](../../examples/records-variants/main.go#L217-L231)

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

Check: `pruned after the card transform changed: 1`. The thumb, whose transform did not change, stays. Run `Prune` once after a deploy that changes transforms; the library starts no background work.

## Step 7 — Purge the file

The plugin implements `records.FileHooks`: `BeforePurge` deletes the file's variant rows in the purge transaction and their objects after it commits (failures go to `records.WithObjectErrorHandler`).

Check: `variant objects after purge: 0`.

## Why there are no signed transform URLs

Image services such as imgproxy and Thumbor accept the transform in the URL (`/resize/320x240/…`) and sign it, so only URLs the server minted are served. This plugin does not offer that, on purpose:

- **Declared names already give the security.** A client can only ask for a variant the application declared, and `URL` signs the link when the file is private. A signed transform URL would protect the same thing with a second mechanism.
- **Unbounded variants.** Every distinct signed size would become a stored variant with its own row and object. `Prune` removes variants whose transform is no longer declared, but it cannot tell which ad-hoc sizes are still in use, so storage would only grow.
- **The server picks the size either way.** A signed URL is minted by the server, so the server already knows the size it wants. Declaring that size is one line, and it gets eager generation, `srcset`, `Prune` and quota accounting with it.

For truly arbitrary sizes (an editor's crop tool, a CDN that resizes on the fly), put an image service or a resizing CDN in front of the original's signed URL instead. Use `records.Store.URL` for the original and let that service cache its outputs.

## The table

`fs_variants`, one row per generated variant:

| Column | Meaning |
|---|---|
| `id` | `var_…` |
| `scope`, `file_id` | The file it was made from |
| `name`, `digest` | Declared name and the transform's digest; unique with `file_id` (`fs_variants_file_name_digest`) |
| `disk`, `storage_key` | Where the variant's object is |
| `content_type`, `size`, `width`, `height` | The generated image |
| `created_at` | When it was generated |

Indexes: `fs_variants_file_name_digest` (unique) and `fs_variants_scope` on `scope` (migration `0002_variants_scope`), which `HeldBytes` sums by.

## With other plugins

- [Versions](versions.md): restoring a version is a replacement, so variants are regenerated from the restored content.
- [Dimensions](dimensions.md) records the original's size; variants record their own `width` and `height`.
- Moving one file to another disk with `records.Store.MoveFile` takes along the variants on the file's own disk; those `WithLocation` put on another disk stay where they are. The plugin implements `records.FileDiskMover`: inside the move's transaction it copies each variant object to the new disk under the same key (checking its size), points the variant rows at the new disk, and deletes the old objects after the commit. If the move rolls back, the copies are deleted again, so a failed move leaves the variants where they were. Quota hears nothing, since the bytes held do not change.
- Renaming a disk with `records.Store.RenameDisk` moves variant rows with the files: the plugin implements `records.DiskRenamer` and rewrites `fs_variants.disk` in the same transaction, so a failure rolls the whole rename back.
- [Quota](quota.md) counts recorded files only by default. With `quota.WithHeldObjects()` it counts variants too: every generated variant is reported with `records.Store.Hold` in the transaction that records its row, and released with `Store.Release` when its row goes (replace, visibility change, trash, prune, purge). A scope at its byte limit then gets `quota_exceeded` (`507` over HTTP) when a new variant would be generated; nothing is recorded and the written object is deleted, while variants already generated keep being served. `HeldBytes` (the scope's total, read through the `fs_variants_scope` index) lets `quota.Recount` include them; turning the option on over existing variants needs a `Recount` of every scope.

## What can go wrong

- `415 rejected` (`CodeRejected`) — the file is not an image (its content type does not start with `image/`), or the decoder refused it (corrupt, too many pixels, too many bytes).
- `404 not_found` — an undeclared name, an unknown file, or a file in the trash. `Get`, `URL` and the handler check the trash before looking up the variant, so even a variant row left from before the trash is never served.
- `501 unsupported` from `Get` — `WithHeaders` names this variant and the file's disk cannot store headers.
- `507 quota_exceeded` from `Get` — quota counts held objects (`WithHeldObjects`) and the scope is at its byte limit; free space or raise the limit.
- `503 unavailable` from `Get` — the file was replaced or changed visibility three times while its variant was generated; the next request generates it.
- Eager failures are silent without an `onError` handler; the variant is then made on its first request.
- Old variant objects pile up after a transform change — run `Prune`.

## Quarantined files

Nothing is generated from a [quarantined](../records/processing-status.md#quarantine-until-scanned) image: it gets no eager variants, `Get`, `URL`, `Generate`, `Srcset` and the handler refuse it (`records.ErrQuarantined`), and `GenerateMissing` skips it. When its scan passes, `file.released` makes its eager variants; a replacement quarantined with `records.QuarantineContent()` drops the old ones until its own release.

## Next

- [Dimensions](dimensions.md): width and height of every image, stored on its file.
- [Replace and visibility](../records/replace-and-visibility.md): the events that drop variants.
- [Events and hooks](../records/events-and-hooks.md): how the plugin hooks into the Store.
- [Product images](../recipes/product-images.md) and [Avatars](../recipes/avatars.md): variants in a full app.
- The [variants README](../../contrib/imaging/variants/README.md) is the reference for the package.
