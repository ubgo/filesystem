# Upload sessions

You will let a client upload without signing in to your API: your server issues a token for one key or one key prefix, with a size cap and types no wider than the endpoint's policy, valid until it expires, and the client sends only the token. This is what Supabase's and Uploadcare's signed uploads are. The `records/uploadsessions` plugin keeps those sessions in the records database, so they survive a restart and every instance behind a load balancer redeems the same tokens.

When you want it: a mobile app handed a one-off upload slot, a third party sending a document into a user's folder, a background uploader that outlives a login, a "send me the file" form. When you do not: a signed-in browser uploading to your own API, where the session cookie already authenticates the request; a direct-to-bucket upload, where a [presigned URL](../concepts/uploads.md) is the token.

| Store | Use it when |
|---|---|
| `upload.MemorySessions` | One process, and losing every open session on restart is fine (tests, a single small server). |
| `records/uploadsessions` (this page) | You already run the records layer, or you run more than one instance. |
| Your own `upload.SessionStore` | Sessions must live elsewhere (Redis). It is four methods; `upload.Sessions` does every check. |

## Before you start

- A records Store, as in [Files](../records/files.md). The plugin is in the records module: `go get github.com/ubgo/filesystem/records`.
- The upload endpoint's policy. [Uploads](../concepts/uploads.md#upload-sessions-a-token-valid-for-a-window) explains how a session narrows it and which handlers take a `Resolver`.
- The example is [`examples/records-upload-sessions`](../../examples/records-upload-sessions): `go run ./records-upload-sessions` in `examples/` prints every step below; `go test ./records-upload-sessions` runs them.

## Step 1 — The endpoint's policy

A session can only narrow the policy of the endpoint it is redeemed at, so that policy is the ceiling for every token:

<!-- ds:block id=endpointpolicy-m3w9t5kc -->
**endpoint** · [`examples/records-upload-sessions/main.go:54-58`](../../examples/records-upload-sessions/main.go#L54-L58)

```go
var endpoint = upload.Policy{
	MaxSize: 1 << 20,
	Allowed: []string{"text/plain", "image/png", "image/jpeg"},
	Key:     upload.RandomKey(""),
}
```
<!-- /ds:block hash=3014aa -->

`Key: upload.RandomKey("")` makes each upload's key random; a session with a `Prefix` puts it under that prefix.

## Step 2 — Install the plugin

<!-- ds:block id=newsessionstore-k8c2v6nw -->
**newStore** · [`examples/records-upload-sessions/main.go:72-82`](../../examples/records-upload-sessions/main.go#L72-L82)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *uploadsessions.Plugin, error) {
	p := uploadsessions.New()
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, p, nil
}
```
<!-- /ds:block hash=56fd88 -->

`Migrate` creates `fs_upload_sessions` (migration `uploadsessions/0001_upload_sessions`): the session's ID, the SHA-256 of its secret (never the secret), its key or prefix, size cap, types and times.

## Step 3 — Build the Sessions

`upload.Sessions` does every check; the plugin only keeps the rows. Build one per process over the Store:

<!-- ds:block id=newsessions-r5t8q2hx -->
**newSessions** · [`examples/records-upload-sessions/main.go:88-90`](../../examples/records-upload-sessions/main.go#L88-L90)

```go
func newSessions(s *records.Store, p *uploadsessions.Plugin, now func() time.Time) (*upload.Sessions, error) {
	return upload.NewSessions(p.Store(s), upload.SessionsOptions{Now: now})
}
```
<!-- /ds:block hash=01f439 -->

The clock (`SessionsOptions.Now`, `time.Now` by default) judges expiry. A session past its `ExpiresAt` is refused whatever the table holds, so a late sweep never extends a token.

## Step 4 — Mount the endpoint with the Resolver

`sessions.Resolver()` goes into the `Resolve` option of every upload handler: `upload.Handler`, `Chunked`, `Uppy`, `PresignHandlerWith`, `LimitsHandler`, `recordshttp.Config.Resolve` and `contrib/tus`'s `Config.Resolve`. A request without a usable session is refused before its body is read; one with a session runs under the session's narrowed policy:

<!-- ds:block id=newuploadhandler-w2n7c4tq -->
**newUploadHandler** · [`examples/records-upload-sessions/main.go:96-98`](../../examples/records-upload-sessions/main.go#L96-L98)

```go
func newUploadHandler(disk *filesystem.Disk, sessions *upload.Sessions) http.Handler {
	return upload.Handler(disk, endpoint, upload.HandlerOptions{Resolve: sessions.Resolver()})
}
```
<!-- /ds:block hash=5f97d2 -->

## Step 5 — Issue a token from your authenticated API

<!-- ds:block id=issuefor-q6h3m9zp -->
**issueFor** · [`examples/records-upload-sessions/main.go:104-111`](../../examples/records-upload-sessions/main.go#L104-L111)

```go
func issueFor(ctx context.Context, sessions *upload.Sessions, userID string) (upload.IssuedSession, error) {
	return sessions.Issue(ctx, endpoint, upload.SessionRequest{
		Prefix:  "users/" + userID,
		MaxSize: sessionMax,
		Allowed: []string{"text/plain"},
		TTL:     sessionTTL,
	})
}
```
<!-- /ds:block hash=c2b0f6 -->

`Issue` needs exactly one of `Key` (every upload stored at that key, for "replace this document") and `Prefix` (the policy's key chosen inside it), a TTL up to `upload.MaxSessionLifetime` (7 days), a `MaxSize` no larger than the endpoint's, and types within the endpoint's `Allowed`. Anything wider is `invalid_argument`. The token is the session ID and a 256-bit secret; hand `is.Token` to the client once. Check: the example prints `issued ups_… for prefix users/alice until 09:15`, and later `issue wider than the endpoint: invalid_argument` for a 2 MiB request against a 1 MiB endpoint.

## Step 6 — The client uploads with the token

The client sends the token in the `Upload-Session` header (`upload.HeaderUploadSession`) with every request of the upload:

<!-- ds:block id=send-c7w4k9rm -->
**send** · [`examples/records-upload-sessions/main.go:115-136`](../../examples/records-upload-sessions/main.go#L115-L136)

```go
func send(h http.Handler, token, name, body string) (int, string, error) {
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, err := mw.CreateFormFile(upload.DefaultField, name)
	if err != nil {
		return 0, "", err
	}
	if _, err := io.WriteString(part, body); err != nil {
		return 0, "", err
	}
	if err := mw.Close(); err != nil {
		return 0, "", err
	}
	req := httptest.NewRequest(http.MethodPost, "/upload", &form)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set(upload.HeaderUploadSession, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), nil
}
```
<!-- /ds:block hash=6ed142 -->

Check: the example prints

```text
with the token:                201 users/alice/…
no token:                      403
forged secret:                 403
over the session's 64 bytes:   413
a PNG the endpoint allows:     415
```

The file lands under the session's prefix. A missing, malformed, unknown, revoked or expired token, or a wrong secret, is the same 403 (code `permission`), so tokens cannot be probed. The narrowed policy then goes through the same checks as any upload: the size is counted while streaming, and the type is detected from the bytes, so a PNG is refused by a plain-text session even though the endpoint allows PNG.

## Step 7 — Every instance redeems it, until it ends

Because the session is a row, a second `upload.Sessions` over the same database (another instance, or this one after a restart) redeems it: the example prints `another instance: 201 users/alice/…`. A session ends three ways:

- **Expiry.** Past `ExpiresAt` by the Sessions clock: `after the expiry: 403`. A chunked or tus upload that outlives its session stops at its next request.
- **Revoke.** `sessions.Revoke(ctx, id)` deletes it at once: `revoked: 403`. An unknown ID is not an error.
- **Sweep.** `sessions.Sweep(ctx)` deletes the expired rows (`sweep deleted 1 expired session(s)`). Schedule it with your other jobs; it is housekeeping, not security.

## Try it

```sh
cd examples
go run ./records-upload-sessions
go test ./records-upload-sessions
```

## Pitfalls

- **Narrowing the endpoint narrows live sessions.** A session's types are judged against the endpoint's policy at redemption, so removing a type from the endpoint stops sessions that relied on it (`rejected`). Widening the endpoint never widens a session.
- **One session is many uploads.** A `Prefix` session accepts uploads until it expires. For one file, issue a `Key` session, or `Revoke` it in your handler's `OnStored`.
- **The token is a bearer credential.** Anyone holding it can upload until it expires. Keep TTLs short, send it over TLS only, and never put it in a URL that is logged.
- **Sessions belong to no scope.** `Store.PurgeScope` does not remove them; they expire within seven days and the sweep deletes them. Revoke a departing tenant's sessions yourself if you keep their IDs.
- **Uploads are not recorded by `upload.Handler`.** The handler stores bytes only. To record files as rows, mount the [records HTTP routes](../records/http-endpoints.md) with `Config.Resolve: sessions.Resolver()`, or call `Store.CreateFile` from `OnStored`.

## Related

- [Uploads](../concepts/uploads.md): policies and every upload path.
- [Resumable uploads](../recipes/resumable-uploads.md): tus, which takes the same Resolver.
- The plugin's [README](../../records/uploadsessions/README.md): the API and the table.
