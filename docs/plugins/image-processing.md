# Image processing

You will know how images are decoded, resized and encoded under the [variants](variants.md) and [dimensions](dimensions.md) plugins and `imaging.Sanitize`, and how to change the backend: add formats (WebP or AVIF output, HEIC input), or replace the pure-Go code with another engine, without touching the plugins.

## Before you start

- `contrib/imaging` is its own module: `go get github.com/ubgo/filesystem/contrib/imaging`. The codec modules (`contrib/imaging/webp`, `avif`, `heic`, `icc`) are separate modules again; add only the ones you use.
- What a single transform does (modes, orientation, smart crop, watermark, GIF, colour, limits) is [Image transforms](image-transforms.md), with its example [`examples/imaging-transforms`](../../examples/imaging-transforms). This page is about the engine underneath.
- The codecs and colour management run in [`examples/imaging-codecs`](../../examples/imaging-codecs), a module of its own: `cd examples/imaging-codecs && go run .`. Upload-time checks and size reduction run in [`examples/imaging-uploads`](../../examples/imaging-uploads).

The steps, in the order an application usually needs them: pick the formats ([adding formats](#adding-formats), [codec modules](#codec-modules-webp-avif-heic)), hand the Pipeline to the plugins ([giving the plugins a processor](#giving-the-plugins-a-processor)), check uploads ([checking dimensions](#checking-dimensions-on-upload)), shrink them ([making uploads smaller](#making-uploads-smaller)), and manage colour ([colour profiles](#colour-profiles-svg-and-golden-tests)).

## The Processor seam

Everything that turns bytes into pixels and back goes through one interface in `contrib/imaging`:

| Method | Does |
|---|---|
| `Process(r, t Transform, opts Options) (Result, error)` | reads an image, applies a `Transform` (mode, size, format, quality, gravity, crop) and returns the encoded output |
| `Probe(r) (Info, error)` | reads the format and displayed dimensions from the header only |

`*imaging.Pipeline` is the implementation that ships: pure Go, no cgo, no native library. It picks a `Decoder` by the file's first bytes, checks the declared size against `Options.MaxPixels` before decoding a single pixel (a 1 KB file can declare a 100,000 × 100,000 image), applies the EXIF orientation, crops and resamples with Catmull-Rom, and writes the output with the `Encoder` for the transform's `Format`. `imaging.Default()` is the Pipeline with the built-in codecs, and the package functions `imaging.Process`, `imaging.Probe` and `imaging.Sanitize` use it.

| Built in | Formats |
|---|---|
| Decoders | JPEG, PNG, GIF (first frame), WebP, BMP, TIFF |
| Encoders | JPEG, PNG |

## Adding formats

A Pipeline is built once and never changes, so adding a codec never alters what another part of the program decodes:

- `imaging.NewPipeline(imaging.WithDecoder(d), imaging.WithEncoder(e))` returns the built-in codecs plus yours.
- An added decoder is tried before the built-ins, so a codec module can replace a built-in decoder for its format.
- An added encoder replaces any encoder for the same `Format`.

The output formats are a closed set: `FormatJPEG`, `FormatPNG`, `FormatWebP`, `FormatAVIF`. `Transform.Validate` accepts all four, but a Pipeline without an encoder for the requested format refuses it with `unsupported` and names the missing codec. The codec modules under `contrib/imaging` (`webp`, `avif`, `heic`) each export a decoder, an encoder or both. They are separate modules, so the default build stays small.

A `Decoder` has a `Name`, a `Match(head []byte)` that recognises the format from its first bytes, and `DecodeConfig` and `Decode` with the standard library's signatures. An `Encoder` has a `Format` and `Encode(w, img, quality)`.

## Codec modules: WebP, AVIF, HEIC

> [!IMPORTANT]
> By default the libraries under these modules load a system libwebp, libavif or libheif when one is installed and use it instead of their built-in pure-Go code, so the same program can produce different bytes on different machines. Build and test with `-tags nodynamic` to always use the pure-Go code; `webp.Native()`, `avif.Native()` and `heic.Native()` report which one is running. The modules' tests pass under both, because they compare decoded pixels within a tolerance.

Three modules add formats the standard library cannot handle. Each is its own Go module, so an application that does not import one never downloads its dependency. All three are pure Go: no cgo, no C compiler.

| Module | Adds | Built on |
|---|---|---|
| `contrib/imaging/webp` | WebP output (`webp.Encoder(webp.Options{Lossless, Method})`) and a WebP decoder that also reads animated files (first frame) and EXIF-rotated photos | `github.com/gen2brain/webp`, libwebp translated to Go |
| `contrib/imaging/avif` | AVIF output (`avif.Encoder(avif.Options{Speed, Lossless})`) and input | `github.com/gen2brain/avif`, libavif compiled to WebAssembly and run by wazero |
| `contrib/imaging/heic` | HEIC input (iPhone photos); there is no HEIC encoder | `github.com/gen2brain/heic`, a decoder compiled to WebAssembly and run by wazero |

A Pipeline with WebP output, ready for `variants.WithProcessor`:

<!-- ds:block id=webppipeline-yw2vxhuh -->
**webpPipeline** · [`contrib/imaging/webp/example_test.go:18-20`](../../contrib/imaging/webp/example_test.go#L18-L20)

```go
func webpPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(webp.Encoder()), imaging.WithDecoder(webp.Decoder()))
}
```
<!-- /ds:block hash=c8af80 -->

AVIF files are often half the size of a JPEG of the same quality, but encoding is slow: seconds for a large photo. Declare AVIF variants with `variants.WithEager`, so they are made after the upload commits rather than on a page's first request:

<!-- ds:block id=avifpipeline-vkyty8te -->
**avifPipeline** · [`contrib/imaging/avif/example_test.go:18-20`](../../contrib/imaging/avif/example_test.go#L18-L20)

```go
func avifPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(avif.Encoder()), imaging.WithDecoder(avif.Decoder()))
}
```
<!-- /ds:block hash=1265b2 -->

HEIC uploads become upright JPEGs with `Sanitize` and a Pipeline that reads HEIC. Pass the same Pipeline to `dimensions.WithProcessor(p, heic.ContentType)` so their size is recorded:

<!-- ds:block id=heicpolicy-6698j5h6 -->
**heicPolicy** · [`contrib/imaging/heic/example_test.go:19-26`](../../contrib/imaging/heic/example_test.go#L19-L26)

```go
func heicPolicy() upload.Policy {
	p := imaging.NewPipeline(imaging.WithDecoder(heic.Decoder()))
	return upload.Policy{
		MaxSize: 20 << 20,
		Allowed: []string{"image/jpeg", "image/png", heic.ContentType},
		Process: imaging.Sanitize(imaging.Options{}, imaging.WithPipeline(p)),
	}
}
```
<!-- /ds:block hash=d6d9d3 -->

One Pipeline can hold every codec at once. The example [`examples/imaging-codecs`](../../examples/imaging-codecs) builds one with WebP and AVIF, both ways:

<!-- ds:block id=modernpipeline-v4k9t2xs -->
**modernPipeline** · [`examples/imaging-codecs/main.go:43-48`](../../examples/imaging-codecs/main.go#L43-L48)

```go
func modernPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(
		imaging.WithEncoder(webp.Encoder()), imaging.WithDecoder(webp.Decoder()),
		imaging.WithEncoder(avif.Encoder()), imaging.WithDecoder(avif.Decoder()),
	)
}
```
<!-- /ds:block hash=693cf3 -->

and writes the same thumbnail in each format. Only `Format` (and the quality that suits it) changes between the transforms:

<!-- ds:block id=thumbnails-c7m2w8rq -->
**thumbnails** · [`examples/imaging-codecs/main.go:53-66`](../../examples/imaging-codecs/main.go#L53-L66)

```go
func thumbnails(w io.Writer, p *imaging.Pipeline, src []byte) error {
	for _, t := range []imaging.Transform{
		{Mode: imaging.ModeFit, Width: thumbSide, Height: thumbSide, Format: imaging.FormatJPEG},
		{Mode: imaging.ModeFit, Width: thumbSide, Height: thumbSide, Format: imaging.FormatWebP, Quality: webpQuality},
		{Mode: imaging.ModeFit, Width: thumbSide, Height: thumbSide, Format: imaging.FormatAVIF, Quality: avifQuality},
	} {
		res, err := p.Process(bytes.NewReader(src), t, imaging.Options{})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-10s %dx%d %s\n", res.ContentType, res.Width, res.Height, t.Extension())
	}
	return nil
}
```
<!-- /ds:block hash=a49ae4 -->

It prints `image/jpeg 200x150 .jpg`, `image/webp 200x150 .webp` and `image/avif 200x150 .avif`, and builds with `CGO_ENABLED=0`.

Check: each module's `go test` runs these as Go examples. The WebP one prints `image/webp 200 150 .webp`, the AVIF one `image/avif 150 100 .avif`, and the HEIC one `IMG_0042.jpg image/jpeg true`.

What to know about the underlying libraries:

- **A system library wins when present.** When the program can load a system libwebp, libavif or libheif (a `.so`, `.dylib` or `.dll`), the library uses it instead of the built-in code, so output bytes can differ between machines. Build with `-tags nodynamic` to always use the built-in code. `webp.Native()`, `avif.Native()` and `heic.Native()` report which one runs.
- **FreeBSD needs the tag.** On FreeBSD without cgo, the WebP and AVIF modules build only with `-tags nodynamic`.
- **They register with image.Decode.** Importing a module registers its format with the standard `image` package, as most codec packages do. `contrib/imaging` itself registers nothing.
- **Probe reports the stored size.** AVIF and WebP files decode upright, but `Probe` reports the stored size, before an `irot` box or EXIF rotation turns it. HEIC decodes upright too.
- **Tolerances.** Lossy WebP, AVIF and HEIC subsample colour, so a flat colour comes back within a few levels. The tests check decoded pixels against the source within such a tolerance, and lossless WebP and AVIF exactly.

## Giving the plugins a processor

- **Variants:** `variants.WithProcessor(proc)` makes every variant with `proc`. When `proc` can say which formats it writes (a Pipeline's `Encodes`), `variants.New` checks every declared transform against it. A WebP variant without the WebP encoder then fails at start-up with `unsupported`, instead of on every request.
- **Dimensions:** `dimensions.WithProcessor(proc, "image/heic")` reads headers with `proc` and also measures the extra content types, so phone photos in HEIC get their size too. Pass the same plugin as `BackfillOptions.Plugin` so the backfill measures them as well.
- **Sanitize:** `imaging.Sanitize(opts, imaging.WithPipeline(p))` decodes with `p`. Camera formats (JPEG, HEIC, HEIF, AVIF) become JPEG; the others become PNG. The file name's extension follows.

## Checking dimensions on upload

`imaging.CheckDimensions(imaging.DimensionLimits{...})` returns an `upload.Inspector` for `upload.Policy.Inspect` that refuses an image outside the limits: an avatar smaller than 200 × 200, a banner wider than 4,000 pixels, a scan over 40 megapixels. An Inspector runs on every upload path (multipart, presigned uploads confirmed with `upload.Confirm`, `tus`, `FromURL`), so a client cannot skip it by uploading straight to the bucket.

| Field | Refuses |
|---|---|
| `MinWidth`, `MinHeight` | an image narrower or lower than this |
| `MaxWidth`, `MaxHeight` | an image wider or higher than this |
| `MaxPixels` | width × height above this: 10,000 × 10,000 passes two side limits of 10,000 yet takes 400 MB to decode |
| `Aspect`, `AspectTolerance` | a shape other than width ÷ height = `Aspect` (16.0/9 for a video poster, 1 for an avatar), within `AspectTolerance` of it (a fraction; 0 means `DefaultAspectTolerance`, 1%), so 1080 × 1921 still passes as 9:16 |

Zero fields are not checked. Sizes are as displayed, after the EXIF rotation, so a portrait phone photo is checked as portrait.

How it behaves:

- **Header only.** It reads `imaging.ProbeBytes` (128 KiB) with `Disk.ReadRange` and parses the header, so a decompression bomb costs nothing.
- **Readable refusals.** A refused upload is `rejected` (415 over HTTP), with a message the client can show: `image is 150x150; it must be at least 200 pixels wide`. The stored object is deleted, as for any failed inspection.
- **What passes untouched.** Files that are not images pass, and so does SVG, which has no pixel size. Allow types with `Policy.Allowed`, not here.
- **Unreadable images are refused.** An image whose header cannot be read is refused, because its size cannot be verified.
- **Bad limits fail at start-up.** A negative limit, or a minimum above its maximum, is `invalid_argument` from `CheckDimensions` itself.
- **Other formats.** `imaging.WithProber(p)` reads headers with a Pipeline that has codec modules, for HEIC or AVIF uploads.

Combine it with other inspectors through `upload.Inspectors(scan, check)`.

The example [`examples/imaging-uploads`](../../examples/imaging-uploads) puts `CheckDimensions` and `Optimize` in one policy:

<!-- ds:block id=photopolicy-psgqc2r8 -->
**photoPolicy** · [`examples/imaging-uploads/main.go:61-74`](../../examples/imaging-uploads/main.go#L61-L74)

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

Check: `go run ./imaging-uploads` in `examples/` prints `icon.jpg refused: rejected: filesystem: rejected: image is 120x120; it must be at least 200 pixels wide`, and stores the 370,170-byte camera photo in 168,554 bytes.

## Making uploads smaller

`imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{Quality: 82})` returns an `upload.Processor` for `upload.Policy.Process` that shrinks JPEG and PNG uploads without visible loss. Where `Sanitize` always re-encodes, `Optimize` works losslessly first:

| Step | JPEG | PNG |
|---|---|---|
| Metadata removed without touching the pixels | EXIF (with the GPS position), XMP, IPTC, comments, every other application segment, anything after the end marker | text chunks, `eXIf`, `tIME` and every other ancillary chunk, anything after `IEND` |
| Kept | the JFIF header, the ICC colour profile, the Adobe segment (CMYK files need it) | transparency (`tRNS`), colour (`iCCP`, `sRGB`, `gAMA`, `cHRM`, `sBIT`), `pHYs`, the APNG animation chunks |
| Recompressed | only when saved above `Quality` (the floor), at `Quality`, and kept only if smaller | at the highest zlib level, kept only if smaller; never when a colour chunk or an animation is present, which Go's encoder would drop |

How it behaves:

- **The quality floor.** A JPEG's saved quality is read from its quantisation table. A photo saved at 95 is recompressed at the floor; one saved at 70 is left at 70 and only stripped, so a file never loses quality twice.
- **Rotation.** A JPEG that EXIF rotates is re-encoded upright, because without its EXIF it would display sideways.
- **Never larger.** The result is never larger than the upload, except a rotated JPEG, which must be re-encoded.
- **Other types and limits.** Other content types pass through untouched. Over `Options.MaxBytes` is `too_large`, a pixel bomb is refused before decoding, and a file whose JPEG or PNG structure is broken is `rejected`.

Use `Optimize` where the original must stay as close as possible (a photo library, product shots). Use `Sanitize` where every byte must be rewritten (untrusted uploads, which can carry payloads inside valid-looking segments).

### Keeping copyright and comments

Stripping is a privacy floor, so `Optimize` keeps nothing by default. An application that must keep authorship names it:

```go
imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{
	Quality:      82,
	KeepMetadata: []imaging.Metadata{imaging.MetadataCopyright},
})
```

`MetadataCopyright` keeps the IPTC copyright notice, by-line, credit and source (rewritten into a Photoshop segment that holds nothing else, so the IPTC city and any embedded XMP still go) and a PNG's `Copyright` and `Author` text chunks. `MetadataComment` keeps JPEG comments and a PNG's `Comment` and `Description` text. Kept segments survive the re-encode of a rotated or recompressed file. `MetadataCaption` keeps the IPTC caption (2:120) in the same rebuilt segment. Embedded XMP goes unless you name properties of it (next section); an unknown kind is `invalid_argument`.

Photo agencies and cameras often put authorship in EXIF instead. `MetadataEXIFCopyright` keeps the EXIF Artist and Copyright fields, and only those:

```go
KeepMetadata: []imaging.Metadata{imaging.MetadataCopyright, imaging.MetadataEXIFCopyright},
```

The source's EXIF is never copied or filtered: `Optimize` reads the two text fields from it and writes a new EXIF block (a JPEG's APP1 segment, a PNG's `eXIf` chunk) holding them and nothing else. The GPS position, camera, time stamps, thumbnail and orientation cannot survive, and a source with neither field keeps no EXIF at all. Each field is capped at 4 KiB.

Try it: [`examples/imaging-uploads`](../../examples/imaging-uploads) runs this section in `exif.go` ([`keepAuthorship`](ds:block?id=keepauthorship-n4v8k2qe), [`exifAuthorship`](ds:block?id=exifauthorship-w7c3m9tf)): `go run ./imaging-uploads` from `examples/`, then look for `exif GPS kept: false, position anywhere in the file: false, camera make kept: false`.

### Licensing fields in XMP

Google Images shows a "Licensable" badge from IPTC fields kept in XMP (`xmpRights:WebStatement`, `plus:Licensor`, `photoshop:Credit`, `dc:creator`, `dc:rights`), and Creative Commons marks its license there (`cc:license`). `KeepXMP` names the XMP properties to keep, by namespace URI and name, with constants for the common ones:

```go
imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{
	KeepMetadata: []imaging.Metadata{imaging.MetadataCopyright},
	KeepXMP: []imaging.XMPProperty{
		imaging.XMPWebStatement, imaging.XMPLicensor, imaging.XMPCredit, imaging.XMPCCLicense,
		{Namespace: "https://example.com/ns/licensing/", Name: "LicenseID"}, // your own property
	},
})
```

As with EXIF, nothing is filtered: the named properties' values (text, a resource URI, a language alternative, a list, or a struct of simple fields such as a licensor's name and URL) are read and written as escaped text into a new XMP packet (a JPEG's APP1 segment, a PNG's `iTXt` chunk) that holds nothing else. GPS is never kept, whatever the list says: `imaging.CheckXMP` refuses any EXIF or TIFF property (where the GPS position lives), any place (`photoshop:City`, `Iptc4xmpExt:LocationShown` and the like) and any name holding "GPS", with `invalid_argument`, and such a field inside a kept struct is dropped. A source with none of the named properties keeps no XMP, and a malformed packet keeps nothing.

### Keeping metadata on resized variants

`Optimize` works on the upload; a variant made by `Process` keeps nothing by default. `Transform.KeepMetadata` and `Transform.KeepXMP` carry the same kinds and properties, rebuilt the same way, into the output:

```go
t := imaging.Transform{Mode: imaging.ModeFit, Width: 1200, Height: 1200, Format: imaging.FormatJPEG,
	KeepMetadata: []imaging.Metadata{imaging.MetadataCopyright, imaging.MetadataEXIFCopyright},
	KeepXMP:      []imaging.XMPProperty{imaging.XMPWebStatement, imaging.XMPLicensor},
}
```

The output must be JPEG or PNG, the formats whose metadata this package writes (`unsupported` for any other), and metadata is kept only from a source of the same format: a PNG made from a JPEG keeps nothing. Both lists join `Transform.Digest` only when set, so existing variant keys do not change.

## Smart crops, focal points, watermarks, animation and colour

These are fields of a `Transform` (`Gravity: imaging.GravitySmart`, `GravityFocal` with `Focal`, `Watermark`, `FirstFrame`) and one function (`imaging.DominantColour`), so the variants plugin and your own handlers get them alike. [Image transforms](image-transforms.md) walks through each with a runnable step: [smart crop and focal point](image-transforms.md#step-3--crop-around-what-matters-gravity-smart-crop-focal-point), [watermarks](image-transforms.md#step-4--draw-a-watermark), [animated GIFs](image-transforms.md#step-5--keep-an-animated-gif-animated) and [the dominant colour](image-transforms.md#step-6--a-placeholder-colour). What matters here, for the engine: watermarks are registered on a Pipeline (`WithWatermark`), so a Processor of your own must offer the same if your transforms use them, and a watermark changes the transform's `Digest`.

## Colour profiles, SVG and golden tests

**Colour profiles.** Phones and cameras embed an ICC profile (Display P3, Adobe RGB) that says what the numbers in the pixels mean. A re-encode drops it, and the picture then looks washed out in a browser, which assumes sRGB. `imaging.WithColourManagement(conv)` makes a Pipeline handle it in `Process` and in `Sanitize` (through `WithPipeline`):

<!-- ds:block id=managedpipeline-h3w8n5kc -->
**managedPipeline** · [`examples/imaging-codecs/main.go:73-75`](../../examples/imaging-codecs/main.go#L73-L75)

```go
func managedPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithColourManagement(icc.ToSRGB))
}
```
<!-- /ds:block hash=1c2623 -->

Check: [`examples/imaging-codecs`](../../examples/imaging-codecs) processes a PNG tagged Display P3 (red 180) both ways and prints `dropped profile 180, converted to sRGB 190, profile kept false`: without management the numbers are kept and the colour now reads as a duller sRGB red; with it the pixels are converted to the same colour in sRGB.

- **Conversion.** An image with a profile is converted to sRGB by `conv`, and the output carries no profile.
- **Fallback.** When `conv` is nil, or cannot handle the profile, the profile is written into JPEG and PNG output instead, so the colours stay right. WebP, AVIF and GIF output is written without it.
- **Off by default.** Without the option, profiles are dropped as before.
- **The pure-Go converter.** `contrib/imaging/icc` imports only the standard library and handles RGB matrix profiles: three colorants and three tone curves, which is what phones and cameras embed. Tests check it against the published Display P3 to sRGB matrix within three levels.
- **Unsupported profiles.** Lookup-table, CMYK and grey profiles are reported unsupported, so the profile is kept. `imaging.ColourConverter` is the seam for an lcms2 converter in its own module.
- **Finding the profile.** `imaging.EmbeddedProfile(data)` returns the profile of a JPEG (its `APP2` segments, reassembled in order) or a PNG (its `iCCP` chunk) without decoding pixels. A profile larger than `MaxProfileBytes` (4 MiB) is ignored: it is treated as not found, and the image is converted as if it had no profile. A PNG's compressed profile is never inflated past that limit.
- **Crafted profiles.** A tone curve that is not finite for every input (a negative gamma, say) makes the profile malformed, so it is kept rather than converted.

**SVG.** `contrib/svg/raster` adds an SVG `Decoder`, so an uploaded SVG gets raster variants, dimensions and a ThumbHash:

```go
p := imaging.NewPipeline(imaging.WithDecoder(raster.Decoder(raster.Options{Side: 1024})))
```

- **Sanitised first.** Every document goes through `contrib/svg`'s sanitiser before drawing: scripts, event handlers, external references and foreign content are removed, and its size, depth and element limits apply (`Options.Sanitize`).
- **Size.** The drawing's longest side is `Options.Side` (default `DefaultSide`, 2048; at most `MaxSide`, 8192), from the document's width and height, or its viewBox, or the browser default of 300 × 150. The Pipeline's `MaxPixels` still applies.
- **Time.** A rendering that takes longer than `Options.Timeout` (default 10 seconds) fails with `rejected`. The renderer cannot be interrupted, so it finishes in the background, bounded by the limits above. At most `Options.MaxRenders` renderings (default 4) run at once, timed-out ones included; another is refused with `unavailable` until one ends.
- **Renderer.** The default renderer is pure Go (oksvg). It draws shapes, paths, gradients and transforms, but not text or filters, which come out missing. `Options.Render` takes another, such as resvg compiled to WebAssembly.

**Golden image tests.** `contrib/imaging/imagingtest` compares a test's output image with a stored PNG by how different it looks, not byte for byte. A one-level rounding change from an encoder passes; a crop in the wrong place or a missing watermark fails.

```go
imagingtest.Golden(t, got, "testdata/card.golden.png")
```

- **What it measures.** `Compare(a, b, threshold)` gives the mean and largest per-pixel distance. The distance is in colour weighted by perceived brightness, over mid grey for transparency, plus the fraction of pixels farther apart than the threshold.
- **Limits.** `Golden` fails above `DefaultMaxMean` (1 level) or `DefaultMaxDifferent` (0.1% of pixels past 16 levels), tunable in `imagingtest.Options`. It saves what it got in a temporary directory and names it in the failure.
- **Updating.** Running the test with `-update` (the test package's own flag) or `Options.Update` writes the golden file instead. The imaging and SVG tests use it.

## Another backend

A different engine (libvips through cgo, a remote image service) implements `Processor` in its own module and is passed wherever a processor is accepted. It must keep the contract the plugins rely on:

- `Transform.Validate` first;
- the geometry of each `Mode` (fit never enlarges, fill crops to the exact box by `Gravity`, pad centres on an exact canvas);
- `Result.ContentType` equal to `Transform.ContentType()`;
- the limits in `Options`;
- the same error codes: `invalid_argument` for a bad transform, `too_large` over `MaxBytes`, `rejected` for input that is not a supported image or is over `MaxPixels`, `unsupported` for a format it cannot write.

Anything that needs cgo or a native library lives in a module of its own, never in `contrib/imaging` itself, so the default build stays pure Go.

## Formats outside the built-in set

`FormatValues` is the set the package itself can describe. An encoder for another format (JPEG XL, HEIF) declares its output by also implementing `imaging.DescribedEncoder`:

```go
type jxlEncoder struct{}

func (jxlEncoder) Format() imaging.Format { return "jxl" }
func (jxlEncoder) Encode(w io.Writer, img image.Image, quality int) error {
	return encodeJXL(w, img, quality) // your codec
}
func (jxlEncoder) ContentType() string { return "image/jxl" }
func (jxlEncoder) Extension() string   { return ".jxl" }

p := imaging.NewPipeline(imaging.WithEncoder(jxlEncoder{}))
if err := p.Err(); err != nil { // a missing or unsafe declaration
	return err
}
t := imaging.Transform{Mode: imaging.ModeFit, Width: 800, Height: 800, Format: "jxl"}
res, err := p.Process(r, t, imaging.Options{}) // res.ContentType is "image/jxl"
```

The declared type must be an image type other than SVG and the extension a dot and up to 8 lower-case letters or digits, since stored variants are served under them. `Transform.Validate` still knows only `FormatValues`; `t.ValidateFor(p)` also accepts what `p` encodes, and `p.FormatInfo(f)` names any format it can. A declaration cannot relabel a built-in format.

### Per-transform encoder settings

`Transform.Lossless` and `Transform.Effort` (1 fastest to `MaxEffort` 10 smallest) reach an encoder that implements `imaging.TunedEncoder`: the `webp` and `avif` encoders and the built-in PNG one. JPEG and GIF are not, and a transform asking them for either is `unsupported` rather than silently ignored. `Transform.Background` fills transparency with a colour in every output format (a brand colour behind a padded logo); nil keeps the transparent border JPEG flattens to white.

## Pitfalls

- `unsupported: imaging: no encoder for webp` (or avif): the Pipeline in use lacks the codec. Every place that processes must get the same Pipeline: `variants.WithProcessor`, `dimensions.WithProcessor`, `imaging.WithPipeline` for `Sanitize`, `imaging.WithProber` for `CheckDimensions`. The package-level `imaging.Process` uses the default Pipeline, which never has codec modules.
- Different bytes on different machines: a system libwebp, libavif or libheif was loaded. Build with `-tags nodynamic` where reproducible output matters (golden tests, content-addressed variants).
- Phone photos look washed out after a resize: the colour profile was dropped. Add `WithColourManagement(icc.ToSRGB)`.
- AVIF variants make the first page view slow: encoding takes seconds. Make them eager (`variants.WithEager`).
- HEIC uploads have no dimensions recorded: give the dimensions plugin the HEIC Pipeline with `dimensions.WithProcessor(p, heic.ContentType)`.
- `Pipeline.Err()` is not nil: an option was invalid (an empty watermark image). Check it at start-up; every `Process` call returns it otherwise.

## Next

- [Variants](variants.md): named, cached outputs of a Transform.
- [Dimensions](dimensions.md): width and height on every image row.
- [Strip photo metadata](../recipes/strip-photo-metadata.md): `Sanitize` on every upload.
- The [imaging README](../../contrib/imaging/README.md) is the reference for the package.
