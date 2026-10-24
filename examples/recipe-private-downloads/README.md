# Private downloads: signed, short-lived links to a customer's documents

A server that hands private documents (invoices, contracts, payslips) only to the customer they belong to: the app checks ownership in the database, then redirects to a two-minute signed URL that `fshttp.Signed` serves with ranges, ETags and conditional requests. It uses `records` (files as rows attached to a customer), `signedurl` (HMAC-signed links with a key ring, so keys rotate without breaking links already handed out), a private `local` disk and `fshttp`. A legacy HTML file imported straight to the disk shows that active content is always served as a download behind a sandboxing CSP. The guide is [Private downloads](../../docs/recipes/private-downloads.md).

## Run it

```sh
cd examples
go run ./recipe-private-downloads
```

It creates a temporary directory (printed at start-up) holding the files and `app.db` (SQLite), seeds customer `c1` with one PDF and one imported HTML file, prints their IDs, and listens on `http://localhost:8112`. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory.

The signing secrets come from `SIGNING_KEY_CURRENT` (key `k2`, signs and verifies) and `SIGNING_KEY_PREVIOUS` (key `k1`, verifies only). Without them a built-in demo secret is used; never ship that. The signed-in customer is the `X-Customer` header, a demo stand-in for a session lookup.

Run the tests with `go test ./recipe-private-downloads`.

## Walkthrough

```text
listening on http://localhost:8112 data in /var/folders/sk/.../T/recipe-private-downloads-2717241252
customer c1 has documents fil_j2mleybwq4udsr32mwteczpbax and fil_zo5v3xojj4u6ubworb2eo3sah2
```

1. The customer's documents:

```sh
curl -s localhost:8112/invoices -H 'X-Customer: c1'
```

```json
[{"id":"fil_j2mleybwq4udsr32mwteczpbax","filename":"Rechnung März 2026.pdf","content_type":"application/pdf","size":477},{"id":"fil_zo5v3xojj4u6ubworb2eo3sah2","filename":"terms.html","content_type":"text/html; charset=utf-8","size":70}]
```

2. Signed out, and as another customer:

```sh
curl -s localhost:8112/invoices
curl -s localhost:8112/invoices/fil_j2mleybwq4udsr32mwteczpbax -H 'X-Customer: c2'
```

```text
{"error":"Forbidden","code":"permission"}
{"error":"Not Found","code":"not_found"}
```

3. As the owner, a `302` to a signed link that names the disk, key, method, expiry, content type and key ID:

```sh
curl -s -i localhost:8112/invoices/fil_j2mleybwq4udsr32mwteczpbax -H 'X-Customer: c1' | grep -iE 'HTTP|cache-control|location'
```

```text
HTTP/1.1 302 Found
Cache-Control: private, no-store
Location: http://localhost:8112/files/documents/invoices/ihdcykppmk5n5gcokwkwevdhr6.pdf?ct=application%2Fpdf&exp=1791257184&kid=k2&m=GET&sig=b7ed3db9b4a2042253fee9f53e9a4d3950964e3b0800d1d6aafccb92f0868c4c
<a href="http://localhost:8112/files/documents/invoices/ihdcykppmk5n5gcokwkwevdhr6.pdf?ct=application%2Fpdf&amp;exp=1791257184&amp;kid=k2&amp;m=GET&amp;sig=b7ed3db9b4a2042253fee9f53e9a4d3950964e3b0800d1d6aafccb92f0868c4c">Found</a>.
```

4. Following the link:

```sh
LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8112/invoices/fil_j2mleybwq4udsr32mwteczpbax -H 'X-Customer: c1')
curl -s -D - -o /dev/null "$LINK"
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 477
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: application/pdf
Etag: "18dbd2cf1213bdf1-1dd"
Last-Modified: Tue, 06 Oct 2026 03:24:23 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 03:24:24 GMT
```

5. A range, as a PDF viewer asks for one, and a revalidation:

```sh
curl -s -D - -r 0-7 "$LINK"
curl -s -o /dev/null -w '%{http_code}\n' -H 'If-None-Match: "18dbd2cf1213bdf1-1dd"' "$LINK"
```

```text
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 8
Content-Range: bytes 0-7/477
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: application/pdf
Etag: "18dbd2cf1213bdf1-1dd"
Last-Modified: Tue, 06 Oct 2026 03:24:23 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 03:24:24 GMT

%PDF-1.4304
```

6. The download link carries a signed `Content-Disposition` with an ASCII fallback and the UTF-8 name:

```sh
DL=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8112/invoices/fil_j2mleybwq4udsr32mwteczpbax/download -H 'X-Customer: c1')
curl -s -D - -o /dev/null "$DL" | grep -i disposition
```

```text
Content-Disposition: attachment; filename="Rechnung M_rz 2026.pdf"; filename*=UTF-8''Rechnung%20M%C3%A4rz%202026.pdf
```

7. Change any parameter, here the expiry, and the signature no longer matches. Expired links, wrong methods and unknown key IDs get the same bare `403`:

```sh
curl -s -i "${LINK/exp=1791257184/exp=9999999999}" | head -1
```

```text
HTTP/1.1 403 Forbidden
```

8. The imported HTML file is a download, never a page:

```sh
HTML_LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8112/invoices/fil_zo5v3xojj4u6ubworb2eo3sah2 -H 'X-Customer: c1')
curl -s -D - -o /dev/null "$HTML_LINK" | grep -iE 'HTTP|content-type|disposition|security|options'
```

```text
HTTP/1.1 200 OK
Content-Disposition: attachment
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/html; charset=utf-8
X-Content-Type-Options: nosniff
```

On S3, GCS or Azure, drop the signer and `fshttp.Signed`: `Disk.SignedURL` uses the driver's own signing, lifetimes are capped at seven days, and the bucket serves the bytes (so the `nosniff` and CSP headers are not sent). The guide covers the differences.

## The code

`documentPolicy` accepts only PDFs, detected from the bytes, stored private under random keys.

<!-- ds:block id=documentpolicy-4t8shmq3 -->
**documentPolicy** · [`examples/recipe-private-downloads/main.go:82-87`](main.go#L82-L87)

```go
var documentPolicy = upload.Policy{
	MaxSize:    maxDocumentBytes,
	Allowed:    []string{"application/pdf"},
	Key:        upload.RandomKey("invoices"),
	Visibility: filesystem.VisibilityPrivate,
}
```
<!-- /ds:block hash=968a85 -->

`signingKeys` builds the key ring from the environment: the newest key signs, the previous one only verifies, so a rotation does not break links already issued.

<!-- ds:block id=signingkeys-age44m9x -->
**signingKeys** · [`examples/recipe-private-downloads/main.go:92-102`](main.go#L92-L102)

```go
func signingKeys(lookup func(string) (string, bool)) []signedurl.Key {
	current, ok := lookup(envKeyCurrent)
	if !ok {
		current = string(demoSecret)
	}
	keys := []signedurl.Key{{ID: keyIDCurrent, Secret: []byte(current)}}
	if previous, ok := lookup(envKeyPrevious); ok {
		keys = append(keys, signedurl.Key{ID: keyIDPrevious, Secret: []byte(previous)})
	}
	return keys
}
```
<!-- /ds:block hash=413aca -->

`openDisk` creates the signer at the public base of `/files` and a private disk that uses it for `SignedURL`.

<!-- ds:block id=opendisk-v6dguss8 -->
**openDisk** · [`examples/recipe-private-downloads/main.go:115-130`](main.go#L115-L130)

```go
func openDisk(dataDir, base string, keys []signedurl.Key) (*filesystem.Manager, *signedurl.Signer, error) {
	signer, err := signedurl.New(base+filesPrefix, keys)
	if err != nil {
		return nil, nil, err
	}
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, filesystem.WithURLSigner(signer), filesystem.WithDefaultVisibility(filesystem.VisibilityPrivate))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	return disks, signer, err
}
```
<!-- /ds:block hash=2e0eb8 -->

`AddDocument` uploads through the policy and attaches the file to the customer in one transaction; `ImportLegacy` registers an object that was written to the disk without the policy, as a migration would.

<!-- ds:block id=app-adddocument-52earrv5 -->
**App.AddDocument** · [`examples/recipe-private-downloads/main.go:168-179`](main.go#L168-L179)

```go
func (a *App) AddDocument(ctx context.Context, customer string, body []byte, filename string) (records.File, error) {
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.Upload(ctx, "", diskName, bytes.NewReader(body), filename, documentPolicy); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
		return err
	})
	return f, err
}
```
<!-- /ds:block hash=15dfb2 -->

<!-- ds:block id=app-importlegacy-nayyqcgg -->
**App.ImportLegacy** · [`examples/recipe-private-downloads/main.go:185-199`](main.go#L185-L199)

```go
func (a *App) ImportLegacy(ctx context.Context, customer, key string, body []byte, filename string) (records.File, error) {
	if _, err := a.Disk.WriteBytes(ctx, key, body, filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		var err error
		if f, err = a.Store.CreateFile(ctx, records.NewFile{Disk: diskName, Key: key, Filename: filename}); err != nil {
			return err
		}
		_, err = a.Store.Attach(ctx, "", f.ID, records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
		return err
	})
	return f, err
}
```
<!-- /ds:block hash=b1743e -->

`customerOf` and `owned` are the authorization: the document must be attached to the signed-in customer, and a document of someone else's is `not_found`, not `permission`, so IDs cannot be probed.

<!-- ds:block id=customerof-ksugmynf -->
**customerOf** · [`examples/recipe-private-downloads/main.go:203-208`](main.go#L203-L208)

```go
func customerOf(r *http.Request) (string, error) {
	if c := r.Header.Get(headerCustomer); c != "" {
		return c, nil
	}
	return "", filesystem.Errorf(filesystem.CodePermission, "not signed in")
}
```
<!-- /ds:block hash=108fbb -->

<!-- ds:block id=app-owned-89sypjub -->
**App.owned** · [`examples/recipe-private-downloads/main.go:214-225`](main.go#L214-L225)

```go
func (a *App) owned(ctx context.Context, customer, fileID string) (records.File, error) {
	list, err := a.Store.Attachments(ctx, "", records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
	if err != nil {
		return records.File{}, err
	}
	for _, af := range list {
		if af.File.ID == fileID {
			return af.File, nil
		}
	}
	return records.File{}, filesystem.Errorf(filesystem.CodeNotFound, "no document %s", fileID)
}
```
<!-- /ds:block hash=893ff6 -->

`list` returns the customer's documents; `redirect` checks ownership, signs, and answers `302` with `Cache-Control: private, no-store`; `view` and `download` differ only in whether the link carries a disposition.

<!-- ds:block id=app-list-3auchpx3 -->
**App.list** · [`examples/recipe-private-downloads/main.go:239-255`](main.go#L239-L255)

```go
func (a *App) list(w http.ResponseWriter, r *http.Request) {
	customer, err := customerOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	attached, err := a.Store.Attachments(r.Context(), "", records.Owner{Type: ownerCustomer, ID: customer}, roleInvoices)
	if err != nil {
		writeError(w, err)
		return
	}
	out := make([]document, 0, len(attached))
	for _, af := range attached {
		out = append(out, document{ID: af.File.ID, Filename: af.File.Filename, ContentType: af.File.ContentType, Size: af.File.Size})
	}
	writeJSON(w, http.StatusOK, out)
}
```
<!-- /ds:block hash=f66e2a -->

<!-- ds:block id=app-redirect-sjw5ktcm -->
**App.redirect** · [`examples/recipe-private-downloads/main.go:261-279`](main.go#L261-L279)

```go
func (a *App) redirect(w http.ResponseWriter, r *http.Request, sign func(context.Context, records.File) (string, error)) {
	customer, err := customerOf(r)
	if err != nil {
		writeError(w, err)
		return
	}
	f, err := a.owned(r.Context(), customer, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	link, err := sign(r.Context(), f)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	http.Redirect(w, r, link, http.StatusFound)
}
```
<!-- /ds:block hash=0b8468 -->

<!-- ds:block id=app-view-38xbwttz -->
**App.view** · [`examples/recipe-private-downloads/main.go:283-287`](main.go#L283-L287)

```go
func (a *App) view(w http.ResponseWriter, r *http.Request) {
	a.redirect(w, r, func(ctx context.Context, f records.File) (string, error) {
		return a.Store.URL(ctx, f, linkLifetime)
	})
}
```
<!-- /ds:block hash=f474bd -->

<!-- ds:block id=app-download-nrwju9uf -->
**App.download** · [`examples/recipe-private-downloads/main.go:292-296`](main.go#L292-L296)

```go
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	a.redirect(w, r, func(ctx context.Context, f records.File) (string, error) {
		return a.Store.DownloadURL(ctx, f, linkLifetime)
	})
}
```
<!-- /ds:block hash=c1ec1c -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, the key ring, the disk and signer, the records store, ownership checks, the redirect handlers and the seeded server. |
| `main_test.go` | Ownership and the signed-link flow, active content served as a download, and key rotation and expiry (with a moved clock). |

## Next

- The guide: [Private downloads](../../docs/recipes/private-downloads.md), including S3, GCS and Azure, and production notes.
- [Serving files](../../docs/concepts/serving-files.md) and [Public and private](../../docs/concepts/public-and-private.md).
- [Share links](../../docs/plugins/share.md) for people without an account.
- Related recipes: [recipe-chat-attachments](../recipe-chat-attachments), [recipe-exports](../recipe-exports), [recipe-account-deletion](../recipe-account-deletion).
