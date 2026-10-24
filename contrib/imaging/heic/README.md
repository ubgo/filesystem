# github.com/ubgo/filesystem/contrib/imaging/heic

> [!IMPORTANT]
> By default the underlying library loads a system libheif when one is installed (`libheif.so`, `.dylib` or `.dll`) and uses it instead of its built-in pure-Go code, so the same program can produce different bytes on different machines. Build and test with `-tags nodynamic` to always use the pure-Go code; `heic.Native()` reports which one is running. The tests pass under both, because they compare decoded pixels within a tolerance.

Adds HEIC input to an `imaging.Pipeline`, so iPhone photos are resized, measured and sanitized like JPEGs. It wraps `github.com/gen2brain/heic`, a decoder compiled to WebAssembly and run by wazero: no cgo. There is no HEIC encoder: HEIC uploads become JPEG variants, or a JPEG original with `imaging.Sanitize` and `WithPipeline`. The guide is [Image processing](../../../docs/plugins/image-processing.md#codec-modules-webp-avif-heic).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/imaging/heic
```

It is its own module, so an application that does not use it never downloads its dependency. Its production code may import only the standard library, `contrib/imaging` and its one codec, which `deps_test.go` checks.

## Wire it

<!-- ds:block id=heicpolicy-6698j5h6 -->
**heicPolicy** · [`contrib/imaging/heic/example_test.go:19-26`](example_test.go#L19-L26)

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

## Go API

| Symbol | What it is |
|---|---|
| `Decoder() imaging.Decoder` | the decoder for `imaging.WithDecoder` (brands `heic`, `heix`, `hevc`, `hevx`, `msf1`); images decode upright |
| `ContentType` | `image/heic`, as uploads are detected; pass it to `dimensions.WithProcessor` so HEIC files are measured |
| `Native() bool` | whether a system libheif is in use instead of the WebAssembly build |

## Limits

- When the program can load the system library the codec wraps, it is used instead of the built-in code, so output bytes can differ between machines. Build with `-tags nodynamic` to always use the built-in code.
- Importing the module registers its format with the standard `image` package (`image.Decode`).
- Lossy output subsamples colour: flat colours come back within a few levels. The tests check decoded pixels against the source within that tolerance, using a HEIC file macOS made from a known picture (`testdata/quadrants.heic`), since no Go encoder can make one in-test.
