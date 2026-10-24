# fshttp

`github.com/ubgo/filesystem/fshttp` serves disks over `net/http`: the signed download and upload URLs the `signedurl` package issues, and a disk's public files. It is what gives a local, memory or SFTP disk the same URL behaviour as an object store. Every response carries `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`, and active content (HTML, SVG, XML, JavaScript) is always served as a download.

The concepts and a runnable walk-through are in [Serving files](../docs/concepts/serving-files.md).

Wiring a signer, two disks and both handlers:

<!-- ds:block id=newapp-rwpby4bv -->
**newApp** · [`examples/concepts-serving/main.go:92-118`](../examples/concepts-serving/main.go#L92-L118)

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

## Handlers

| Name | What it is |
|---|---|
| `Signed(disks, verifier, SignedOptions...)` | serves signed URLs mounted where the signer's base points: GET and HEAD download (ranges, `ETag`, `If-None-Match`, `If-Modified-Since`), PUT uploads when the URL was signed for PUT (requiring exactly the `Content-Type` and stored headers the URL fixes, and storing those headers); every signature, method or expiry failure is 403 with no detail |
| `SignedOptions` | `MaxUploadBytes`: the cap on a signed PUT body, 0 meaning `DefaultMaxUploadBytes`; `CacheControl`: sent on every signed download in place of the object's stored one, empty meaning `DefaultSignedCacheControl` (`private, no-store`); `StoredCacheControl`: send the object's stored `Cache-Control` when it has one; `Sendfile`: leave download bodies to the front proxy; `TrustedProxies`: the proxy ranges whose `X-Forwarded-For` is believed when checking an IP-restricted URL (`PresignOptions.AllowedIP`), which is otherwise checked against the connection's own address; `InlinePDF`: serve inline PDFs with `InlinePDFCSP` (off by default, see below); `Errors`: an `ErrorWriter` for every failure, nil meaning plain status text; `Now`: the clock a `RequestVerifier`'s claims are checked against, nil meaning `time.Now` |
| `DefaultMaxUploadBytes` | 5 GiB, S3's single-request limit |
| `Public(disk, PublicOptions...)` | serves a disk's public objects by key; private and missing objects both answer 404; GET and HEAD only. `Cache-Control` is the object's stored one (unless `PublicOptions.ForceCacheControl`), else `PublicOptions.CacheControl`, else `DefaultPublicCacheControl` (`public, no-cache`: cacheable, revalidated on every use) |
| `PublicOptions` | `CacheControl`, `ForceCacheControl` (above); `Sendfile`: leave download bodies to the front proxy; `InlinePDF`: serve inline PDFs with `InlinePDFCSP`; `Errors`: an `ErrorWriter` for every failure, nil meaning plain status text (and `http.NotFound`'s body for a private object) |
| `Sendfile` | `Header` (empty: `HeaderAccelRedirect`) and `Path(disk, key) (value, ok)`: after the handler has checked the request and set every response header (type, disposition, the attachment for active types, cache, `ETag`, `Last-Modified`, `nosniff`, CSP) and answered `If-None-Match` and `If-Modified-Since` with 304 itself, it answers with `Header: value` and no body; ranges are left to the proxy, and nginx, Apache or lighttpd sends the file. `Path` receives the disk's name and the object's driver-level key (`Disk.DriverKey`: cleaned, with every scope prefix applied, so a scoped tenant disk can only name its own files), never the request path; `ok` false, an empty value or one with a control character streams the object as usual. nginx drops `X-Content-Type-Options` and `Content-Security-Policy` on an internal redirect, so its internal location must `add_header` them |
| `HeaderAccelRedirect`, `HeaderSendfile` | `"X-Accel-Redirect"` (nginx: an internal-location URI) and `"X-Sendfile"` (Apache mod_xsendfile, lighttpd: an absolute path) |
| `ServeEntry(w, r, disk, entry, ServeOptions{ContentType, ContentDisposition, CacheControl, Sendfile, InlinePDF, Errors})` | the serve path of `Signed` and `Public`, for an application handler that has authorised the request and has the object's `Entry` from `Stat`: answers `Range` (206, 416), `If-None-Match` and `If-Modified-Since` (304) and reads only the requested range from the backend; every response keeps `nosniff`, the sandbox CSP and active content as an attachment, which no option can loosen; empty options keep the stored type, disposition and `Cache-Control`; `Sendfile` hands the body to the front proxy and `InlinePDF` serves an inline PDF with `InlinePDFCSP`, exactly as on `PublicOptions`, both off by default; an entry whose key the disk refuses is a 400 through `Errors` (nil is `PlainErrors`) before any header is set |
| `ServeZip(w, r, disk, filename, files, ZipOptions)` | streams objects as one zip without buffering; checks the count (`MaxFiles`, default `DefaultZipMaxFiles` 1,000), every entry name (clean, portable, no traversal, no case or file/directory collisions, at most `MaxZipNameBytes`) and every object's existence and total size (`MaxBytes`, default `DefaultZipMaxBytes` 4 GiB) before the first byte; `Deflate` compresses; `CacheControl` replaces the default `private, no-store` (for archives of public files a CDN may keep); `Errors` renders a failure found before the first byte; a failure after the headers goes to `OnError` and aborts the connection (`http.ErrAbortHandler`) so a cut-short archive never looks complete |
| `ZipFile` | `Key`, and `Name` inside the archive (empty: the key) |
| `Disks` | `Disk(name) (*filesystem.Disk, error)`: resolves the disk a signed URL names; `*filesystem.Manager` implements it |
| `Verifier` | `Verify(disk, key, method, query) (signedurl.Claims, error)`, given the disk and key from the path `/{disk}/{key}` and the query string; `*signedurl.Signer` implements it |
| `RequestVerifier` | optional upgrade of `Verifier`: `VerifyRequest(r, method) (signedurl.Claims, error)` sees the whole request, for a token in a header (`Authorization: Bearer`), a cookie or the path, or another signing scheme. `Signed` uses it when the verifier implements it, then fails closed: claims without a disk or key, for another method, without an expiry or already expired are 403 |
| `ErrorWriter` | `func(w, r, status, err)`: renders an error response in the application's own envelope (problem+json, its API's shape); every handler of the library accepts one. It gets the status the handler chose and the error (never nil; for logs; show clients `ClientMessage(err)`), with `Retry-After` already set |
| `PlainErrors` | fshttp's default `ErrorWriter`: the status text as `text/plain` |

A signed PUT with a content type fixed in the URL must send exactly that `Content-Type`, and the object is stored under it; without one, the Disk detects the type from the bytes. The body is capped while streaming, and nothing larger is stored. A signed PUT does not check the bytes against an upload policy: pair it with `upload.Confirm`.

## Helpers and constants

| Name | What it is |
|---|---|
| `StatusOf(err)` | the HTTP status for an error's `filesystem.Code` (404, 409, 400, 501, 403, 413, 415, 507, 503, 500); for application handlers built on a Disk |
| `HeaderContentTypeOptions` | `"X-Content-Type-Options"`, sent as `nosniff` on every response |
| `HeaderCSP` | `"Content-Security-Policy"`, sent as `default-src 'none'; sandbox` on every response, except an inline PDF when `InlinePDF` is on |
| `InlinePDFCSP` | `default-src 'none'; object-src 'self'`: what an inline PDF gets with `InlinePDF`, because the `sandbox` directive and `object-src 'none'` both make Chromium's and WebKit's PDF viewers show a blank page. No other type and no attachment is affected, and `nosniff` stays. The viewer's own script then runs with the file's origin, so turn it on only for PDFs from trusted users, or serve files from another origin |
| `ClientMessage(err)` | the text a client may be shown for `err`: the error's own cause for `too_large`, `rejected`, `quota_exceeded`, `invalid_argument` and `invalid_key` (never the operation, disk or key), the status text of `StatusOf(err)` otherwise. Application handlers must answer with it instead of `err.Error()`, which names the disk and the storage key |
| `SetRetryAfter(h, err)` | sets `Retry-After` (whole seconds, rounded up) when err carries a backend's requested wait (`filesystem.RetryAfterOf`); every error response from this package and from `upload` calls it |
| `HeaderRetryAfter` | `"Retry-After"` |
| `ParseRetryAfter(value, now)` | reads a `Retry-After` value (seconds or an HTTP date) into a wait; false for an empty, malformed, negative or past value. Drivers use it to fill `filesystem.Error.RetryAfter` from a 429 or 503 |

Error responses carry the status text only, so driver messages and other keys never reach a client. `Public`, `Signed` and `ServeEntry` open the read before the status on a plain GET (no `Range`, no `If-*` header), so a read that fails there (an object deleted since `Stat`, a driver failure, a throttled backend) is an error response with `StatusOf(err)` and `Retry-After`, not a 200 with an empty body; the opened reader is the one streamed. Ranged, HEAD and conditional requests read after the status, as does the rest of every body, so a failure mid-stream cannot change it. To answer in another shape, give the handler an `ErrorWriter`:

```go
problem := func(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status >= 500 {
		slog.ErrorContext(r.Context(), "storage", "err", err) // the cause, for logs only
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "title": fshttp.ClientMessage(err)})
}
mux.Handle("/files/", http.StripPrefix("/files", fshttp.Signed(disks, signer, fshttp.SignedOptions{Errors: problem})))
```

See [Errors](../docs/concepts/errors.md) and [Security](../docs/concepts/security.md).
