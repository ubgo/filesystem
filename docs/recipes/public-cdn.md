# Public files behind a CDN

Public images, downloads and site assets should be served by a CDN, not by your application, and cached as long as possible. Private files must never be reachable without a signed link, and a file that was public and is made private must stop being public everywhere, including in the CDN's edge caches. This recipe is for CMSs, site builders, marketplaces, documentation sites and any product that publishes user content, and for any app that wants its build assets cached for a year.

The program is [examples/recipe-public-cdn](../../examples/recipe-public-cdn). Every code block below is copied from it, and every transcript is from running it.

## What you get

- Two buckets: a public one behind a CDN and a private one (the "vault") that is only ever read through signed URLs. Since 2023 new S3 buckets disable object ACLs, so "one bucket per visibility" is the layout that works on every provider.
- Uploads land private. Publishing moves the file to the public bucket; unpublishing moves it back and asks the CDN to purge the old URL.
- Build assets (CSS, JS, fonts) are stored under keys derived from their content and served with `Cache-Control: public, max-age=31536000, immutable`.
- Without any environment the program runs offline on memory disks; with three environment variables it runs on real buckets, with the same code.

## The design

| Piece | Why |
|---|---|
| `records.WithVisibilityDisks("cdn", "vault")` | The store keeps public files on one disk and private ones on the other. `Store.Upload` with an empty disk name picks by the policy's visibility. |
| `Store.SetVisibility` | Moves a file between the disks: copied first, the row updated, the old object deleted after the commit. A rolled-back transaction deletes the copy, so the row never claims a visibility the object does not have. |
| `Store.AfterCommit` | Purges the CDN only once the move committed. A purge for a change that rolled back would be harmless but wasteful; a purge before the commit could be refilled from the old object. |
| `WithDefaultVisibility(public)` + `WithPublicURL(cdn)` on the public disk | Every object on it is public, and `PublicURL` returns the CDN's URL with no backend call. |
| Content-addressed keys | A key derived from the bytes changes whenever the bytes do, so a cached copy can never be stale. That is the only condition under which `immutable` is safe. |
| `WriteOptions.Headers` | `HTTPHeaders.CacheControl` is stored with the object and sent by the bucket, the CDN and `fshttp`. Needs the `Headers` capability: memory, S3, GCS and Azure have it; local and SFTP do not. |

## Step 1: Two disks

Offline, both buckets are memory disks, and the program serves them itself: `fshttp.Public` plays the CDN and `fshttp.Signed` the signed-URL endpoint.

<!-- ds:block id=disks-hq2zqbew -->
**Disks** · [`examples/recipe-public-cdn/main.go:112-115`](../../examples/recipe-public-cdn/main.go#L112-L115)

```go
type Disks struct {
	CDN, Vault *filesystem.Disk
	Serve      map[string]http.Handler
}
```
<!-- /ds:block hash=25f575 -->

<!-- ds:block id=memorydisks-fjqmsg48 -->
**memoryDisks** · [`examples/recipe-public-cdn/main.go:121-144`](../../examples/recipe-public-cdn/main.go#L121-L144)

```go
func memoryDisks(origin string, secret []byte) (Disks, error) {
	cdn, err := filesystem.NewDisk(diskCDN, memory.New(),
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic),
		filesystem.WithPublicURL(origin+cdnPrefix))
	if err != nil {
		return Disks{}, err
	}
	signer, err := signedurl.New(origin+signedPrefix, []signedurl.Key{{ID: "k1", Secret: secret}})
	if err != nil {
		return Disks{}, err
	}
	vault, err := filesystem.NewDisk(diskVault, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return Disks{}, err
	}
	mgr, err := filesystem.NewManager(cdn, vault)
	if err != nil {
		return Disks{}, err
	}
	return Disks{CDN: cdn, Vault: vault, Serve: map[string]http.Handler{
		cdnPrefix:    fshttp.Public(cdn),
		signedPrefix: fshttp.Signed(mgr, signer),
	}}, nil
}
```
<!-- /ds:block hash=0a4eee -->

With real buckets, both disks come from DSNs through a `Mux`. The vault signs its own URLs (SigV4 on S3), and the public disk's URLs are the CDN's:

<!-- ds:block id=bucketdisks-wqfyd4st -->
**bucketDisks** · [`examples/recipe-public-cdn/main.go:151-167`](../../examples/recipe-public-cdn/main.go#L151-L167)

```go
func bucketDisks(ctx context.Context, cdnDSN, vaultDSN, cdnBase string) (Disks, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(s3.Scheme, s3.Opener); err != nil {
		return Disks{}, err
	}
	cdn, err := mux.Open(ctx, diskCDN, cdnDSN,
		filesystem.WithDefaultVisibility(filesystem.VisibilityPublic),
		filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return Disks{}, err
	}
	vault, err := mux.Open(ctx, diskVault, vaultDSN)
	if err != nil {
		return Disks{}, err
	}
	return Disks{CDN: cdn, Vault: vault}, nil
}
```
<!-- /ds:block hash=3b4c7c -->

<!-- ds:block id=opendisks-j4hsr23q -->
**openDisks** · [`examples/recipe-public-cdn/main.go:409-415`](../../examples/recipe-public-cdn/main.go#L409-L415)

```go
func openDisks(ctx context.Context) (Disks, error) {
	cdnDSN, vaultDSN := os.Getenv(envCDNDSN), os.Getenv(envVaultDSN)
	if cdnDSN == "" || vaultDSN == "" {
		return memoryDisks(baseURL, bytes.Repeat([]byte("change-me"), 4))
	}
	return bucketDisks(ctx, cdnDSN, vaultDSN, os.Getenv(envCDNBase))
}
```
<!-- /ds:block hash=586f66 -->

```sh
export CDN_DSN='s3://site-public?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com'
export VAULT_DSN='s3://site-private?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com'
export CDN_BASE_URL='https://cdn.example.com'
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…
go run ./recipe-public-cdn
```

The bucket side is configuration, not code: make `site-public` readable by the CDN (an R2 custom domain, a CloudFront distribution with origin access, a public GCS bucket) and keep `site-private` closed. The library cannot check that the public bucket really is public or that the vault really is not; `Disk.PublicURL` builds the URL without asking the backend.

## Step 2: The store and the upload

<!-- ds:block id=openstore-w3rxwyq4 -->
**openStore** · [`examples/recipe-public-cdn/main.go:186-204`](../../examples/recipe-public-cdn/main.go#L186-L204)

```go
func openStore(ctx context.Context, dataDir string, d Disks) (*records.Store, error) {
	mgr, err := filesystem.NewManager(d.CDN, d.Vault)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "site.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store, err := records.New(db, sqlb.SQLiteDialect{}, mgr,
		records.WithOwnerTypes(ownerSite),
		records.WithVisibilityDisks(diskCDN, diskVault))
	if err != nil {
		return nil, err
	}
	_, err = store.Migrate(ctx)
	return store, err
}
```
<!-- /ds:block hash=ee940f -->

<!-- ds:block id=uploadpolicy-mdq86khj -->
**uploadPolicy** · [`examples/recipe-public-cdn/main.go:102-107`](../../examples/recipe-public-cdn/main.go#L102-L107)

```go
var uploadPolicy = upload.Policy{
	MaxSize:    maxUpload,
	Allowed:    []string{"image/*", "application/pdf", "video/mp4"},
	Key:        upload.RandomKey("uploads"),
	Visibility: filesystem.VisibilityPrivate,
}
```
<!-- /ds:block hash=3baecd -->

<!-- ds:block id=app-uploadasset-tawk584g -->
**App.uploadAsset** · [`examples/recipe-public-cdn/main.go:248-262`](../../examples/recipe-public-cdn/main.go#L248-L262)

```go
func (a *App) uploadAsset(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+64<<10)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the file in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.Store.Upload(r.Context(), "", "", file, header.Filename, uploadPolicy)
	if err != nil {
		writeError(w, err)
		return
	}
	a.respond(w, r, f)
}
```
<!-- /ds:block hash=f464c7 -->

## Step 3: Publish and unpublish

<!-- ds:block id=app-publish-vzs5yhrh -->
**App.publish** · [`examples/recipe-public-cdn/main.go:283-295`](../../examples/recipe-public-cdn/main.go#L283-L295)

```go
func (a *App) publish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.Store.SetVisibility(r.Context(), "", id, filesystem.VisibilityPublic); err != nil {
		writeError(w, err)
		return
	}
	f, err := a.Store.File(r.Context(), "", id)
	if err != nil {
		writeError(w, err)
		return
	}
	a.respond(w, r, f)
}
```
<!-- /ds:block hash=c2a556 -->

Making a file private has one step the library cannot take for you: the CDN may hold the public copy for as long as its cache rule says. The object leaves the bucket, so new edge locations get a 404, but edges that already cached it keep serving it until they are purged.

<!-- ds:block id=cdnpurger-e54eggag -->
**CDNPurger** · [`examples/recipe-public-cdn/main.go:172`](../../examples/recipe-public-cdn/main.go#L172-L172)

```go
type CDNPurger func(ctx context.Context, urls []string) error
```
<!-- /ds:block hash=417a17 -->

<!-- ds:block id=app-makeprivate-tu66zhch -->
**App.makePrivate** · [`examples/recipe-public-cdn/main.go:301-325`](../../examples/recipe-public-cdn/main.go#L301-L325)

```go
func (a *App) makePrivate(ctx context.Context, id string) (records.File, error) {
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := a.Store.File(ctx, "", id)
		if err != nil || f.Visibility != filesystem.VisibilityPublic {
			return err
		}
		public, err := a.Store.URL(ctx, f, linkLifetime)
		if err != nil {
			return err
		}
		if err := a.Store.SetVisibility(ctx, "", id, filesystem.VisibilityPrivate); err != nil {
			return err
		}
		a.Store.AfterCommit(ctx, func(ctx context.Context) {
			if err := a.Purge(ctx, []string{public}); err != nil {
				fmt.Fprintf(os.Stderr, "cdn purge of %s: %v\n", public, err)
			}
		})
		return nil
	})
	if err != nil {
		return records.File{}, err
	}
	return a.Store.File(ctx, "", id)
}
```
<!-- /ds:block hash=16c86b -->

`SetVisibility` joins the outer transaction, so the copy back to the vault, the row update, the deletion of the public object and the purge all follow the same commit. If the image has variants (the `variants` plugin), they are dropped and regenerated on the private disk on their next request; purge their public URLs as well.

## Step 4: Immutable build assets

<!-- ds:block id=publishasset-vwgjvveb -->
**publishAsset** · [`examples/recipe-public-cdn/main.go:342-359`](../../examples/recipe-public-cdn/main.go#L342-L359)

```go
func publishAsset(ctx context.Context, disk *filesystem.Disk, ext string, body []byte) (string, error) {
	sum := sha256.Sum256(body)
	key := filesystem.JoinKey(assetDir, hex.EncodeToString(sum[:16])+ext)
	exists, err := disk.Exists(ctx, key)
	if err != nil {
		return "", err
	}
	if !exists {
		opts := filesystem.WriteOptions{ContentType: assetTypes[ext], MaxSize: maxAsset}
		if disk.Capabilities().Headers {
			opts.Headers = filesystem.HTTPHeaders{CacheControl: immutable}
		}
		if _, err := disk.WriteBytes(ctx, key, body, opts); err != nil {
			return "", err
		}
	}
	return disk.PublicURL(ctx, key)
}
```
<!-- /ds:block hash=40ba1c -->

A deploy step calls this for every file of a build and writes the returned URLs into its HTML. Upload the assets before the HTML that references them, and keep old assets for a while: a browser that loaded yesterday's HTML still asks for yesterday's CSS.

Uploaded files do not get per-object `Cache-Control`. `upload.Policy` has no headers field, so `Store.Upload`, `recordshttp` and the variants plugin write objects without one. They do not need `immutable` per object to be cached safely, because `upload.RandomKey` gives every upload, and every `Store.Replace`, a new key that is never reused; set the caching on the CDN instead, as a rule for the bucket or for the `uploads/` and `variants/` paths.

## Try it

```sh
cd examples
go run ./recipe-public-cdn
```

```text
listening on http://localhost:8103
```

Upload: the file lands in the vault, and its URL is signed:

```sh
curl -s -F file=@back.png localhost:8103/assets
```

```json
{"file_id":"fil_3ffbkzptw2vqda7mw3rhzsirtu","disk":"vault","visibility":"private","url":"http://localhost:8103/files/vault/uploads/of45fsfynccnutcqnwfddpkzyu.png?ct=image%2Fpng&exp=1791257775&kid=k1&m=GET&sig=7eac8bccc471fc4c4b81282ad359fda9d7473e947b027011e788e845a9042d36"}
```

Publish: it moves to the CDN disk and gets a plain public URL:

```sh
curl -s -X POST localhost:8103/assets/fil_3ffbkzptw2vqda7mw3rhzsirtu/publish
curl -s -I http://localhost:8103/cdn/uploads/of45fsfynccnutcqnwfddpkzyu.png
```

```text
{"file_id":"fil_3ffbkzptw2vqda7mw3rhzsirtu","disk":"cdn","visibility":"public","url":"http://localhost:8103/cdn/uploads/of45fsfynccnutcqnwfddpkzyu.png"}
HTTP/1.1 200 OK
Cache-Control: public, no-cache
Content-Type: image/png
X-Content-Type-Options: nosniff
```

Unpublish: back in the vault, the public URL is gone, and the server log shows the purge request:

```sh
curl -s -X POST localhost:8103/assets/fil_3ffbkzptw2vqda7mw3rhzsirtu/unpublish
curl -s -I http://localhost:8103/cdn/uploads/of45fsfynccnutcqnwfddpkzyu.png
```

```text
{"file_id":"fil_3ffbkzptw2vqda7mw3rhzsirtu","disk":"vault","visibility":"private","url":"http://localhost:8103/files/vault/uploads/of45fsfynccnutcqnwfddpkzyu.png?ct=image%2Fpng&exp=1791257775&kid=k1&m=GET&sig=7eac8bccc471fc4c4b81282ad359fda9d7473e947b027011e788e845a9042d36"}
HTTP/1.1 404 Not Found
```

```text
cdn purge: [http://localhost:8103/cdn/uploads/of45fsfynccnutcqnwfddpkzyu.png]
```

A build asset: the same bytes give the same URL, changed bytes a new one:

```sh
printf 'body{margin:0}' > app.css
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.css'
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.css'
printf 'body{margin:0;color:#222}' > app.css
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.css'
```

```json
{"url":"http://localhost:8103/cdn/assets/2007703776e20c24376ebae0a759bc90.css"}
{"url":"http://localhost:8103/cdn/assets/2007703776e20c24376ebae0a759bc90.css"}
{"url":"http://localhost:8103/cdn/assets/adf1c26ee231057476e47c3f836f8e9b.css"}
```

```sh
curl -s -I http://localhost:8103/cdn/assets/2007703776e20c24376ebae0a759bc90.css
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: public, max-age=31536000, immutable
Content-Length: 14
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: text/css; charset=utf-8
Etag: "7411b9041dad72eeed20b4bced355941"
Last-Modified: Tue, 06 Oct 2026 03:26:15 GMT
X-Content-Type-Options: nosniff
```

Check: `Cache-Control` is the one stored with the object. On S3 the bucket returns it the same way; the example's test runs the bucket mode against an in-process S3 server and checks that the driver sends the header with the `PUT`. That fake does not store `Cache-Control`, so the read-back is shown here on the memory disk.

## Edge cases this handles

- Publishing fails half-way (the copy succeeds, the row update fails): the transaction rolls back and the copy on the public disk is deleted; the file stays private.
- The public object cannot be deleted after a move to private: the row already says private and the file is served only through signed links, but the stray public object remains; set `records.WithObjectErrorHandler` to log it and retry the delete.
- Publishing a file that is already public, or unpublishing a private one, does nothing and purges nothing.
- A build asset written twice: the second write is skipped by `Exists`; two deploys racing write the same bytes to the same key, which is harmless.
- An asset type outside the closed set (`.html`): refused, so the public bucket never serves a page from your CDN domain.

## For production

- Authenticate the routes: here every caller is an editor.
- Give the public bucket its own domain (`cdn.example.com`), never your application's origin, so user content cannot read your app's cookies.
- Wire `CDNPurger` to your CDN's API (Cloudflare `purge_cache`, CloudFront `CreateInvalidation`, Fastly purge by URL). Purges can fail; log them and retry from a queue.
- For an uploaded file whose key is reused (you write to a fixed key such as `og/<post>.png`), do not use `immutable`: use a short `max-age` and purge on change, or put a version in the key.
- With `acl=true` on AWS (per-object ACLs) one bucket can hold both visibilities, and `SetVisibility` changes the ACL in place instead of moving the object ([S3](../adapters/s3.md)). Most providers have no per-object ACLs, which is why this recipe uses two disks.

## Related

- [Public and private](../concepts/public-and-private.md): visibility, default visibility, visibility disks.
- [Serving files](../concepts/serving-files.md): public URLs, signed URLs, `fshttp`.
- [Private downloads](private-downloads.md): links to files that stay private.
- [Product images](product-images.md) and [avatars](avatars.md): public images with variants.
- [S3-compatible stores](../adapters/s3-compatible.md): DSNs for R2, B2, Spaces and others.
