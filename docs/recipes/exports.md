# Exports

"Download my data", "export to CSV", the monthly report a customer pulls from an admin screen: the server generates a file, stores it, and hands the user a link. The parts that go wrong are the ones nobody sees in a demo: a 2 GB export buffered in memory, a temporary file left on a full disk, a link that works forever for whoever finds it in a chat log, two clicks producing two exports, and last year's exports still paying rent in the bucket. This recipe is for any SaaS, e-commerce or internal tool that lets users export their data, including the data-portability exports privacy laws require.

The program is [examples/recipe-exports](../../examples/recipe-exports). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `POST /users/{id}/exports` builds a ZIP (`profile.json` and `orders.csv`) and stores it under `exports/<user>/<day>.zip`.
- The ZIP is streamed into storage as it is produced: no temporary file, no copy of the whole export in memory, and a size cap that aborts a runaway export without storing it.
- One export per user per day. Asking again returns a new link to the same file; concurrent requests produce exactly one file.
- A download link that expires after 15 minutes and makes the browser save the file as `export-2026-10-03.zip`.
- Exports older than seven days are deleted by a sweep the application schedules.

## The design

| Piece | Why |
|---|---|
| `io.Pipe` into `Disk.Write` | The archive writer and the storage upload run at the same time; memory use is a buffer, not the export. On S3 a body of unknown length is uploaded as multipart. |
| `WriteOptions{MaxSize: …}` | The Disk counts bytes as they stream and aborts past the cap with `CodeTooLarge`; nothing larger is stored. |
| A key named by user and day | Makes the operation idempotent: the same request on the same day names the same object. |
| `WriteOptions{IfNotExists: true}` | Two requests that both found no export race to create it; the backend lets one win and the other gets `CodeExists`, which means "use the existing one". Needs the `ConditionalWrite` capability. |
| `Disk.SignedURL` with `ContentDisposition` | The link is short-lived and signed; the disposition is part of the signature, so it cannot be edited, and the browser saves instead of displaying. |
| `upload.Sweep` on `exports/` | Deletes objects under the prefix older than a cutoff. It refuses the root, so it can only ever touch exports. |

## Step 1: Name the export

<!-- ds:block id=exportkey-nr5pxscq -->
**exportKey** · [`examples/recipe-exports/main.go:135-137`](../../examples/recipe-exports/main.go#L135-L137)

```go
func exportKey(userID string, now time.Time) string {
	return exportDir + "/" + userID + "/" + now.UTC().Format(dayLayout) + ".zip"
}
```
<!-- /ds:block hash=f14b44 -->

The user ID becomes part of the key, so it is checked against a strict pattern before use (see `Export` below). The Disk would refuse a key that escapes the root anyway, but `a/b` is a valid key and would nest one user inside another's directory.

## Step 2: Build the archive

The data comes from wherever the application keeps it, behind a small interface:

<!-- ds:block id=source-km4dyh5y -->
**Source** · [`examples/recipe-exports/main.go:91-94`](../../examples/recipe-exports/main.go#L91-L94)

```go
type Source interface {
	Profile(ctx context.Context, userID string) (Profile, error)
	Orders(ctx context.Context, userID string) ([]Order, error)
}
```
<!-- /ds:block hash=f08b6c -->

<!-- ds:block id=writeorders-sd6bsf75 -->
**writeOrders** · [`examples/recipe-exports/main.go:141-157`](../../examples/recipe-exports/main.go#L141-L157)

```go
func writeOrders(zw *zip.Writer, orders []Order) error {
	w, err := zw.Create("orders.csv")
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "placed_at", "total_cents"}); err != nil {
		return err
	}
	for _, o := range orders {
		if err := cw.Write([]string{o.ID, o.Placed.UTC().Format(time.RFC3339), strconv.FormatInt(o.Total, 10)}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
```
<!-- /ds:block hash=a2ad2f -->

<!-- ds:block id=app-buildarchive-y4guuwge -->
**App.buildArchive** · [`examples/recipe-exports/main.go:161-182`](../../examples/recipe-exports/main.go#L161-L182)

```go
func (a *App) buildArchive(ctx context.Context, w io.Writer, userID string) error {
	profile, err := a.Data.Profile(ctx, userID)
	if err != nil {
		return err
	}
	orders, err := a.Data.Orders(ctx, userID)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	pw, err := zw.Create("profile.json")
	if err != nil {
		return err
	}
	if err := json.NewEncoder(pw).Encode(profile); err != nil {
		return err
	}
	if err := writeOrders(zw, orders); err != nil {
		return err
	}
	return zw.Close()
}
```
<!-- /ds:block hash=e71753 -->

`buildArchive` only knows an `io.Writer`. It does not know that the writer is a pipe into S3, which is also what makes it easy to test.

## Step 3: Stream it into storage

<!-- ds:block id=app-store-3qrnrf2f -->
**App.store** · [`examples/recipe-exports/main.go:195-207`](../../examples/recipe-exports/main.go#L195-L207)

```go
func (a *App) store(ctx context.Context, key, userID string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(a.buildArchive(ctx, pw, userID)) }()
	opts := filesystem.WriteOptions{ContentType: "application/zip", IfNotExists: true, MaxSize: maxExportBytes}
	if a.Disk.Capabilities().Headers {
		opts.Headers.ContentDisposition = records.AttachmentDisposition(downloadName(key))
	}
	_, err := a.Disk.Write(ctx, key, pr, opts)
	// Unblocks the producer if Write stopped reading early (refused,
	// too large, or the object already existed).
	pr.CloseWithError(err)
	return err
}
```
<!-- /ds:block hash=08d261 -->

The producer goroutine ends one of three ways: it finishes and closes the pipe (the upload completes), it fails and closes the pipe with its error (the upload fails with that error and nothing is stored), or the upload stops reading (too large, a network failure) and `pr.CloseWithError` makes the producer's next write fail so it returns. No path leaks the goroutine.

Whether a failed upload leaves a partial object depends on the backend, not on this code: the local driver stages the bytes in a temporary file and renames (or, with `IfNotExists`, hard-links) it into place, S3 completes a multipart upload only on success, so in both cases a failed export stores nothing. The test proves it for local disk by failing the data source half-way.

When the disk stores HTTP headers (S3, GCS, Azure, memory), the `Content-Disposition` is stored with the object too, so a link the backend builds itself (a CDN URL, a presigned URL without overrides) also downloads under the right name. The local driver does not store headers, which is why the signed link in step 5 carries it as well.

## Step 4: One per user per day

<!-- ds:block id=app-export-7xz2vvb7 -->
**App.Export** · [`examples/recipe-exports/main.go:213-231`](../../examples/recipe-exports/main.go#L213-L231)

```go
func (a *App) Export(ctx context.Context, userID string) (string, bool, error) {
	if !userIDPattern.MatchString(userID) {
		return "", false, filesystem.Errorf(filesystem.CodeInvalidArgument, "invalid user id")
	}
	key := exportKey(userID, a.Now())
	// The cheap check skips rebuilding an export that exists; IfNotExists
	// in store settles the race between two requests that both missed it.
	if ok, err := a.Disk.Exists(ctx, key); err != nil || ok {
		return key, false, err
	}
	err := a.store(ctx, key, userID)
	switch filesystem.CodeOf(err) {
	case "":
		return key, true, nil
	case filesystem.CodeExists:
		return key, false, nil
	}
	return "", false, err
}
```
<!-- /ds:block hash=cf65f9 -->

`Exists` first is an optimization: it saves rebuilding an export that is already there. It is not the guarantee, because two requests can both see "missing". The guarantee is `IfNotExists`: the second writer gets `CodeExists`, and `Export` reports the existing file. The condition is checked when the object is committed, so the loser builds its archive before it learns it lost; the backend then discards it.

On S3 `IfNotExists` needs `conditional_write=true` in the DSN, and only on providers that honour `If-None-Match` (AWS, R2, recent MinIO, SeaweedFS); without the capability the write fails with `CodeUnsupported` rather than silently overwriting. On such a provider, drop `IfNotExists` and accept that a race produces the same export twice (the later write replaces the earlier), or take a lock per user.

## Step 5: Hand out a link

<!-- ds:block id=app-link-ucr5t82z -->
**App.Link** · [`examples/recipe-exports/main.go:237-244`](../../examples/recipe-exports/main.go#L237-L244)

```go
func (a *App) Link(ctx context.Context, key string) (filesystem.SignedRequest, error) {
	return a.Disk.SignedURL(ctx, key, filesystem.PresignOptions{
		Method:             filesystem.MethodGet,
		Expires:            linkLifetime,
		ContentType:        "application/zip",
		ContentDisposition: records.AttachmentDisposition(downloadName(key)),
	})
}
```
<!-- /ds:block hash=da93f0 -->

`records.AttachmentDisposition` renders the header with an ASCII fallback and the exact UTF-8 name, so a name like `Exporté.zip` survives every browser. On S3, GCS and Azure the link is the backend's own presigned URL with a response-header override; on local disk it is a `signedurl` link served by `fshttp.Signed`, which answers anything tampered with or expired with a bare `403`.

<!-- ds:block id=app-createexport-cryb6828 -->
**App.createExport** · [`examples/recipe-exports/main.go:256-272`](../../examples/recipe-exports/main.go#L256-L272)

```go
func (a *App) createExport(w http.ResponseWriter, r *http.Request) {
	key, created, err := a.Export(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := a.Link(r.Context(), key)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, exportResponse{Key: key, Created: created, URL: link.URL, ExpiresAt: link.ExpiresAt})
}
```
<!-- /ds:block hash=d39923 -->

## Step 6: Sweep old exports

<!-- ds:block id=app-sweep-spgm4eu7 -->
**App.Sweep** · [`examples/recipe-exports/main.go:277-279`](../../examples/recipe-exports/main.go#L277-L279)

```go
func (a *App) Sweep(ctx context.Context) (int, error) {
	return upload.Sweep(ctx, a.Disk, exportDir, a.Now().Add(-retention))
}
```
<!-- /ds:block hash=4abf9c -->

<!-- ds:block id=app-sweeploop-3cpwhxhd -->
**App.sweepLoop** · [`examples/recipe-exports/main.go:283-296`](../../examples/recipe-exports/main.go#L283-L296)

```go
func (a *App) sweepLoop(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.Sweep(ctx); err != nil || n > 0 {
				fmt.Fprintf(os.Stderr, "export sweep: deleted %d, err %v\n", n, err)
			}
		}
	}
}
```
<!-- /ds:block hash=918b7e -->

`upload.Sweep` deletes as it lists, so a failure part-way returns the count so far and the next run continues. In a deployment with several instances, run it from one place (a cron job, a scheduler with a lock) rather than a loop in every instance; running it twice at once is harmless but wasteful. On S3 a bucket lifecycle rule on the `exports/` prefix does the same without any code.

## Try it

```sh
cd examples
go run ./recipe-exports
```

```text
listening on http://localhost:8117 data in /var/folders/sk/.../T/recipe-exports-2433998207
```

```sh
curl -s -i -X POST localhost:8117/users/u1/exports
```

```text
HTTP/1.1 201 Created
Content-Type: application/json

{"key":"exports/u1/2026-10-06.zip","created":true,"url":"http://localhost:8117/files/media/exports/u1/2026-10-06.zip?cd=attachment%3B+filename%3D%22export-2026-10-06.zip%22%3B+filename%2A%3DUTF-8%27%27export-2026-10-06.zip&ct=application%2Fzip&exp=1791257794&kid=k1&m=GET&sig=1ab2f77cf7ca0a73c939a76183f7773a6ca892d4624bc6d838e5bffe6e567542","expires_at":"2026-10-06T09:06:34+05:30"}
```

Asking again the same day reuses the file (`200`, `"created":false`):

```sh
curl -s -X POST localhost:8117/users/u1/exports
```

```json
{"key":"exports/u1/2026-10-06.zip","created":false,"url":"http://localhost:8117/files/media/exports/u1/2026-10-06.zip?cd=…&sig=1ab2f77c…","expires_at":"2026-10-06T09:06:34+05:30"}
```

Download it with the link:

```sh
curl -s -D - -o export.zip "$URL"
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Disposition: attachment; filename="export-2026-10-06.zip"; filename*=UTF-8''export-2026-10-06.zip
Content-Length: 390
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: application/zip
Etag: "18dbd2a7b8a2d975-186"
Last-Modified: Tue, 06 Oct 2026 03:21:34 GMT
X-Content-Type-Options: nosniff
```

```sh
unzip -l export.zip
unzip -p export.zip orders.csv
```

```text
Archive:  export.zip
  Length      Date    Time    Name
---------  ---------- -----   ----
       37  00-00-1980 00:00   profile.json
       89  00-00-1980 00:00   orders.csv
---------                     -------
      126                     2 files
id,placed_at,total_cents
ord_1,2026-09-01T10:00:00Z,1999
ord_2,2026-09-03T10:00:00Z,4500
```

The 1980 dates are `zip.Writer.Create`'s default; use `CreateHeader` with `Modified` set if the dates matter to your users.

A link with one changed character of its signature, and an invalid user ID:

```sh
curl -s -o /dev/null -w '%{http_code}\n' "${URL/sig=/sig=0}"
curl -s -X POST localhost:8117/users/U%201/exports
```

```text
403
{"error":"invalid user id","code":"invalid_argument"}
```

## Edge cases this handles

- A double click, or two tabs: one file, both get a link to it.
- The data source failing half-way: the request fails and no partial ZIP is stored.
- A runaway export (a bug, an enormous account): aborted at `maxExportBytes` with `413`, nothing stored.
- A leaked link: useless after 15 minutes; ask for a new one with the same `POST`.
- A tampered link (another key, another disposition, a later expiry): `403`.
- A user ID that would change the key's shape: refused before it reaches storage.
- Old exports: deleted after seven days by the sweep.

## For production

- Generate off the request path. Small exports finish within a request; large ones should not hold an HTTP connection or a proxy timeout. Enqueue a job (`Export` is the job body), answer `202 Accepted`, and email or notify the link when the job finishes, or let the client poll a status route that calls `Disk.Exists` on today's key. `Export` is already safe to run twice, so an at-least-once queue is fine.
- Check that the signed-in user is `{id}`: the handler trusts the path.
- Put exports on a private disk (or a private bucket with `records.WithVisibilityDisks`) and never give them a public URL; they are the user's whole account.
- If users should see their past exports in the app, record each one with `Store.CreateFile` and attach it to the user (role `export`), and purge the records instead of sweeping the objects ([files as records](../records/README.md), [lifecycle](../records/lifecycle.md)).
- If the same export must be re-created within a day (the user changed data), add a version to the key (`2026-10-03-2.zip`) or delete the old object first; `IfNotExists` will otherwise keep returning the first one.
- For data-portability requests that precede an account deletion, run the export first and keep it for the legal retention period ([account deletion](account-deletion.md)).

## Related

- [Private downloads](private-downloads.md): authorizing a download and redirecting to a signed link.
- [Serving files](../concepts/serving-files.md): signed URLs, `fshttp`, and why downloads carry `nosniff` and a sandbox.
- [Capabilities](../concepts/capabilities.md): `ConditionalWrite` and `Headers`, and which backends have them.
- [Uploads](../concepts/uploads.md): `upload.Sweep` for abandoned uploads, the same mechanism used here.
- [Account deletion](account-deletion.md): export before delete.
