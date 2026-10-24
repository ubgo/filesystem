# Exports: "download my data" as a signed ZIP link

The "download my data" feature a privacy request or a self-service settings page needs: a user asks for an export, the server streams a ZIP (a JSON profile and a CSV of their orders) straight into storage with no temporary file, and answers with a short-lived signed link that saves the file under a readable name. Asking again the same day reuses the same file, a concurrent second request cannot overwrite it (`WriteOptions.IfNotExists`), a runaway export is cut off (`WriteOptions.MaxSize`), and exports older than seven days are deleted with `upload.Sweep`. It uses a local disk, `signedurl` with `fshttp.Signed` for the links, and `records.AttachmentDisposition` for the download name. The guide is [Exports](../../docs/recipes/exports.md).

## Run it

```sh
cd examples
go run ./recipe-exports
```

It writes exports under a new temporary directory (printed at start-up), listens on `http://localhost:8117`, and sweeps old exports every hour. The user data comes from a fixed demo source: every user has the same two orders. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory. Run the tests with `go test ./recipe-exports`.

## Walkthrough

1. Ask for an export. The first request of the day builds it (`201`, `"created":true`) and returns a link valid for 15 minutes:

```sh
curl -s -X POST localhost:8117/users/u1/exports | jq .
```

```json
{
  "key": "exports/u1/2026-10-06.zip",
  "created": true,
  "url": "http://localhost:8117/files/media/exports/u1/2026-10-06.zip?cd=attachment%3B+filename%3D%22export-2026-10-06.zip%22%3B+filename%2A%3DUTF-8%27%27export-2026-10-06.zip&ct=application%2Fzip&exp=1791257794&kid=k1&m=GET&sig=1ab2f77cf7ca0a73c939a76183f7773a6ca892d4624bc6d838e5bffe6e567542",
  "expires_at": "2026-10-06T09:06:34+05:30"
}
```

2. Asking again the same day reuses the file: `200` and `"created":false`, with a link to the same key:

```sh
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8117/users/u1/exports
URL=$(curl -s -X POST localhost:8117/users/u1/exports | jq -r .url)
```

```text
200
```

3. Download it. The signed `Content-Disposition` names the file, and `fshttp` adds `nosniff` and a sandboxing CSP:

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
Etag: "18dbd2a7cb1a1f0e-186"
Last-Modified: Tue, 06 Oct 2026 03:21:34 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 03:21:34 GMT
```

```sh
unzip -l export.zip
unzip -p export.zip orders.csv
unzip -p export.zip profile.json
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
{"id":"u1","email":"u1@example.com"}
```

The 1980 dates are `zip.Writer.Create`'s default; use `CreateHeader` with `Modified` set if they matter.

4. A link with one changed character in its signature is refused, and a user ID that is not safe to put in a key is rejected before it is used:

```sh
curl -s -o /dev/null -w '%{http_code}\n' "${URL/sig=/sig=0}"
curl -s -X POST localhost:8117/users/U%201/exports
```

```text
403
{"error":"invalid user id","code":"invalid_argument"}
```

## The code

`Source` is where the export reads the user's data; the demo's is fixed, a real app reads its own tables.

<!-- ds:block id=source-km4dyh5y -->
**Source** · [`examples/recipe-exports/main.go:91-94`](main.go#L91-L94)

```go
type Source interface {
	Profile(ctx context.Context, userID string) (Profile, error)
	Orders(ctx context.Context, userID string) ([]Order, error)
}
```
<!-- /ds:block hash=f08b6c -->

`exportKey` names the export by user and day, which is what makes the feature idempotent: the same request the same day names the same object.

<!-- ds:block id=exportkey-nr5pxscq -->
**exportKey** · [`examples/recipe-exports/main.go:135-137`](main.go#L135-L137)

```go
func exportKey(userID string, now time.Time) string {
	return exportDir + "/" + userID + "/" + now.UTC().Format(dayLayout) + ".zip"
}
```
<!-- /ds:block hash=f14b44 -->

`writeOrders` and `buildArchive` write the CSV and the JSON profile into a ZIP on any `io.Writer`.

<!-- ds:block id=writeorders-sd6bsf75 -->
**writeOrders** · [`examples/recipe-exports/main.go:141-157`](main.go#L141-L157)

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
**App.buildArchive** · [`examples/recipe-exports/main.go:161-182`](main.go#L161-L182)

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

`store` streams the archive into `Disk.Write` through an `io.Pipe`, so neither a temporary file nor an in-memory copy of the export exists. `IfNotExists` makes a racing second request fail with `CodeExists` instead of overwriting, and `MaxSize` aborts an export that grows too large.

<!-- ds:block id=app-store-3qrnrf2f -->
**App.store** · [`examples/recipe-exports/main.go:195-207`](main.go#L195-L207)

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

`Export` validates the user ID, skips the build when today's export exists, and treats `CodeExists` from a race as "reuse".

<!-- ds:block id=app-export-7xz2vvb7 -->
**App.Export** · [`examples/recipe-exports/main.go:213-231`](main.go#L213-L231)

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

`Link` signs a GET with a `Content-Disposition`, so the browser saves a readable name whatever the backend stored.

<!-- ds:block id=app-link-ucr5t82z -->
**App.Link** · [`examples/recipe-exports/main.go:237-244`](main.go#L237-L244)

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

`createExport` is the route: export or reuse, then link, answering `201` or `200`.

<!-- ds:block id=app-createexport-cryb6828 -->
**App.createExport** · [`examples/recipe-exports/main.go:256-272`](main.go#L256-L272)

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

`Sweep` and `sweepLoop` delete exports older than the retention period. The library starts no background work; the app schedules it.

<!-- ds:block id=app-sweep-spgm4eu7 -->
**App.Sweep** · [`examples/recipe-exports/main.go:277-279`](main.go#L277-L279)

```go
func (a *App) Sweep(ctx context.Context) (int, error) {
	return upload.Sweep(ctx, a.Disk, exportDir, a.Now().Add(-retention))
}
```
<!-- /ds:block hash=4abf9c -->

<!-- ds:block id=app-sweeploop-3cpwhxhd -->
**App.sweepLoop** · [`examples/recipe-exports/main.go:283-296`](main.go#L283-L296)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The data source, key naming, archive building, the streamed write, links, the route, the sweep and the server. |
| `main_test.go` | Export, download and reuse; concurrent requests making one export; a failed export storing nothing; the next day's export and the sweep. |

## Next

- The guide: [Exports](../../docs/recipes/exports.md), with the edge cases it handles and production notes.
- [Serving files](../../docs/concepts/serving-files.md) for signed URLs and `fshttp`, and [Capabilities](../../docs/concepts/capabilities.md) for `ConditionalWrite` and `Headers` on each backend.
- Related recipes: [recipe-private-downloads](../recipe-private-downloads) (authorizing a download, then redirecting to a signed link) and [recipe-account-deletion](../recipe-account-deletion) (export before delete).
