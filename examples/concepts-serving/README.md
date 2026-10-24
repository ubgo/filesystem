# Serving files: signed links, public files and a CDN

Stored files reach browsers in two ways: private files through links that are signed and expire, public files by a plain URL. This program signs download and upload links with `signedurl`, serves them with `fshttp.Signed`, and fetches them for real: a whole file, a byte range, a 304 from the ETag, a tampered link, an expired one, a link signed with the previous key during rotation, an HTML file forced to download, a PUT link that pins the content type, public files with their stored `Cache-Control`, a public URL built from a CDN base, links limited to an IP range, and `HEAD` requests. It answers the product need behind every "download your invoice" or avatar URL, on a backend that has no presigning of its own. It uses the core `filesystem` package, `memory`, `signedurl` and `fshttp`. The guide [Serving files](../../docs/concepts/serving-files.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-serving
go test ./concepts-serving
```

It needs no environment variables. It starts an in-process `httptest` server on a random loopback port, makes every request against it, prints the results and closes the server before it exits. The signing secrets are fixed so the run is repeatable; real ones come from a secret store and are at least `signedurl.MinKeyBytes` (32) long. `cdn.example.com` is only used to build a URL and is never fetched.

## What it does

```sh
go run ./concepts-serving
```

```text
link: /files/private/invoices/42.pdf?exp&kid&m&sig
GET 200 application/pdf "%PDF-1.7 invoice"
Range 206 "%PDF"
If-None-Match 304
tampered 403 "Forbidden"
eight-day link: invalid_argument
expired 403
signed with the old key 200
html: text/html; charset=utf-8 | attachment | nosniff | default-src 'none'; sandbox
PUT with another type 403
PUT with the signed headers 200, stored 19 bytes as image/png
public logo.png 200 public, max-age=31536000, immutable
public draft.png 404 
public missing.png 404 
public URL https://cdn.example.com/products/blue%20shirt.jpg
zip 200 attachment; filename=invoices.zip 2026/41.pdf,2026/42.pdf
zip with ../ in a name 400
fs walk invoices/41.pdf,invoices/42.pdf | read "%PDF-1.7 41" | missing is fs.ErrNotExist: true
link for 127.0.0.1/32: 200
link for 203.0.113.0/24: 403
non-canonical range: invalid_argument
HEAD signed 200 length=11 type=application/pdf body=0 bytes
HEAD public missing 404
bearer GET with its token: 200 "quarterly"
bearer GET without a token: 403 "Forbidden"
bearer GET with an upload token: 403 "Forbidden"
bearer GET with an expired token: 403 "Forbidden"
WithURLSigner(cdn) on S3: GET CloudFront link on cdn.example.com, PUT S3 presigned on s3.example.com
WithURLSigner(cdn, GET, PUT) on S3: GET CloudFront link on cdn.example.com, PUT unsupported
own handler as bob: 403
own handler as alice: 200 "Q3 report: revenue up" | nosniff | private, max-age=300
own handler Range: 206 "Q3" bytes 0-1/21
own handler If-None-Match: 304
own handler html asked inline: attachment | default-src 'none'; sandbox
```

1. A signed GET link names the disk and key and carries an expiry, a key ID, the method and a signature (values left out because they change every run). The handler serves the file, a byte range (206) and a 304 for a matching `If-None-Match`.
2. Changing the key in the link gives a bare 403 that says nothing about why; a link longer than seven days is refused when it is signed.
3. A link that expired is a 403; a link signed with the previous key still works while that key stays in the signer's list, newest first.
4. An HTML file is served as an attachment with `nosniff` and a sandboxing CSP even though the link asked for `inline`.
5. A PUT link pins the content type: sending another type is a 403, sending the signed headers stores the file.
6. `fshttp.Public` serves public objects with their stored headers; a private object on the public disk answers 404, exactly like a missing one.
7. `WithPublicURL` builds the CDN URL with no backend call, escaping each key segment.
8. `fshttp.ServeZip` streams two invoices as one zip under new names, and refuses a name with `..` with 400 before sending a byte.
9. `Disk.FS` is a read-only `io/fs` view: `fs.WalkDir`, `fs.ReadFile` and `fs.Stat` work on the disk, and a missing object is `fs.ErrNotExist`.
10. A link limited to the loopback range works for this client; one limited to another range is a bare 403; a range that is not in canonical form is refused when the link is signed.
11. `HEAD` on a GET link and on a public URL returns the headers (size, type) with no body; a missing public object is a 404.
12. A verifier that also implements `fshttp.RequestVerifier` sees the whole request, so download tokens can travel in an `Authorization: Bearer` header (or a cookie, or the path) and come from the app's own token table. `Signed` still fails closed on the claims it returns: no token, a token for uploads only and an expired token are all a bare 403.
13. A CloudFront signer on an S3 disk signs every download as a CDN link while uploads stay S3 presigned PUTs to the bucket. Naming the methods (`GET, PUT`) routes uploads to the signer too, and since CloudFront cannot sign an upload the call is `unsupported` rather than falling back to a bucket link. Nothing connects to either host: presigning and CloudFront signing are local computations.
14. `ownhandler.go` is a download handler of the app's own (`ownDownload`): it authorises the request itself, here only the report's owner may read it, then hands the `Entry` from `disk.Stat` to `fshttp.ServeEntry`. Another user gets 403 before anything is read. The owner gets the whole report with `nosniff` and the handler's `Cache-Control`, the first two bytes for `Range: bytes=0-1` (206), and 304 for its `ETag`. An HTML page served through it with `ContentDisposition: "inline"` still goes out as an attachment under the sandbox policy, because nothing in `ServeOptions` can render active content inline.

## The code

The wiring: a private disk with a signer whose newest key signs and older key verifies, a public disk, a Manager over both, and the two handlers.

<!-- ds:block id=newapp-rwpby4bv -->
**newApp** · [`examples/concepts-serving/main.go:92-118`](main.go#L92-L118)

```go
func newApp() (*app, error) {
	routes := http.NewServeMux()
	srv := httptest.NewServer(routes)
	signer, err := signedurl.New(srv.URL+signedPrefix, []signedurl.Key{newKey, oldKey})
	if err != nil {
		srv.Close()
		return nil, err
	}
	private, err := filesystem.NewDisk("private", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		srv.Close()
		return nil, err
	}
	public, err := filesystem.NewDisk("public", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		srv.Close()
		return nil, err
	}
	disks, err := filesystem.NewManager(private, public)
	if err != nil {
		srv.Close()
		return nil, err
	}
	routes.Handle(signedPrefix+"/", http.StripPrefix(signedPrefix, fshttp.Signed(disks, signer)))
	routes.Handle(publicPrefix+"/", http.StripPrefix(publicPrefix, fshttp.Public(public)))
	return &app{srv: srv, private: private, public: public}, nil
}
```
<!-- /ds:block hash=1a0997 -->

A signed download, fetched whole, as a range and with its ETag.

<!-- ds:block id=signeddownload-7p72b6jy -->
**signedDownload** · [`examples/concepts-serving/main.go:152-177`](main.go#L152-L177)

```go
func signedDownload(ctx context.Context, a *app, out io.Writer) error {
	if _, err := a.private.WriteBytes(ctx, "invoices/42.pdf", []byte("%PDF-1.7 invoice"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "link: %s\n", shape(link.URL))
	resp, body, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "GET %d %s %q\n", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	resp, body, err = fetch(http.MethodGet, link.URL, nil, http.Header{"Range": {"bytes=0-3"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Range %d %q\n", resp.StatusCode, body)
	resp, _, err = fetch(http.MethodGet, link.URL, nil, http.Header{"If-None-Match": {resp.Header.Get("ETag")}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "If-None-Match %d\n", resp.StatusCode)
	return nil
}
```
<!-- /ds:block hash=c8e975 -->

The refusals: a tampered key, an over-long lifetime, an expired link.

<!-- ds:block id=tampered-7sz65bwn -->
**tampered** · [`examples/concepts-serving/main.go:183-196`](main.go#L183-L196)

```go
func tampered(ctx context.Context, a *app, out io.Writer) error {
	link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, body, err := fetch(http.MethodGet, strings.Replace(link.URL, "42.pdf", "43.pdf", 1), nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "tampered %d %q\n", resp.StatusCode, strings.TrimSpace(body))
	_, err = a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 8 * 24 * time.Hour})
	fmt.Fprintf(out, "eight-day link: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=7f373e -->

<!-- ds:block id=expired-vc9eaxv7 -->
**expired** · [`examples/concepts-serving/main.go:201-217`](main.go#L201-L217)

```go
func expired(ctx context.Context, a *app, out io.Writer) error {
	past := func() time.Time { return time.Now().Add(-time.Hour) }
	old, err := signedurl.New(a.srv.URL+signedPrefix, []signedurl.Key{newKey}, signedurl.WithClock(past))
	if err != nil {
		return err
	}
	link, err := old.SignURL(ctx, "private", "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "expired %d\n", resp.StatusCode)
	return nil
}
```
<!-- /ds:block hash=cf4c18 -->

Key rotation: a link signed with the old key still verifies.

<!-- ds:block id=rotation-dzyj6rqc -->
**rotation** · [`examples/concepts-serving/main.go:222-237`](main.go#L222-L237)

```go
func rotation(ctx context.Context, a *app, out io.Writer) error {
	before, err := signedurl.New(a.srv.URL+signedPrefix, []signedurl.Key{oldKey})
	if err != nil {
		return err
	}
	link, err := before.SignURL(ctx, "private", "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed with the old key %d\n", resp.StatusCode)
	return nil
}
```
<!-- /ds:block hash=6f20d1 -->

Active content is always a download.

<!-- ds:block id=activecontent-9jn8d9f3 -->
**activeContent** · [`examples/concepts-serving/main.go:243-261`](main.go#L243-L261)

```go
func activeContent(ctx context.Context, a *app, out io.Writer) error {
	page := []byte("<html><script>alert(document.cookie)</script></html>")
	if _, err := a.private.WriteBytes(ctx, "uploads/page.html", page, filesystem.WriteOptions{}); err != nil {
		return err
	}
	link, err := a.private.SignedURL(ctx, "uploads/page.html", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: linkLifetime, ContentDisposition: "inline",
	})
	if err != nil {
		return err
	}
	resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
	if err != nil {
		return err
	}
	h := resp.Header
	fmt.Fprintf(out, "html: %s | %s | %s | %s\n", h.Get("Content-Type"), h.Get("Content-Disposition"), h.Get(fshttp.HeaderContentTypeOptions), h.Get(fshttp.HeaderCSP))
	return nil
}
```
<!-- /ds:block hash=f55718 -->

A signed upload that pins the content type.

<!-- ds:block id=signedupload-gkp2atb5 -->
**signedUpload** · [`examples/concepts-serving/main.go:266-289`](main.go#L266-L289)

```go
func signedUpload(ctx context.Context, a *app, out io.Writer) error {
	link, err := a.private.SignedURL(ctx, "avatars/u1.png", filesystem.PresignOptions{
		Method: filesystem.MethodPut, Expires: linkLifetime, ContentType: "image/png",
	})
	if err != nil {
		return err
	}
	png := []byte("\x89PNG\r\n\x1a\n not really")
	resp, _, err := fetch(http.MethodPut, link.URL, bytes.NewReader(png), http.Header{"Content-Type": {"text/html"}})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "PUT with another type %d\n", resp.StatusCode)
	resp, _, err = fetch(http.MethodPut, link.URL, bytes.NewReader(png), link.Header)
	if err != nil {
		return err
	}
	e, err := a.private.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "PUT with the signed headers %d, stored %d bytes as %s\n", resp.StatusCode, e.Size, e.ContentType)
	return nil
}
```
<!-- /ds:block hash=bd66ef -->

Public files, and a private one that answers 404.

<!-- ds:block id=publicfiles-94jkcbat -->
**publicFiles** · [`examples/concepts-serving/main.go:295-311`](main.go#L295-L311)

```go
func publicFiles(ctx context.Context, a *app, out io.Writer) error {
	headers := filesystem.HTTPHeaders{CacheControl: "public, max-age=31536000, immutable"}
	if _, err := a.public.WriteBytes(ctx, "logo.png", []byte("\x89PNG\r\n\x1a\n"), filesystem.WriteOptions{Headers: headers}); err != nil {
		return err
	}
	if _, err := a.public.WriteBytes(ctx, "draft.png", []byte("\x89PNG\r\n\x1a\n"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate}); err != nil {
		return err
	}
	for _, key := range []string{"logo.png", "draft.png", "missing.png"} {
		resp, _, err := fetch(http.MethodGet, a.srv.URL+publicPrefix+"/"+key, nil, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "public %s %d %s\n", key, resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	return nil
}
```
<!-- /ds:block hash=2ec47e -->

A public URL from a CDN base.

<!-- ds:block id=cdnurl-jevvsku7 -->
**cdnURL** · [`examples/concepts-serving/main.go:316-327`](main.go#L316-L327)

```go
func cdnURL(ctx context.Context, _ *app, out io.Writer) error {
	disk, err := filesystem.NewDisk("cdn", memory.New(), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return err
	}
	u, err := disk.PublicURL(ctx, "products/blue shirt.jpg")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "public URL %s\n", u)
	return nil
}
```
<!-- /ds:block hash=d426ce -->

`zipDownload` hands `fshttp.ServeZip` the keys and the names they get in the archive; every check runs before the first byte.

<!-- ds:block id=zipdownload-k3v8q2wm -->
**zipDownload** · [`examples/concepts-serving/main.go:334-358`](main.go#L334-L358)

```go
func zipDownload(ctx context.Context, a *app, out io.Writer) error {
	for key, body := range map[string]string{"invoices/41.pdf": "%PDF-1.7 41", "invoices/42.pdf": "%PDF-1.7 42"} {
		if _, err := a.private.WriteBytes(ctx, key, []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	files := []fshttp.ZipFile{{Key: "invoices/41.pdf", Name: "2026/41.pdf"}, {Key: "invoices/42.pdf", Name: "2026/42.pdf"}}
	rec := httptest.NewRecorder()
	fshttp.ServeZip(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/invoices.zip", nil), a.private, "invoices.zip", files, fshttp.ZipOptions{})
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	fmt.Fprintf(out, "zip %d %s %s\n", rec.Code, rec.Header().Get("Content-Disposition"), strings.Join(names, ","))

	rec = httptest.NewRecorder()
	evil := []fshttp.ZipFile{{Key: "invoices/41.pdf", Name: "../../etc/passwd"}}
	fshttp.ServeZip(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/evil.zip", nil), a.private, "evil.zip", evil, fshttp.ZipOptions{})
	fmt.Fprintf(out, "zip with ../ in a name %d\n", rec.Code)
	return nil
}
```
<!-- /ds:block hash=c13b81 -->

`fsView` reads the disk through `io/fs`, the interface the standard library's file functions accept.

<!-- ds:block id=fsview-p6t4h9zr -->
**fsView** · [`examples/concepts-serving/main.go:364-383`](main.go#L364-L383)

```go
func fsView(ctx context.Context, a *app, out io.Writer) error {
	fsys := a.private.FS(ctx)
	var files []string
	err := fs.WalkDir(fsys, "invoices", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		return err
	}
	b, err := fs.ReadFile(fsys, "invoices/41.pdf")
	if err != nil {
		return err
	}
	_, statErr := fs.Stat(fsys, "invoices/missing.pdf")
	fmt.Fprintf(out, "fs walk %s | read %q | missing is fs.ErrNotExist: %v\n", strings.Join(files, ","), b, errors.Is(statErr, fs.ErrNotExist))
	return nil
}
```
<!-- /ds:block hash=2c3e67 -->

A link limited to one address range.

<!-- ds:block id=iprestricted-3hmkztb6 -->
**ipRestricted** · [`examples/concepts-serving/main.go:390-409`](main.go#L390-L409)

```go
func ipRestricted(ctx context.Context, a *app, out io.Writer) error {
	for _, allowed := range []string{loopback, officeRange} {
		link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{
			Method: filesystem.MethodGet, Expires: linkLifetime, AllowedIP: allowed,
		})
		if err != nil {
			return err
		}
		resp, _, err := fetch(http.MethodGet, link.URL, nil, nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "link for %s: %d\n", allowed, resp.StatusCode)
	}
	_, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: linkLifetime, AllowedIP: "203.0.113.7/24",
	})
	fmt.Fprintf(out, "non-canonical range: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=3ff438 -->

Headers without the body.

<!-- ds:block id=headrequests-ng53kc7b -->
**headRequests** · [`examples/concepts-serving/main.go:415-432`](main.go#L415-L432)

```go
func headRequests(ctx context.Context, a *app, out io.Writer) error {
	link, err := a.private.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	resp, body, err := fetch(http.MethodHead, link.URL, nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "HEAD signed %d length=%s type=%s body=%d bytes\n",
		resp.StatusCode, resp.Header.Get("Content-Length"), resp.Header.Get("Content-Type"), len(body))
	resp, _, err = fetch(http.MethodHead, a.srv.URL+publicPrefix+"/missing.png", nil, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "HEAD public missing %d\n", resp.StatusCode)
	return nil
}
```
<!-- /ds:block hash=813942 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the app wiring and one function per step |
| `schemes.go` | Bearer tokens through `fshttp.RequestVerifier`, and a CloudFront signer on a presigning S3 disk |
| `ownhandler.go` | Serving from the app's own handler with `fshttp.ServeEntry`; code: [`ownDownload`](ds:block?id=owndownload-c7m2x9ve), [`ownHandler`](ds:block?id=ownhandler-q4n8t2wb) |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Serving files](../../docs/concepts/serving-files.md), with the handler reference and pitfalls.
- Related examples: [concepts-visibility](../concepts-visibility) for public and private disks, [concepts-security](../concepts-security), [recipe-private-downloads](../recipe-private-downloads), [recipe-public-cdn](../recipe-public-cdn), [recipe-browser-direct-uploads](../recipe-browser-direct-uploads).
