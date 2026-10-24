# sniff

`github.com/ubgo/filesystem/sniff` detects a file's MIME type from its first bytes, refined by its file name only where the bytes cannot tell. A client's declared `Content-Type` and file extension cannot be trusted; serving a client-declared `text/html` or `image/svg+xml` from your origin is stored XSS. The Disk uses it to detect the type of every write that does not declare one, and every upload path uses it to check content against a policy.

The reasons and the rest of the defaults are in [Security](../docs/concepts/security.md#content-type-from-the-bytes).

<!-- ds:block id=detection-cq5kkx8x -->
**detection** · [`examples/concepts-security/main.go:95-104`](../examples/concepts-security/main.go#L95-L104)

```go
func detection(out io.Writer) {
	for _, c := range []struct{ name, data string }{
		{"invoice.pdf", pngBytes}, {"photo.jpg", htmlBytes}, {"logo.png", svgBytes}, {"page.html", "just text"},
	} {
		t := sniff.Detect([]byte(c.data), c.name)
		fmt.Fprintf(out, "detect %s -> %s (active: %t)\n", c.name, t, sniff.IsActive(t))
	}
	err := images.Accepts(sniff.SVG)
	fmt.Fprintf(out, "policy allowing image/* and an SVG: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=b7f1a6 -->

## API

| Name | What it is |
|---|---|
| `Detect(head, name)` | the MIME type of content whose first bytes are `head` (at most `HeadSize` are read) and whose name is `name` (may be empty); never empty, unknown binary is `OctetStream` |
| `Detector` | `func(head []byte, name string) string`: labels content the built-in tables do not know (DICOM, STL, camera RAW, Parquet), answering `""` when it does not know either; `upload.Policy.Detector` and `filesystem.WithContentDetector` take one |
| `DetectWith(custom, head, name)` | `Detect`, asking `custom` only when the answer is `OctetStream`, so a detector can label unknown content but never relabel a known type (HTML stays HTML); an empty or malformed answer stays `OctetStream`; a nil `custom` is plain `Detect` |
| `HeadSize` | 512, the bytes `Detect` needs, matching `net/http`'s sniffing window |
| `Matches(mime, pattern)` | whether a type satisfies `image/png`, `image/*` or `*/*`; parameters such as `; charset=utf-8` are ignored, case-insensitive |
| `MatchesAny(mime, patterns)` | whether a type satisfies at least one pattern |
| `ActiveTypes` | types a browser renders with script: `text/html`, `application/xhtml+xml`, `image/svg+xml`, `text/xml`, `application/xml`, `text/xsl`, `application/xslt+xml`, `text/javascript`, `application/javascript` and the legacy JavaScript names (`application/x-javascript`, `application/ecmascript`, `text/ecmascript`, `text/jscript`, `text/javascript1.0` to `1.5` and others) |
| `IsActive(mime)` | whether a type is one of `ActiveTypes` or any structured `+xml` type (`application/atom+xml`, `application/mathml+xml`, …), since XHTML script runs inside any XML document; every check for script-running content goes through it: `fshttp` serves such types as downloads, signed GET links force an attachment, `DataURI` refuses them |
| `Extension(mime)` | the conventional extension (with the dot) for a type from a fixed table, or `""`; `upload.RandomKey` names files with it |
| `TypeByExtension(name)` | the type a file name's extension conventionally means, from a fixed table, or `""`; reads no system files, so it answers the same on every machine; `Policy.MatchExtension` uses it |

## How detection works

1. ISO base media files are recognised by their `ftyp` brand: AVIF, HEIC, HEIF, QuickTime, MP4, M4A and 3GPP, including short heads `net/http` misses.
2. Everything else goes through `http.DetectContentType`.
3. Plain text or XML containing `<svg` is `SVG`, so a policy can refuse it.
4. Plain text is refined by extension into `CSV`, `JSON` or `Markdown`, and a zip into `DOCX`, `XLSX`, `PPTX` or `EPUB`. Nothing is ever refined into a type a browser would execute.

## Constants

`OctetStream`, `TextPlain`, `TextXML`, `Zip`, `SVG`, `AVIF`, `HEIC`, `HEIF`, `QuickTime`, `MP4`, `M4A`, `ThreeGPP`, `CSV`, `JSON`, `Markdown`, `DOCX`, `XLSX`, `PPTX`, `EPUB`: the MIME types this package returns or refines into, for comparing results without string literals.

## Limits

Detection reads 512 bytes. It tells a PNG from an HTML page; it does not validate a whole file, so a malformed or malicious file of an allowed type passes. Use `Policy.Inspect` (a scan, a full decode) to look further; see [Uploads](../docs/concepts/uploads.md#step-8--process-and-inspect).
