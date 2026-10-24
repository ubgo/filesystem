# Public CDN: publish and unpublish files between a public and a private bucket

The storage side of a CMS or site builder: an uploaded file is private (on a "vault" bucket, reachable only through signed URLs) until an editor publishes it, which moves it to a public bucket behind a CDN; unpublishing moves it back and asks the CDN to purge the old URL; and the app's own build assets are stored under content-addressed keys with `Cache-Control: public, max-age=31536000, immutable`. It uses `records` with `WithVisibilityDisks` (the store picks the disk by visibility, so `SetVisibility` moves the object), `AfterCommit` for the purge, `signedurl`, `fshttp`, and either two `memory` disks (offline) or two buckets opened by DSN through `adapters/s3`. The guide is [Public CDN](../../docs/recipes/public-cdn.md).

## Run it

```sh
cd examples
go run ./recipe-public-cdn
```

Offline (the default), both "buckets" are memory disks, this server plays the CDN at `/cdn/…` and the signed-URL endpoint at `/files/…`, the CDN purge is a log line, and the signing secret is a hard-coded demo value. It creates `site.db` (SQLite) in a new temporary directory, removed when Ctrl-C or SIGTERM stops the server, and listens on `http://localhost:8103`. Stop it with Ctrl-C; the files themselves are in memory and go with the process.

With `CDN_DSN`, `VAULT_DSN` and `CDN_BASE_URL` set, it uses real buckets (credentials from the AWS default chain); the vault's URLs are then the bucket's own SigV4 presigned URLs:

```sh
CDN_DSN='s3://site-public?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com' \
VAULT_DSN='s3://site-private?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com' \
CDN_BASE_URL='https://cdn.example.com' \
go run ./recipe-public-cdn
```

Run the tests with `go test ./recipe-public-cdn`. They run the offline mode and the bucket mode against an in-process S3 server (gofakes3), checking that the driver sends `Cache-Control` with a build asset's `PUT`.

## Walkthrough

Every route here is an editor's; authentication is out of scope.

1. Upload: the file lands in the vault, and its URL is signed:

```sh
curl -s -F file=@back.png localhost:8103/assets
```

```json
{"file_id":"fil_gruzfp3v2bkomcvg5x26xdduv7","disk":"vault","visibility":"private","url":"http://localhost:8103/files/vault/uploads/xp3qibk37yrjz3an73w2c2rmvv.png?ct=image%2Fpng&exp=1791257776&kid=k1&m=GET&sig=0c618ffaa33f4dc297f27966d160e206a9eb0ec653f9325ddad41c2b938c1b99"}
```

2. Publish: it moves to the CDN disk and gets a plain public URL, and `GET /assets/{id}` now redirects there:

```sh
curl -s -X POST localhost:8103/assets/fil_gruzfp3v2bkomcvg5x26xdduv7/publish
curl -s -I http://localhost:8103/cdn/uploads/xp3qibk37yrjz3an73w2c2rmvv.png
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' localhost:8103/assets/fil_gruzfp3v2bkomcvg5x26xdduv7
```

```text
{"file_id":"fil_gruzfp3v2bkomcvg5x26xdduv7","disk":"cdn","visibility":"public","url":"http://localhost:8103/cdn/uploads/xp3qibk37yrjz3an73w2c2rmvv.png"}
HTTP/1.1 200 OK
Accept-Ranges: bytes
Cache-Control: public, no-cache
Content-Length: 5909
Content-Security-Policy: default-src 'none'; sandbox
Content-Type: image/png
Etag: "3a45d454913924bbe424cdeeb304edad"
Last-Modified: Tue, 06 Oct 2026 03:26:16 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 03:26:16 GMT

302 http://localhost:8103/cdn/uploads/xp3qibk37yrjz3an73w2c2rmvv.png
```

3. Unpublish: back in the vault with a signed URL, the public URL is gone, and the server log shows the purge request, sent after the transaction committed:

```sh
curl -s -X POST localhost:8103/assets/fil_gruzfp3v2bkomcvg5x26xdduv7/unpublish
curl -s -I http://localhost:8103/cdn/uploads/xp3qibk37yrjz3an73w2c2rmvv.png | head -1
```

```text
{"file_id":"fil_gruzfp3v2bkomcvg5x26xdduv7","disk":"vault","visibility":"private","url":"http://localhost:8103/files/vault/uploads/xp3qibk37yrjz3an73w2c2rmvv.png?ct=image%2Fpng&exp=1791257776&kid=k1&m=GET&sig=0c618ffaa33f4dc297f27966d160e206a9eb0ec653f9325ddad41c2b938c1b99"}
HTTP/1.1 404 Not Found
```

```text
cdn purge: [http://localhost:8103/cdn/uploads/xp3qibk37yrjz3an73w2c2rmvv.png]
```

4. A build asset: the same bytes give the same URL, changed bytes a new one, and only the closed set of extensions is accepted:

```sh
printf 'body{margin:0}' > app.css
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.css'
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.css'
printf 'body{margin:0;color:#222}' > app.css
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.css'
curl -s --data-binary @app.css 'localhost:8103/site-assets?ext=.html'
```

```json
{"url":"http://localhost:8103/cdn/assets/2007703776e20c24376ebae0a759bc90.css"}
{"url":"http://localhost:8103/cdn/assets/2007703776e20c24376ebae0a759bc90.css"}
{"url":"http://localhost:8103/cdn/assets/adf1c26ee231057476e47c3f836f8e9b.css"}
{"error":"ext must be one of .css, .js, .woff2, .png","code":"invalid_argument"}
```

5. The asset is served with the `Cache-Control` stored with the object:

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
Last-Modified: Tue, 06 Oct 2026 03:26:16 GMT
X-Content-Type-Options: nosniff
Date: Tue, 06 Oct 2026 03:26:16 GMT
```

## The code

`uploadPolicy` stores every editor upload private, which with visibility disks means on the vault.

<!-- ds:block id=uploadpolicy-mdq86khj -->
**uploadPolicy** · [`examples/recipe-public-cdn/main.go:102-107`](main.go#L102-L107)

```go
var uploadPolicy = upload.Policy{
	MaxSize:    maxUpload,
	Allowed:    []string{"image/*", "application/pdf", "video/mp4"},
	Key:        upload.RandomKey("uploads"),
	Visibility: filesystem.VisibilityPrivate,
}
```
<!-- /ds:block hash=3baecd -->

`Disks` holds the public and private disk and, offline, the handlers that stand in for the CDN and the signed-URL endpoint.

<!-- ds:block id=disks-hq2zqbew -->
**Disks** · [`examples/recipe-public-cdn/main.go:112-115`](main.go#L112-L115)

```go
type Disks struct {
	CDN, Vault *filesystem.Disk
	Serve      map[string]http.Handler
}
```
<!-- /ds:block hash=25f575 -->

`memoryDisks` is the offline pair: a public memory disk whose URLs point at `/cdn`, and a private one signed by `signedurl` and served by `fshttp.Signed`.

<!-- ds:block id=memorydisks-fjqmsg48 -->
**memoryDisks** · [`examples/recipe-public-cdn/main.go:121-144`](main.go#L121-L144)

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

`bucketDisks` opens two buckets by DSN; the CDN disk's public URLs start with `CDN_BASE_URL`, and the vault signs with the bucket's own presigning.

<!-- ds:block id=bucketdisks-wqfyd4st -->
**bucketDisks** · [`examples/recipe-public-cdn/main.go:151-167`](main.go#L151-L167)

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

`CDNPurger` is the app's hook to its CDN's purge API; the demo's `logPurge` prints instead.

<!-- ds:block id=cdnpurger-e54eggag -->
**CDNPurger** · [`examples/recipe-public-cdn/main.go:172`](main.go#L172-L172)

```go
type CDNPurger func(ctx context.Context, urls []string) error
```
<!-- /ds:block hash=417a17 -->

`openStore` opens the records store with `WithVisibilityDisks(diskCDN, diskVault)`, which is what makes a visibility change a move between buckets.

<!-- ds:block id=openstore-w3rxwyq4 -->
**openStore** · [`examples/recipe-public-cdn/main.go:186-204`](main.go#L186-L204)

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

`uploadAsset` stores an upload through the policy and answers with its (signed) URL.

<!-- ds:block id=app-uploadasset-tawk584g -->
**App.uploadAsset** · [`examples/recipe-public-cdn/main.go:248-262`](main.go#L248-L262)

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

`publish` sets the file public, which copies it to the CDN bucket and removes it from the vault; `makePrivate` does the reverse in a transaction and queues the CDN purge with `AfterCommit`, so a rolled-back change purges nothing.

<!-- ds:block id=app-publish-vzs5yhrh -->
**App.publish** · [`examples/recipe-public-cdn/main.go:283-295`](main.go#L283-L295)

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

<!-- ds:block id=app-makeprivate-tu66zhch -->
**App.makePrivate** · [`examples/recipe-public-cdn/main.go:301-325`](main.go#L301-L325)

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

`publishAsset` names a build asset by the SHA-256 of its bytes, writes it only if absent, and stores the immutable `Cache-Control` where the driver can store headers.

<!-- ds:block id=publishasset-vwgjvveb -->
**publishAsset** · [`examples/recipe-public-cdn/main.go:342-359`](main.go#L342-L359)

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

`openDisks` picks the mode: buckets when both DSNs are set, memory otherwise.

<!-- ds:block id=opendisks-j4hsr23q -->
**openDisks** · [`examples/recipe-public-cdn/main.go:409-415`](main.go#L409-L415)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The two disk setups, the records store with visibility disks, upload, publish, unpublish with the CDN purge, content-addressed build assets and the server. |
| `main_test.go` | The publish cycle and build assets offline, and the bucket mode by DSN against an in-process S3 server that records the `Cache-Control` sent. |

## Next

- The guide: [Public CDN](../../docs/recipes/public-cdn.md), with edge cases and production notes.
- [Public and private](../../docs/concepts/public-and-private.md), [Serving files](../../docs/concepts/serving-files.md) and [S3-compatible stores](../../docs/adapters/s3-compatible.md).
- Related recipes: [recipe-private-downloads](../recipe-private-downloads), [recipe-product-images](../recipe-product-images), [recipe-avatars](../recipe-avatars).
