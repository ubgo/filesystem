# Import from a URL

"Paste a link instead of uploading" shows up everywhere: set an avatar from a Gravatar or social profile, attach a file from a Dropbox or Drive share link, let a CMS pull an image from another site, import a product catalogue's images. It is also the textbook server-side request forgery (SSRF): the server fetches whatever URL a user typed, and the user types `http://169.254.169.254/latest/meta-data/` to read your cloud credentials, or `http://localhost:6379/` to talk to your Redis. This recipe is for any app that fetches user-supplied URLs, and the guard it uses applies equally to webhooks and link previews.

The program is [examples/recipe-import-from-url](../../examples/recipe-import-from-url). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `POST /imports {"url": "…"}` fetches the URL, stores the file under the same policy as an upload, and records and attaches it to the user.
- Connections to loopback, private networks, link-local addresses (cloud metadata), CGNAT, multicast, reserved and NAT64 addresses are refused, after DNS resolution and on every redirect, so a hostname that resolves to `127.0.0.1` or a public URL that redirects inward is caught.
- Only `http` and `https`, no credentials in the URL, at most 3 redirects, 30 seconds for the whole fetch.
- A response announcing more than 10 MiB is refused before its body is read; a body without a length is counted as it streams.
- The type comes from the fetched bytes, never from the remote server's `Content-Type`. The file name comes from `Content-Disposition`, else the URL's last path segment, made safe.
- An operator can allow exact internal addresses (`IMPORT_ALLOW_ADDRS`) for a known internal source, without opening the guard for anything else.

## The design

| Piece | Why |
|---|---|
| `upload.Fetch` | The guarded GET. The check runs in the dialer's `Control` hook, on the resolved IP the connection is about to use, which is the only place DNS rebinding cannot get around. The environment's `HTTP_PROXY` is never read, since a proxy would be dialed instead of the target. |
| `upload.FetchOptions` | Timeout, redirect limit, the announced-size check, and `AllowAddr` for exceptions. `Header` sends a `User-Agent`, `Accept` or internal `Authorization` with every request of the fetch (Go drops credentials on a redirect to another host); `Proxy` sends every connection through an egress proxy such as Smokescreen with `CONNECT` to the address the guard already checked, never to the name, so the guard still decides the destination. |
| `Store.Upload` in a `Tx` with `Store.Attach` | Streams the body under the policy and records it; a failure anywhere deletes the object. |
| `upload.FromURL` | The same fetch and policy without records, returning an `upload.Result`. Use it when you only need the object on a disk. |

## Step 1: The policy and the limits

<!-- ds:block id=importpolicy-6yfetmpk -->
**importPolicy** · [`examples/recipe-import-from-url/main.go:85-89`](../../examples/recipe-import-from-url/main.go#L85-L89)

```go
var importPolicy = upload.Policy{
	MaxSize: maxImportBytes,
	Allowed: []string{"image/jpeg", "image/png", "image/gif", "image/webp", "application/pdf"},
	Key:     upload.RandomKey("imports"),
}
```
<!-- /ds:block hash=9f2aa8 -->

<!-- ds:block id=fetchoptions-f7b2zwau -->
**fetchOptions** · [`examples/recipe-import-from-url/main.go:96-103`](../../examples/recipe-import-from-url/main.go#L96-L103)

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

`upload.FromURL` sets `MaxSize` from the policy itself; with `Fetch` you set it, as here. The announced-size check saves downloading a body that would be refused anyway; the real cap is the policy's, applied while the body streams, because a server can announce nothing or lie.

## Step 2: Exceptions, by exact address

Sometimes an import must reach an internal server: an intranet asset store, a sibling service in the same VPC. `AllowAddr` is consulted only for addresses the guard refuses, and is checked on every connection, redirects included.

<!-- ds:block id=allowlist-qudb84q4 -->
**allowList** · [`examples/recipe-import-from-url/main.go:109-122`](../../examples/recipe-import-from-url/main.go#L109-L122)

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

Allow exact addresses, not ranges. `func(a netip.Addr) bool { return a.IsPrivate() }` would allow every private address, which is the whole attack surface the guard exists to close.

## Step 3: Fetch, store, record

<!-- ds:block id=app-importfrom-a59m22gc -->
**App.importFrom** · [`examples/recipe-import-from-url/main.go:175-190`](../../examples/recipe-import-from-url/main.go#L175-L190)

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

<!-- ds:block id=app-importurl-6t52qvb4 -->
**App.importURL** · [`examples/recipe-import-from-url/main.go:207-224`](../../examples/recipe-import-from-url/main.go#L207-L224)

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

The errors `Fetch` returns are classified so the handler can answer without inspecting them: `CodeInvalidArgument` for a refused URL, address, redirect or a `4xx` answer other than those below (`400`); `CodeNotFound` for a remote `404` or `410`; `CodeTooLarge` for an announced size over the limit (`413`); `CodeUnavailable` for network failures, timeouts, a remote `429` and remote `5xx` (`503`).

<!-- ds:block id=writeerror-dwktbxnt -->
**writeError** · [`examples/recipe-import-from-url/main.go:254-257`](../../examples/recipe-import-from-url/main.go#L254-L257)

```go
func writeError(w http.ResponseWriter, err error) {
	status := fshttp.StatusOf(err)
	writeJSON(w, status, upload.ErrorResponse{Error: fshttp.ClientMessage(err), Code: filesystem.CodeOf(err)})
}
```
<!-- /ds:block hash=e4052c -->

The guard's message is `address is not on the public internet`, without the address: an attacker probing your network learns that the address is internal, not what a hostname resolved to.

## Try it

```sh
cd examples
go run ./recipe-import-from-url
```

```text
listening on http://localhost:8108 data in /var/folders/sk/.../T/recipe-import-from-url-998535079
```

```sh
for u in http://localhost:8108/samples/photo.jpg http://127.0.0.1:6379/ http://10.0.0.5/admin \
         http://169.254.169.254/latest/meta-data/ "http://[::1]:8108/samples/photo.jpg" \
         file:///etc/passwd "http://user:pw@example.com/a.jpg"; do
  curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d "{\"url\":\"$u\"}" -w '%{http_code}\n'
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

Check: `localhost` and `[::1]` are refused too, because the check runs on the resolved address, not on the URL's text.

From the public internet, a text file is refused by type and an image is imported:

```sh
curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d '{"url":"https://www.google.com/robots.txt"}' -w '%{http_code}\n'
curl -s -X POST localhost:8108/imports -H 'X-User: u1' \
  -d '{"url":"https://www.google.com/images/branding/googlelogo/1x/googlelogo_color_272x92dp.png"}' -w '%{http_code}\n'
```

```text
{"error":"content type \"text/plain; charset=utf-8\" is not allowed","code":"rejected"}
415
{"file_id":"fil_uav4gfbebb5s7ov4vagmeb7jmp","filename":"googlelogo_color_272x92dp.png","content_type":"image/png","size":5969}
201
```

Now allow `127.0.0.1`, so the demo can import from its own sample routes, which stand in for a known internal server:

```sh
IMPORT_ALLOW_ADDRS=127.0.0.1 go run ./recipe-import-from-url
```

```sh
for u in http://127.0.0.1:8108/samples/photo.jpg http://localhost:8108/samples/photo.jpg \
         http://127.0.0.1:8108/samples/huge.jpg http://127.0.0.1:8108/samples/metadata \
         http://127.0.0.1:8108/samples/missing.jpg http://127.0.0.2:8108/samples/photo.jpg; do
  curl -s -X POST localhost:8108/imports -H 'X-User: u1' -d "{\"url\":\"$u\"}" -w '%{http_code}\n'
done
```

```text
{"file_id":"fil_z5csjocse6cmc63ibourf5iwgj","filename":"sunset.jpg","content_type":"image/jpeg","size":4180}
201
{"file_id":"fil_gntbhulvzv77avllfmfetgpasq","filename":"sunset.jpg","content_type":"image/jpeg","size":4180}
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
- `localhost` works now because one of its addresses is the allowed `127.0.0.1`; its `::1` address stays refused.
- `/samples/huge.jpg` announces 64 MiB and is refused before any body byte is read.
- `/samples/metadata` is allowed, but it redirects to `169.254.169.254`, and the redirect's connection is checked again and refused.
- `127.0.0.2` is loopback too, but not the listed address.

## Edge cases this handles

- DNS rebinding (a name that resolves to a public address when checked and an internal one when used): the check runs on the address the connection actually uses.
- Redirects into the network: each redirect's connection goes through the same guard; more than 3 redirects is `400`.
- IPv4-mapped IPv6 (`::ffff:127.0.0.1`), NAT64 (`64:ff9b::/96`) and CGNAT (`100.64.0.0/10`) forms of internal addresses: refused (`upload.PublicAddr`).
- A server that never answers, or drips bytes: the 30-second timeout covers the whole fetch, body included.
- A server that lies about `Content-Type` or sends HTML named `.jpg`: the type is detected from the bytes.
- A body larger than announced, or with no length: counted while streaming, refused at the policy's limit, nothing stored.

## For production

- Run imports off the request path if they can be slow: accept the URL, enqueue a job, and let the client poll or receive a notification. The 30-second budget then belongs to the worker, not to a user's request.
- Rate-limit imports per user; each one makes your server fetch a URL of the user's choosing.
- Use `upload.PublicAddr` (or `upload.Fetch` itself) for webhooks and link previews too, the same attack with a different feature name.
- Egress firewall rules are a second layer, not a replacement: they rarely cover the metadata address on every cloud, and never cover services on the same host.
- Some links are pages, not files: a Dropbox or Google Drive share URL returns HTML unless rewritten to its direct-download form (`?dl=1` on Dropbox). Rewrite known hosts before fetching; the policy refuses the HTML otherwise.

## Related

- [Avatars](avatars.md): what to do with an imported profile picture.
- [Strip photo metadata](strip-photo-metadata.md): `Policy.Process` runs on imports, so imported photos can be sanitized too.
- [Uploads](../concepts/uploads.md): `Fetch`, `FromURL`, `PublicAddr` and the policy.
- [Security](../concepts/security.md): the library's security model.
