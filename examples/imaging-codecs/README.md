# Image codecs: WebP, AVIF and colour profiles

The codec modules of `contrib/imaging`, in a module of their own so the main examples module does not depend on them: a Pipeline that writes WebP and AVIF as well as JPEG and PNG, and a Pipeline that converts a photo with an embedded Display P3 profile to sRGB. It uses `contrib/imaging`, `contrib/imaging/webp`, `contrib/imaging/avif` and `contrib/imaging/icc`. The guide [Image processing](../../docs/plugins/image-processing.md) explains each module.

## Run it

```sh
cd examples/imaging-codecs
go run .
go test .
CGO_ENABLED=0 go test .
```

It needs no network, no cgo and no environment variables. The WebP and AVIF codecs are WebAssembly builds run by wazero, so the program builds with `CGO_ENABLED=0`. When a system libwebp or libavif is installed, the codecs load it instead (`webp.Native()` and `avif.Native()` report it); build with `-tags nodynamic` to always use the built-in code. The output below is the same either way, because it prints dimensions and colours, not bytes.

## What it does

```text
image/jpeg 200x150 .jpg
image/webp 200x150 .webp
image/avif 200x150 .avif
display p3 red 180: dropped profile 180, converted to sRGB 190, profile kept false
```

1. A 400×300 photo is resized into a 200×200 box three times on one Pipeline, as JPEG, WebP (quality 75) and AVIF (quality 60).
2. A PNG tagged Display P3, built in `p3.go` as a phone would write it, is processed twice. The default Pipeline drops the profile and keeps the numbers (red 180), so the colour is now read as sRGB and looks duller. The Pipeline with `WithColourManagement(icc.ToSRGB)` converts the pixels to sRGB (red 190, the same colour) and writes no profile.

`go test` also decodes the WebP and AVIF outputs with the same Pipeline and probes their size, and checks that the lossless WebP encoder keeps every pixel.

## Files

| File | What it holds |
|---|---|
| `main.go` | The two Pipelines and the steps |
| `p3.go` | Builds a Display P3 ICC profile and a PNG that embeds it |
| `main_test.go` | Runs the steps, a round trip through each codec and the lossless WebP check |
| `go.mod` | The module, with `replace` lines back to this repository |

## Next

- The guide: [Image processing](../../docs/plugins/image-processing.md), and [Image transforms](../../docs/plugins/image-transforms.md) for what a transform does.
- Related examples: [imaging-transforms](../imaging-transforms) for resize modes, crops and watermarks, [records-variants](../records-variants) to store variants in these formats.
