# contrib/imaging

`github.com/ubgo/filesystem/contrib/imaging` resizes and re-encodes images in pure Go, safely enough to run on user uploads. It reads an image's dimensions from its header before decoding it (a 1 KB file can declare a 100,000 × 100,000 image and exhaust memory when decoded), applies the EXIF orientation phones write, and re-encodes every output, which drops EXIF, GPS and anything smuggled after the image. It is its own module because it depends on `golang.org/x/image`.

```sh
go get github.com/ubgo/filesystem/contrib/imaging
```

Inputs: JPEG, PNG, GIF (first frame), WebP, BMP and TIFF. Outputs: JPEG and PNG; WebP and AVIF output and HEIC input come from the codec modules under this directory. Resampling is Catmull-Rom from `golang.org/x/image/draw`. The work is done by a `Processor`; the built-in `*Pipeline` is pure Go, and the guide [Image processing](../../docs/plugins/image-processing.md) explains how to add codecs or swap the backend.

Two records plugins build on it, and one standalone package sits beside it, each with its own reference README:

| Package | Does |
|---|---|
| [`contrib/imaging/variants`](variants/README.md) | named, cached image variants of stored files ("thumb", "avatar-64"), generated on first request or eagerly after an upload commits, served by an HTTP handler; only declared names can be generated |
| [`contrib/imaging/dimensions`](dimensions/README.md) | stores the width and height of every raster image in its file's metadata, from the header only |
| [`contrib/imaging/webp`](webp/README.md), [`avif`](avif/README.md), [`heic`](heic/README.md) | codec modules, each its own Go module: WebP output, AVIF output and input, HEIC input, added to a `Pipeline` with `WithEncoder` and `WithDecoder`; pure Go (no cgo) |
| [`contrib/imaging/focal`](focal/README.md) | stores a focal point per file (`Set`, `Of`); variants declared with `GravityFocal` crop around it (`variants.WithFocal`) |
| [`contrib/imaging/icc`](icc/README.md) | its own module, standard library only: converts RGB matrix/TRC ICC profiles (Display P3, Adobe RGB) to sRGB for `WithColourManagement` |
| [`contrib/imaging/imagingtest`](imagingtest/README.md) | golden-image tests by perceptual difference, with `-update` |
| [`contrib/imaging/placeholder`](placeholder/README.md) | the fallback image for a user without an avatar: initials on a seeded colour, or an identicon, as deterministic PNGs, with a cache-friendly HTTP handler |

Guides: [strip photo metadata](../../docs/recipes/strip-photo-metadata.md) (`Sanitize`), [avatars](../../docs/recipes/avatars.md) and [product images](../../docs/recipes/product-images.md) (crops and variants), [variants](../../docs/plugins/variants.md), [dimensions](../../docs/plugins/dimensions.md).

## API

| Name | What it is |
|---|---|
| `Processor` | the interface the plugins use: `Process` and `Probe`. `*Pipeline` implements it; another backend can too |
| `NewPipeline(...PipelineOption) *Pipeline`, `Default() *Pipeline` | a pure-Go Processor with the built-in codecs plus `WithDecoder(d)` (tried before the built-ins) and `WithEncoder(e)` (replaces the encoder for its format); `Default()` is the built-in one the package functions use. `Pipeline.Encodes(f)` reports whether it writes a format |
| `Decoder`, `Encoder` | one input format (`Name`, `Match(head)`, `DecodeConfig`, `Decode`) and one output format (`Format`, `Encode(w, img, quality)`), as the codec modules export them |
| `DescribedEncoder` (`ContentType()`, `Extension()`) | an `Encoder` of a format outside `FormatValues` (JPEG XL, HEIF) declares what it writes; `WithEncoder` records a configuration error (`Err`) for one that does not, or that declares a type other than `image/*`, `image/svg+xml`, or a malformed extension. For formats of `FormatValues` the built-in type and extension always win |
| `Pipeline.FormatInfo(f) (contentType, extension string, ok bool)`, `Pipeline.Formats()`, `Pipeline.Encodes(f)` | what a Pipeline can name and encode, custom formats included; the variants plugin uses them for keys, negotiation and `Prune` |
| `TunedEncoder` (`EncodeTuned(w, img, EncodeSettings{Quality, Lossless, Effort})`) | an encoder taking per-transform settings beyond quality; the built-in PNG encoder (Effort picks the zlib level) and the `webp` and `avif` encoders are ones. A transform setting `Lossless` or `Effort` on an encoder that is not one is `unsupported` rather than ignored |
| `Process(r io.Reader, t Transform, opts Options) (Result, error)` | reads an image and applies `t`; `invalid_argument` for a bad transform, `too_large` for input over `MaxBytes`, `rejected` for input that is not a supported image or has more than `MaxPixels` |
| `CheckDimensions(DimensionLimits, ...DimensionsOption) (upload.Inspector, error)` | for `upload.Policy.Inspect`: refuses an image outside `MinWidth`, `MinHeight`, `MaxWidth`, `MaxHeight` or `MaxPixels` (zero: unchecked) with `rejected` and a message naming the bound, reading the header only; non-images and SVG pass, an unreadable image is refused; `WithProber(p)` reads headers with another Processor; inconsistent limits are `invalid_argument` at construction |
| `ThumbHash(img image.Image) []byte`, `ThumbHashOf(proc, r, opts) ([]byte, error)` | the ThumbHash of an image (scaled to fit 100 × 100 first): a placeholder of about 25 bytes any ThumbHash decoder draws; `ThumbHashOf` decodes `r` with `proc` within `opts` first. The dimensions plugin stores it with `WithThumbHash` |
| `Optimize(opts Options, o OptimizeOptions) upload.Processor` | for `upload.Policy.Process`: makes JPEG and PNG smaller, lossless first. It strips metadata (EXIF and GPS, XMP, IPTC, comments, text chunks, trailing data) but keeps the colour profile, transparency and APNG chunks; recompresses a JPEG saved above `o.Quality` (the floor, default `DefaultOptimizeQuality` 82) and a PNG at the highest zlib level, each only when smaller; re-encodes an EXIF-rotated JPEG upright; never grows a file otherwise |
| `OptimizeOptions.KeepMetadata []Metadata`, `MetadataCopyright`, `MetadataComment`, `MetadataEXIFCopyright`, `MetadataValues` | opt-in allowlist of metadata `Optimize` keeps; nil (the default) strips it all. `MetadataCopyright` keeps the IPTC copyright, by-line, credit and source fields (rewritten into a segment holding nothing else) and PNG `Copyright`/`Author` text; `MetadataComment` keeps JPEG comments and PNG `Comment`/`Description` text. `MetadataEXIFCopyright` keeps the EXIF Artist (0x013B) and Copyright (0x8298) fields by writing a fresh EXIF block (a JPEG's APP1, a PNG's `eXIf`) holding only those two, so GPS, camera, time stamps, thumbnail and orientation never survive. `MetadataCaption` keeps the IPTC caption (2:120) in the same rebuilt segment. No other EXIF is ever kept; an unknown kind is `invalid_argument` |
| `OptimizeOptions.KeepXMP []XMPProperty`, `XMPProperty{Namespace, Name}`, `XMPWebStatement`, `XMPUsageTerms`, `XMPMarked`, `XMPLicensor`, `XMPCCLicense`, `XMPRights`, `XMPCreator`, `XMPDescription`, `XMPCredit`, `CheckXMP` | opt-in XMP allowlist; nil (the default) keeps no XMP. The named properties' values (text, a resource URI, a language alternative, a list, or a struct of simple fields) are copied as escaped text into an XMP packet rebuilt from scratch (a JPEG's APP1, a PNG's `iTXt`), so nothing unnamed survives. GPS is never kept: `CheckXMP` refuses any EXIF or TIFF property, any place (city, state, country, location) and any name holding "GPS" (`invalid_argument`), and such fields are dropped from a kept struct |
| `Transform.KeepMetadata`, `Transform.KeepXMP` | carry the same kinds and properties into a `Process` output (a resized variant keeping its creator and copyright); nil (the default) keeps nothing. The output must be JPEG or PNG (`unsupported` otherwise) and only a source of the same format has metadata to keep; both join `Digest` only when set |
| `GravitySmart`, `GravityFocal`, `Focal{X, Y}` | `ModeFill` crops that keep the most detailed region (edges, skin tones, saturation), or `Transform.Focal`, a point in fractions of the image |
| `Watermark{Name, Anchor, Opacity, Scale, Margin}`, `WithWatermark(name, img)`, `Anchor` | draw an image registered on the Pipeline over the output; part of the `Digest`. A nil or empty image is recorded as a configuration error, reported by `Pipeline.Err()` and returned by every `Process` |
| `FormatGIF`, `Transform.FirstFrame`, `Options.MaxFrames` | animated GIFs keep their animation (every frame composited, resized, watermarked); frame count and canvas × frames are checked before decoding |
| `DominantColour(img) (color.NRGBA, bool)`, `Hex(c)` | the most common colour, for a placeholder background |
| `Preview(proc, r, opts) (image.Image, error)` | a copy fitting 100 × 100 from one decode, as `ThumbHash` and `DominantColour` take it |
| `WithColourManagement(conv ColourConverter)`, `ColourConverter`, `EmbeddedProfile(data)` | handle embedded ICC profiles in `Process` and `Sanitize`: convert to sRGB with `conv` (`contrib/imaging/icc.ToSRGB`), or keep the profile in JPEG and PNG output when `conv` is nil or declines; off by default |
| `Probe(r io.Reader) (Info, error)` | the format and displayed dimensions from the first `ProbeBytes`, without decoding pixels; anything without a readable supported header is `rejected` |
| `Sanitize(opts Options, options ...SanitizeOption) upload.Processor` | for `upload.Policy.Process`: re-encodes JPEG, and HEIC, HEIF and AVIF when its Pipeline decodes them (as JPEG at `SanitizeQuality`, or `WithQuality`), and PNG, WebP, BMP and TIFF (as PNG), renaming the extension when the type changes, applying the EXIF orientation and dropping everything else the file carried; GIF and non-images pass through untouched; an image that does not decode or is over the limits is `rejected` or `too_large` |
| `SanitizeOption`, `WithQuality(q int)`, `WithPipeline(p)` | options for `Sanitize`; `WithQuality` sets the JPEG quality (1 to 100) in place of `SanitizeQuality`, so already-compressed photos do not grow when re-encoded; `WithPipeline` decodes with `p`, so formats its codec modules read (HEIC, AVIF) are sanitized too, and camera formats (JPEG, HEIC, HEIF, AVIF) come out as JPEG |
| `Transform` | one output: `Mode`, `Width`, `Height`, `Format`, `Quality` (JPEG, 1 to 100; 0 means `DefaultQuality`), `Gravity` (what `ModeFill` keeps), `Crop` (a region of the upright image, in its pixels, taken before resizing) |
| `Transform.Validate()` | refuses an unknown mode, format or gravity, a size outside 1 to `MaxDimension`, a quality outside 0 to 100 |
| `Transform.ValidateFor(p Processor)` | `Validate`, also accepting a format `p` says it encodes (an `Encodes` method, as `*Pipeline` has); `Validate` keeps the closed set |
| `Transform.Background *color.NRGBA`, `Transform.Lossless`, `Transform.Effort`, `MaxEffort` | fill transparency (the `ModePad` border and transparent pixels) with a colour in every format, nil keeping today's transparent border; ask a `TunedEncoder` for lossless output and an effort from 1 (fastest) to `MaxEffort` 10 (smallest). Each joins the digest only when set |
| `Transform.ContentType()`, `Extension()` | the output's MIME type and extension, with the dot, for a format of `FormatValues` ("" for any other: ask `Pipeline.FormatInfo`) |
| `Transform.Digest()` | identifies the output: equal transforms have equal digests, and any change, or a new resampling version, gives a new one; variants name cached results with it |
| `Mode`: `ModeFit`, `ModeFill`, `ModePad` | fit inside the box keeping the ratio, never enlarging; cover the box and crop the overflow (by `Gravity`); fit, then centre on a canvas of exactly the box, white for JPEG and transparent for PNG |
| `Format`: `FormatJPEG`, `FormatPNG`, `FormatWebP`, `FormatAVIF` | the output encodings; `FormatValues` lists them. WebP and AVIF need their codec module's encoder in the Pipeline, and are `unsupported` otherwise |
| `Gravity`: `GravityCenter` (zero value), `GravityTop`, `GravityBottom`, `GravityLeft`, `GravityRight` | the part `ModeFill` keeps; `GravityValues` lists them |
| `Options` | `MaxPixels`, `MaxBytes`; zero means the defaults |
| `Result` | `Data`, `ContentType`, `Width`, `Height` |
| `Info` | `Format` (the decoder's name), `Width`, `Height` as displayed: swapped for the EXIF orientations that rotate by 90 degrees |
| `ModeValues` | every `Mode` |

## Limits and defaults

| Constant | Value | Meaning |
|---|---|---|
| `MaxDimension` | 8192 | the largest `Width` and `Height` of a transform |
| `DefaultQuality` | 82 | JPEG quality when `Transform.Quality` is 0 |
| `DefaultMaxPixels` | 50,000,000 | inputs above 50 megapixels are refused before decoding |
| `DefaultMaxBytes` | 50 MiB | inputs above this are refused |
| `ProbeBytes` | 128 KiB | how much `Probe` reads; every supported format states its size near the start, and a JPEG's EXIF segment comes before the image data |
| `SanitizeQuality` | 90 | JPEG quality `Sanitize` re-encodes at unless `WithQuality(q)` sets another (1 to 100; outside that range every JPEG is refused with `invalid_argument`); high because the original is not kept, so photos saved at a lower quality grow |

Animated GIFs keep only their first frame when processed, which is why `Sanitize` passes GIFs through. A TIFF whose header points past `ProbeBytes` is refused by `Probe`. Processing runs in the calling goroutine and holds the decoded image in memory, roughly 4 bytes per pixel for RGBA images, so `MaxPixels` bounds one call; bound how many run at once yourself (a semaphore or worker pool) on busy servers.
