# Image transforms

You will turn one uploaded image into the image a page needs: resized to a box in the right mode, upright whatever the camera stored, cropped around what matters, watermarked, still animated if it was a GIF, with a placeholder colour, and never at the cost of decoding a pixel bomb. Everything here is a field of `imaging.Transform` or a function of `contrib/imaging`, so the same transform works in your own handler, in the [variants plugin](variants.md) and in [dimensions](dimensions.md). Use this page when you write transforms; use [Image processing](image-processing.md) when you change the engine behind them (WebP and AVIF codecs, colour management, another backend).

## Before you start

- `go get github.com/ubgo/filesystem/contrib/imaging`. It is pure Go (no cgo, no native library) and decodes JPEG, PNG, GIF, WebP, BMP and TIFF, and writes JPEG, PNG and GIF. WebP and AVIF output need their codec modules; [imaging-codecs](../../examples/imaging-codecs) shows them.
- The example is [`examples/imaging-transforms`](../../examples/imaging-transforms). It draws its images in code and needs no database or disk: `go run ./imaging-transforms` in `examples/` prints every step below, and `go test ./imaging-transforms` runs them with a golden image test.
- Nothing here stores anything. To keep transformed copies of recorded files (generated once, deleted with the original), declare these transforms as [variants](variants.md).

## Step 1 — Pick a resize mode

A `Transform` names a mode, a box (`Width` × `Height`, each 1 to `imaging.MaxDimension`, 8192), an output `Format` and, for lossy formats, a `Quality` (1 to 100; 0 means `DefaultQuality`, 82). The example puts a 400×200 photo into a 100×100 box four ways:

<!-- ds:block id=modes-t8w2k5nr -->
**modes** · [`examples/imaging-transforms/main.go:77-82`](../../examples/imaging-transforms/main.go#L77-L82)

```go
var modes = map[string]imaging.Transform{
	"fit":  {Mode: imaging.ModeFit, Width: box, Height: box, Format: imaging.FormatPNG},
	"fill": {Mode: imaging.ModeFill, Width: box, Height: box, Format: imaging.FormatJPEG, Quality: 80},
	"pad":  {Mode: imaging.ModePad, Width: box, Height: box, Format: imaging.FormatPNG},
	"crop": {Mode: imaging.ModeFill, Width: box, Height: box, Format: imaging.FormatPNG, Crop: image.Rect(300, 0, 400, 100)},
}
```
<!-- /ds:block hash=76d6de -->

| Mode | Output for 400×200 into 100×100 | Use it for |
|---|---|---|
| `ModeFit` | 100×50: the whole image inside the box, aspect kept | Previews, lightboxes, "max 1600 px" copies. It never enlarges, so it is also what other libraries call `limit`: a 60×30 image stays 60×30. |
| `ModeFill` | 100×100: covers the box, the overflow cropped by `Gravity` | Avatars, grid tiles, cards: anything that must be an exact size. It enlarges a smaller source to cover the box. |
| `ModePad` | 100×100: fits like `ModeFit`, then centred on a canvas of the box (white for JPEG, transparent for PNG) | Product grids that must be uniform but must not crop. |
| any, with `Crop` | The region is cut out of the upright image first, then the mode applies | The square a user picked in an avatar cropper. A region outside the image is `rejected` when applied. |

Each transform is applied with `imaging.Process(r, t, opts)`, which returns the encoded bytes with their type and size:

<!-- ds:block id=resizeall-m3q7v9cz -->
**resizeAll** · [`examples/imaging-transforms/main.go:87-96`](../../examples/imaging-transforms/main.go#L87-L96)

```go
func resizeAll(w io.Writer, src []byte) error {
	for _, name := range []string{"fit", "fill", "pad", "crop"} {
		res, err := imaging.Process(bytes.NewReader(src), modes[name], imaging.Options{})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-4s %dx%d %s\n", name, res.Width, res.Height, res.ContentType)
	}
	return nil
}
```
<!-- /ds:block hash=4bf402 -->

`Transform.Validate()` reports a bad mode, format, size, quality, gravity or crop with `invalid_argument`; `Process` calls it first, and the variants plugin calls it at start-up. `t.Digest()` names the output (equal transforms, equal digests; any change, a new digest), which is how variants know to regenerate.

Check: the demo prints `fit  100x50 image/png`, `fill 100x100 image/jpeg`, `pad  100x100 image/png` and `crop 100x100 image/png`.

## Step 2 — Let EXIF orientation turn the photo upright

A phone photo taken in portrait is usually stored landscape, with an EXIF orientation tag that says how to turn it. Nothing is needed on your side: `Process` reads the tag and applies it before cropping and resizing, so `Crop` coordinates and the output are in the upright image. `Probe` reports the displayed dimensions from the header alone, swapped for the orientations that rotate by 90 degrees:

```go
info, err := imaging.Probe(bytes.NewReader(rotated)) // 200x300 for a 300x200 JPEG tagged 6
```

The output carries no EXIF, so it cannot be turned twice. Check: the example writes a 300×200 JPEG tagged 6 and prints `exif 6: stored 300x200, probe 200x300, output 200x300`. Stripping location and camera data from what you store (not only from transformed copies) is [`imaging.Sanitize`](../recipes/strip-photo-metadata.md).

## Step 3 — Crop around what matters: gravity, smart crop, focal point

`ModeFill` must drop part of the image. `Gravity` decides which part:

| Gravity | Keeps |
|---|---|
| `GravityCenter` (the zero value) | The centre |
| `GravityTop`, `GravityBottom`, `GravityLeft`, `GravityRight` | That edge. `GravityTop` keeps the head of a portrait. |
| `GravitySmart` | The most detailed part: each pixel of a 128-pixel copy is scored for edges, saturated colour and skin tones, after smartcrop.js, and the best window wins. A featureless image is cropped from the centre. |
| `GravityFocal` | `Transform.Focal`, a point in fractions of the upright image (`{X: 0.5, Y: 0.5}` is the centre), as near the crop's centre as the edges allow. Without a `Focal` it is the centre. |

<!-- ds:block id=smartcard-x5r8n2wk -->
**smartCard** · [`examples/imaging-transforms/main.go:101-104`](../../examples/imaging-transforms/main.go#L101-L104)

```go
var smartCard = imaging.Transform{
	Mode: imaging.ModeFill, Width: box, Height: box, Format: imaging.FormatPNG,
	Gravity: imaging.GravitySmart,
}
```
<!-- /ds:block hash=bbc918 -->

When to use which: `GravitySmart` when you know nothing about the image (user uploads, product photos on plain backgrounds); `GravityFocal` when someone or something already knows where the subject is (an editor clicked it, a face detector found it). The [`contrib/imaging/focal`](variants.md#focal-points-and-format-negotiation) plugin stores a focal point per recorded file and the variants plugin passes it in. Smart crop favours skin tones but it is not a face detector.

Check: the example's photo is plain grey on the left and a detailed checkerboard on the right quarter. It prints `checkerboard kept: smart crop 50%, centre crop 0%`.

## Step 4 — Draw a watermark

Register the watermark image once on a Pipeline (it is converted there, so a request decodes nothing), then name it in the transform:

<!-- ds:block id=watermarked-c9v4h7ma -->
**watermarked** · [`examples/imaging-transforms/main.go:125-135`](../../examples/imaging-transforms/main.go#L125-L135)

```go
func watermarked() (*imaging.Pipeline, imaging.Transform, error) {
	p := imaging.NewPipeline(imaging.WithWatermark(logoName, logo()))
	if err := p.Err(); err != nil {
		return nil, imaging.Transform{}, err
	}
	t := imaging.Transform{
		Mode: imaging.ModeFit, Width: 200, Height: 200, Format: imaging.FormatPNG,
		Watermark: &imaging.Watermark{Name: logoName, Anchor: imaging.AnchorBottomRight, Opacity: 0.5, Scale: 0.2, Margin: 0.02},
	}
	return p, t, nil
}
```
<!-- /ds:block hash=336bb6 -->

- `Anchor` is `AnchorBottomRight` (the zero value), `AnchorBottomLeft`, `AnchorTopRight`, `AnchorTopLeft` or `AnchorCenter`.
- `Scale` is its width as a fraction of the output's (0 means `DefaultWatermarkScale`, 25%), `Opacity` 0 to 1 (0 means opaque), `Margin` the gap from the anchored edges as a fraction of the width (up to 0.5).
- A watermark taller than the output is fitted to its height. On an animated GIF it is scaled once and drawn on every frame.
- The watermark is part of the transform's `Digest`, so a watermarked variant never shares a key with a plain one.
- Check `p.Err()` at start-up: a nil or empty watermark image is recorded there, and every `Process` call on that Pipeline returns the same error.

Check: the demo prints `watermarked 200x100, bottom right corner changed true`.

### A coloured border, lossless output

```go
brand := color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xFF}
logo := imaging.Transform{Mode: imaging.ModePad, Width: 512, Height: 512, Format: imaging.FormatWebP,
	Background: &brand, Lossless: true, Effort: 8}
```

`Background` fills the pad border and the image's own transparent pixels; `Lossless` and `Effort` need an encoder that implements `imaging.TunedEncoder` (WebP, AVIF, PNG). Each joins the digest only when set, so existing variants keep their names.

## Step 5 — Keep an animated GIF animated

`FormatGIF` output keeps a GIF's animation: every frame is composited by the format's disposal rules, cropped, resized and watermarked, and the delays and loop count are kept. `FirstFrame` makes a still instead, and any other output format takes the first frame:

<!-- ds:block id=gifthumbs-p2k8w4xd -->
**gifThumbs** · [`examples/imaging-transforms/main.go:158-178`](../../examples/imaging-transforms/main.go#L158-L178)

```go
func gifThumbs(src []byte) (animated, still int, err error) {
	t := imaging.Transform{Mode: imaging.ModeFit, Width: 30, Height: 30, Format: imaging.FormatGIF}
	moving, err := imaging.Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return 0, 0, err
	}
	t.FirstFrame = true
	first, err := imaging.Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return 0, 0, err
	}
	a, err := gif.DecodeAll(bytes.NewReader(moving.Data))
	if err != nil {
		return 0, 0, err
	}
	s, err := gif.DecodeAll(bytes.NewReader(first.Data))
	if err != nil {
		return 0, 0, err
	}
	return len(a.Image), len(s.Image), nil
}
```
<!-- /ds:block hash=374162 -->

A smart crop is chosen on the first frame and kept for the rest, so it does not jump as the detail moves. Check: the demo prints `gif: animated 3 frames, first frame 1`.

## Step 6 — A placeholder colour

`imaging.DominantColour(img)` returns the most common colour: the average of the fullest of 4,096 colour buckets over an even grid of at most 64 × 64 samples, ignoring pixels less than half opaque. It is deterministic, so it can be stored. `imaging.Hex(c)` writes `#rrggbb` for a CSS background while the image loads:

<!-- ds:block id=placeholdercolour-w7n3c5qe -->
**placeholderColour** · [`examples/imaging-transforms/main.go:184-190`](../../examples/imaging-transforms/main.go#L184-L190)

```go
func placeholderColour(img image.Image) string {
	c, ok := imaging.DominantColour(img)
	if !ok {
		return ""
	}
	return imaging.Hex(c)
}
```
<!-- /ds:block hash=277bf3 -->

The bucket wins over the mean, which turns every photo brown. For recorded files, the dimensions plugin stores the colour with `dimensions.WithDominantColour` beside the ThumbHash, sharing one decode ([dimensions](dimensions.md#step-5--a-placeholder-while-the-image-loads-thumbhash)). Check: the demo prints `dominant colour #969696`, the grey that covers three quarters of the photo.

## Step 7 — Bound what is decoded

A 1 KB PNG can declare a 100,000 × 100,000 image; decoding it would allocate 40 GB. `Options` bounds every call, and the declared size is checked from the header before a single pixel is decoded:

| Option | Zero means | Refused with |
|---|---|---|
| `MaxBytes` | `DefaultMaxBytes`, 50 MiB of input | `too_large` |
| `MaxPixels` | `DefaultMaxPixels`, 50 megapixels; for an animation, the canvas times the frame count | `rejected` |
| `MaxFrames` | `DefaultMaxFrames`, 500; counted from the GIF's block structure before any frame is decoded | `rejected` |

<!-- ds:block id=toobig-r4j9m2vt -->
**tooBig** · [`examples/imaging-transforms/main.go:196-199`](../../examples/imaging-transforms/main.go#L196-L199)

```go
func tooBig(src []byte) error {
	_, err := imaging.Process(bytes.NewReader(src), modes["fit"], imaging.Options{MaxPixels: tinyPixels})
	return err
}
```
<!-- /ds:block hash=bcfad2 -->

The zero `Options{}` is already safe; set smaller limits on paths that only ever see avatars. Check: under a budget of 10,000 pixels the 80,000-pixel photo prints `over 10000 pixels: rejected`. Refusing images that are too small or too large at upload time, before they are stored, is `imaging.CheckDimensions` ([image processing](image-processing.md#checking-dimensions-on-upload)).

## Step 8 — Plug in a processor of your own

Everything above goes through one interface, `imaging.Processor`, with two methods: `Process` and `Probe`. `*imaging.Pipeline` is the implementation that ships, and anything that accepts a processor (`variants.WithProcessor`, the dimensions plugin, `CheckDimensions`) takes another. A wrapper is the smallest one: metrics, a cache, a rate limit, or a different engine for some formats.

<!-- ds:block id=countingprocessor-h6t2q8zn -->
**countingProcessor** · [`examples/imaging-transforms/main.go:206-209`](../../examples/imaging-transforms/main.go#L206-L209)

```go
type countingProcessor struct {
	next imaging.Processor
	n    atomic.Int64
}
```
<!-- /ds:block hash=5c5558 -->

A real replacement (libvips through cgo, a remote image service) must keep the contract the plugins rely on: `Validate` first, the geometry of each mode, `Result.ContentType` equal to `t.ContentType()`, the `Options` limits, and the same error codes. [Image processing](image-processing.md#another-backend) lists it. Check: the demo prints `custom processor ran 1 time(s)`.

## Step 9 — Test transforms with golden images

A byte-exact golden file fails whenever an encoder rounds one pixel differently between Go versions. `contrib/imaging/imagingtest` compares how different two images look instead: a one-level change passes, a misplaced crop or a missing watermark fails.

<!-- ds:block id=testwatermarkgolden-n5c8r3tw -->
**TestWatermarkGolden** · [`examples/imaging-transforms/main_test.go:42-60`](../../examples/imaging-transforms/main_test.go#L42-L60)

```go
func TestWatermarkGolden(t *testing.T) {
	src, err := encodePNG(photo())
	if err != nil {
		t.Fatal(err)
	}
	p, tr, err := watermarked()
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Process(bytes.NewReader(src), tr, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	imagingtest.Golden(t, got, "testdata/watermarked.golden.png", imagingtest.Options{Update: *update})
}
```
<!-- /ds:block hash=1c39c1 -->

`Golden` fails above a mean difference of `DefaultMaxMean` (1 level) or when more than `DefaultMaxDifferent` (0.1%) of pixels are farther apart than `DefaultPixelThreshold` (16 levels), tunable in `imagingtest.Options`. On failure it saves what it got under the temporary directory and names the file. A missing golden file fails with a hint. Write or rewrite it with the test package's `-update` flag:

```sh
cd examples
go test ./imaging-transforms -run Golden -update
```

## Try it

```sh
cd examples
go run ./imaging-transforms
go test ./imaging-transforms
```

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

## Pitfalls

- `unsupported: imaging: no encoder for webp`: WebP and AVIF output need their codec modules added to a Pipeline with `WithEncoder` ([imaging-codecs](../../examples/imaging-codecs)). The test `TestWebPNeedsItsCodec` pins it.
- `ModeFit` does not enlarge, so a 300-pixel upload never fills a 1280-pixel `srcset` slot. That is deliberate; describe the real width (the variants plugin's `SrcSet` does).
- `ModeFill` does enlarge. A 40-pixel upload filled to a 400-pixel card is blurry; refuse small uploads with `CheckDimensions`, or use `ModeFit` for large slots.
- `Crop` is in the upright image's pixels. A cropper that sends coordinates of the image as stored (before EXIF rotation) crops the wrong region; probe first and send the displayed size to the client.
- `invalid_argument: imaging: no watermark "…"`: the transform names a watermark the Pipeline it runs on did not register. The default Pipeline (`imaging.Process`) has none.
- Animated GIF output is far heavier than a still: each frame is decoded and encoded. `MaxFrames` and `MaxPixels` cover the canvas times the frames, but set them lower on request-time paths, or make GIF variants eager.
- Re-encoding drops an embedded colour profile unless the Pipeline has colour management, and photos from phones then look washed out. See [colour profiles](image-processing.md#colour-profiles-svg-and-golden-tests).

## Related

- [Image variants](variants.md): store these transforms for recorded files, with `srcset`, densities and eager generation.
- [Image processing](image-processing.md): codecs, colour management, SVG input and other backends.
- [Dimensions](dimensions.md): width, height, ThumbHash and the dominant colour stored on each file.
- [Avatars recipe](../recipes/avatars.md): a crop, a fill and a variant family end to end.
