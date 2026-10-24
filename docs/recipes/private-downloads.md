# Private downloads

Invoices, contracts, payslips, medical letters, signed agreements: files that exactly one customer (or one team) may read, and that must never be reachable by guessing a URL. This recipe is for any SaaS, marketplace, fintech or client-portal backend that hands such documents out over HTTP.

The program is [examples/recipe-private-downloads](../../examples/recipe-private-downloads). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `GET /invoices` lists the signed-in customer's documents, and nobody else's.
- `GET /invoices/{id}` checks ownership, then redirects to a signed URL that lives two minutes; the browser's PDF viewer opens it.
- `GET /invoices/{id}/download` does the same with a `Content-Disposition` that saves the file under its original name, `Rechnung März 2026.pdf` included.
- The signed URLs support byte ranges, ETags and `304 Not Modified`, so PDF viewers can fetch pages lazily and re-opening a document costs nothing.
- A changed or expired link is a bare `403`. An HTML file that slipped in from an old system is served as a download, never rendered.
- Signing keys rotate without breaking links already handed out.

## The design

| Piece | Why |
|---|---|
| App-side ownership check, then a redirect | The library does not know your users. The app decides who may read what, then issues a link that carries that decision for a short time. |
| `Store.URL` / `Store.DownloadURL` | Signed GET for a private file. `DownloadURL` adds `records.AttachmentDisposition(filename)`, an RFC 6266/5987 header with an ASCII fallback and the exact UTF-8 name. |
| `signedurl.Signer` + `fshttp.Signed` | A local disk cannot sign its own URLs. The signer makes HMAC-SHA256 links to the app's own `/files/` route, and `fshttp.Signed` verifies them and streams the object. On S3, GCS and Azure the driver signs instead and these two pieces go away. |
| Key ring `[current, previous]` | The first key signs, every key verifies. Rotating means adding a new first key and dropping the old one once the longest link lifetime has passed. |
| `Cache-Control: private, no-store` on the redirect | The redirect carries a link that dies in minutes; a shared cache must never replay it. |

## Step 1: The keys

<!-- ds:block id=signingkeys-age44m9x -->
**signingKeys** · [`examples/recipe-private-downloads/main.go:92-102`](../../examples/recipe-private-downloads/main.go#L92-L102)

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

Each secret must be at least 32 bytes (`signedurl.MinKeyBytes`); `signedurl.New` refuses a shorter one. The key ID travels in the URL (`kid=k2`), so the verifier picks the right key directly.

To rotate: deploy with `SIGNING_KEY_CURRENT` set to a new secret and `SIGNING_KEY_PREVIOUS` set to the old one. New links are signed with the new key, links issued before the deploy still verify with the old one. After the longest lifetime you issue (two minutes here) has passed, remove `SIGNING_KEY_PREVIOUS`. If a secret leaks, skip the waiting: remove it at once, and every link it signed stops working.

## Step 2: The disk

<!-- ds:block id=opendisk-v6dguss8 -->
**openDisk** · [`examples/recipe-private-downloads/main.go:115-130`](../../examples/recipe-private-downloads/main.go#L115-L130)

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

`WithURLSigner` is what makes `Disk.SignedURL`, and so `Store.URL`, work on a local disk. `WithDefaultVisibility(VisibilityPrivate)` means anything written without an explicit visibility is private; the local driver stores that as a file mode.

Documents arrive through a policy that accepts PDFs only, stored under random keys:

<!-- ds:block id=documentpolicy-4t8shmq3 -->
**documentPolicy** · [`examples/recipe-private-downloads/main.go:82-87`](../../examples/recipe-private-downloads/main.go#L82-L87)

```go
var documentPolicy = upload.Policy{
	MaxSize:    maxDocumentBytes,
	Allowed:    []string{"application/pdf"},
	Key:        upload.RandomKey("invoices"),
	Visibility: filesystem.VisibilityPrivate,
}
```
<!-- /ds:block hash=968a85 -->

<!-- ds:block id=app-adddocument-52earrv5 -->
**App.AddDocument** · [`examples/recipe-private-downloads/main.go:168-179`](../../examples/recipe-private-downloads/main.go#L168-L179)

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

The key is random (`invoices/eiqnqo5a….pdf`), so even the signed URL reveals nothing about the customer or the document; the display name lives only in the row.

## Step 3: Check ownership, then redirect

<!-- ds:block id=customerof-ksugmynf -->
**customerOf** · [`examples/recipe-private-downloads/main.go:203-208`](../../examples/recipe-private-downloads/main.go#L203-L208)

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
**App.owned** · [`examples/recipe-private-downloads/main.go:214-225`](../../examples/recipe-private-downloads/main.go#L214-L225)

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

Another customer's document answers exactly like a missing one, `404`, so file IDs cannot be probed. Here the check walks the customer's attachments; an app with tenants can instead put each customer's files in their own scope, where `Store.File(ctx, customerID, id)` is `CodeNotFound` for every other customer ([SaaS tenants](saas-tenants.md)).

<!-- ds:block id=app-redirect-sjw5ktcm -->
**App.redirect** · [`examples/recipe-private-downloads/main.go:261-279`](../../examples/recipe-private-downloads/main.go#L261-L279)

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
**App.view** · [`examples/recipe-private-downloads/main.go:283-287`](../../examples/recipe-private-downloads/main.go#L283-L287)

```go
func (a *App) view(w http.ResponseWriter, r *http.Request) {
	a.redirect(w, r, func(ctx context.Context, f records.File) (string, error) {
		return a.Store.URL(ctx, f, linkLifetime)
	})
}
```
<!-- /ds:block hash=f474bd -->

<!-- ds:block id=app-download-nrwju9uf -->
**App.download** · [`examples/recipe-private-downloads/main.go:292-296`](../../examples/recipe-private-downloads/main.go#L292-L296)

```go
func (a *App) download(w http.ResponseWriter, r *http.Request) {
	a.redirect(w, r, func(ctx context.Context, f records.File) (string, error) {
		return a.Store.DownloadURL(ctx, f, linkLifetime)
	})
}
```
<!-- /ds:block hash=c1ec1c -->

The list endpoint returns IDs, not links:

<!-- ds:block id=app-list-3auchpx3 -->
**App.list** · [`examples/recipe-private-downloads/main.go:239-255`](../../examples/recipe-private-downloads/main.go#L239-L255)

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

A link is made when it is clicked, so none sits in a rendered page, a browser history or a log for longer than its lifetime.

## Step 4: Active content from elsewhere

Files that came through `documentPolicy` are PDFs. Files imported from an older system never met a policy:

<!-- ds:block id=app-importlegacy-nayyqcgg -->
**App.ImportLegacy** · [`examples/recipe-private-downloads/main.go:185-199`](../../examples/recipe-private-downloads/main.go#L185-L199)

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

`fshttp` serves every file with `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src 'none'; sandbox`, and serves any active type (HTML, SVG, XML, JavaScript) with `Content-Disposition: attachment`, whatever the stored or signed disposition says. The transcript below shows it.

## Try it

```sh
cd examples
go run ./recipe-private-downloads
```

```text
listening on http://localhost:8112 data in /var/folders/sk/.../T/recipe-private-downloads-1879261996
customer c1 has documents fil_fkp5idnwxvxjdrj7qtdrqkm2fu and fil_xs42rqrgwl55pzmhyxavdoih2z
```

```sh
curl -s localhost:8112/invoices -H 'X-Customer: c1'
```

```json
[{"id":"fil_fkp5idnwxvxjdrj7qtdrqkm2fu","filename":"Rechnung März 2026.pdf","content_type":"application/pdf","size":477},{"id":"fil_xs42rqrgwl55pzmhyxavdoih2z","filename":"terms.html","content_type":"text/html; charset=utf-8","size":70}]
```

Signed out, and as another customer:

```sh
curl -s localhost:8112/invoices
curl -s localhost:8112/invoices/fil_fkp5idnwxvxjdrj7qtdrqkm2fu -H 'X-Customer: c2'
```

```text
{"error":"Forbidden","code":"permission"}
{"error":"Not Found","code":"not_found"}
```

As the owner:

```sh
curl -s -i localhost:8112/invoices/fil_fkp5idnwxvxjdrj7qtdrqkm2fu -H 'X-Customer: c1'
```

```text
HTTP/1.1 302 Found
Cache-Control: private, no-store
Location: http://localhost:8112/files/documents/invoices/opavkdloyfv7274leybwn7q6ss.pdf?ct=application%2Fpdf&exp=1791257182&kid=k2&m=GET&sig=1b266fafee505e2fb07760bbb430fed3a74e5acfab123d9c7c7df0003b0791fb
```

The link names the disk, the key, the method, the expiry (Unix seconds), the content type to answer with, the key ID and the signature over all of them. Following it:

```sh
LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8112/invoices/fil_fkp5idnwxvxjdrj7qtdrqkm2fu -H 'X-Customer: c1')
curl -s -D - -o /dev/null "$LINK"
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 477
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: application/pdf
Etag: "18dbd2cecc6e3291-1dd"
Last-Modified: Tue, 06 Oct 2026 03:24:22 GMT
X-Content-Type-Options: nosniff
```

A range, as a PDF viewer asks for one, and a revalidation:

```sh
curl -s -D - -r 0-7 "$LINK"
curl -s -o /dev/null -w '%{http_code}\n' -H 'If-None-Match: "18dbd2cecc6e3291-1dd"' "$LINK"
```

```text
HTTP/1.1 206 Partial Content
Accept-Ranges: bytes
Cache-Control: private, no-store
Content-Length: 8
Content-Range: bytes 0-7/477
Content-Type: application/pdf
Etag: "18dbd2cecc6e3291-1dd"

%PDF-1.4304
```

`fshttp` reads only the requested range from the disk (a ranged `Disk.ReadRange`), not the whole object.

The download link carries the disposition, signed like everything else:

```sh
DOWNLOAD_LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8112/invoices/fil_fkp5idnwxvxjdrj7qtdrqkm2fu/download -H 'X-Customer: c1')
echo "$DOWNLOAD_LINK"
curl -s -D - -o /dev/null "$DOWNLOAD_LINK" | grep -i disposition
```

```text
http://localhost:8112/files/documents/invoices/opavkdloyfv7274leybwn7q6ss.pdf?cd=attachment%3B+filename%3D%22Rechnung+M_rz+2026.pdf%22%3B+filename%2A%3DUTF-8%27%27Rechnung%2520M%25C3%25A4rz%25202026.pdf&ct=application%2Fpdf&exp=1791257183&kid=k2&m=GET&sig=574de0c86e4b33ed99d210715b28e3d2326a3b2a001570fc6cac38c5b5399f73
Content-Disposition: attachment; filename="Rechnung M_rz 2026.pdf"; filename*=UTF-8''Rechnung%20M%C3%A4rz%202026.pdf
```

Browsers use `filename*` and save `Rechnung März 2026.pdf`; very old clients fall back to the ASCII name with `_` for each character it cannot carry.

Change any parameter, here the expiry, and the signature no longer matches:

```sh
curl -s -i "${LINK/exp=1791257182/exp=9999999999}" | head -1
```

```text
HTTP/1.1 403 Forbidden
```

An expired link, a wrong method and an unknown key ID answer the same `403` with no detail, so a forger learns nothing. (The expiry case is in the example's test, which moves the verifier's clock with `signedurl.WithClock`.)

The imported HTML file is a download, never a page:

```sh
HTML_LINK=$(curl -s -o /dev/null -w '%{redirect_url}' localhost:8112/invoices/fil_xs42rqrgwl55pzmhyxavdoih2z -H 'X-Customer: c1')
curl -s -D - -o /dev/null "$HTML_LINK" | grep -iE 'HTTP|content-type|disposition|security|options'
```

```text
HTTP/1.1 200 OK
Content-Disposition: attachment
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/html; charset=utf-8
X-Content-Type-Options: nosniff
```

## Edge cases this handles

- Guessing file IDs: another customer's document is `404`, like a missing one.
- A link copied into a chat or an email: it stops working after `linkLifetime`. Keep the lifetime short and issue links on click.
- Tampering with any signed value (key, expiry, content type, disposition): `403`.
- Key rotation: old links verify until the previous key is removed; a leaked key is revoked by removing it.
- Large documents in a viewer: ranges are served from ranged reads; a re-open is a `304`.
- A stored HTML, SVG or script file: always a download with a sandboxing CSP, so it cannot run script on your origin.
- Non-ASCII file names: exact in modern browsers through `filename*`, a safe ASCII fallback elsewhere.

## On S3, GCS or Azure

Point the disk at the bucket and drop the signer and `fshttp.Signed`: `Disk.SignedURL` uses the driver's own signing (SigV4 on S3 and every S3-compatible store, V4 on GCS, SAS on Azure), and `Store.URL` and `Store.DownloadURL` work unchanged. The app code above does not change. Differences to know:

- Lifetimes are capped at `filesystem.MaxPresignExpiry`, seven days, the S3 maximum; longer is `CodeInvalidArgument`. Every lifetime is required, there is no default.
- The bucket serves the bytes, so ranges and ETags are the bucket's, and the `nosniff`/CSP headers `fshttp` adds are not sent. Serving from the bucket's own domain keeps active content off your origin; still keep the policy strict for uploads.
- The signing credentials are the bucket credentials. Rotating them is the cloud provider's key rotation, not the `signedurl` key ring.
- A presigned GET on S3 honours `PresignOptions.ContentDisposition` and `ContentType` through the `response-content-disposition` and `response-content-type` parameters.

## For production

- Replace `customerOf` with your session lookup; never trust a header the client sets.
- Load the secrets from your secret store, at least 32 random bytes each, and remove the `demoSecret` fallback: fail at start-up when the key is missing.
- Serve the app over HTTPS: a signed link is a bearer credential while it lives.
- Log who asked for which document in the redirect handler; the signed request itself carries no user. The [audit plugin](../plugins/audit.md) logs changes, not reads.
- If documents must survive account deletion for legal retention, keep them out of the user's `PurgeOwner` path ([account deletion](account-deletion.md)).

## Related

- [Serving files](../concepts/serving-files.md): signed URLs, public URLs and fshttp in depth.
- [Public and private](../concepts/public-and-private.md): visibility, and separate public and private disks.
- [Chat attachments](chat-attachments.md): private files whose readers are the members of a conversation.
- [Share links](../plugins/share.md): links for people without an account, with download limits and revocation.
- [Exports](exports.md): generate a file and hand out a signed link to it.
