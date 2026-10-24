# github.com/ubgo/filesystem/contrib/imaging/variants

A records plugin that serves named image variants of stored files ("thumb", "avatar-64"), generated on first request (or eagerly after an upload) and then served from storage. Only names the application declares exist, so a client can never ask for an arbitrary size. Variants are regenerated when their original is replaced or changes visibility, dropped when it is trashed (and made again after a restore), pruned when a transform changes, and deleted with their file. The guide is [Image variants](../../../docs/plugins/variants.md).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/imaging
```

The package is in the `contrib/imaging` module, which requires the core module, the `records` module and `golang.org/x/image` (WebP, BMP and TIFF decoding, resampling). Images are processed by `imaging.Process`.

## Wire it

<!-- ds:block id=transforms-n65mkxr9 -->
**transforms** · [`examples/records-variants/main.go:68-71`](../../../examples/records-variants/main.go#L68-L71)

```go
var transforms = map[string]imaging.Transform{
	nameThumb: {Mode: imaging.ModeFill, Width: 128, Height: 128, Format: imaging.FormatJPEG},
	nameCard:  {Mode: imaging.ModeFit, Width: 400, Height: 400, Format: imaging.FormatJPEG, Quality: 80},
}
```
<!-- /ds:block hash=28ade7 -->

<!-- ds:block id=newplugin-k2uvchpd -->
**newPlugin** · [`examples/records-variants/main.go:110-112`](../../../examples/records-variants/main.go#L110-L112)

```go
func newPlugin(t map[string]imaging.Transform, onEager func(context.Context, records.File, string, error)) (*variants.Plugin, error) {
	return variants.New(t, imaging.Options{}, variants.WithEager(onEager, nameThumb))
}
```
<!-- /ds:block hash=366b96 -->

Install the plugin with `records.WithPlugins(p)` and run `Store.Migrate`, which creates `fs_variants`. Mount the handler with its prefix stripped:

<!-- ds:block id=newhandler-7bqv9349 -->
**newHandler** · [`examples/records-variants/main.go:172-177`](../../../examples/records-variants/main.go#L172-L177)

```go
func newHandler(s *records.Store, p *variants.Plugin) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/variants/", http.StripPrefix("/variants/",
		variants.Handler(s, p, scopeOf, variants.HandlerOptions{LinkLifetime: linkTTL})))
	return mux
}
```
<!-- /ds:block hash=38cbde -->

## Options

| Option | What it does |
|---|---|
| `New(transforms, imaging.Options, ...Option)` | The declared variants; names must match `[a-z0-9][a-z0-9_-]{0,63}` and every transform must pass `Validate`. `imaging.Options{MaxPixels, MaxBytes}` bound decoding (zero: 50 megapixels, 50 MiB) |
| `WithProcessor(proc imaging.Processor)` | Make variants with `proc` instead of `imaging.Default()`: a Pipeline with codec modules (WebP or AVIF output, HEIC input) or another backend. When `proc` reports the formats it writes (`Encodes`), `New` refuses a transform it cannot write with `CodeUnsupported`. A nil `proc` is `CodeInvalidArgument`. See [Image processing](../../../docs/plugins/image-processing.md) |
| Custom formats | A transform may name a format outside `imaging.FormatValues` when the processor encodes it (an `imaging.DescribedEncoder` on a Pipeline): `New` validates every transform with `ValidateFor(processor)` after the options, keys get the encoder's extension, `Accept` negotiation its type, and `Prune` keeps such variants |
| `WithFocal(src FocalSource)` | Crop `GravityFocal` variants around `src.Focal(file)`, and drop a file's variants on `src.FocalChanged()`; `contrib/imaging/focal` is a source. A nil `src` is `CodeInvalidArgument` |
| `WithEager(onError, names...)` | Generate these variants after an image is recorded, replaced, restored from the trash or changes visibility, after the transaction commits. Failures go to `onError` (may be nil); the variant is then made on its first request. Undeclared names are `CodeInvalidArgument` |
| `WithHeaders(h filesystem.HTTPHeaders, names...)` | Store `h` (such as `CacheControl: "public, max-age=31536000, immutable"`, but see the cache limits in [Storage](#storage)) as the HTTP headers of the named variants, or of every declared variant when no name is given. Headers are never inherited from the original, so an original stored with `Content-Disposition: attachment` does not make its thumbnails downloads. The disk must have the `Headers` capability (memory, S3, GCS, Azure); on one without it every generation of these variants fails with `CodeUnsupported`. CR or LF in a value, or an undeclared name, is `CodeInvalidArgument` |
| `WithLocation(l Location)` | Store variants where `l(file, name)` says instead of under `variants/<fileID>/` on the file's disk: it returns a disk name of the Manager (`""` for the file's own) and a key prefix, and the plugin appends `<name>-<digest>-<id><ext>`, so every generation still writes a new key. Use it for thumbnails on a public CDN disk while originals stay private, a cheaper bucket, or CDN-friendly paths. The disk is stored on the variant's row. An error fails the request (an eager run marks the file `failed`) with nothing written. A nil `l` is `CodeInvalidArgument` |
| `WithDensities(base, factors...)` | Declare density versions of the variant `base`: one variant per factor, named `DensityName(base, factor)` (`avatar-2x`, `avatar-3x`, `avatar-1_5x`), its transform base's with `Width` and `Height` multiplied and rounded down. No factors means 2 and 3 (iOS @2x/@3x); factors run from 1 to `MaxDensity` (4, Android's xxxhdpi). A version is never made larger than its source (or `Crop`) fills: it is capped at the largest factor the source supports, never below 1x, by reading the source's header (`Processor.Probe`), so a request for `avatar-3x` of a small photo still answers. A version without its own `WithHeaders` takes base's. List it before `WithEager` and any `WithHeaders` that names a version. An undeclared base, a base that is itself a version, densities given twice, a factor outside 1..4 or listed twice, a derived name already declared or too long, or a scaled size past `imaging.MaxDimension` is `CodeInvalidArgument` |

## Go API

| Method | What it does |
|---|---|
| `Get(ctx, s, scope, fileID, name) (Variant, error)` | The variant, generated and stored on first request. `CodeNotFound` for an undeclared name, an unknown file, or a trashed file (the trash is checked before the variant table, so no variant of a trashed file is ever returned, and a file trashed while its variant is generated leaves no row and no object); `CodeUnavailable` when the file was replaced or changed visibility on every one of three attempts; `CodeRejected` for a file that is not `image/*` or cannot be decoded; a usage plugin's refusal of the hold (`CodeQuotaExceeded` from quota with `WithHeldObjects`), with nothing recorded and the object deleted; `CodeUnsupported` when `WithHeaders` names it and the disk cannot store headers |
| `URL(ctx, s, scope, fileID, name, ttl) (string, error)` | `Get`, then the public URL for a public file on a disk that has one, otherwise a signed GET valid for `ttl` |
| `Generate(ctx, s, scope, fileID, names...) error` | Make the named variants of one file now (every declared one when none named), for a job runner: status `processing` then `ready`, or `failed` with `<variant>: <code>`, and `EventReady` or `EventFailed` in that final transaction. Existing variants are found, not remade. The error is the first failure; `CodeNotFound` for an unknown or trashed file or an undeclared name; nil for a non-image |
| `GenerateMissing(ctx, s, MissingOptions) (MissingResult, error)` | Run `Generate` for a scope's images lacking a named variant, newest first, bounded by `MaxFiles` and resumable through `NextCursor`; failures listed in `Failed`, the run carries on |
| `EventReady`, `EventFailed`, `DetailVariants`, `DetailFailed`, `DetailCode` | the plugin's events (`variants.ready`, `variants.failed`) and their `Event.Detail` keys |
| `SrcSet(ctx, s, scope, fileID, ttl, names...) (string, []Candidate, error)` | the `srcset` attribute of the named variants, linked as `URL` links them, with each variant's real width as its `w` descriptor, smallest first, one candidate per distinct width; no names is `CodeInvalidArgument` and any variant's failure fails the call |
| `DensitySrcSet(ctx, s, scope, fileID, base, ttl) (string, []Candidate, error)` | the `srcset` of `base` and its density versions with `x` descriptors: each version's real density (its width over the 1x width, two decimals, set as `Candidate.Density`), so a version capped by a small source says `2.5x`, not `3x`; a version no denser than the previous one is left out. `CodeNotFound` for an undeclared base, `CodeInvalidArgument` for one without densities; any version's failure fails the call |
| `DensityName(base, factor) string` | the declared name of `base` at `factor`: `base` at 1, otherwise `<base>-<factor>x` with a fractional point written `_` |
| `Prune(ctx, s) (int, error)` | Deletes variants whose digest no longer matches their transform, or whose name is no longer declared, releasing each (`Store.Release`); objects after the commit |
| `HeldBytes(ctx, s, scope) (int64, error)` | The total size of the scope's variants (`records.HeldCounter`), so `quota.Recount` includes them |
| `Handler(s, p, scope ScopeFunc, HandlerOptions) http.Handler` | `GET`/`HEAD /{fileID}/{name}`: `302` to `URL` with `Cache-Control: private, max-age=60`; `403` when `scope` errors, `404` for an undeclared name, `405` for other methods. Generation survives a client disconnect |
| `HandlerOptions.Mode` (`ModeRedirect`, `ModeProxy`), `HandlerOptions.CacheControl`, `DefaultCacheControl` | `ModeProxy` streams the variant itself (one URL for the client and a CDN, the bucket never reached) through `fshttp.ServeEntry`: ranges (206), conditional requests (304), `nosniff` and fshttp's sandbox CSP, the read reported with `Store.Served` before any byte; an unknown mode answers 500. `CacheControl` sets every response's header (default `private, max-age=60`); use a public value only when every file served is public |
| `HandlerOptions.Errors`, `HandlerOptions.Sendfile` | `Errors` (an `fshttp.ErrorWriter`) renders every failure (405, 404 for a path naming no variant, 403 when the `ScopeFunc` refuses, the store's and disk's errors with `fshttp.StatusOf`) in the application's envelope, with any `Retry-After` already set; nil keeps the plain status text. `Sendfile` (an `*fshttp.Sendfile`), in `ModeProxy`, leaves the variant's body to nginx `X-Accel-Redirect` or `X-Sendfile` after every header is set; nil streams it, and `ModeRedirect` ignores it |

`HandlerOptions.LinkLifetime` is the signed URL's lifetime, `DefaultLinkLifetime` (10 minutes) when zero. `HandlerOptions.Negotiate` lists formats (such as AVIF, then WebP) to serve instead of the declared one when `Accept` names them explicitly and the processor reports (by `Encodes`) that it writes them; each is a sibling variant made on first request and kept by `Prune`, and responses carry `Vary: Accept`. `HandlerOptions.Density` serves a base declared with `WithDensities` in the version the request's device pixel ratio needs: the `dpr` query parameter (`DPRParam`), else the `Sec-CH-DPR` or `DPR` client hint, rounded up to the next declared factor (the densest when above them all); none, 1 or less, or an unparseable value gets the base, and responses carry `Vary: Sec-CH-DPR, DPR`. A browser sends the hint only after the page answers with `Accept-CH: Sec-CH-DPR`.

## Hooks

| Interface | Behaviour |
|---|---|
| `records.Migrator` | Creates `fs_variants` (migration `variants/0001_variants`) and its scope index (`variants/0002_variants_scope`) |
| `records.FileDiskMover` | `MoveFileDisk`, inside `Store.MoveFile`'s transaction: copies the file's variant objects to the new disk (size checked), points their rows at it, deletes the old objects after the commit and the copies on rollback |
| `records.DiskRenamer` | `RenameDisk` rewrites `fs_variants.disk` from the old name to the new inside `Store.RenameDisk`'s transaction; an error rolls the whole rename back |
| `records.HeldCounter` | `HeldBytes`; each generated variant is reported with `Store.Hold` in the transaction that records it, and each deleted one with `Store.Release` in the transaction that deletes its row, so `records.UsageHooks` plugins (quota) count variants |
| `records.FileHooks` | `BeforePurge` deletes the file's variant rows and releases them; their objects go after the commit |
| `records.ObjectOwner` | `OwnedObjects` claims the variant objects named in `fs_variants`, so `Store.SweepOrphans` never deletes a variant |
| `records.EventHooks` | `file.replaced`, `file.visibility_changed` and `file.trashed` delete the file's variants (objects after the commit); with `WithEager`, `file.created`, `file.restored`, `file.released`, `file.replaced` and `file.visibility_changed` set an image's status (never a quarantined one's) to `processing` in the change's transaction and schedule eager generation with `Store.AfterCommit`, which sets it `ready` or `failed` and emits `variants.ready` or `variants.failed` |

## Table

`fs_variants` (`variants.TableNameVariants`): `id` (`var_…`, primary key), `scope`, `file_id`, `name`, `digest`, `disk`, `storage_key`, `content_type`, `size`, `width`, `height`, `created_at`. Unique index `fs_variants_file_name_digest` on `(file_id, name, digest)`; index `fs_variants_scope` on `scope` (migration `0002_variants_scope`, guarded on MySQL so a rerun after a crash succeeds). Typed columns (`variants.FileID`, `variants.Name`, …) are exported for queries of your own.

## Storage

By default a variant is written on its file's disk under `variants/<fileID>/<name>-<digest>-<id><ext>` (`variants.KeyDir`); `WithLocation` chooses the disk and the part before `<name>`. It gets the file's visibility when its disk supports per-object visibility. A variant on another disk than its file's stays there when the file moves disks (`MoveFileDisk` moves only those on the file's own). Its `created_at` comes from the Store's clock (`records.WithClock`), and so does the expiry check that refuses variants of an expired file. `<digest>` identifies the transform and `<id>` is the random part of the variant's ID, so no key is ever written twice: a deferred deletion only reaches the objects of the rows it dropped, a losing or stale generation deletes only its own object, and a cached copy of a key never shows another picture. Rows recorded with an older key shape keep the key they stored.

Cache limits: a copy a browser or CDN already holds is served until it expires, even after the file is made private, trashed or purged, because only the object is deleted. Use `immutable` with a year-long `max-age` only for variants of files that stay public; otherwise use a short `max-age`, purge the CDN on `file.visibility_changed`, `file.trashed` and `file.purged`, or serve the variants private and signed.

## Concurrency

Generation runs outside any transaction. The row is recorded in a transaction that locks the file's row (an `UPDATE` assigning `storage_key` to itself, the one row lock SQLite, Postgres and MySQL share) and reads it again: a file trashed or purged meanwhile fails with `CodeNotFound` and the written object is deleted; a file replaced or made public or private is generated again from its current state. A `Trash`, `Replace` or `SetVisibility` that starts after the lock waits for the commit and then drops the new row. Two requests for the same variant each write their own key; the one whose insert loses deletes its object and returns the winner's row.

## Limits

- Variant objects count toward quota only with `quota.WithHeldObjects()`; then a scope at its byte limit gets `CodeQuotaExceeded` when a new variant would be generated.
- Output formats with the default processor (`imaging.Default()`) are JPEG, PNG and GIF (an animated GIF keeps its animation unless `FirstFrame`). WebP and AVIF output need a Pipeline with `contrib/imaging/webp`'s or `contrib/imaging/avif`'s `Encoder` passed through `WithProcessor`; without it `New` refuses a WebP or AVIF transform with `CodeUnsupported`, and `HandlerOptions.Negotiate` drops those formats. `contrib/imaging/heic` only adds HEIC input (decoding), never output.

## Quarantined files

Nothing is generated from a [quarantined](../../../docs/records/processing-status.md#quarantine-until-scanned) image: it gets no eager variants, `Get`, `URL`, `Generate`, `Srcset` and the handler refuse it (`records.ErrQuarantined`), and `GenerateMissing` skips it. When its scan passes, `file.released` makes its eager variants; a replacement quarantined with `records.QuarantineContent()` drops the old ones until its own release.
