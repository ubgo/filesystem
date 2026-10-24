# contrib/svg

`github.com/ubgo/filesystem/contrib/svg` (package `svg`) makes user-uploaded SVG safe to accept: a pure-Go sanitiser that rebuilds a document from an allowlist of SVG elements and attributes, so scripts, event handlers, `foreignObject`, external references, unsafe CSS and link-changing animations are gone, plus the upload processor, inspector and policy helper built on it. It runs no program, needs no cgo and imports nothing but the standard library (`encoding/xml`) and the core (`deps_test.go` enforces it).

```sh
go get github.com/ubgo/filesystem/contrib/svg
```

The guide, with output from a run: [accepting SVG safely](../../docs/plugins/svg.md). The policy of [`examples/media/svg`](../../examples/media/svg/main.go):

<!-- ds:block id=newpolicy-8uk22a4e -->
**newPolicy** · [`examples/media/svg/main.go:38-40`](../../examples/media/svg/main.go#L38-L40)

```go
func newPolicy() (upload.Policy, error) {
	return svg.Policy(images, svg.Options{AllowDataImages: true})
}
```
<!-- /ds:block hash=dcda2f -->

## API

| Name | What it is |
|---|---|
| `Sanitize(r io.Reader, w io.Writer, opts Options) (Report, error)` | writes the sanitised document to `w`; not well-formed XML or a root other than `svg` is `CodeRejected`, over the limits `CodeTooLarge`; nothing is written on failure |
| `Options{MaxBytes, MaxDepth, MaxElements, AllowDataImages, DenyElements, DenyAttributes}` | limits (10 MiB, 256, 100,000 by default) and whether base64 PNG, JPEG, GIF and WebP `data:` images are kept |
| `Options.DenyElements`, `Options.DenyAttributes` | drop more elements (with their content) or attributes than the built-in allowlist does, such as `animate` or `style`; elements match as SVG spells them, attributes without case. They only narrow: no option widens the allowlist, and the root `svg` is always kept |
| `Report{Removed}`, `Report.Clean()` | what was removed (`element script`, `attribute onload`, `style rules`, …) |
| `Policy(base upload.Policy, opts) (upload.Policy, error)` | `base` accepting SVG safely: `AllowActiveContent` set, every other active type denied, SVG sanitised in `Process`, refused in `Inspect` when still active or named like an executable |
| `Processor(opts) upload.Processor` | sanitises SVG uploads; other types pass |
| `Inspect(opts) upload.Inspector` | refuses a stored SVG that `Sanitize` would change (presigned and Uppy uploads); other types pass |
| `IsSVG(contentType) bool` | whether a type is `image/svg+xml` |
| `ContentType`, `NamespaceSVG`, `NamespaceXLink`, `NamespaceXML` | the type and the namespaces the sanitiser reads |
| `DefaultMaxBytes`, `DefaultMaxDepth`, `DefaultMaxElements` | the default limits |

## Limits

- The sanitised document is held in memory (bounded by `MaxBytes`) because nothing is written until the whole input is checked.
- Sanitising is not a reason to serve SVG inline from your origin: show it with `<img src>` from a separate origin or CDN. `fshttp` keeps serving SVG as a download.
