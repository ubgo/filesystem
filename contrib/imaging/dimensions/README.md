# github.com/ubgo/filesystem/contrib/imaging/dimensions

A records plugin that stores the width and height of every raster image in its file's metadata, when the file is recorded and when its content is replaced. Layouts need them before an image loads, and reading them later would mean fetching every object again. The guide is [Image dimensions](../../../docs/plugins/dimensions.md).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/imaging
```

The package is in the `contrib/imaging` module, which requires the core module, the `records` module and `golang.org/x/image`.

## Wire it

<!-- ds:block id=newstore-mk9e9dyp -->
**newStore** · [`examples/records-dimensions/main.go:79-88`](../../../examples/records-dimensions/main.go#L79-L88)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, opts ...dimensions.Option) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(dimensions.New(opts...)))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=8930da -->

The plugin has no table and no migration.

## Behaviour

- Measures files whose content type is in `RasterTypes`: `image/jpeg`, `image/png`, `image/gif`, `image/webp`, `image/bmp`, `image/tiff`. Other files, SVG included, are left alone.
- Reads only the first `imaging.ProbeBytes` (128 KiB) with `Disk.ReadRange` and parses the header with `imaging.Probe`; no pixels are decoded.
- Reports display dimensions, after EXIF orientation.
- Sets `Metadata["width"]` and `Metadata["height"]` (`MetaWidth`, `MetaHeight`) as decimal strings, after removing any existing values; other keys are kept. The caller's map is copied, never changed in place.
- Sets and clears its keys only on the types it measures. `width` and `height` are contrib/video's keys too, so a video, or an application's own file of another type, keeps them, whichever order the plugins are installed in. When an image's content is replaced by a type the plugin does not measure, its keys are dropped, except `width` and `height` when the new content is a video, which contrib/video sets; without it installed they keep the image's values.

## Options

| Option | What it does | Default |
|---|---|---|
| `WithProcessor(proc, types...)` | Read headers with `proc` (a Pipeline with codec modules) and also measure the content types given, such as `image/heic` | `imaging.Default()`, `RasterTypes` |
| `WithThumbHash(limits imaging.Options)` | Also store each image's ThumbHash (`MetaThumbHash`, base64), reading the whole object once and decoding within `limits`; an image over them or that does not decode gets none (refused with `WithStrict`) | off |
| `WithDominantColour(limits imaging.Options)` | Also store the dominant colour as `#rrggbb` (`MetaColour`, read with `ColourOf`), within its own limits, sharing one decode with `WithThumbHash` when their limits are equal | off |
| `WithStrict()` | Refuse a raster image whose header cannot be read with `CodeRejected`, failing the create or replace | record it without dimensions |

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...) *Plugin` | The plugin, for `records.WithPlugins` |
| `Backfill(ctx, s, BackfillOptions) (BackfillResult, error)` | Measures files recorded before the plugin was installed, one scope at a time, resumable through `NextCursor` and bounded per call by `MaxFiles`; `BackfillOptions.Plugin` passes the installed plugin so its processor and types apply (see the guide) |
| `ThumbHashOf(f records.File) ([]byte, bool)` | The recorded ThumbHash (`WithThumbHash`); `ok` is false when there is none |
| `Of(f records.File) (width, height int, ok bool)` | The recorded dimensions; `ok` is false when there are none (not a raster image, unreadable, or recorded before the plugin was installed) |
| `PluginName` | `"dimensions"` |
| `MetaWidth`, `MetaHeight`, `MetaThumbHash` | The metadata keys, `"width"`, `"height"` and `"thumbhash"` |
| `RasterTypes` | The content types measured |

## Hooks

| Interface | Behaviour |
|---|---|
| `records.FileHooks` | `BeforeCreate` measures the new file; `AfterCreate` and `BeforePurge` do nothing |
| `records.ReplaceHooks` | `BeforeReplace` measures the new content, so old dimensions never outlive it; a replacement by another type drops the keys the plugin wrote (see Behaviour for the keys shared with contrib/video) |

## Limits

- Files recorded before the plugin was installed have no dimensions until `Backfill` runs over their scope. Backfill writes the row directly, guarded on the object key and the metadata it read, so it never overwrites a concurrent replace; no hooks or events run for it.
- A disk read failure always fails the operation; only an unparseable header is governed by `WithStrict`.

## Quarantined files

A [quarantined](../../../docs/records/processing-status.md#quarantine-until-scanned) image is not read when it is recorded or replaced (its unscanned bytes stay unparsed, and dimensions a caller set are cleared); it is measured when its scan passes (`BeforeRelease`). `Backfill` leaves it alone until then.
