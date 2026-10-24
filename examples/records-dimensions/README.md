# Image width and height on every file

The dimensions plugin: when a raster image is recorded, and again when its content is replaced, its width and height are read from the image header and stored in the file's metadata. A gallery can then lay out images before they load, and an app can refuse images that are too small, without opening the bytes again. Other files are left alone; a corrupt image is recorded without dimensions, or refused with `WithStrict`. It uses `github.com/ubgo/filesystem/records` and `contrib/imaging/dimensions` on SQLite over an in-memory disk. The guide [Image dimensions](../../docs/plugins/dimensions.md) explains each step.

## Run it

```sh
cd examples
go run ./records-dimensions
go test ./records-dimensions
```

It needs no network or environment variables. The images are generated in memory, the SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-dimensions
```

```text
photo.png: 640x480, metadata map[height:480 width:640]
notes.txt: no dimensions
broken.png: no dimensions
broken.png with WithStrict: rejected
photo.png after replace: 1920x1080
```

1. A 640×480 PNG is uploaded; the plugin stores `width` and `height` in its metadata, and `dimensions.Of` reads them back.
2. A text file gets no dimensions and is recorded as usual.
3. A file with a PNG signature followed by garbage is recorded without dimensions by the default plugin.
4. The same file under `dimensions.WithStrict()` is refused with `rejected`, and leaves neither a row nor an object.
5. Replacing the photo with a 1920×1080 PNG updates the dimensions: old ones never outlive the content they described.

## The code

The plugin has no table: dimensions live in the file's metadata column.

<!-- ds:block id=newstore-mk9e9dyp -->
**newStore** · [`examples/records-dimensions/main.go:79-88`](main.go#L79-L88)

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

Upload and replace, each reading the recorded dimensions back with `dimensions.Of`:

<!-- ds:block id=uploadandmeasure-2szbd73c -->
**uploadAndMeasure** · [`examples/records-dimensions/main.go:100-110`](main.go#L100-L110)

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

<!-- ds:block id=replaceandmeasure-zhdjsmdx -->
**replaceAndMeasure** · [`examples/records-dimensions/main.go:115-125`](main.go#L115-L125)

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

The run: an image, a text file, a corrupt image under both stores, and a replacement.

<!-- ds:block id=demo-sx8w9dyt -->
**demo** · [`examples/records-dimensions/main.go:152-186`](main.go#L152-L186)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: a lenient and a strict store, PNG generation, upload, replace |
| `main_test.go` | Runs the same steps, plus a check that a strict refusal leaves nothing behind and `Of` on a file without metadata |

## Next

- The guide: [Image dimensions](../../docs/plugins/dimensions.md).
- Related examples: [records-variants](../records-variants) for resized copies of the same images, [recipe-product-images](../recipe-product-images) for a product catalogue.
