# Serving files

Stored files reach browsers in two ways: a **public URL** anyone can fetch (a logo, a product photo, usually through a CDN), or a **signed URL** that authorises one download or upload of one key for a few minutes (an invoice, a private attachment, a direct upload). This page explains both, how they work on backends that cannot sign (local disk, memory, SFTP) through the `signedurl` and `fshttp` packages, and the headers that keep user content from running script on your origin. Anyone serving user files needs it.

The snippets come from `examples/concepts-serving`, which mounts the handlers on a test server and fetches real responses. Run it with `go run ./concepts-serving` from the `examples` directory.

## Two kinds of URL

| | Public URL | Signed URL |
|---|---|---|
| Method | `Disk.PublicURL(ctx, key)` | `Disk.SignedURL(ctx, key, PresignOptions)` |
| Who can use it | anyone, forever | whoever holds it, until it expires (at most seven days) |
| Built by | `WithPublicURL(base)` or the `public_url` DSN parameter: base plus escaped key, no backend call | the disk's `URLSigner` for the methods it signs (by default every download, and uploads when the backend cannot sign), otherwise the backend (S3 SigV4, GCS V4, Azure SAS) when it has `Presign` |
| Served by | the CDN or bucket, or `fshttp.Public` for local disk | the backend, or `fshttp.Signed` for a disk signed by `signedurl` |
| For | files meant to be public | private files, and uploads straight to storage |

`PublicURL` does not check that the object exists or is public: a URL for a private object fails when fetched. `SignedURL` needs an expiry; there is deliberately no default, so every caller decides how long a link may live.

## Step 1 — Signed URLs on any backend

S3, GCS (with a signing key) and Azure (with a shared key) sign their own URLs. Local disk, memory and SFTP cannot, so the application signs them: `signedurl.New(base, keys)` returns a `Signer` that implements `filesystem.URLSigner`, the disk gets it through `WithURLSigner`, and `fshttp.Signed` serves the links where `base` points. The same `SignedURL` call then works in development on local disk and in production on S3. A signer set on a disk whose backend signs too takes the downloads and leaves the uploads to the backend; [Step 11](#step-11--signed-cdn-links) shows how to choose.

<!-- ds:block id=newapp-rwpby4bv -->
**newApp** · [`examples/concepts-serving/main.go:92-118`](../../examples/concepts-serving/main.go#L92-L118)

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

The handler is mounted with its prefix stripped. It resolves the disk named in the link through `fshttp.Disks`, which `*filesystem.Manager` implements, and checks the signature with `fshttp.Verifier`, which `*signedurl.Signer` implements.

A link has the form `<base>/<disk>/<escaped key>?exp=…&kid=…&m=GET&sig=…`, plus `ct` and `cd` when the link overrides the content type or disposition. The signature is HMAC-SHA256 over the method, disk, key, expiry, content type and disposition, each length-prefixed so no value can be shifted into its neighbour, so none of them can be changed. Each secret must be at least `signedurl.MinKeyBytes` (32) bytes.

## Step 2 — Download

`fshttp.Signed` answers GET and HEAD through `http.ServeContent`, so ranges, `ETag` with `If-None-Match`, and `If-Modified-Since` work, and a range reads only that range from the backend:

<!-- ds:block id=signeddownload-7p72b6jy -->
**signedDownload** · [`examples/concepts-serving/main.go:152-177`](../../examples/concepts-serving/main.go#L152-L177)

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

```text
link: /files/private/invoices/42.pdf?exp&kid&m&sig
GET 200 application/pdf "%PDF-1.7 invoice"
Range 206 "%PDF"
If-None-Match 304
```

`PresignOptions.ContentType` overrides the response's `Content-Type` and `ContentDisposition` its `Content-Disposition`, for example `attachment; filename="invoice.pdf"` to make the browser save the file. The records Store's `DownloadURL` builds that disposition from the original file name, with a UTF-8 form for non-ASCII names.

## Step 3 — Refusals say nothing

A link whose key, method or any signed value was changed, an expired link, and a link signed with an unknown key ID all answer 403 with the status text only, so a forger learns nothing about which check failed. Expiries outside `(0, MaxPresignExpiry]` (seven days, the S3 SigV4 limit) are refused when the link is made: a link that must live longer is effectively public and should be a public file.

<!-- ds:block id=tampered-7sz65bwn -->
**tampered** · [`examples/concepts-serving/main.go:183-196`](../../examples/concepts-serving/main.go#L183-L196)

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
**expired** · [`examples/concepts-serving/main.go:201-217`](../../examples/concepts-serving/main.go#L201-L217)

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

Check: `tampered 403 "Forbidden"`, `eight-day link: invalid_argument`, `expired 403`.

A token carried anywhere but the query string (an `Authorization: Bearer` header, a cookie, the path itself) or a signing scheme other than `signedurl`'s needs a verifier that sees the whole request: when the verifier given to `fshttp.Signed` also implements `fshttp.RequestVerifier`, `VerifyRequest(r, method)` is called instead of `Verify`, and its claims name the disk and key to serve. `Signed` still fails closed on top of it: claims without a disk or key, for another method, without an expiry or already expired (by `SignedOptions.Now`) are 403, and the address range, content type and pinned headers are enforced as for any signed link.

```go
// bearer accepts download tokens from the Authorization header only.
type bearer struct{ tokens *TokenStore }

// Verify is never called: Signed prefers VerifyRequest.
func (bearer) Verify(string, string, filesystem.HTTPMethod, url.Values) (signedurl.Claims, error) {
	return signedurl.Claims{}, filesystem.ErrPermission
}

func (b bearer) VerifyRequest(r *http.Request, m filesystem.HTTPMethod) (signedurl.Claims, error) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return b.tokens.Claims(r.Context(), tok, m) // your store: disk, key, method, expiry
}

mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(manager, bearer{tokens})))
```

A signed link cannot be revoked before it expires: anyone holding it can use it until then. Keep lifetimes short (minutes for downloads a page renders, an hour for an emailed link) and, for links you must be able to withdraw, use the records `share` plugin, whose links are rows that can be revoked and limited to a number of downloads; see [Share links](../plugins/share.md).

Try it: [`examples/concepts-serving`](../../examples/concepts-serving) runs this section in `schemes.go` ([`bearer`](ds:block?id=bearer-c4n9w2rk), [`bearerTokens`](ds:block?id=bearertokens-h8v3m5qx)): `go run ./concepts-serving` from `examples/`.

## Step 4 — Rotating keys

`signedurl.New` takes a list of keys. The first signs; every key verifies. To rotate, put the new key first and keep the old one second until every link it signed has expired (at most `MaxPresignExpiry`), then remove it. The key ID travels in the link (`kid`), so the verifier picks the right secret directly.

<!-- ds:block id=rotation-dzyj6rqc -->
**rotation** · [`examples/concepts-serving/main.go:222-237`](../../examples/concepts-serving/main.go#L222-L237)

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

Check: `signed with the old key 200`.

### Limiting a link to an IP range

`PresignOptions.AllowedIP` restricts a URL to clients inside one CIDR prefix, in canonical form: `"203.0.113.0/24"`, or `"203.0.113.7/32"` for one address (`filesystem.ParseAllowedIP` checks it). The Disk issues such a URL only where the restriction is enforced: through `signedurl`, which signs the range as a claim that `fshttp.Signed` checks, or through a driver that declares `PresignIP` (Azure, as the SAS `sip` range, IPv4 only). Anywhere else the call is `unsupported`, never a URL that silently works from everywhere. `fshttp.Signed` checks the connection's own address; behind a proxy, list the proxy ranges in `SignedOptions.TrustedProxies` and only then is `X-Forwarded-For` believed. A request from outside the range gets the same bare 403 as a bad signature.

Use it for links that should only work from inside an office network or from one partner's servers: an export link sent by email that must not be usable if the email is forwarded, or a link handed to a back-office tool. Do not use it for links meant for phones, whose address changes between Wi-Fi and the mobile network. The program signs two links to one invoice, one for the loopback range its own client connects from and one for a documentation range it is not in:

<!-- ds:block id=iprestricted-3hmkztb6 -->
**ipRestricted** · [`examples/concepts-serving/main.go:390-409`](../../examples/concepts-serving/main.go#L390-L409)

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

Check: `link for 127.0.0.1/32: 200`, `link for 203.0.113.0/24: 403`, and `non-canonical range: invalid_argument` for `203.0.113.7/24`, whose host bits are set: the range must be written the way `ParseAllowedIP` would print it, so a typo cannot widen it.

## Step 5 — Active content is always a download

HTML, SVG, XML and JavaScript run script when a browser renders them. Served inline from your application's origin, an uploaded file of one of those types can read your users' cookies and call your API as them: stored cross-site scripting. Three defaults stop it, independently:

- `fshttp` sends every response with `X-Content-Type-Options: nosniff` (the browser must not guess another type) and `Content-Security-Policy: default-src 'none'; sandbox` (a document runs with no script and a unique origin; images and media embedded by other pages are unaffected).
- `fshttp` serves any type in `sniff.ActiveTypes` with `Content-Disposition: attachment`, whatever was stored or signed, so it downloads instead of rendering. The demo's link even asks for `inline`:

<!-- ds:block id=activecontent-9jn8d9f3 -->
**activeContent** · [`examples/concepts-serving/main.go:243-261`](../../examples/concepts-serving/main.go#L243-L261)

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

```text
html: text/html; charset=utf-8 | attachment | nosniff | default-src 'none'; sandbox
```

- `Disk.SignedURL` adds `Content-Disposition: attachment` to a GET link for an active `PresignOptions.ContentType` when the caller set no disposition, so links served by S3, GCS or Azure (which `fshttp` never sees) download too. It needs the content type in the options, which the records Store always passes; when you sign links yourself for user content, pass `ContentType: entry.ContentType`.

The upload policies refuse active content in the first place unless `AllowActiveContent` is set; see [Security](security.md).

### PDFs shown inline

Under the sandbox a browser shows an inline PDF as a blank page: its PDF viewer is a plugin object, which `sandbox` and `object-src 'none'` both forbid. `PublicOptions{InlinePDF: true}` and `SignedOptions{InlinePDF: true}` send `fshttp.InlinePDFCSP` (`default-src 'none'; object-src 'self'`) for a PDF that is not an attachment, and nothing changes for any other type or for a PDF served as an attachment. The viewer's script then runs with the file's origin, so turn it on only where the PDFs are trusted or files are served from a separate domain; see [Security](security.md).

## Step 6 — Signed uploads

A PUT link lets a client upload straight to storage. `fshttp.Signed` accepts it only when the link was signed for PUT. A content type fixed in the link must match the request's `Content-Type` header exactly; otherwise 403. Stored headers fixed in the link (`PresignOptions.Headers`: `Cache-Control`, `Content-Disposition`, `Content-Encoding`) work the same way: the request's three headers must be exactly those, they are stored with the object and sent with it on download (`Cache-Control` on a signed download only with `StoredCacheControl`, see [Step 9](#step-9--cache-control)), and a link that fixes none stores none of what the client sends, so a client never chooses how its upload is served. `SignedRequest.Header` lists every header the client must send. The body is capped at `SignedOptions.MaxUploadBytes` (default `DefaultMaxUploadBytes`, 5 GiB, S3's single-request limit) while streaming, and nothing larger is stored.

<!-- ds:block id=signedupload-gkp2atb5 -->
**signedUpload** · [`examples/concepts-serving/main.go:266-289`](../../examples/concepts-serving/main.go#L266-L289)

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

Check: `PUT with another type 403` and `PUT with the signed headers 200, stored 19 bytes as image/png`.

Note what the second line shows: the bytes are stored under the type the link fixed, without being checked. A signed PUT authorises the client to put bytes at a key; it does not check what they are. That is why direct uploads go through `upload.Presign` and then `upload.Confirm`, which reads the stored bytes and deletes what the policy refuses. `SignedRequest.Header` holds the headers the client must send unchanged (the signed content type); pass it to the client with the URL. See [Uploads](uploads.md).

## Step 7 — Public files

`fshttp.Public(disk)` serves a disk's public objects by key, for mounting where the disk's public URL points (with the prefix stripped). An object is served only when its visibility is public, or, on a disk without per-object visibility, when the disk's default is public. Private and missing objects both answer 404, so the handler does not reveal that a private key exists. Only GET and HEAD are allowed. Stored `HTTPHeaders` (`Cache-Control`, `Content-Disposition`, `Content-Encoding`) are sent with the file, by `fshttp` and by the backends that store them; an object without a stored `Cache-Control` gets the handler's default ([Step 9](#step-9--cache-control)).

<!-- ds:block id=publicfiles-94jkcbat -->
**publicFiles** · [`examples/concepts-serving/main.go:295-311`](../../examples/concepts-serving/main.go#L295-L311)

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

Check: `public logo.png 200 public, max-age=31536000, immutable`, `public draft.png 404`, `public missing.png 404`.

In production, public files are usually served by the bucket or a CDN, not by your application. `fshttp.Public` is for local disk in development and for small deployments that serve files themselves.

## Step 8 — A CDN in front

`WithPublicURL(base)` (or `public_url=` in the DSN) makes `PublicURL` return `base + "/" + escaped key` with no backend call. Point it at the CDN that fronts the bucket. With a `prefix`, the base gains the prefix too.

<!-- ds:block id=cdnurl-jevvsku7 -->
**cdnURL** · [`examples/concepts-serving/main.go:316-327`](../../examples/concepts-serving/main.go#L316-L327)

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

Check: `public URL https://cdn.example.com/products/blue%20shirt.jpg`.

## Step 9 — Cache-Control

Both handlers send a `Cache-Control` on every download, and both defaults are safe for any file: neither lets a shared cache hand out a copy the server was not asked for.

| Handler | Sends | Default | Configure |
|---|---|---|---|
| `fshttp.Public` | the object's stored `Cache-Control` when it has one (the application's choice for serving it publicly), else the configured value, else the default | `public, no-cache` (`DefaultPublicCacheControl`): caches may keep the file but revalidate it by `ETag` on every use, so a file made private or deleted stops being served at once | `PublicOptions{CacheControl: "public, max-age=86400"}`; `ForceCacheControl: true` sends it even over a stored value |
| `fshttp.Signed` | the configured value, else the default, whatever the object stored | `private, no-store` (`DefaultSignedCacheControl`): a signed link is a short-lived capability for one person, so no browser or CDN may keep the response past it | `SignedOptions{CacheControl: "private, max-age=60"}`; `StoredCacheControl: true` sends the object's stored value when it has one |

A signed download ignores the stored value by default because stored values are usually chosen for public serving (`public, max-age=31536000, immutable` for a CDN); sent with a private download, such a value would let a shared cache keep serving a file only the link's holder may read, past the link's expiry. Turn on `StoredCacheControl` only when every stored value is right for signed links.

A long `max-age` on public files is a promise that the bytes behind a URL never change and never become private: use it (in `PublicOptions` or per object) for content-addressed keys, or for files that stay public, and keep the default where a file can be hidden or replaced in place.

## Step 10 — Many files as one zip

`fshttp.ServeZip(w, r, disk, "photos.zip", files, fshttp.ZipOptions{})` streams the objects named in `files` (a `[]fshttp.ZipFile{Key, Name}`, `Name` defaulting to the key) as one zip archive. It reads one object at a time and writes it straight to the response, so neither an object nor the archive is held in memory, and the first bytes reach the client while later objects are still being read. Call it from your own handler after authorising the request and choosing the keys: it serves whatever it is given.

Everything that can be checked is checked before the first byte, and a failure there is an ordinary error response:

- the count: at least one file, at most `ZipOptions.MaxFiles` (default `DefaultZipMaxFiles`, 1,000), else 400;
- every entry name: a clean relative path (valid UTF-8, no control characters, no backslash, no leading `/` or drive letter, no `.` or `..` elements, at most `MaxZipNameBytes`), unique ignoring case, and never both a file and the directory of another entry, so the archive extracts the same on Windows, macOS and Linux and never outside its folder, else 400;
- every object: it must exist (404 otherwise), and their total size must stay within `ZipOptions.MaxBytes` (default `DefaultZipMaxBytes`, 4 GiB), else 413.

The response is `application/zip`, an attachment named by the filename argument (default `download.zip`, non-ASCII names encoded for every browser), with `Cache-Control: private, no-store`. Entries are stored uncompressed, since photos, videos and PDFs are compressed already; `ZipOptions.Deflate` compresses them. HEAD runs the same checks and sends the headers only.

After the headers, a failure cannot change the status, and an archive cut short could still look like a finished download. So a failure while streaming (a read error, an object whose size changed since it was checked, the client hanging up) is passed to `ZipOptions.OnError` for logging and the response is aborted with `http.ErrAbortHandler`: the connection is reset, the archive never gets its central directory, and the client sees a failed download rather than a complete-looking archive with files missing. An object that grows during the download is refused rather than streamed, so `MaxBytes` holds.

The example zips two invoices under new names, then shows a name with `..` refused before anything is sent:

<!-- ds:block id=zipdownload-k3v8q2wm -->
**zipDownload** · [`examples/concepts-serving/main.go:334-358`](../../examples/concepts-serving/main.go#L334-L358)

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

```text
zip 200 attachment; filename=invoices.zip 2026/41.pdf,2026/42.pdf
zip with ../ in a name 400
```

An archive is sent `Cache-Control: private, no-store`; `ZipOptions.CacheControl` replaces it for an archive of public files a CDN may keep, and `ZipOptions.Errors` renders a refusal found before the first byte in your own envelope.

## Step 11 — Signed CDN links

When a bucket sits behind a CDN that checks signatures itself, give the disk that CDN's signer instead of `signedurl`: `Disk.SignedURL` then returns a link the CDN accepts until it expires, and your application never sees the download. Each signer is a module of its own; the program is [`examples/concepts-delivery`](../../examples/concepts-delivery/README.md).

| Module | CDN feature | What the signer issues |
|---|---|---|
| [`contrib/cloudfront`](../../contrib/cloudfront/README.md) | CloudFront restricted viewer access (a trusted key group) | canned-policy URLs signed with the key group's private key, and signed cookies for every key under a prefix |
| [`contrib/cloudflare`](../../contrib/cloudflare/README.md) | Cloudflare token authentication (a WAF rule calling `is_timed_hmac_valid_v0`) | `?verify=<timestamp>-<mac>` URLs, timed so the rule expires each link exactly when asked |

<!-- ds:block id=cloudfrontlinks-p94veyrr -->
**cloudfrontLinks** · [`examples/concepts-delivery/main.go:70-103`](../../examples/concepts-delivery/main.go#L70-L103)

```go
func cloudfrontLinks(ctx context.Context, out io.Writer) error {
	// Real code loads the key from a secret store, e.g. with
	// sign.LoadPEMPrivKey; the demo makes a throwaway one.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	signer, err := cloudfront.New(cloudfront.Config{BaseURL: cloudfrontBase, KeyPairID: "K2JCJMDEHXQW5F", PrivateKey: key})
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	req, err := disk.SignedURL(ctx, "videos/42/poster.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cloudfront URL: %s\n", shape(req.URL))
	_, err = disk.SignedURL(ctx, "page.html", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime, ContentType: "text/html"})
	fmt.Fprintf(out, "cloudfront HTML link: %s\n", filesystem.CodeOf(err))
	cookies, _, err := signer.SignCookies(ctx, "videos/42", time.Hour)
	if err != nil {
		return err
	}
	names := make([]string, len(cookies))
	for i, c := range cookies {
		names[i] = c.Name
	}
	fmt.Fprintf(out, "cloudfront cookies: %s (Secure %t, HttpOnly %t)\n", strings.Join(names, ","), cookies[0].Secure, cookies[0].HttpOnly)
	fmt.Fprintf(out, "printed signer: %v\n", signer)
	return nil
}
```
<!-- /ds:block hash=a4f2aa -->

<!-- ds:block id=cloudflarelink-8bqds4qz -->
**cloudflareLink** · [`examples/concepts-delivery/main.go:108-127`](../../examples/concepts-delivery/main.go#L108-L127)

```go
func cloudflareLink(ctx context.Context, out io.Writer) error {
	// The rule's key; real code reads it from a secret store.
	secret := bytes.Repeat([]byte("k"), cloudflare.MinSecretBytes)
	signer, err := cloudflare.New(cloudflare.Config{BaseURL: cloudflareBase, Secret: secret, TTL: cloudflareRuleTTL})
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("downloads", memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	req, err := disk.SignedURL(ctx, "reports/q3.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "cloudflare URL: %s, valid %s\n", shape(req.URL), req.ExpiresAt.Sub(time.Now()).Round(time.Minute))
	_, err = disk.SignedURL(ctx, "reports/q3.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 4 * time.Hour})
	fmt.Fprintf(out, "cloudflare link longer than the rule's TTL: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=88dc1c -->

```text
cloudfront URL: https://cdn.example.com/media/videos/42/poster.jpg?Expires&Key-Pair-Id&Signature
cloudfront HTML link: unsupported
cloudfront cookies: CloudFront-Policy,CloudFront-Signature,CloudFront-Key-Pair-Id (Secure true, HttpOnly true)
printed signer: cloudfront.Signer{cloudfront.Config{BaseURL: "https://cdn.example.com/media", KeyPairID: "K2JCJMDEHXQW5F", PrivateKey: [REDACTED]}}
cloudflare URL: https://downloads.example.com/files/reports/q3.pdf?verify, valid 10m0s
cloudflare link longer than the rule's TTL: invalid_argument
```

A CDN serves what the origin stored: it cannot apply a per-link `Content-Type` or `Content-Disposition`. Both signers therefore refuse, with `unsupported`, a link that asks for one, and that includes the attachment the Disk forces on HTML, SVG, XML and JavaScript; a link that would render active content inline is never issued. CloudFront in front of S3 with origin access control can apply S3's `response-content-disposition`: set `cloudfront.Config.ResponseOverrides` once the cache policy forwards it. Store active files with `Content-Disposition: attachment` (and serve them from a separate domain) when they must go through a CDN. Neither signer accepts a PUT, and neither needs to: on a disk whose backend presigns, uploads stay with the bucket (see below). CloudFront signed cookies cover every object under a prefix as stored, query strings included, so an HTML or SVG file stored without `Content-Disposition: attachment` renders inline for the cookie holder: store active files with that header, or keep them out of cookie-signed prefixes. `SignCookies` refuses to run with `ResponseOverrides`, which would let the holder pick the type of any object. Keys and secrets print as `[REDACTED]` in every `fmt` verb and in `slog`.

CloudFront's cookie names are fixed, so a second `SignCookies` grant on the same `Path` replaces the first in the browser. A user who needs two prefixes at once (a member of two workspaces, a course plus a shared library) needs each grant on its own `Path`: `cloudfront.Config.CookiePathFor` returns the `Path` for each call's cleaned prefix, and nil or `""` keeps `CookiePath`. The returned `Path` is checked like `CookiePath`, and an error fails the call with no cookie issued. The `Path` only decides which requests carry the cookie; the signed policy still names the prefix, so a wider `Path` grants nothing more.

```go
signer, err := cloudfront.New(cloudfront.Config{
	BaseURL: "https://cdn.example.com/media", KeyPairID: keyPairID, PrivateKey: key,
	// The distribution serves the base URL's path "/media", so a grant on
	// "ws/a" is sent with requests under "/media/ws/a/".
	CookiePathFor: func(prefix string) (string, error) {
		if prefix == "" {
			return "", nil // the whole base URL: CookiePath
		}
		return "/media/" + prefix + "/", nil
	},
})
```

### Which path signs which method

A disk can have two signers: the backend's own presigning (S3, GCS, Azure) and the `URLSigner` given with `WithURLSigner`. The rule for who signs what:

| `WithURLSigner` call | Downloads (`MethodGet`) | Uploads (`MethodPut`) |
|---|---|---|
| none | the backend, or `CodeUnsupported` when it cannot sign | the backend, or `CodeUnsupported` |
| `WithURLSigner(s)` | `s`, always, even when the backend could sign | the backend when it can sign, otherwise `s` |
| `WithURLSigner(s, filesystem.MethodGet, filesystem.MethodPut)` | `s` | `s` |
| `WithURLSigner(s, filesystem.MethodPut)` | the backend, or `CodeUnsupported` | `s` |

So the CDN setup is one option on the S3 disk: CloudFront signs every download link and S3 still presigns the direct uploads, which go to the bucket, not through the CDN.

```go
signer, err := cloudfront.New(cloudfront.Config{BaseURL: "https://cdn.example.com/media", KeyPairID: keyPairID, PrivateKey: key})
disk, err := filesystem.NewDisk("media", s3Driver, filesystem.WithURLSigner(signer))
get, err := disk.SignedURL(ctx, "videos/42/poster.jpg", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: 10 * time.Minute}) // a CloudFront link
put, err := disk.SignedURL(ctx, "uploads/clip.mp4", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: 10 * time.Minute}) // an S3 presigned PUT
```

To swap in another model, name the methods: an application that wants every link, uploads included, to come through its own `fshttp.Signed` endpoint (to count, log or rate-limit them) passes `WithURLSigner(appSigner, filesystem.MethodGet, filesystem.MethodPut)`. A method routed to the signer never falls back to the backend: a signer error, or a signer that cannot honour `AllowedIP` or pinned `Headers`, fails the call rather than handing out a raw bucket link. Before this rule, a signer on an S3, GCS or Azure disk was silently ignored and every link was a bucket link.

Try it: [`examples/concepts-serving`](../../examples/concepts-serving) runs this section in `schemes.go` ([`cdnOverPresigning`](ds:block?id=cdnoverpresigning-z7k2p4tw)): `go run ./concepts-serving` from `examples/`.

## Step 12 — Let the proxy send the bytes

On a local disk behind nginx, Apache or lighttpd, `fshttp` can check the request and set the headers, then leave the bytes to the proxy, which sends files faster and frees the Go process. Set `Sendfile` in `PublicOptions` or `SignedOptions`:

<!-- ds:block id=sendfile-fz527whq -->
**sendfile** · [`examples/concepts-delivery/main.go:132-163`](../../examples/concepts-delivery/main.go#L132-L163)

```go
func sendfile(ctx context.Context, out io.Writer) error {
	dir, err := os.MkdirTemp("", "concepts-delivery-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	drv, err := local.New(dir)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("assets", drv, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return err
	}
	if _, err := disk.Write(ctx, "logos/acme corp.png", bytes.NewReader(pixel), filesystem.WriteOptions{}); err != nil {
		return err
	}
	handler := fshttp.Public(disk, fshttp.PublicOptions{Sendfile: &fshttp.Sendfile{
		Header: fshttp.HeaderAccelRedirect,
		// Only the cleaned key reaches the mapping, never the request path.
		Path: func(_, key string) (string, bool) {
			return protectedLocation + filesystem.EscapeKey(key), true
		},
	}})
	for _, path := range []string{"/logos/acme%20corp.png", "/logos/../../etc/passwd"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.test"+path, nil))
		h := rec.Header()
		fmt.Fprintf(out, "sendfile %s: %d %s=%q body %d bytes | %s | %s\n", path, rec.Code, fshttp.HeaderAccelRedirect, h.Get(fshttp.HeaderAccelRedirect), rec.Body.Len(), h.Get("Content-Type"), h.Get(fshttp.HeaderContentTypeOptions))
	}
	return nil
}
```
<!-- /ds:block hash=1ff5f6 -->

```text
sendfile /logos/acme%20corp.png: 200 X-Accel-Redirect="/protected/logos/acme%20corp.png" body 0 bytes | image/png | nosniff
sendfile /logos/../../etc/passwd: 400 X-Accel-Redirect="" body 12 bytes | text/plain; charset=utf-8 | nosniff
```

The mapping is the only link between a key and a file on disk, and it is the application's: it receives the disk's name and the object's driver-level key (`Disk.DriverKey`: cleaned, with every scope prefix applied, so on a tenant's scoped disk it is `tenants/t1/a.png`, never another tenant's `a.png`), never the request path, and it runs only after the signature, visibility and method checks pass. The handler answers `If-None-Match` and `If-Modified-Since` with 304 from the object's own `ETag` and `Last-Modified` before offloading; ranges are left to the proxy. Returning `false` (a disk the proxy cannot reach) streams the object as usual. With nginx, mark the location `internal` so clients cannot request it directly, and add the security headers there, because nginx keeps `Content-Type`, `Content-Disposition` and `Cache-Control` from the handler on an internal redirect but drops the others:

```nginx
location /protected/ {
    internal;
    alias /srv/app/assets/;
    add_header X-Content-Type-Options nosniff always;
    add_header Content-Security-Policy "default-src 'none'; sandbox" always;
}
```

For Apache's mod_xsendfile or lighttpd, set `Header: fshttp.HeaderSendfile` and return an absolute path under the disk's directory.

## Step 13 — Tiny files as data URIs

`Disk.DataURI(ctx, key, maxBytes)` returns a file as `data:<type>;base64,…`, to inline an icon or a placeholder into HTML, CSS or JSON and save a request:

<!-- ds:block id=datauri-awmfa2av -->
**dataURI** · [`examples/concepts-delivery/main.go:167-189`](../../examples/concepts-delivery/main.go#L167-L189)

```go
func dataURI(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("icons", memory.New())
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"pixel.png": pixel,
		"logo.svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"hero.png":  append(slices.Clone(pixel), make([]byte, inlineLimit)...),
	}
	for _, key := range []string{"pixel.png", "logo.svg", "hero.png"} {
		if _, err := disk.Write(ctx, key, bytes.NewReader(files[key]), filesystem.WriteOptions{}); err != nil {
			return err
		}
		uri, err := disk.DataURI(ctx, key, inlineLimit)
		if err != nil {
			fmt.Fprintf(out, "data URI %s: %s\n", key, filesystem.CodeOf(err))
			continue
		}
		fmt.Fprintf(out, "data URI %s: %s… (%d chars)\n", key, uri[:len("data:image/png;base64,")], len(uri))
	}
	return nil
}
```
<!-- /ds:block hash=db1e26 -->

```text
data URI pixel.png: data:image/png;base64,… (114 chars)
data URI logo.svg: rejected
data URI hero.png: too_large
```

It refuses anything `Stat` reports over `maxBytes` (`too_large`) and stored active types (`rejected`) before reading a byte, then caps the read at `maxBytes` + 1 because a backend's size can be stale, and sniffs the bytes, refusing content that is active whatever its stored type says. The type is the stored one with only a plain `charset` kept, so no stored parameter can break out of the URI. Choose `maxBytes` for where the URI goes: base64 is a third larger than the file, and a large URI in a page costs more than the request it saves.

## A disk as an io/fs.FS

`disk.FS(ctx)` is a read-only `io/fs` view of a disk: it implements `fs.FS`, `fs.StatFS`, `fs.ReadDirFS` and `fs.ReadFileFS`, and passes `testing/fstest.TestFS` on the memory and local drivers and on a driver with no optional capabilities. Anything written against the standard interfaces works on any backend: `http.FileServerFS(disk.FS(ctx))` for a quick static server (it has none of the protections of the handlers above, so use it for trusted content only), `template.ParseFS`, `fs.WalkDir`, `fs.Sub` for a directory.

- `io/fs` has no context, so `ctx` governs every call made through the view; every call goes through the Disk, so keys are cleaned and interceptors run.
- Names are `io/fs` paths (`.` is the root). A name the Disk would clean into a different key (a backslash, a control character) does not exist in the view, so two names never reach one object.
- Directories are implied by keys, as in `Disk.List`: a directory exists while a key lies below it, and the root always exists. Files report mode `0444`, directories `fs.ModeDir|0555`; `FileInfo.Sys()` returns the `filesystem.Entry`.
- Errors are `*fs.PathError`; `errors.Is(err, fs.ErrNotExist)` holds for a missing object and `fs.ErrPermission` for a refused one, and `filesystem.CodeOf(err)` still gives the Disk's code. An object is read only on the first `Read`, so `Stat` after `Open` downloads nothing.

The example walks a directory, reads a file and checks that a missing one is `fs.ErrNotExist`, all with standard library functions:

<!-- ds:block id=fsview-p6t4h9zr -->
**fsView** · [`examples/concepts-serving/main.go:364-383`](../../examples/concepts-serving/main.go#L364-L383)

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

```text
fs walk invoices/41.pdf,invoices/42.pdf | read "%PDF-1.7 41" | missing is fs.ErrNotExist: true
```

## HEAD requests

Both handlers answer `HEAD` as well as `GET`: the same status and headers (`Content-Length`, `Content-Type`, `ETag`, `Last-Modified`, `Accept-Ranges`), and no body. A client uses it to learn a file's size before choosing between a direct download and a ranged, resumable one, or to check that a link still works without spending the bandwidth. A signed GET link serves `HEAD` too, under the same signature, expiry and IP range; nothing separate has to be signed. `fshttp.Public` answers 404 to a `HEAD` for a private or missing object, exactly as for a `GET`.

<!-- ds:block id=headrequests-ng53kc7b -->
**headRequests** · [`examples/concepts-serving/main.go:415-432`](../../examples/concepts-serving/main.go#L415-L432)

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

Check: `HEAD signed 200 length=11 type=application/pdf body=0 bytes` and `HEAD public missing 404`.

## Handler reference

| Handler | Mount | Methods | Answers |
|---|---|---|---|
| `fshttp.Signed(disks, verifier, SignedOptions{})` | where the signer's base points, prefix stripped | GET, HEAD, PUT | 200, 206, 304; 403 for any signature, method or expiry failure; 404 for a missing object or an unknown disk; 405 otherwise; on a plain GET a read that fails before the body is answered with its own status (404, 503 with `Retry-After`) |
| `fshttp.Public(disk, PublicOptions{})` | where the disk's public URL points, prefix stripped | GET, HEAD | 200, 206, 304; 404 for private and missing; 405 otherwise; on a plain GET a read that fails before the body is answered with its own status (404, 503 with `Retry-After`) |
| `fshttp.ServeZip(w, r, disk, filename, files, ZipOptions{})` | called from your own handler | any (HEAD sends headers only) | 200 with a streamed zip; 400 for a bad list, 404 for a missing object, 413 over `MaxBytes`; a failure while streaming resets the connection |
| `fshttp.ServeEntry(w, r, disk, entry, ServeOptions{})` | called from your own handler, after your access check and a `Stat` | GET, HEAD | 200, 206, 304, 416, with the same `nosniff`, sandbox CSP and active-content-as-attachment rules as the handlers above |
| `fshttp.StatusOf(err)` | for your own handlers | | the status for an error's code; see [Errors](errors.md) |

### Serving from your own handler

A handler that authorises a download itself (a record's owner check, a variant proxy) and then copies the object with `io.Copy` loses ranges, conditional requests and the security headers. `fshttp.ServeEntry` is the path `Signed` and `Public` serve with, exported: hand it the `Entry` from `disk.Stat`, whose size, `ETag` and modification time answer `Range`, `If-None-Match` and `If-Modified-Since`, and optionally a content type, disposition or `Cache-Control` to send instead of the stored ones. Nothing in `ServeOptions` can drop `nosniff` or the sandbox policy, or render active content inline. Two opt-ins match `Public` and `Signed`: `Sendfile` leaves the body to nginx or Apache once your handler has checked the request and every header is set (see [Let the proxy send the bytes](#step-12--let-the-proxy-send-the-bytes)), and `InlinePDF` lets an inline PDF render in the browser's viewer with `InlinePDFCSP` ([PDFs shown inline](#pdfs-shown-inline)); no other type is affected. An entry whose key the disk refuses (a traversal, a key that did not come from `Stat`) is answered 400 before any header is sent, through `ServeOptions.Errors` when you set one. On a plain GET (no `Range` and no `If-*` header) the read is opened before the status too, so an object deleted since `Stat`, a driver failure or a throttled backend is answered with its status (404, or 503 with `Retry-After`) through the same writer rather than as an empty 200, and the reader opened is the one streamed. A ranged, HEAD or conditional request reads only after the status is sent, as do failures mid-stream, so hand it a fresh `Entry`.

```go
func download(w http.ResponseWriter, r *http.Request) {
	f, err := store.File(r.Context(), scopeOf(r), r.PathValue("id")) // a records.Store; check the caller may read it here
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	e, err := disk.Stat(r.Context(), f.Key)
	if err != nil {
		http.Error(w, fshttp.ClientMessage(err), fshttp.StatusOf(err))
		return
	}
	fshttp.ServeEntry(w, r, disk, e, fshttp.ServeOptions{CacheControl: "private, max-age=300"})
}
```

Try it: [`examples/concepts-serving`](../../examples/concepts-serving) runs this section in `ownhandler.go` ([`ownDownload`](ds:block?id=owndownload-c7m2x9ve), [`ownHandler`](ds:block?id=ownhandler-q4n8t2wb)): `go run ./concepts-serving` from `examples/`, then look for `own handler Range: 206 "Q3" bytes 0-1/21`.

## Pitfalls

- Mounting `fshttp.Signed` without `http.StripPrefix`: the handler expects paths of the form `/<disk>/<key>` and answers 404 to anything else.
- A disk that signs links but is not in the Manager given to `fshttp.Signed` (a scoped disk, typically): its links answer 404. Add every signing disk to that Manager.
- Serving user uploads from a public bucket on your application's own domain without the protections above. Put user content on a separate domain (a CDN hostname, `usercontent.example.com`), the defence the large hosting products use; the headers here are the second line.
- Long-lived signed links in emails or caches: they work for everyone who sees them until they expire.

## Next

- [Public and private files](public-and-private.md): which files should be public, and the two-bucket layout.
- [Uploads](uploads.md): presigned uploads with server-side confirmation.
- Recipes: [Private downloads](../recipes/private-downloads.md), [Public files behind a CDN](../recipes/public-cdn.md).
- Reference: [fshttp](../../fshttp/README.md), [signedurl](../../signedurl/README.md).
