# Accepting SVG safely

You will have user-uploaded SVG (logos, icons, illustrations) accepted and stored with everything a browser could run or fetch removed, so an uploaded logo cannot run script with your users' sessions. The package is `contrib/svg`: a sanitiser in pure Go on the standard library's XML parser, an upload processor and inspector built on it, and `svg.Policy`, which lets SVG through an upload policy and nothing else that browsers run.

## Before you start

- Why SVG is refused by default: it is a document format as well as an image. A browser that opens an uploaded SVG from your origin runs its `<script>` elements and `onload` handlers as your page would, with the user's cookies. The upload package refuses `image/svg+xml` with the other active types (HTML, XML, JavaScript) unless a policy lists it in `AllowActive` (or sets `AllowActiveContent`, which lifts every rule), and [serving files](../concepts/serving-files.md) explains that `fshttp` serves them as downloads.
- `go get github.com/ubgo/filesystem/contrib/svg`. It is its own module, needs no program and no cgo, and imports nothing but the standard library and the core (`deps_test.go` enforces it).
- The example is [`examples/media/svg`](../../examples/media/svg/main.go), in the separate module `examples/media`: `go run ./svg logo.svg` from `examples/media` prints what was removed and the stored document; `go test ./svg` runs it on an attacker's logo.

## What the sanitiser does

`svg.Sanitize(r, w, opts)` rebuilds the document from allowlists rather than deleting known-bad parts, so a construct nobody thought of is dropped rather than kept:

| Input | What happens |
|---|---|
| Elements outside the SVG allowlist: `script`, `foreignObject`, `iframe`, `metadata`, elements of any other namespace (XHTML, editor namespaces) | dropped with everything inside them |
| `<a>` | unwrapped: its children stay, the link goes |
| Attributes outside the allowlist, every `on*` event handler among them, in any case | dropped |
| `href` and `xlink:href` other than `#fragment` (or a base64 PNG, JPEG, GIF or WebP `data:` URI with `AllowDataImages`) | dropped: no external sprites, images or filter inputs |
| `url(...)` other than `url(#fragment)` in any attribute | the attribute is dropped |
| Values naming `javascript:`, `vbscript:` or `data:text/html`, however spaced or split | dropped |
| CSS (`style` attributes, `<style>` elements) with `@import`, an external `url()`, `@font-face`, escapes, `expression()` or legacy script hooks | the whole rule set is dropped |
| `animate`, `set` and friends that would animate `href` or an `on*` attribute | dropped |
| DOCTYPEs, entities beyond XML's five, processing instructions, comments | dropped or refused; nothing is fetched or expanded, so XXE and billion-laughs documents do nothing |

The output is a fresh document with one `svg` root in the SVG namespace; sanitising it again changes nothing. A document that is not well-formed XML or whose root is not `svg` is refused (`CodeRejected`), and the input is bounded by `MaxBytes` (10 MiB), `MaxDepth` (256) and `MaxElements` (100,000), beyond which it is `CodeTooLarge`. Nothing is written until the whole input has been read and checked.

## Step 1 — Let SVG into the policy

`svg.Policy(base, opts)` returns `base` changed to accept SVG safely: SVG is added to `AllowActive`, the upload package's narrow exception, so SVG passes and every other active type (HTML, XHTML, every other `+xml` type, JavaScript) and the script-name rules (`logo.php.svg`) stay refused even if `base.Allowed` is too wide; `Process` sanitises SVG after `base.Process`, and `Inspect` refuses an SVG that is still not clean before `base.Inspect`. `base.Allowed` must already list `image/svg+xml`:

<!-- ds:block id=newpolicy-8uk22a4e -->
**newPolicy** · [`examples/media/svg/main.go:38-40`](../../examples/media/svg/main.go#L38-L40)

```go
func newPolicy() (upload.Policy, error) {
	return svg.Policy(images, svg.Options{AllowDataImages: true})
}
```
<!-- /ds:block hash=dcda2f -->

The inspector matters for direct uploads: presigned and Uppy uploads go straight to the bucket and never pass through `Process`, but `upload.Confirm` runs `Inspect`, so an SVG with active content is deleted rather than confirmed.

## Step 2 — Upload

<!-- ds:block id=run-3mvatpcw -->
**run** · [`examples/media/svg/main.go:58-89`](../../examples/media/svg/main.go#L58-L89)

```go
func run(ctx context.Context, out io.Writer, body []byte, filename string) error {
	rep, err := svg.Sanitize(bytes.NewReader(body), io.Discard, svg.Options{AllowDataImages: true})
	if err != nil {
		return err
	}
	for _, r := range rep.Removed {
		fmt.Fprintf(out, "removed   %s\n", r)
	}
	p, err := newPolicy()
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		return err
	}
	res, err := upload.Receive(ctx, disk, bytes.NewReader(body), filename, p)
	if err != nil {
		return err
	}
	stored, err := disk.ReadAll(ctx, res.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stored    %s (%s, %d bytes)\n%s\n", res.Key, res.ContentType, res.Size, stored)
	png, err := thumbnail(stored, thumbSide)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "raster    %dx%d %s\n", png.Width, png.Height, png.ContentType)
	return nil
}
```
<!-- /ds:block hash=b7b37e -->

Check: upload a logo carrying a script, a handler, a link, an embedded page and an external sprite, then an HTML page named `.svg`:

```sh
cd examples/media
go run ./svg logo.svg
go run ./svg page.svg
```

```text
removed   attribute onload
removed   element script
removed   element a
removed   element foreignObject
removed   attribute href
stored    logos/4nzfxd5cc3n4a6skat2mmbhbe4.svg (image/svg+xml, 187 bytes)
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="64" height="64">
  
  <rect width="64" height="64" fill="#2563eb"></rect>
  
  <use></use>
</svg>
raster    128x128 image/png
2026/10/06 11:52:24 filesystem: rejected: svg: the document is not an SVG image
```

The rectangle the link wrapped is still drawn; the link, the script and everything that reached outside the file are gone.

## Step 3 — Rasterise SVG for thumbnails and previews

A stored SVG is safe to serve as an image, but the image tools expect pixels: [variants](variants.md) for a PNG thumbnail in a list, [dimensions](dimensions.md) for a ThumbHash, an email or a PDF that cannot embed SVG. `contrib/svg/raster` adds an SVG `Decoder` to an `imaging.Pipeline`, so an SVG goes through every image path like a PNG. It is its own module (`go get github.com/ubgo/filesystem/contrib/svg/raster`), pure Go, drawing with oksvg.

<!-- ds:block id=svgpipeline-b8x3k7qw -->
**svgPipeline** · [`examples/media/svg/raster.go:19-21`](../../examples/media/svg/raster.go#L19-L21)

```go
func svgPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithDecoder(raster.Decoder(raster.Options{Side: rasterSide})))
}
```
<!-- /ds:block hash=3a670a -->

Every document is sanitised by `contrib/svg` again before it is drawn, whatever the policy did on upload, so a document stored before you added `svg.Policy` is still drawn without scripts or external references. It is drawn with its longest side at `Options.Side` (here 512; default `raster.DefaultSide`, 2048; at most `MaxSide`, 8192), from the document's `width` and `height`, else its `viewBox`, else the browser default of 300 × 150. The transform then resizes that drawing like any image:

<!-- ds:block id=thumbnail-v5n2c8rj -->
**thumbnail** · [`examples/media/svg/raster.go:25-28`](../../examples/media/svg/raster.go#L25-L28)

```go
func thumbnail(doc []byte, side int) (imaging.Result, error) {
	t := imaging.Transform{Mode: imaging.ModeFit, Width: side, Height: side, Format: imaging.FormatPNG}
	return svgPipeline().Process(bytes.NewReader(doc), t, imaging.Options{})
}
```
<!-- /ds:block hash=a0f135 -->

Pass the same Pipeline to `variants.WithProcessor` and `dimensions.WithProcessor(p, svg.ContentType)`, and recorded SVG files get PNG variants and a size. Check: after the upload above the example prints `raster    128x128 image/png` (the 64-pixel logo drawn at 512 and fitted to 128), and `TestRasterColour` checks the drawn rectangle is the logo's blue.

| Option | Bounds |
|---|---|
| `Side` | The size it is drawn at; the Pipeline's `MaxPixels` still applies to the result. |
| `Timeout` | One rendering (default 10 seconds); longer is `rejected`. The renderer cannot be interrupted, so it finishes in the background, bounded by `Side` and the sanitiser's limits. |
| `MaxRenders` | Renderings in flight at once, timed-out ones included (default 4); another is refused at once with `unavailable`, so slow documents cannot pile up. |
| `Sanitize` | The `svg.Options` of the sanitising that runs first: input size, nesting, element count, embedded raster images. |
| `Render` | Another renderer, such as resvg compiled to WebAssembly, for text and filters. |

The pure-Go renderer draws shapes, paths, gradients and transforms, but not text or filters: they come out missing, not as an error. Logos and icons render well; an SVG of a typeset document does not. A document that is not SVG (an HTML page renamed `.svg`) is `rejected`, as `TestRasterRefusesHTML` checks.

## Narrowing the allowlist

The built-in allowlist suits most drawings. An application with a stricter rule names what else to drop, and nothing outside the allowlist can ever be let back in:

```go
opts := svg.Options{
	// No animation, no raster embeds, no inline CSS.
	DenyElements:   []string{"animate", "animateMotion", "animateTransform", "set", "image", "feImage"},
	DenyAttributes: []string{"style"},
}
policy, err := svg.Policy(upload.Policy{MaxSize: 1 << 20, Allowed: []string{svg.ContentType}}, opts)
```

Denied elements go with their content and are listed in the `Report`; element names match as SVG spells them, attribute names without case (`xlink:href` with its prefix). Denying `svg` drops nested `svg` elements only: the root is always kept, since a document without it is refused.

## Without svg.Policy

`svg.Processor(opts)` and `svg.Inspect(opts)` are the two halves on their own, for a policy you assemble yourself: both pass every type but SVG untouched. If you assemble a policy by hand, put `image/svg+xml` in `AllowActive` rather than setting `AllowActiveContent`, which lifts the refusal of HTML and script names too.

## Pitfalls

- **Serve SVG as an image, not as a page.** `fshttp` serves every active type, SVG included, as a download with a sandbox CSP, sanitised or not. To show user SVG inline, put it in an `<img src>` (where browsers never run script) from a separate origin or CDN; never inline the markup into your HTML.
- **Sanitising changes files.** Animation of links, external fonts and `<a>` links are removed; an SVG that relies on them is drawn without them. `svg.Report.Removed` lists what went, for logs or for telling the user.
- **Text in a rasterised SVG is missing.** The pure-Go renderer skips `<text>`; convert text to paths in the design tool, or plug in another renderer with `raster.Options.Render`.
- **`unavailable` from the raster decoder under load:** `MaxRenders` renderings are already running. Make SVG variants eager so they are drawn once after upload, not on a burst of page views.
- **`AllowDataImages` is off by default.** Design tools often embed raster images as `data:` URIs; turn it on if your users upload such files. SVG and HTML `data:` URIs are never kept.

## Next

- [Image transforms](image-transforms.md): what a transform does to the drawn image.
- [Video](video.md) and [PDF](pdf.md): the other untrusted formats.
- [Security](../concepts/security.md): what every upload path checks.
