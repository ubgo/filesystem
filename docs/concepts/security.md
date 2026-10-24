# Security

Storage code handles the most hostile input an application receives: files and file names chosen by strangers, URLs they paste, and links that outlive the request that made them. This page lists every security default of the library, what it stops and why it is on by default, then what the library does not do and remains your job. Read it before exposing an upload endpoint or a download link to users.

The snippets come from `examples/concepts-security`, which triggers each default and prints what it refused. The steps follow a file's life: its key, its type, its name, how it is served, imported, linked, scanned, stripped and verified, and what never reaches a log. Run it with `go run ./concepts-security` from the `examples` directory.

## The defaults at a glance

| Default | Stops | Where |
|---|---|---|
| Keys are cleaned and `..` is refused before any driver runs | path traversal: reading or overwriting files outside the intended directory | `Disk`, `CleanKey`; the local driver also confines every call to an `os.Root` |
| The type is detected from the bytes | a client calling an HTML file `image/png` | `sniff`, every upload path |
| Active content is refused on upload | stored cross-site scripting through HTML, SVG, XML, JavaScript | `upload.DefaultDenied` |
| Executable and script names are refused, in any position | `shell.php.jpg` executed by a misconfigured server, `.htaccess` rewriting your server's rules | `upload.DefaultDeniedExtensions` |
| Size is counted while streaming | a client declaring 1 KB and sending 10 GB | `WriteOptions.MaxSize`, `Policy.MaxSize`, `fshttp.Signed` |
| Active content is served as a download, with `nosniff` and a sandbox policy | user HTML or SVG running script on your origin | `fshttp`, `Disk.SignedURL` |
| Stored header values may not contain CR or LF | header injection when the object is served | `HTTPHeaders` |
| Imports refuse non-public addresses at dial time | server-side request forgery: reaching cloud metadata, admin ports, internal services | `upload.FromURL`, `upload.Fetch` |
| Signed URLs always expire, within seven days; signing keys are at least 32 bytes | links that work forever, brute-forceable secrets | `filesystem.ValidatePresign`, `signedurl` |
| New objects are private | publishing a file by accident | `Disk` default visibility |
| Scanning fails closed | an outage of the virus scanner letting files through unscanned | `scan.Inspect` |
| Photos can be re-encoded before storage | GPS positions and camera serials leaking, payloads smuggled after the image | `imaging.Sanitize` |
| Errors never print DSNs, and handlers never send internal messages | credentials and internal keys leaking into logs and responses | `Mux`, `OpenManager`, `fshttp`, `upload` |

<a id="traversal"></a>

## Step 1 — Traversal

Every key passes `CleanKey` before a driver sees it. A `..` segment is refused, never resolved, so there is no input for which `uploads/<user input>` means a file outside `uploads/`. On local disk every operation additionally goes through an `os.Root`, which refuses any path (symlinks included) that leaves the disk's directory, so even a bug that bypassed the Disk could not escape. See [Keys](keys.md).

<!-- ds:block id=traversal-vtm67qpc -->
**traversal** · [`examples/concepts-security/main.go:87-90`](../../examples/concepts-security/main.go#L87-L90)

```go
func traversal(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	_, err := disk.WriteBytes(ctx, "avatars/../../etc/cron.d/job", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "traversal key: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=fd8fc9 -->

Check: `traversal key: invalid_key`.

<a id="content-type-from-the-bytes"></a>

## Step 2 — Content type from the bytes

A client chooses its `Content-Type` header and its file name, so neither says what a file is. `sniff.Detect(head, name)` reads the first 512 bytes, the same window browsers sniff. The name is consulted only to refine a generic answer (plain text into CSV, JSON or Markdown; a zip into DOCX, XLSX, PPTX or EPUB) from a fixed table, and never into a type a browser would execute. SVG is recognised specifically, because `net/http` calls it plain text or XML and it can carry script.

<!-- ds:block id=detection-cq5kkx8x -->
**detection** · [`examples/concepts-security/main.go:95-104`](../../examples/concepts-security/main.go#L95-L104)

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

```text
detect invoice.pdf -> image/png (active: false)
detect photo.jpg -> text/html; charset=utf-8 (active: true)
detect logo.png -> image/svg+xml (active: true)
detect page.html -> text/plain; charset=utf-8 (active: false)
policy allowing image/* and an SVG: rejected
```

The last line matters: `image/*` matches `image/svg+xml`, and the policy still refuses it, because `DefaultDenied` applies after `Allowed`. Only `AllowActiveContent` lifts it.

### MIME helpers

`sniff` also holds the type tables the rest of the library uses, for your own code: `TypeByExtension(name)` (the type a name's extension conventionally means, case-insensitive), `Extension(mime)` (the conventional extension for a type, with the dot), `Matches(mime, pattern)` and `MatchesAny(mime, patterns)` (the same wildcard matching `Policy.Allowed` uses, `image/*` included), and `IsActive(mime)` (whether a browser may run script in it, which covers every `+xml` type and not only the ones `ActiveTypes` lists). They are fixed tables compiled into the binary: unlike `mime.TypeByExtension`, which reads `/etc/mime.types` and the Windows registry, they answer the same on every machine, so a policy cannot behave differently in production than in tests.

<!-- ds:block id=mimehelpers-9cmqcw4g -->
**mimeHelpers** · [`examples/concepts-security/more.go:46-53`](../../examples/concepts-security/more.go#L46-L53)

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

Check: `TypeByExtension(report.PDF) = application/pdf`, `Extension(image/jpeg) = .jpg`, `Matches(image/png, image/*) = true`, `MatchesAny(application/pdf, [image/* text/*]) = false` and `IsActive(application/xhtml+xml) = true`. None of them decides what a file is: a name's extension is a claim, and `Detect` reads the bytes.

### Formats the detection does not know

The built-in detection knows the formats browsers and `net/http` know. Anything else, a DICOM scan, an STL or glTF model, a camera RAW, a Parquet file, comes back `application/octet-stream`, and a strict allowlist then has to allow octet-stream itself, which lets every unknown binary in. Instead, give the policy a `sniff.Detector`, a function from the first bytes and the name to a type, and allow only that type:

```go
// DICOM files carry "DICM" after a 128-byte preamble.
dicom := func(head []byte, _ string) string {
	if len(head) >= 132 && string(head[128:132]) == "DICM" {
		return "application/dicom"
	}
	return ""
}
scans := upload.Policy{MaxSize: 512 << 20, Allowed: []string{"application/dicom"}, Detector: dicom}
```

The detector cannot weaken the policy. `sniff.DetectWith` asks it only when the built-in answer is `application/octet-stream`, so it can never relabel a type Go already recognises: HTML stays HTML and is refused, whatever the detector says. An empty or malformed answer stays octet-stream, which a strict policy refuses, so a detector that fails fails closed. Its answer then goes through `Allowed`, `Denied` and the active-content rule like any other, so a detector answering `text/html` is refused too. `Receive`, `Confirm` and the chunked handler all use it; for writes that bypass the upload package, give the disk the same function with `filesystem.WithContentDetector(dicom)`.

<a id="file-names"></a>

Try it: [`examples/concepts-security`](../../examples/concepts-security) runs this section in `detector.go` ([`dicom`](ds:block?id=dicom-h7r2k9vm), [`customDetector`](ds:block?id=customdetector-b6t3p8zs)): `go run ./concepts-security` from `examples/`.

## Step 3 — File names

Script and executable extensions are refused in every dotted segment of the name, not only the last, because some servers execute `shell.php.jpg`; dot-files such as `.htaccess` are checked whole. `MatchExtension` additionally refuses a name that claims another type than the bytes, so a PNG cannot be stored as `invoice.pdf` and later opened by a PDF reader that trusts the extension. `Extensions` restricts the accepted extensions outright.

<!-- ds:block id=names-5fjd9yyp -->
**names** · [`examples/concepts-security/main.go:109-117`](../../examples/concepts-security/main.go#L109-L117)

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

Check: `name shell.php.jpg: "rejected"`, `name .htaccess: "rejected"`, `name setup.exe: "rejected"`, `name holiday.jpg: ""`, and `the name says application/pdf but the content is image/png`.

The name is display data in any case: `upload.SafeFilename` keeps the base name only, strips control characters and caps it at 255 bytes, and `upload.RandomKey` never puts it in the key.

### The extension comes from the detected type

A stored file's extension matters after upload: a web server, a CDN or a desktop app opening a downloaded file may pick its handler from it. So `upload.RandomKey` (and `ShardedKey`) never copies the client's extension: it appends the conventional extension of the type detected from the bytes (`sniff.Extension`). A file sent with no extension, or with one that lies, is stored under the truth:

<!-- ds:block id=extensionfromtype-6mq54e6t -->
**extensionFromType** · [`examples/concepts-security/more.go:28-40`](../../examples/concepts-security/more.go#L28-L40)

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

Check: `named "photo": stored with extension .png`, `named "photo.gif": stored with extension .png`, and `named "photo.png.exe.jpg": rejected` for the executable segment in the middle. When you build keys yourself, do the same: `sniff.Extension(res.ContentType)`, never `path.Ext(filename)`. For a download under the client's own name, store the name as data and send it in `Content-Disposition` (`upload.Attachment`).

<a id="serving-user-content"></a>

## Step 4 — Serving user content

Three independent layers keep an uploaded file from running script on your origin. The upload policy refuses active types. `fshttp` sends `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src 'none'; sandbox` on every response and serves any active type as `attachment`, whatever was stored or signed. `Disk.SignedURL` adds `attachment` to GET links for active types when given the content type, so links served by S3, GCS or Azure download too. The demonstration with real responses is in [Serving files](serving-files.md).

The sandbox also stops browsers showing a PDF inline (their PDF viewers are plugin objects, which `sandbox` and `object-src 'none'` both forbid), so a PDF link shows a blank page. `PublicOptions.InlinePDF` and `SignedOptions.InlinePDF` serve a PDF that is not an attachment with `fshttp.InlinePDFCSP` (`default-src 'none'; object-src 'self'`) instead; every other type keeps the sandbox, and `nosniff` stays on every response. It is off by default because the PDF viewer's script then runs with the file's origin: turn it on for PDFs your own users upload only when they are trusted, or serve files from a separate domain.

Stored HTTP header values (`Cache-Control`, `Content-Disposition`, `Content-Encoding`) may not contain CR or LF, which would let a value add a header of its own when the object is served:

<!-- ds:block id=headerinjection-4se5g2gk -->
**headerInjection** · [`examples/concepts-security/main.go:122-126`](../../examples/concepts-security/main.go#L122-L126)

```go
func headerInjection(ctx context.Context, disk *filesystem.Disk, out io.Writer) {
	h := filesystem.HTTPHeaders{ContentDisposition: "attachment\r\nSet-Cookie: session=stolen"}
	_, err := disk.WriteBytes(ctx, "report.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{Headers: h})
	fmt.Fprintf(out, "header with CRLF: %s\n", filesystem.CodeOf(err))
}
```
<!-- /ds:block hash=2d5d8c -->

Check: `header with CRLF: invalid_argument`.

<a id="server-side-request-forgery"></a>

## Step 5 — Server-side request forgery

`upload.FromURL` and `upload.Fetch` connect only to public internet addresses. The check runs in the dialer, after DNS resolution and for every connection including redirects, so a hostname that resolves to an internal address, or re-resolves to one between checks (DNS rebinding), is refused. No proxy is used, only `http` and `https` without credentials are fetched, redirects are capped at five and the fetch at one minute. `upload.PublicAddr` is the predicate; use it wherever your server connects to an address a user chose (webhooks, link previews).

<!-- ds:block id=ssrf-zsbvsyea -->
**ssrf** · [`examples/concepts-security/main.go:131-135`](../../examples/concepts-security/main.go#L131-L135)

```go
func ssrf(out io.Writer) {
	for _, a := range []string{"127.0.0.1", "10.0.0.8", "169.254.169.254", "100.64.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "fd00::1", "93.184.215.14"} {
		fmt.Fprintf(out, "PublicAddr(%s): %t\n", a, upload.PublicAddr(netip.MustParseAddr(a)))
	}
}
```
<!-- /ds:block hash=905505 -->

Check: every address but the last prints `false`: loopback, a private network, cloud metadata (`169.254.169.254`), CGNAT, an IPv4-mapped loopback, a NAT64 address and an IPv6 unique-local address. `FetchOptions.AllowAddr` admits a known internal server deliberately.

`FetchOptions.Proxy` sends the fetch through an `http://` CONNECT proxy (Smokescreen, an egress gateway) and keeps this guard for the final destination: the fetch resolves the name itself, checks the address, and asks the proxy to connect to that address rather than to the name, so the proxy never resolves it again. The environment's `HTTP_PROXY` is never read.

<a id="signed-urls"></a>

## Step 6 — Signed URLs

A signed URL needs an expiry in `(0, MaxPresignExpiry]`; there is no default and no "never". Seven days is the S3 SigV4 limit, and a link that must live longer is a public file. HMAC secrets must be at least 32 bytes. The signature covers the method, disk, key, expiry and response overrides, a failed check answers 403 without saying which part failed, and a disk with neither a backend signer nor a `URLSigner` refuses to issue links rather than inventing one.

<!-- ds:block id=signing-aaf37gtf -->
**signing** · [`examples/concepts-security/main.go:140-149`](../../examples/concepts-security/main.go#L140-L149)

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

Check: `expiry 0s: invalid_argument`, `expiry 168h0m1s: invalid_argument`, `5-byte signing key: invalid_argument`, `signed URL with no signer configured: unsupported`.

A signed PUT authorises bytes at a key; it does not check them. Direct uploads are safe only with `upload.Confirm` after them. See [Uploads](uploads.md).

<a id="scanning-fails-closed"></a>

## Step 7 — Scanning fails closed

`scan.Inspect(scanner)` plugs a `scan.Scanner` into `Policy.Inspect`, so every upload path scans the stored file before reporting success. An infected file is `rejected` and deleted. A scan that could not run (the daemon is down, a timeout) is `unavailable`, and the file is deleted too: a file is accepted only on a clean verdict. `scan.Clamd{Network: "tcp", Address: "127.0.0.1:3310"}` talks to ClamAV's daemon; the demo uses a stand-in scanner so it runs anywhere.

<!-- ds:block id=demoscanner-79yz8hz2 -->
**demoScanner** · [`examples/concepts-security/main.go:158-172`](../../examples/concepts-security/main.go#L158-L172)

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
**scanning** · [`examples/concepts-security/main.go:178-194`](../../examples/concepts-security/main.go#L178-L194)

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

```text
scan (down=false): "rejected" stored=0 filesystem: rejected: scan: the file is infected (Win.Test.EICAR_HDB-1)
scan (down=true): "unavailable" stored=0 filesystem: unavailable: dial tcp 127.0.0.1:3310: connection refused
scan (down=false): "" stored=1 <nil>
```

An application that prefers to stay up while its scanner is down wraps the `Scanner` and decides there (accept and queue a rescan, say); that choice is deliberate and visible in your code, never a silent default. See [Virus scanning](../recipes/virus-scanning.md) and [the scan reference](../../scan/README.md).

<a id="photo-metadata"></a>

## Step 8 — Photo metadata

Phones record the GPS position in every photo, and cameras their serial number; image viewers ignore anything appended after the image, which is where payloads hide. `imaging.Sanitize(imaging.Options{})` is a `Processor` that re-encodes photos before they are stored: the EXIF orientation is applied, and everything else the file carried is dropped. JPEG stays JPEG; PNG, WebP, BMP and TIFF become PNG. An image that does not decode, or that declares more pixels than the limit (a pixel bomb), is refused.

<!-- ds:block id=stripmetadata-9nbz8sj2 -->
**stripMetadata** · [`examples/concepts-security/main.go:222-240`](../../examples/concepts-security/main.go#L222-L240)

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

Check: `sanitized image/jpeg: payload in upload true, payload stored false`. It runs wherever `Policy.Process` runs: form uploads, imports, tus and records, not presigned or Uppy uploads, whose bytes never pass through your server. See [Strip photo metadata](../recipes/strip-photo-metadata.md).

<a id="integrity"></a>

## Step 9 — Integrity

Every write is hashed by the Disk while it streams (`Entry.SHA256`). When the expected digest is known (a client that declared it, a file copied from a store that reported it), pass it as `WriteOptions.SHA256`: a body whose SHA-256 differs is refused with `invalid_argument` before the driver commits, so a body corrupted on the way never replaces the object already stored, on any backend. A backend with the `ChecksumSHA256` capability is also given the digest and checks it on its own side of the wire. A positive `SizeHint` is held to the body the same way, which catches a truncated upload, and `upload.ConfirmExpected` does both checks for direct uploads.

The checks, the checksums each backend verifies on its own side, deduplication and scrubbing are in [Integrity](integrity.md).

<a id="private-by-default"></a>

## Step 10 — Private by default

A Disk's default visibility is private unless configured otherwise, because exposing a file by accident is worse than having to opt in. A public file is one you chose to publish, through `WriteOptions.Visibility`, `Policy.Visibility`, the disk's `visibility=public`, or a public disk in the records two-disk layout. See [Public and private files](public-and-private.md).

## Step 11 — Secrets stay out of logs and errors

Storage code holds the most valuable secrets an application has: signing keys, bucket credentials, DSNs with passwords in them. They leak most often not through an attack but through a log line or an error message that printed a struct. So the types that carry secrets print without them everywhere: `signedurl.Key` and `signedurl.Signer` implement `fmt.Stringer`, `fmt.GoStringer`, `fmt.Formatter` and `slog.LogValuer`, so `%v`, `%+v`, `%#v`, `%s` and structured logs all show the key ID and `[REDACTED]`. Signed URLs are never logged by the library's own interceptors (`layer.Log` records the operation, disk and key, not the URL), and every error the `Mux` and `OpenManager` return names the disk and the environment variable, never the DSN itself.

<!-- ds:block id=redaction-2vb5zj87 -->
**redaction** · [`examples/concepts-security/more.go:59-77`](../../examples/concepts-security/more.go#L59-L77)

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

Check: `key printed: signedurl.Key{ID: "2026-10", Secret: [REDACTED]}`, `secret in any print or log: false`, and `DSN error: filesystem: invalid_key: disk media: key contains a ".." segment (password shown: false)`.

Your own code can still leak them: a signed URL is a credential until it expires, so do not log it whole (log the key and the expiry instead), and do not put DSNs in error messages or panics of your own.

## What stays your job

- **Authentication and authorisation.** No handler in this library knows who the user is. Mount `upload.Handler`, `upload.PresignHandler` and `upload.Uppy` behind your authentication, set `UppyOptions.Authorize`, and give `recordshttp` its required `Scope` and `Authorize` hooks.
- **Rate limits and total quotas.** `MaxSize`, `MaxFiles` and `HandlerOptions.MaxTotal` bound one request; how many requests a user may send is your limiter's decision, called from a `Resolver`, which runs before any byte is read and can refuse with `Retry-After`. Per-scope storage limits are the records `quota` plugin.
- **A separate domain for user content.** The headers above are a second line. Serving user files from a domain that holds no cookies of your application (a CDN hostname, `usercontent.example.com`) is the first.
- **Deep content checks.** Detection reads the first 512 bytes. A file that starts like a PDF is a PDF to the policy; a malicious PDF is still a PDF. Scanning and full decoding (`Inspect`) are how you look further.
- **Bucket configuration.** Block public access on private buckets, keep ACLs off unless you need them, enable encryption at rest where the provider offers it (`sse=` on the S3 DSN), and give the application's credentials only the bucket permissions it uses.
- **Revocation.** A signed URL works until it expires. Use short lifetimes, or the records `share` plugin for links you must be able to revoke.
- **Secrets.** Keep signing keys and storage credentials in a secret store, rotate signing keys with the newest first and the previous one kept until its links expire.

## Pitfalls

- Calling the driver directly. Every check on this page lives in the `Disk` and the `upload` package; a `Driver` call skips traversal checks, size caps and detection. Applications hold a `*filesystem.Disk`.
- `AllowActiveContent` on an endpoint whose files are served from your application's origin. It re-opens stored cross-site scripting; serve such files from a separate domain, or not at all.
- `*/*` in `Policy.Allowed` because "the scanner will catch it". Active content is still refused, but archives, office documents with macros and executables renamed to something harmless are not. Allow the types the feature needs.
- A presigned upload recorded without `Confirm`. The bucket holds whatever the client sent until `Confirm` reads it; a signed PUT authorises bytes, it does not check them.
- Building keys from the client's file name, or taking the extension from it. Use `RandomKey` or a key of your own with `sniff.Extension` of the detected type.
- `FetchOptions.AllowAddr` that admits a whole private range "for the internal service". Admit the one address you mean, or a user can reach every other host in that range through your importer.
- Logging a signed URL, or a request URL that carries one. The signature is the credential: anyone who reads the log can use the link until it expires.
- Long link lifetimes as a substitute for public files. A seven-day link sent by email is a public file for seven days that you cannot revoke; use the records `share` plugin for links you must be able to revoke.
- A scanner wrapper that accepts on error "to stay up". It turns every scanner outage into a window for unscanned uploads. If you need that trade-off, queue the file for a rescan and keep it private until it passes.

## Next

- [Uploads](uploads.md): every path these defaults apply to.
- [Serving files](serving-files.md): the response headers in action.
- [Production](../start/production.md): the checklist before going live.
