# Import from URL: fetching a pasted link without SSRF

"Import from URL", "set avatar from a link", "attach from a Dropbox link", a CMS pulling an image from another site: the server fetches a URL a user typed, which is exactly what an attacker wants it to do with `http://169.254.169.254/` or `http://localhost:6379/`. This program routes every connection through the SSRF guard of `upload.Fetch` (checked on the resolved address, again on every redirect, with a timeout, a redirect limit and a size limit read from `Content-Length` before the body), then stores and records the bytes with `records.Store.Upload` under the same `upload.Policy` as any other upload, so the type is detected from the bytes and never from the remote server's header. The guide is [Import from URL](../../docs/recipes/import-from-url.md).

## Run it

```sh
cd examples
go run ./recipe-import-from-url
```

It creates a new temporary directory (printed at start-up) holding `app.db` (SQLite, the `fs_` tables) and `files/`, and listens on `http://localhost:8108`. The user is the `X-User` header, a demo shortcut: a real app reads its session. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Run the tests with `go test ./recipe-import-from-url`.

`IMPORT_ALLOW_ADDRS` lists exact addresses (comma-separated) that imports may reach despite the guard. The demo's own `/samples` routes stand in for another site, so to import from them start it with:

```sh
IMPORT_ALLOW_ADDRS=127.0.0.1 go run ./recipe-import-from-url
```

## Walkthrough

1. Without an allow list, every internal or non-HTTP target is refused. `localhost` and `[::1]` are refused too, because the check runs on the resolved address, not on the URL's text:

```sh
for u in http://localhost:8108/samples/photo.jpg http://127.0.0.1:6379/ http://10.0.0.5/admin \
         http://169.254.169.254/latest/meta-data/ "http://[::1]:8108/samples/photo.jpg" \
         file:///etc/passwd "http://user:pw@example.com/a.jpg"; do
  curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d "{\"url\":\"$u\"}" -w " %{http_code}\n"
done
```

```text
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
{"error":"upload: only http and https URLs can be fetched","code":"invalid_argument"}
 400
{"error":"upload: URLs with credentials are refused","code":"invalid_argument"}
 400
```

2. From the public internet (this step needs network access), a text file is refused by its detected type and an image is imported; without `X-User` the request is refused:

```sh
curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d '{"url":"https://www.google.com/robots.txt"}' -w ' %{http_code}\n'
curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d '{"url":"https://www.google.com/images/branding/googlelogo/1x/googlelogo_color_272x92dp.png"}' -w ' %{http_code}\n'
curl -s -X POST localhost:8108/imports -d '{"url":"https://www.google.com/robots.txt"}' -w ' %{http_code}\n'
```

```text
{"error":"content type \"text/plain; charset=utf-8\" is not allowed","code":"rejected"}
 415
{"file_id":"fil_lmfna3s4h6cs34uj7syyecpwbv","filename":"googlelogo_color_272x92dp.png","content_type":"image/png","size":5969}
 201
{"error":"Forbidden","code":"permission"}
 403
```

3. Restarted with `IMPORT_ALLOW_ADDRS=127.0.0.1`, imports from the sample routes:

```sh
for u in http://127.0.0.1:8108/samples/photo.jpg http://localhost:8108/samples/photo.jpg \
         http://127.0.0.1:8108/samples/huge.jpg http://127.0.0.1:8108/samples/metadata \
         http://127.0.0.1:8108/samples/missing.jpg http://127.0.0.2:8108/samples/photo.jpg; do
  curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d "{\"url\":\"$u\"}" -w " %{http_code}\n"
done
```

```text
{"file_id":"fil_4qv7u33inkeivsitypbtvnffut","filename":"sunset.jpg","content_type":"image/jpeg","size":4180}
 201
{"file_id":"fil_pwtmpujcbdwx63lf6kygygjbed","filename":"sunset.jpg","content_type":"image/jpeg","size":4180}
 201
{"error":"upload: the remote file is 67108864 bytes, over the limit of 10485760","code":"too_large"}
 413
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
{"error":"Not Found","code":"not_found"}
 404
{"error":"address is not on the public internet","code":"invalid_argument"}
 400
```

What each line shows:

- The sample is served as `application/octet-stream` with `Content-Disposition: attachment; filename="sunset.jpg"`: the name comes from the header, the type from the bytes.
- `localhost` works now because one of its addresses is the allowed `127.0.0.1`.
- `/samples/huge.jpg` announces 64 MiB and is refused before any body byte is read.
- `/samples/metadata` is on an allowed address, but it redirects to `169.254.169.254`, and the redirect's connection is checked again and refused.
- `127.0.0.2` is loopback too, but not the listed address.

## The code

`importPolicy` accepts images and PDFs, detected from the fetched bytes.

<!-- ds:block id=importpolicy-6yfetmpk -->
**importPolicy** · [`examples/recipe-import-from-url/main.go:85-89`](main.go#L85-L89)

```go
var importPolicy = upload.Policy{
	MaxSize: maxImportBytes,
	Allowed: []string{"image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf"},
	Key:     upload.RandomKey("imports"),
}
```
<!-- /ds:block hash=9f2aa8 -->

`fetchOptions` bounds every import: a timeout for the whole fetch, three redirects, and the policy's size limit so an oversized `Content-Length` is refused up front.

<!-- ds:block id=fetchoptions-f7b2zwau -->
**fetchOptions** · [`examples/recipe-import-from-url/main.go:96-103`](main.go#L96-L103)

```go
func fetchOptions(allow func(netip.Addr) bool) upload.FetchOptions {
	return upload.FetchOptions{
		Timeout:      fetchTimeout,
		MaxRedirects: maxRedirects,
		MaxSize:      importPolicy.MaxSize,
		AllowAddr:    allow,
	}
}
```
<!-- /ds:block hash=0a07aa -->

`allowList` turns `IMPORT_ALLOW_ADDRS` into an `AllowAddr` function. Exact addresses, never "any private address", keep the guard closed for the rest of the network.

<!-- ds:block id=allowlist-qudb84q4 -->
**allowList** · [`examples/recipe-import-from-url/main.go:109-122`](main.go#L109-L122)

```go
func allowList(raw string) (func(netip.Addr) bool, error) {
	if raw == "" {
		return nil, nil
	}
	allowed := map[netip.Addr]bool{}
	for _, s := range strings.Split(raw, ",") {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envAllowAddrs, err)
		}
		allowed[a] = true
	}
	return func(a netip.Addr) bool { return allowed[a] }, nil
}
```
<!-- /ds:block hash=59fcf6 -->

`importFrom` fetches through the guard, then uploads and attaches the file in one `Store.Tx`; a rolled-back transaction deletes the stored object. The name comes from `Content-Disposition`, else the URL's last path segment, made safe.

<!-- ds:block id=app-importfrom-a59m22gc -->
**App.importFrom** · [`examples/recipe-import-from-url/main.go:175-190`](main.go#L175-L190)

```go
func (a *App) importFrom(ctx context.Context, user, rawURL string) (records.File, error) {
	remote, err := upload.Fetch(ctx, rawURL, a.Fetch)
	if err != nil {
		return records.File{}, err
	}
	defer func() { _ = remote.Body.Close() }()
	var f records.File
	err = a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if f, err = a.Store.Upload(ctx, "", diskName, remote.Body, remote.Filename, importPolicy); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerUser, ID: user}, roleImports)
		return err
	})
	return f, err
}
```
<!-- /ds:block hash=c64ba2 -->

`importURL` is the route: it requires a user, caps the JSON body and answers with the recorded file.

<!-- ds:block id=app-importurl-6t52qvb4 -->
**App.importURL** · [`examples/recipe-import-from-url/main.go:207-224`](main.go#L207-L224)

```go
func (a *App) importURL(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get(headerUser)
	if user == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	var req importRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes)).Decode(&req); err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid JSON body"))
		return
	}
	f, err := a.importFrom(r.Context(), user, req.URL)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fileResponse{FileID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size})
}
```
<!-- /ds:block hash=cd204e -->

`writeError` returns the message only for codes about the client's input, and the message never includes a resolved internal address.

<!-- ds:block id=writeerror-dwktbxnt -->
**writeError** · [`examples/recipe-import-from-url/main.go:254-257`](main.go#L254-L257)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=e4052c -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, fetch options, allow list, the import route, the sample routes standing in for another site, and the server. |
| `main_test.go` | An import from an allowed host, the guard refusing internal addresses, and allow-list parsing. |

## Next

- The guide: [Import from URL](../../docs/recipes/import-from-url.md), with the edge cases it handles and production notes.
- [Uploads](../../docs/concepts/uploads.md) for `Fetch`, `FromURL`, `PublicAddr` and the policy, and [Security](../../docs/concepts/security.md) for the security model.
- Related recipes: [recipe-avatars](../recipe-avatars) (what to do with an imported profile picture) and [recipe-strip-photo-metadata](../recipe-strip-photo-metadata) (`Policy.Process` runs on imports too).
