# Security defaults, each one refusing something

Storage code is where traversal, disguised files, header injection and server-side request forgery get in. This program runs every security default the library ships and prints what each one refused: a traversal key, content types detected from the bytes rather than the name, active content (HTML, SVG) kept out of an images-only policy, script and executable file names, a name that claims another type, a stored header with CRLF, private and metadata addresses refused for URL imports, signed URL limits, scanning that fails closed when the scanner is down, photo metadata stripped by re-encoding, extensions taken from the detected type, the MIME tables, and secrets kept out of logs and errors. It uses the core `filesystem` package, `memory`, `sniff`, `upload`, `scan`, `signedurl` and `contrib/imaging`. The guide [Security](../../docs/concepts/security.md) explains every default and what stays the application's job.

## Run it

```sh
cd examples
go run ./concepts-security
go test ./concepts-security
```

It needs no network, files or environment variables and exits after printing its results. The virus scanner is a stand-in (`demoScanner`) that flags any file containing `EICAR` and, when told it is down, returns the error a stopped clamd would; nothing connects to port 3310. Production code passes a real scanner, such as `scan.Clamd`, to `scan.Inspect`.

## What it does

```sh
go run ./concepts-security
```

```text
traversal key: invalid_key
detect invoice.pdf -> image/png (active: false)
detect photo.jpg -> text/html; charset=utf-8 (active: true)
detect logo.png -> image/svg+xml (active: true)
detect page.html -> text/plain; charset=utf-8 (active: false)
policy allowing image/* and an SVG: rejected
name shell.php.jpg: "rejected"
name .htaccess: "rejected"
name setup.exe: "rejected"
name holiday.jpg: ""
PNG named invoice.pdf with MatchExtension: filesystem: rejected: the name says application/pdf but the content is image/png
header with CRLF: invalid_argument
PublicAddr(127.0.0.1): false
PublicAddr(10.0.0.8): false
PublicAddr(169.254.169.254): false
PublicAddr(100.64.0.1): false
PublicAddr(::ffff:127.0.0.1): false
PublicAddr(64:ff9b::a00:1): false
PublicAddr(fd00::1): false
PublicAddr(93.184.215.14): true
expiry 0s: invalid_argument
expiry 168h0m1s: invalid_argument
5-byte signing key: invalid_argument
signed URL with no signer configured: unsupported
scan (down=false): "rejected" stored=0 filesystem: rejected: scan: the file is infected (Win.Test.EICAR_HDB-1)
scan (down=true): "unavailable" stored=0 filesystem: unavailable: dial tcp 127.0.0.1:3310: connection refused
scan (down=false): "" stored=1 <nil>
sanitized image/jpeg: payload in upload true, payload stored false
named "photo": stored with extension .png
named "photo.gif": stored with extension .png
named "photo.png.exe.jpg": rejected
TypeByExtension(report.PDF) = application/pdf
Extension(image/jpeg) = .jpg
Matches(image/png, image/*) = true
MatchesAny(application/pdf, [image/* text/*]) = false
IsActive(application/xhtml+xml) = true
detector, scan without the detector: rejected
detector, scan with the detector: stored as application/dicom
detector, unknown binary: rejected
detector, HTML named .dcm: rejected
detector on the disk: Write sniffs application/dicom
key printed: signedurl.Key{ID: "2026-10", Secret: [REDACTED]}
secret in any print or log: false
DSN error: filesystem: invalid_key: disk media: key contains a ".." segment (password shown: false)
```

1. A key with `..` is refused, never resolved.
2. `sniff.Detect` reads the bytes: a PNG named `invoice.pdf` is a PNG, HTML named `photo.jpg` is HTML and active. The name only refines a generic answer and never into an active type, so plain text named `page.html` stays `text/plain`. An `image/*` policy refuses SVG because it is active content.
3. Script and executable extensions are refused in any position of the name (`shell.php.jpg`), and `MatchExtension` refuses a name that claims a different type than the bytes.
4. A stored header value with CR or LF is refused, so it cannot add a header of its own when served.
5. `upload.PublicAddr` refuses loopback, private, link-local (cloud metadata), carrier-grade NAT, IPv4-mapped, NAT64 and unique-local addresses; URL imports check this at dial time, after DNS and for every redirect.
6. Signed URLs must expire, within seven days (`MaxPresignExpiry`); HMAC keys must be at least 32 bytes; a disk with no signer says `unsupported` rather than inventing a link.
7. An infected file is refused and deleted; a scanner that is down refuses the upload (`unavailable`) instead of letting it through unscanned; a clean file is stored.
8. `imaging.Sanitize` re-encodes a JPEG before it is stored, so a script appended after the image (and any EXIF or GPS data) is gone.
9. A random key takes its extension from the detected type, never from the name: a PNG named `photo` or `photo.gif` is stored as `.png`; an executable segment anywhere in the name is still refused.
10. The `sniff` tables answer the same on every machine: type by extension, extension by type, wildcard matching and whether a type is active.
11. A format Go does not know (a DICOM scan) comes back `application/octet-stream`, which an allowlist without octet-stream refuses. The policy's `Detector` labels it `application/dicom`, so the policy can allow exactly that type; an unknown binary is still refused, and HTML named `.dcm` stays HTML, because the detector is only asked when the built-in answer is octet-stream. `filesystem.WithContentDetector` gives a disk the same function for writes that bypass the upload package.
12. A signing key prints `[REDACTED]` under every verb and in structured logs, and an error from opening a DSN names the disk, never the DSN or its password.

## The code

Traversal, refused by the disk before the driver.

<!-- ds:block id=traversal-vtm67qpc -->
**traversal** · [`examples/concepts-security/main.go:87-90`](main.go#L87-L90)

```go
func traversal(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	_, err := disk.WriteBytes(ctx, "avatars/../../etc/cron.d/job", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "traversal key: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=fd8fc9 -->

Detection from the bytes, and the images-only policy refusing an SVG.

<!-- ds:block id=detection-cq5kkx8x -->
**detection** · [`examples/concepts-security/main.go:95-104`](main.go#L95-L104)

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

File-name checks, including `MatchExtension`.

<!-- ds:block id=names-5fjd9yyp -->
**names** · [`examples/concepts-security/main.go:109-117`](main.go#L109-L117)

```go
func names(out io.Writer) {
	for _, n := range []string{"shell.php.jpg", ".htaccess", "setup.exe", "holiday.jpg"} {
		fmt.Fprintf(out, "name %s: %q\n", n, filesystem.CodeOf(images.AcceptsName(n, "image/jpeg")))
	}
	strict := images
	strict.MatchExtension = true
	err := strict.AcceptsName("invoice.pdf", "image/png")
	fmt.Fprintf(out, "PNG named invoice.pdf with MatchExtension: %v\n", err)
}
```
<!-- /ds:block hash=85ac2c -->

A `Content-Disposition` value carrying a second header.

<!-- ds:block id=headerinjection-4se5g2gk -->
**headerInjection** · [`examples/concepts-security/main.go:122-126`](main.go#L122-L126)

```go
func headerInjection(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	h := filesystem.HTTPHeaders{ContentDisposition: "attachment\r\nSet-Cookie: session=stolen"}
	_, err := disk.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{Headers: h})
	fmt.Fprintf(out, "header with CRLF: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=2d5d8c -->

The address check URL imports run at dial time.

<!-- ds:block id=ssrf-zsbvsyea -->
**ssrf** · [`examples/concepts-security/main.go:131-135`](main.go#L131-L135)

```go
func ssrf(out io.Writer) {
	for _, a := range []string{"127.0.0.1", "10.0.0.8", "169.254.169.254", "100.64.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "fd00::1", "93.184.215.14"} {
		fmt.Fprintf(out, "PublicAddr(%s): %t\n", a, upload.PublicAddr(netip.MustParseAddr(a)))
	}
}
```
<!-- /ds:block hash=905505 -->

The limits on signed URLs and signing keys.

<!-- ds:block id=signing-aaf37gtf -->
**signing** · [`examples/concepts-security/main.go:140-149`](main.go#L140-L149)

```go
func signing(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	for _, d := range []time.Duration{0, filesystem.MaxPresignExpiry + time.Second} {
		err := filesystem.ValidatePresign(filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: d})
		fmt.Fprintf(out, "expiry %s: %s\n", d, filesystem.CodeOf(err))
	}
	_, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("short")}})
	fmt.Fprintf(out, "5-byte signing key: %s\n", filesystem.CodeOf(err))
	_, err = disk.SignedURL(ctx, "a.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	fmt.Fprintf(out, "signed URL with no signer configured: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=4fddee -->

The stand-in scanner, then scanning wired into an upload policy as an `Inspect` hook, which fails closed.

<!-- ds:block id=demoscanner-79yz8hz2 -->
**demoScanner** · [`examples/concepts-security/main.go:158-172`](main.go#L158-L172)

```go
func demoScanner(down bool) scan.Scanner {
	return scan.ScannerFunc(func(_ context.Context, r io.Reader) (scan.Verdict, error) {
		if down {
			return scan.Verdict{}, errors.New("dial tcp 127.0.0.1:3310: connection refused")
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return scan.Verdict{}, err
		}
		if bytes.Contains(b, []byte("EICAR")) {
			return scan.Verdict{Signature: eicar}, nil
		}
		return scan.Verdict{Clean: true}, nil
	})
}
```
<!-- /ds:block hash=f7bf90 -->

<!-- ds:block id=scanning-rsgwpjcn -->
**scanning** · [`examples/concepts-security/main.go:178-194`](main.go#L178-L194)

```go
func scanning(ctx context.Context, _ *filesystem.Disk, out io.Writer) {
	for _, c := range []struct {
		data string
		down bool
	}{{"PNG EICAR", false}, {pngBytes, true}, {pngBytes, false}} {
		disk, err := filesystem.NewDisk("scanned", memory.New())
		if err != nil {
			fmt.Fprintln(out, err)
			return
		}
		p := images
		p.Allowed = []string{"image/*", "text/plain"}
		p.Inspect = scan.Inspect(demoScanner(c.down))
		_, err = upload.Receive(ctx, disk, strings.NewReader(c.data), "file", p)
		fmt.Fprintf(out, "scan (down=%t): %q stored=%d %v\n", c.down, filesystem.CodeOf(err), objects(ctx, disk), err)
	}
}
```
<!-- /ds:block hash=6dc2d6 -->

Re-encoding a photo with a `Process` hook so nothing smuggled after the image is stored.

<!-- ds:block id=stripmetadata-9nbz8sj2 -->
**stripMetadata** · [`examples/concepts-security/main.go:222-240`](main.go#L222-L240)

```go
func stripMetadata(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	photo, err := photoWithPayload()
	if err != nil {
		return err
	}
	p := images
	p.Process = imaging.Sanitize(imaging.Options{})
	res, err := upload.Receive(ctx, disk, bytes.NewReader(photo), "holiday.jpg", p)
	if err != nil {
		return err
	}
	stored, err := disk.ReadAll(ctx, res.Key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sanitized %s: payload in upload %t, payload stored %t\n",
		res.ContentType, bytes.Contains(photo, []byte(smuggled)), bytes.Contains(stored, []byte(smuggled)))
	return nil
}
```
<!-- /ds:block hash=956a0c -->

The extension comes from the detected type.

<!-- ds:block id=extensionfromtype-6mq54e6t -->
**extensionFromType** · [`examples/concepts-security/more.go:28-40`](more.go#L28-L40)

```go
func extensionFromType(ctx context.Context, disk *filesystem.Disk, out io.Writer) error {
	p := images
	p.Key = upload.RandomKey("avatars")
	for _, name := range []string{"photo", "photo.gif", "photo.png.exe.jpg"} {
		res, err := upload.Receive(ctx, disk, strings.NewReader(pngBytes), name, p)
		if err != nil {
			fmt.Fprintf(out, "named %q: %s\n", name, filesystem.CodeOf(err))
			continue
		}
		fmt.Fprintf(out, "named %q: stored with extension %s\n", name, path.Ext(res.Key))
	}
	return nil
}
```
<!-- /ds:block hash=b062ca -->

The MIME helpers.

<!-- ds:block id=mimehelpers-9cmqcw4g -->
**mimeHelpers** · [`examples/concepts-security/more.go:46-53`](more.go#L46-L53)

```go
func mimeHelpers(out io.Writer) {
	fmt.Fprintf(out, "TypeByExtension(report.PDF) = %s\n", sniff.TypeByExtension("report.PDF"))
	fmt.Fprintf(out, "Extension(image/jpeg) = %s\n", sniff.Extension("image/jpeg"))
	fmt.Fprintf(out, "Matches(image/png, image/*) = %t\n", sniff.Matches("image/png", "image/*"))
	fmt.Fprintf(out, "MatchesAny(application/pdf, [image/* text/*]) = %t\n",
		sniff.MatchesAny("application/pdf", []string{"image/*", "text/*"}))
	fmt.Fprintf(out, "IsActive(application/xhtml+xml) = %t\n", sniff.IsActive("application/xhtml+xml"))
}
```
<!-- /ds:block hash=e5f41d -->

Secrets kept out of prints, logs and errors.

<!-- ds:block id=redaction-2vb5zj87 -->
**redaction** · [`examples/concepts-security/more.go:59-77`](more.go#L59-L77)

```go
func redaction(ctx context.Context, out io.Writer) error {
	key := signedurl.Key{ID: "2026-10", Secret: demoSigningSecret}
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{key})
	if err != nil {
		return err
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("signer ready", "key", key, "signer", signer)
	printed := fmt.Sprintf("%v %+v %#v %s", key, key, key, key) + logs.String()
	fmt.Fprintf(out, "key printed: %v\n", key)
	fmt.Fprintf(out, "secret in any print or log: %t\n", strings.Contains(printed, string(demoSigningSecret)))
	mux := filesystem.NewMux()
	if err := mux.Register("mem", memory.Opener); err != nil {
		return err
	}
	_, err = mux.Open(ctx, "media", "mem://bucket?password=hunter2&prefix=../x")
	fmt.Fprintf(out, "DSN error: %v (password shown: %t)\n", err, strings.Contains(fmt.Sprint(err), "hunter2"))
	return nil
}
```
<!-- /ds:block hash=ff77f5 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: one function per default, the demo scanner and the photo with an appended payload |
| `more.go` | Extensions from the detected type, the MIME helpers, and secret redaction |
| `detector.go` | A detector for DICOM scans, on an upload policy and on a disk; code: [`scanPolicy`](ds:block?id=scanpolicy-q4w8n2xc) |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Security](../../docs/concepts/security.md), including serving user content and what stays the application's job.
- Related examples: [concepts-keys](../concepts-keys), [concepts-uploads](../concepts-uploads), [concepts-serving](../concepts-serving), [recipe-virus-scanning](../recipe-virus-scanning), [recipe-strip-photo-metadata](../recipe-strip-photo-metadata).
