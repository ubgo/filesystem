# Image transforms

What `contrib/imaging` does to one image, with no database and no disk: the three resize modes and a crop region, EXIF orientation, a smart crop, a watermark, an animated GIF kept animated, the dominant colour, the pixel-bomb limits, a `Processor` of its own wrapped around the Pipeline, and a golden image test. Every image is drawn in code, so it needs no files. The guide [Image transforms](../../docs/plugins/image-transforms.md) explains each step.

## Run it

```sh
cd examples
go run ./imaging-transforms
go test ./imaging-transforms
```

It needs no network, no cgo and no environment variables.

## What it does

```text
fit  100x50 image/png
fill 100x100 image/jpeg
pad  100x100 image/png
crop 100x100 image/png
exif 6: stored 300x200, probe 200x300, output 200x300
checkerboard kept: smart crop 50%, centre crop 0%
watermarked 200x100, bottom right corner changed true
gif: animated 3 frames, first frame 1
dominant colour #969696
over 10000 pixels: rejected
custom processor ran 1 time(s)
```

1. A 400×200 photo goes into a 100×100 box four ways: `ModeFit` keeps the whole image (100×50), `ModeFill` covers the box and crops, `ModePad` fits and pads to the exact box, and a `Crop` region (the square a user picked) is cut out before resizing.
2. A JPEG stored 300×200 with EXIF orientation 6 (a portrait phone photo) is probed and processed upright: 200×300.
3. The photo is grey on the left and a detailed checkerboard on the right. A square crop with `GravitySmart` keeps the checkerboard; the centre crop keeps none of it.
4. A logo registered once on a Pipeline is drawn in the bottom right corner of the output at half opacity.
5. A three-frame GIF stays animated with `FormatGIF`, and becomes one frame with `FirstFrame`.
6. `DominantColour` returns the most common colour, as `#rrggbb` for a placeholder.
7. Under `Options{MaxPixels: 10000}` the 80,000-pixel photo is refused with `rejected` before any pixel is decoded.
8. A `Processor` of the app's own wraps the default Pipeline and counts the images it processed.

`go test` also compares the watermarked output with `testdata/watermarked.golden.png` through `imagingtest.Golden`, and pins an unknown watermark, WebP without its codec, `ModeFit` on a small image and the frame limit. Run `go test ./imaging-transforms -run Golden -update` to rewrite the golden image after a deliberate change.

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the drawn images, the transforms and each step |
| `exif.go` | Writes a JPEG with an EXIF orientation tag, as a camera does |
| `main_test.go` | Runs the steps, the golden image test and the refusals |
| `testdata/watermarked.golden.png` | The golden image |

## Next

- The guide: [Image transforms](../../docs/plugins/image-transforms.md), then [Image processing](../../docs/plugins/image-processing.md) for codecs and other backends.
- Related examples: [imaging-codecs](../imaging-codecs) for WebP, AVIF and colour profiles, [records-variants](../records-variants) for stored variants of recorded files, [imaging-uploads](../imaging-uploads) for upload-time checks.
