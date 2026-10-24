# github.com/ubgo/filesystem/contrib/imaging/webp

> [!IMPORTANT]
> By default the underlying library loads a system libwebp when one is installed (`libwebp.so`, `.dylib` or `.dll`) and uses it instead of its built-in pure-Go code, so the same program can produce different bytes on different machines. Build and test with `-tags nodynamic` to always use the pure-Go code; `webp.Native()` reports which one is running. The tests pass under both, because they compare decoded pixels within a tolerance.

Adds WebP output, and a WebP decoder that also reads animated files (first frame) and EXIF-rotated photos, to an `imaging.Pipeline`. It wraps `github.com/gen2brain/webp`, libwebp translated to Go: no cgo. The guide is [Image processing](../../../docs/plugins/image-processing.md#codec-modules-webp-avif-heic).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/imaging/webp
```

It is its own module, so an application that does not use it never downloads its dependency. Its production code may import only the standard library, `contrib/imaging` and its one codec, which `deps_test.go` checks.

## Wire it

<!-- ds:block id=webppipeline-yw2vxhuh -->
**webpPipeline** · [`contrib/imaging/webp/example_test.go:18-20`](example_test.go#L18-L20)

```go
func webpPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(webp.Encoder()), imaging.WithDecoder(webp.Decoder()))
}
```
<!-- /ds:block hash=c8af80 -->

## Go API

| Symbol | What it is |
|---|---|
| `Encoder(opts ...Options) imaging.Encoder` | the encoder for `imaging.WithEncoder`: `FormatWebP` at the Transform quality; `Options.Lossless` writes lossless WebP (for screenshots and drawings), `Options.Method` is the speed/size trade-off 0 to 6 (0 means `DefaultMethod`, 4) |
| `imaging.TunedEncoder` (on the encoder) | a transform's `Lossless` adds to `Options.Lossless`, and its `Effort` (1 to 10) replaces `Method`, scaled onto 0 to 6 |
| `Decoder() imaging.Decoder` | the decoder for `imaging.WithDecoder`; it replaces the built-in `golang.org/x/image/webp`, adding animated files (first frame) and the EXIF orientation |
| `Native() bool` | whether a system libwebp is in use instead of the built-in code |

## Limits

- When the program can load the system library the codec wraps, it is used instead of the built-in code, so output bytes can differ between machines. Build with `-tags nodynamic` to always use the built-in code; on FreeBSD without cgo the module builds only with that tag.
- Importing the module registers its format with the standard `image` package (`image.Decode`).
- Lossy output subsamples colour: flat colours come back within a few levels. The tests check decoded pixels against the source within that tolerance, and lossless output exactly.
