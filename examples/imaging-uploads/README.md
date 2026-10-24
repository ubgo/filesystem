# Smaller photos, size checks, placeholders and srcset

The image tools an upload path combines, in one program: `imaging.Optimize` makes photos smaller losslessly first (metadata out, recompression only above a quality floor), `imaging.CheckDimensions` refuses photos that are too small, the dimensions plugin stores a ThumbHash placeholder next to the width and height, and the variants plugin builds the `srcset` of a family of widths. It uses `contrib/imaging`, its `dimensions` and `variants` plugins and `records` on SQLite over an in-memory disk with public URLs. The guide [Image processing](../../docs/plugins/image-processing.md) explains each tool.

## Run it

```sh
cd examples
go run ./imaging-uploads
go test ./imaging-uploads
```

It needs no network or environment variables. The photos are generated in memory (seeded noise, so the sizes are the same on every run), the SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./imaging-uploads
```

```text
beach.jpg: 370170 bytes uploaded, 168554 stored, 900x600
thumbhash: ErYBBYCrdjuUWmaXb4VnhiCh2tkI
srcset: https://cdn.example.com/variants/fil_2nelnwdqcdbugswz4wvc5wbtcz/w320-4a037c837207feea-3fpmmy35qc7socg5mqbem3dcux.jpg 320w, https://cdn.example.com/variants/fil_2nelnwdqcdbugswz4wvc5wbtcz/w640-9e47a9ff640dc3df-nlcolbxigkkislfl63ihpom2ms.jpg 640w, https://cdn.example.com/variants/fil_2nelnwdqcdbugswz4wvc5wbtcz/w1280-ef1758c2c8879208-if2zeitnb5bzwxt2sfmkjqfyqz.jpg 900w
icon.jpg refused: rejected: filesystem: rejected: image is 120x120; it must be at least 200 pixels wide
exif by default: kept false
exif with MetadataEXIFCopyright: 2 fields, Artist "Ada Lens", Copyright "(c) 2026 Ada Lens"
exif GPS kept: false, position anywhere in the file: false, camera make kept: false
exif of a photo with neither field: kept false
keeping an unknown kind: invalid_argument
```

1. A 900×600 photo saved at quality 95, with an EXIF block holding a GPS position, is uploaded. `Optimize` drops the EXIF and recompresses it at the floor of 80, because it was saved above it. The stored file is less than half the size.
2. The dimensions plugin records 900×600 and the ThumbHash, about 20 bytes a page turns into a blurred placeholder.
3. `SrcSet` generates the three widths and links them. The original is only 900 wide and `ModeFit` never enlarges, so `w1280` is described as `900w`.
4. A 120×120 icon is refused by `CheckDimensions`, with a message the client can show, and nothing is stored. File IDs and variant keys are random, so they differ on every run.
5. `exif.go` optimises a camera photo whose EXIF block holds the camera make, the photographer's Artist and Copyright, and a GPS IFD with the position. By default `Optimize` keeps no EXIF. `keepAuthorship` names `MetadataEXIFCopyright` (with `MetadataCopyright` for the IPTC notice), and the output's EXIF holds the two fields and nothing else: no GPS IFD, the position nowhere in the file, no camera make, because the block is rebuilt rather than filtered. A photo with neither field keeps no EXIF at all, and an unknown kind is `invalid_argument`. The test `TestEXIFAuthorship` first checks that the source really carries the GPS IFD, the position and the make, so the absences are not vacuous.

## The code

The upload policy: optimise every photo, refuse ones under 200 pixels on a side.

<!-- ds:block id=photopolicy-psgqc2r8 -->
**photoPolicy** · [`examples/imaging-uploads/main.go:61-74`](main.go#L61-L74)

```go
func photoPolicy() (upload.Policy, error) {
	check, err := imaging.CheckDimensions(imaging.DimensionLimits{MinWidth: minSide, MinHeight: minSide, MaxPixels: 40_000_000})
	if err != nil {
		return upload.Policy{}, err
	}
	return upload.Policy{
		MaxSize:    maxUpload,
		Allowed:    []string{"image/jpeg", "image/png"},
		Key:        upload.RandomKey("photos"),
		Visibility: filesystem.VisibilityPublic,
		Process:    imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{Quality: 80}),
		Inspect:    check,
	}, nil
}
```
<!-- /ds:block hash=5ca077 -->

The store: dimensions with ThumbHash, and the variants of the srcset family.

<!-- ds:block id=newstore-em5m7df6 -->
**newStore** · [`examples/imaging-uploads/main.go:79-93`](main.go#L79-L93)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *variants.Plugin, error) {
	v, err := variants.New(widths, imaging.Options{})
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(
		dimensions.New(dimensions.WithThumbHash(imaging.Options{})), v))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, v, nil
}
```
<!-- /ds:block hash=79d91b -->

One upload, and what a page needs back from it.

<!-- ds:block id=uploadphoto-6tjxscru -->
**uploadPhoto** · [`examples/imaging-uploads/main.go:98-114`](main.go#L98-L114)

```go
func uploadPhoto(ctx context.Context, w io.Writer, s *records.Store, v *variants.Plugin, p upload.Policy, name string, body []byte) error {
	f, err := s.Upload(ctx, tenant, diskName, bytes.NewReader(body), name, p)
	if err != nil {
		fmt.Fprintf(w, "%s refused: %s: %v\n", name, filesystem.CodeOf(err), err)
		return nil
	}
	width, height, _ := dimensions.Of(f)
	hash, _ := dimensions.ThumbHashOf(f)
	set, _, err := v.SrcSet(ctx, s, tenant, f.ID, linkLifetime, "w320", "w640", "w1280")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: %d bytes uploaded, %d stored, %dx%d\n", name, len(body), f.Size, width, height)
	fmt.Fprintf(w, "thumbhash: %s\n", base64.StdEncoding.EncodeToString(hash))
	fmt.Fprintf(w, "srcset: %s\n", set)
	return nil
}
```
<!-- /ds:block hash=1d6aaa -->

## Files

- `main.go`: the policy, the store, the upload and the demo.
- `exif.go`: keeping the EXIF Artist and Copyright through `Optimize` while the GPS position goes; code: [`keepAuthorship`](ds:block?id=keepauthorship-n4v8k2qe), [`exifAuthorship`](ds:block?id=exifauthorship-w7c3m9tf).
- `main_test.go`: runs the demo and checks its output, and pins that the EXIF source carries a GPS position the output does not.

## Next

- [Image processing](../../docs/plugins/image-processing.md): the processor seam, `CheckDimensions`, `Optimize`.
- [Image dimensions](../../docs/plugins/dimensions.md): `WithThumbHash`.
- [Image variants](../../docs/plugins/variants.md): `SrcSet`.
