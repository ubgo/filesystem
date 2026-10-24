# github.com/ubgo/filesystem/contrib/imaging/avif

> [!IMPORTANT]
> By default the underlying library loads a system libavif when one is installed (`libavif.so`, `.dylib` or `.dll`) and uses it instead of its built-in pure-Go code, so the same program can produce different bytes on different machines. Build and test with `-tags nodynamic` to always use the pure-Go code; `avif.Native()` reports which one is running. The tests pass under both, because they compare decoded pixels within a tolerance.

Adds AVIF output and input to an `imaging.Pipeline`. It wraps `github.com/gen2brain/avif`, libavif compiled to WebAssembly and run by wazero: no cgo. AVIF is often half the size of a JPEG of the same quality, but slow to encode (seconds for a large photo), so make AVIF variants eagerly (`variants.WithEager`) rather than on the first request for a page. The guide is [Image processing](../../../docs/plugins/image-processing.md#codec-modules-webp-avif-heic).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/imaging/avif
```

It is its own module, so an application that does not use it never downloads its dependency. Its production code may import only the standard library, `contrib/imaging` and its one codec, which `deps_test.go` checks.

## Wire it

<!-- ds:block id=avifpipeline-vkyty8te -->
**avifPipeline** · [`contrib/imaging/avif/example_test.go:18-20`](example_test.go#L18-L20)

```go
func avifPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(avif.Encoder()), imaging.WithDecoder(avif.Decoder()))
}
```
<!-- /ds:block hash=1265b2 -->

## Go API

| Symbol | What it is |
|---|---|
| `Encoder(opts ...Options) imaging.Encoder` | the encoder for `imaging.WithEncoder`: `FormatAVIF` at the Transform quality (alpha at the same quality); `Options.Speed` 1 (slowest, smallest) to 10 (0 means `DefaultSpeed`, 10); `Options.Lossless` writes lossless 4:4:4 AVIF |
| `imaging.TunedEncoder` (on the encoder) | a transform's `Lossless` adds to `Options.Lossless`, and its `Effort` (1 fastest to 10 smallest) replaces `Speed`, which runs the other way (effort 1 is speed 10) |
| `Decoder() imaging.Decoder` | the decoder for `imaging.WithDecoder` (brands `avif` and `avis`); images decode upright by their `irot`/`imir` boxes, while `DecodeConfig` (and `Probe`) report the stored size |
| `Native() bool` | whether a system libavif is in use instead of the WebAssembly build |

## Limits

- When the program can load the system library the codec wraps, it is used instead of the built-in code, so output bytes can differ between machines. Build with `-tags nodynamic` to always use the built-in code; on FreeBSD without cgo the module builds only with that tag.
- Importing the module registers its format with the standard `image` package (`image.Decode`).
- Lossy output subsamples colour: flat colours come back within a few levels. The tests check decoded pixels against the source within that tolerance, and lossless output exactly.
