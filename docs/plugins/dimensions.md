# Image dimensions

You will have the width and height of every uploaded image stored on its file row, so a page can reserve the right box before the image loads (no layout shift), lay out a masonry grid, or pick a `srcset` entry, without fetching any object again. Use it in any app that shows user images in a layout; it costs one header read per image upload.

## Before you start

- A [records Store](../records/README.md). The plugin has no table: it writes two keys, `width` and `height`, into the file's `Metadata` (the `metadata` column of `fs_files`).
- The plugin is in the `contrib/imaging` module: `go get github.com/ubgo/filesystem/contrib/imaging`.
- The example is [`examples/records-dimensions`](../../examples/records-dimensions): a SQLite Store over a memory disk. `go run ./records-dimensions` in `examples/` prints every step below; `go test ./records-dimensions` runs them.

What it measures: files whose content type is one of `dimensions.RasterTypes` (`image/jpeg`, `image/png`, `image/gif`, `image/webp`, `image/bmp`, `image/tiff`). SVG and every other type are left alone. Only the first `imaging.ProbeBytes` (128 KiB) of the object are read, and the header is parsed without decoding pixels, so a pixel bomb costs nothing. Dimensions are as displayed: a portrait phone photo stored sideways with an EXIF rotation reports its portrait size.

## Step 1 — Install the plugin

<!-- ds:block id=newstore-mk9e9dyp -->
**newStore** · [`examples/records-dimensions/main.go:79-88`](../../examples/records-dimensions/main.go#L79-L88)

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

`dimensions.New()` takes two options: `dimensions.WithStrict()`, described in step 3, and `dimensions.WithProcessor(proc, types...)`, which reads headers with another [processor](image-processing.md) and measures extra content types (`image/heic` with the HEIC codec module). `Migrate` applies nothing for it.

## Step 2 — Upload and read the size back

The plugin implements `records.FileHooks`: `BeforeCreate` runs inside the transaction that records the file, after the object is stored, and sets the metadata before the row is inserted. Every way of recording a file goes through it: `Store.Upload`, `Store.CreateFile`, and the upload route of [recordshttp](../records/http-endpoints.md). `dimensions.Of(f)` reads the values back as integers.

<!-- ds:block id=uploadandmeasure-2szbd73c -->
**uploadAndMeasure** · [`examples/records-dimensions/main.go:100-110`](../../examples/records-dimensions/main.go#L100-L110)

```go
func uploadAndMeasure(ctx context.Context, s *records.Store, name string, body []byte) (records.File, string, error) {
	f, err := s.Upload(ctx, tenant, diskName, bytes.NewReader(body), name, policy)
	if err != nil {
		return records.File{}, "", err
	}
	w, h, ok := dimensions.Of(f)
	if !ok {
		return f, "no dimensions", nil
	}
	return f, fmt.Sprintf("%dx%d", w, h), nil
}
```
<!-- /ds:block hash=9c8cda -->

<!-- ds:block id=demo-sx8w9dyt -->
**demo** · [`examples/records-dimensions/main.go:152-186`](../../examples/records-dimensions/main.go#L152-L186)

```go
func demo(ctx context.Context, w io.Writer, lenient, strict *records.Store) error {
	photo, err := makePNG(640, 480)
	if err != nil {
		return err
	}
	f, dims, err := uploadAndMeasure(ctx, lenient, "photo.png", photo)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "photo.png: %s, metadata %v\n", dims, f.Metadata)
	_, dims, err = uploadAndMeasure(ctx, lenient, "notes.txt", []byte("plain text"))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "notes.txt: %s\n", dims)
	corrupt := []byte(pngSignature + strings.Repeat("x", 64))
	_, dims, err = uploadAndMeasure(ctx, lenient, "broken.png", corrupt)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "broken.png: %s\n", dims)
	if _, _, err = uploadAndMeasure(ctx, strict, "broken.png", corrupt); err != nil {
		fmt.Fprintf(w, "broken.png with WithStrict: %s\n", filesystem.CodeOf(err))
	}
	wide, err := makePNG(1920, 1080)
	if err != nil {
		return err
	}
	dims, err = replaceAndMeasure(ctx, lenient, f.ID, wide)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "photo.png after replace: %s\n", dims)
	return nil
}
```
<!-- /ds:block hash=cece9e -->

Check: the program prints

```
photo.png: 640x480, metadata map[height:480 width:640]
notes.txt: no dimensions
broken.png: no dimensions
broken.png with WithStrict: rejected
photo.png after replace: 1920x1080
```

The values are decimal strings in `Metadata`, so they are also visible to SQL (`metadata` is JSON text: `{"height":"480","width":"640"}`) and to anything that serializes the file. Any `width` or `height` the caller put in `NewFile.Metadata` is removed first: the plugin's values are the only ones that land, and a non-image never carries stale ones. Other metadata keys are kept.

## Step 3 — Decide what a corrupt image does

A file detected as PNG by its magic bytes but with an unreadable header is recorded without dimensions by default (`broken.png: no dimensions`), so `Of` returns `ok == false`. With `WithStrict()` the plugin refuses it with `CodeRejected` and the whole upload fails: `Upload` deletes the stored object again, so nothing is left. Use strict mode when the image must be usable (an avatar, a product photo); leave it off when any upload must succeed (a chat attachment).

Only a header that cannot be parsed is subject to this choice. A read failure from the disk always fails the operation, strict or not.

## Step 4 — Replace the content

The plugin implements `records.ReplaceHooks` too: `BeforeReplace` measures the new content, so `Replace`, `ReplaceObject`, a [version restore](versions.md) and `PUT /files/{id}` all leave the right size on the row.

<!-- ds:block id=replaceandmeasure-zhdjsmdx -->
**replaceAndMeasure** · [`examples/records-dimensions/main.go:115-125`](../../examples/records-dimensions/main.go#L115-L125)

```go
func replaceAndMeasure(ctx context.Context, s *records.Store, id string, body []byte) (string, error) {
	f, err := s.Replace(ctx, tenant, id, bytes.NewReader(body), "", policy)
	if err != nil {
		return "", err
	}
	w, h, ok := dimensions.Of(f)
	if !ok {
		return "no dimensions", nil
	}
	return fmt.Sprintf("%dx%d", w, h), nil
}
```
<!-- /ds:block hash=6993b1 -->

Check: `photo.png after replace: 1920x1080`. Replacing an image with a non-image removes the keys, except `width` and `height` when the new content is a video: those are [video](video.md)'s to set.

## Step 5 — A placeholder while the image loads (ThumbHash)

`dimensions.WithThumbHash(imaging.Options{})` also stores each image's [ThumbHash](https://evanw.github.io/thumbhash/) under the `thumbhash` metadata key (`MetaThumbHash`, standard base64). A ThumbHash is about 25 bytes from which a page draws a blurred placeholder with the image's colours, aspect ratio and transparency, before a single byte of the image arrives. Send it with the file's JSON and decode it in the browser with the `thumbhash` npm package (`thumbHashToDataURL`), or with the Swift, Kotlin or Rust decoders. `dimensions.ThumbHashOf(f)` returns the bytes.

- **Cost.** Unlike the dimensions, a ThumbHash needs the whole image decoded. The plugin reads the full object once per create or replace, scales it to fit 100 × 100 through the plugin's processor (EXIF orientation applied) and hashes that.
- **Limits.** The `imaging.Options` given bound the decode (zero fields: 50 megapixels and 50 MiB). An image over them, or one whose body does not decode behind a readable header, keeps its dimensions and gets no hash; with `WithStrict()` it is refused instead.
- **Read failures fail the change.** A failure to read the object is an error, as it is for the dimensions.
- **Compatibility.** The encoder follows the reference implementation. Tests compare it with the reference JavaScript encoder: the header (average colour, scales, aspect ratio, alpha) matches byte for byte, and every detail component within one of its fifteen steps; a component exactly half way between two steps can round either way, because Go's and JavaScript's cosines can differ in the last bit.
- **Dominant colour.** `dimensions.WithDominantColour(limits)` also stores the image's dominant colour as `#rrggbb` (`MetaColour`, read with `dimensions.ColourOf`), within its own limits; when `WithThumbHash` has the same limits the two share one decode.
- **Backfill.** With the option, a file that has dimensions but no hash (or no colour, with `WithDominantColour`) is not `Present`, so a backfill run with `BackfillOptions.Plugin` set to the plugin adds the hashes to files recorded before.

## Files recorded before the plugin

The plugin measures only at create and replace, so files recorded before it was installed have no dimensions until you backfill them. `dimensions.Backfill(ctx, store, dimensions.BackfillOptions{Scope: "tenant-1"})` walks one scope newest first and writes `width` and `height` into every raster file that lacks them. Run it once from a deploy step or an admin command after installing the plugin, once per scope.

- **Batches and resuming.** `MaxFiles` bounds how many raster files one call examines; the result's `NextCursor` resumes after the last one. Call it again with `Cursor: res.NextCursor` until `NextCursor` is empty, one call per job or with a pause between calls. `PageSize` sets how many rows each database read fetches (default `records.DefaultPageSize`).
- **Cost.** Each file costs one header read of at most `imaging.ProbeBytes` (128 KiB) through `Disk.ReadRange`, never the whole object, and one `UPDATE` of its row. Files that already have dimensions are skipped without being read, and only `RasterTypes` are listed, so running it twice is cheap and harmless.
- **Safe beside live traffic.** Each write is guarded on the file's object key and the metadata the listing saw. A file replaced (or its metadata rewritten) between the read and the write is not overwritten: it is counted in `Changed`, and the replace measured it itself.
- **No hooks, no events.** The backfill writes the metadata column directly: the file's content did not change, so no plugin hook runs and no event is emitted.
- **The plugin's options.** Pass the installed plugin as `BackfillOptions.Plugin`, so a `WithProcessor` processor and its extra types apply to the backfill too.
- **Trashed files.** Set `Trashed: true` for a second pass over the scope's trash, so a file restored later has its size too.

The result counts `Measured`, `Present` (already had dimensions), `Unreadable` (a raster type whose header cannot be parsed; it stays without dimensions, as the plugin records it without strict mode) and `Changed`, and lists in `Failed` each file whose object could not be read (a missing object, a disk error) with its error; the run carries on past them, and a later run retries them. `Backfill` itself returns an error only when listing or writing fails or the context ends, and the result then holds the progress so far, with a `NextCursor` to resume from.

## With other plugins

- [Variants](variants.md) store their own width and height in `fs_variants`; this plugin measures originals only.
- [Video](video.md) writes the same `width` and `height` keys for videos, so one layout reads both. Each plugin sets and clears them only on its own type, so neither wipes the other's values whichever is installed first; an application's own `width` and `height` on another type (a PDF, a link card) are left alone too.
- [Quota](quota.md) and the other `BeforeCreate` hooks run in installation order; a strict refusal rolls back whatever an earlier hook charged.

## What can go wrong

- `rejected` (`415` over HTTP) on an upload — `WithStrict()` is on and the image header cannot be read.
- `Of` returns `ok == false` for an image — not a raster type in `RasterTypes`, an unreadable header without strict mode, or a file recorded before the plugin was installed and not yet backfilled.
- Sizes that look swapped — they are display sizes after EXIF orientation, which is what a browser shows.

## Quarantined files

A [quarantined](../records/processing-status.md#quarantine-until-scanned) image is not read when it is recorded or replaced (its unscanned bytes stay unparsed, and dimensions a caller set are cleared); it is measured when its scan passes (`BeforeRelease`). `Backfill` leaves it alone until then.

## Next

- [Image variants](variants.md): resized copies under declared names.
- [Files](../records/files.md): metadata, and how a file is recorded.
- [Events and hooks](../records/events-and-hooks.md): `FileHooks` and `ReplaceHooks`, for a plugin of your own.
- The [dimensions README](../../contrib/imaging/dimensions/README.md) is the reference for the package.
