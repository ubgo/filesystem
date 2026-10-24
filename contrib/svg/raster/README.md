# github.com/ubgo/filesystem/contrib/svg/raster

An SVG `imaging.Decoder`, so an uploaded SVG gets raster variants, dimensions and a ThumbHash through an ordinary `imaging.Pipeline`. Every document is sanitised by `contrib/svg` first. The guide is [Image processing: SVG](../../../docs/plugins/image-processing.md#colour-profiles-svg-and-golden-tests).

```go
p := imaging.NewPipeline(imaging.WithDecoder(raster.Decoder(raster.Options{Side: 1024})))
```

## Options

| Field | What it does | Default |
|---|---|---|
| `Side` | the longest side the document is drawn at, from its width and height, else its viewBox, else 300 × 150 | `DefaultSide` (2048), at most `MaxSide` (8192) |
| `Timeout` | a rendering that takes longer fails with `CodeRejected`; it finishes in the background, bounded by the other limits | `DefaultTimeout` (10 s) |
| `MaxRenders` | renderings in flight at once, counting ones that timed out but still run; another `Decode` is refused with `CodeUnavailable` | `DefaultMaxRenders` (4) |
| `Sanitize` | the `contrib/svg` limits and options applied before drawing | its defaults |
| `Render` | the `Renderer`, `func(doc io.Reader, w, h int) (*image.RGBA, error)` | oksvg, pure Go |

## Limits

- The default renderer (`github.com/srwiley/oksvg`) draws shapes, paths, gradients and transforms, not text or filters, and has not changed since 2022. Pass another `Render` (resvg compiled to WebAssembly) where text matters.
- A renderer that panics fails that decode with `CodeUnknown` and frees its slot; it never takes the process down.
- Production code imports only the core, imaging, the sanitiser and the renderer, which `deps_test.go` checks.
