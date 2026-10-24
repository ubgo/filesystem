# Vercel Blob

The Vercel Blob driver stores files in a Vercel Blob store, for applications deployed on Vercel that want their uploads next to their functions without running a bucket of their own. It speaks the Blob REST API over `net/http`, with no dependency beyond the library.

Every snippet on this page is cut from [examples/adapters-hosted](../../examples/adapters-hosted), which runs the driver against an in-process fake of the Blob API, and every transcript is from running it.

## When to choose it over a bucket

Choose Vercel Blob when the application already runs on Vercel and its files are modest: avatars, uploads, generated exports. The store is created and connected from the Vercel dashboard, the token arrives as an environment variable, public blobs are served from Vercel's CDN with no setup, and there is no IAM, bucket policy or CORS to get right. Choose a bucket ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md), or an [S3-compatible](s3-compatible.md) service such as R2) when you need signed links that expire, per-object visibility, multipart uploads of large files, a create-only write, or storage classes and lifecycle rules: Vercel Blob offers none of these through this driver. A store is wholly public or wholly private, so an application that keeps both kinds of file uses two stores, one disk each.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/vercelblob`, a module of its own (`go get github.com/ubgo/filesystem/adapters/vercelblob`). Its reference is [adapters/vercelblob/README.md](../../adapters/vercelblob/README.md).
- Create a store in the Vercel dashboard and connect it to the project; Vercel puts its read-write token in `BLOB_READ_WRITE_TOKEN`. A store is public or private when created, and `Config.Access` must say which.
- Tested against an in-process fake of the API, mirroring the requests `@vercel/blob` 2.x makes. What only a real store shows: the exact URL Vercel gives a pathname holding characters that need escaping, and how long the CDN serves an old copy.

## Step 1 — Open a disk

```go
drv, err := vercelblob.New(vercelblob.Config{
	Token:  os.Getenv("BLOB_READ_WRITE_TOKEN"),
	Access: vercelblob.AccessPrivate,
})
if err != nil {
	return err
}
disk, err := filesystem.NewDisk("blob", drv)
```

From a DSN the host is the store's access mode, and the token comes from `BLOB_READ_WRITE_TOKEN` (the variable Vercel injects) unless `token_env` names another:

```go
mux.Register(vercelblob.Scheme, vercelblob.Opener)
disk, err := mux.Open(ctx, "blob", "vercelblob://private")
```

The blob host (`https://<store>.<access>.blob.vercel-storage.com`) is derived from the token. The token is the store's only credential: `Config` prints and logs it redacted, and the driver's transport sends it only to the API and, for a private store, to the blob host. `New` makes no request, so a wrong token shows up on the first operation, not at startup.

The example opens one disk per store. `APIURL` and `BlobURL` point it at the fake; against Vercel leave them empty. A public store's disk is opened with `WithDefaultVisibility(VisibilityPublic)`, since every blob in it is public:

<!-- ds:block id=openvercel-5wv46szv -->
**openVercel** · [`examples/adapters-hosted/vercel.go:78-93`](../../examples/adapters-hosted/vercel.go#L78-L93)

```go
func openVercel(token string, access vercelblob.Access, fake *fakes.Vercel) (*filesystem.Disk, error) {
	drv, err := vercelblob.New(vercelblob.Config{
		Token:   token,
		Access:  access,
		APIURL:  fake.API.URL,
		BlobURL: fake.Blobs.URL,
	})
	if err != nil {
		return nil, err
	}
	var opts []filesystem.Option
	if access == vercelblob.AccessPublic {
		opts = append(opts, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	}
	return filesystem.NewDisk("blob-"+string(access), drv, opts...)
}
```
<!-- /ds:block hash=2753db -->

## Step 2 — Know the token and the URL shape

A read-write token has the shape `vercel_blob_rw_<store id>_<secret>`. The driver lower-cases the store ID and builds the blob host from it and the access mode; a public blob's URL is that host plus the escaped key, permanent and readable by anyone who has it. This runs with no request at all:

<!-- ds:block id=verceltokenandurls-7qeurunm -->
**vercelTokenAndURLs** · [`examples/adapters-hosted/vercel.go:52-71`](../../examples/adapters-hosted/vercel.go#L52-L71)

```go
func vercelTokenAndURLs(ctx context.Context, out io.Writer) error {
	cfg := vercelblob.Config{Token: vercelToken, Access: vercelblob.AccessPublic}
	drv, err := vercelblob.New(cfg)
	if err != nil {
		return err
	}
	disk, err := filesystem.NewDisk("blob", drv, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return err
	}
	for _, key := range []string{"avatars/u1.png", "docs/annual report.pdf"} {
		u, err := disk.PublicURL(ctx, key)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "public URL: %s\n", u)
	}
	fmt.Fprintf(out, "config prints as: %v\n", cfg)
	return nil
}
```
<!-- /ds:block hash=b24842 -->

```text
public URL: https://storeabc123.public.blob.vercel-storage.com/avatars/u1.png
public URL: https://storeabc123.public.blob.vercel-storage.com/docs/annual%20report.pdf
config prints as: vercelblob.Config{Access: "public", Token: [redacted]}
```

A token that does not start with `vercel_blob_rw_` (a client upload token, say) names no store, and `New` refuses it unless `BlobURL` is given.

## Step 3 — Write, read, list, copy and delete

A key is the blob's pathname, written with no random suffix and overwriting allowed, so keys mean what they say. Vercel keeps a content type, so the type the Disk sniffs from the first bytes is stored and comes back from `Stat`. The ETag is Vercel's, without quotes.

<!-- ds:block id=vercelbasics-unjm7x3q -->
**vercelBasics** · [`examples/adapters-hosted/vercel.go:98-131`](../../examples/adapters-hosted/vercel.go#L98-L131)

```go
func vercelBasics(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "stat: %s %d bytes %s etag=%s\n", e.Key, e.Size, e.ContentType, e.ETag)
	r, err := disk.ReadRange(ctx, "avatars/u1.png", filesystem.ByteRange{Offset: 1, Length: 3})
	if err != nil {
		return err
	}
	head, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "range read: %q\n", head)
	if _, err := disk.Copy(ctx, "avatars/u1.png", "avatars/u1-backup.png"); err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "avatars", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "list avatars: %d entries after a server-side copy\n", len(page.Entries))
	if err := disk.Delete(ctx, "avatars/u1-backup.png"); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, "avatars/u1-backup.png")
	fmt.Fprintf(out, "after delete: exists=%t (%s)\n", ok, outcome(err))
	return err
}
```
<!-- /ds:block hash=d3bcd1 -->

```text
stat: avatars/u1.png 29 bytes image/png etag=e1
range read: "PNG"
list avatars: 2 entries after a server-side copy
after delete: exists=false (ok)
```

- Writes are one streamed `PUT`, then a head for the size and time. A body that fails part-way aborts the request and nothing is stored.
- Reads `GET` the blob's URL with a `Range` header. Public blobs are served through Vercel's CDN, so after an overwrite or a delete a read can return the old bytes for up to a minute; private blobs are read with `cache=0`. `Stat` and `List` ask the API and are current.
- `Copy` is server-side (`PUT ?fromUrl=`). There is no `Move`: the Disk copies and deletes, which is not atomic.
- `List` is the API's prefix listing, following its cursor; folder placeholders the dashboard creates (pathnames ending in `/`) are skipped. `DeleteKeys` sends 100 blobs per request.
- `Delete` of a missing key succeeds.

## Step 4 — Capabilities, visibility and conditional writes

`Copy` on the server, `DeleteKeys`, `CompareAndSwap` (`IfMatch` is the API's `x-if-match`), `ContentType`, and `PublicURL` for a public store. `IfNotExists` is refused with `CodeUnsupported`: Vercel refuses to overwrite when asked, but with no documented status the driver could rely on. There is no `Move`, no signed reads (Vercel's presigned URLs are client upload tokens), and no per-object visibility, metadata or headers.

<!-- ds:block id=vercelvisibility-4dq65ubj -->
**vercelVisibility** · [`examples/adapters-hosted/vercel.go:137-156`](../../examples/adapters-hosted/vercel.go#L137-L156)

```go
func vercelVisibility(ctx context.Context, out io.Writer, public, private *filesystem.Disk) error {
	fmt.Fprintf(out, "public store capabilities: %s\n", public.Capabilities())
	fmt.Fprintf(out, "private store capabilities: %s\n", private.Capabilities())
	_, err := private.PublicURL(ctx, "avatars/u1.png")
	fmt.Fprintf(out, "private store PublicURL: %s\n", outcome(err))
	_, err = public.WriteBytes(ctx, "secret.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	fmt.Fprintf(out, "private write on the public store: %s\n", outcome(err))
	_, err = private.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "IfNotExists: %s\n", outcome(err))
	e, err := private.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	if _, err := private.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{IfMatch: e.ETag}); err != nil {
		return err
	}
	_, err = private.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "IfMatch with the current etag: ok; with the same etag again: %s\n", outcome(err))
	return nil
}
```
<!-- /ds:block hash=27c2ec -->

```text
public store capabilities: [copy,delete-keys,public-url,content-type,compare-and-swap]
private store capabilities: [copy,delete-keys,content-type,compare-and-swap]
private store PublicURL: unsupported
private write on the public store: unsupported
IfNotExists: unsupported
IfMatch with the current etag: ok; with the same etag again: conflict
```

The refusal of a private write on the public store is the Disk keeping its promise: a store's access mode is fixed, so a write that asks for another visibility than the disk's default fails instead of being stored public. For a create-only write, which `IfNotExists` would give on a bucket, use a fresh key per upload (`upload.RandomKey`) instead.

## Step 5 — Handle errors

Errors follow the API's error codes: `not_found` `CodeNotFound`, `precondition_failed` `CodeConflict`, `file_too_large` `CodeTooLarge`, `content_type_not_allowed` `CodeRejected`, `forbidden` and `store_suspended` `CodePermission`, `rate_limited`, `service_unavailable` and 5xx `CodeUnavailable` (with the `Retry-After` the API sent, read by `filesystem.RetryAfterOf`), other 400 `CodeInvalidArgument`.

<!-- ds:block id=vercelerrors-d9h25xtq -->
**vercelErrors** · [`examples/adapters-hosted/vercel.go:161-179`](../../examples/adapters-hosted/vercel.go#L161-L179)

```go
func vercelErrors(ctx context.Context, out io.Writer, fake *fakes.Vercel) error {
	wrong, err := openVercel("vercel_blob_rw_StoreAbc123_wrongsecret", vercelblob.AccessPublic, fake)
	if err != nil {
		return err
	}
	_, err = wrong.Stat(ctx, "avatars/u1.png")
	fmt.Fprintf(out, "wrong token: %s\n", outcome(err))
	disk, err := openVercel(vercelToken, vercelblob.AccessPublic, fake)
	if err != nil {
		return err
	}
	fake.FailNext(fakes.Failure{Status: http.StatusTooManyRequests, Code: "rate_limited", RetryAfter: int(retryHint.Seconds())})
	_, err = disk.Stat(ctx, "avatars/u1.png")
	fmt.Fprintf(out, "rate limited: %s\n", outcome(err))
	fake.MaxBytes = vercelUploadLimit
	_, err = disk.WriteBytes(ctx, "big.bin", make([]byte, vercelUploadLimit+1), filesystem.WriteOptions{})
	fmt.Fprintf(out, "over the size limit: %s\n", outcome(err))
	return nil
}
```
<!-- /ds:block hash=8851b2 -->

```text
wrong token: permission
rate limited: unavailable (retry after 30s)
over the size limit: too_large
```

## Try it

```sh
cd examples
go run ./adapters-hosted
```

The first part of the output is the Vercel Blob walk-through above; `go test ./adapters-hosted` checks every quoted line.

## Pitfalls

- **The token is the whole store.** It reads, writes and deletes every blob; there is no read-only token. Keep it in the environment (Vercel injects it), never in source or a client bundle, and let `Config`'s redaction keep it out of logs. The driver only sends it to the API and a private store's blob host, never to a public blob host or across a redirect.
- **Public means public forever.** On a public store every blob's URL is permanent and needs no signature; anyone who has the link can read it, and there is no expiry to fall back on. Put anything private (invoices, documents, exports) in a private store and serve it through your own handler.
- **`Access` must match the store.** It is not a per-write choice: a mismatch makes every write fail.
- **The CDN can serve old bytes.** For up to a minute after an overwrite or a delete, a read of a public blob can return the previous version. Content-addressed keys (a new key per version) avoid the staleness entirely; `Stat` and `List` are always current.
- **Browsers and the CDN cache a blob for a month by default.** For blobs you overwrite under the same key, set `Config.CacheMaxAge` (`cache_max_age=` in seconds, one minute at least), which every write and copy sends as `x-cache-control-max-age`:

```go
drv, err := vercelblob.New(vercelblob.Config{
	Token: os.Getenv("BLOB_READ_WRITE_TOKEN"), Access: vercelblob.AccessPublic,
	CacheMaxAge: 5 * time.Minute, // avatars change; let caches drop them sooner
})
```
- **No `Move`, no `IfNotExists`.** A Disk `Move` is copy-then-delete and can leave both keys on a failure in between; a create-only write must use a key nobody else can pick.
- **Size limits are the plan's.** A body over the store's limit is `CodeTooLarge`, after it was sent; enforce your own `WriteOptions.MaxSize` or upload `Policy.MaxSize` so the request is refused before it starts.
- **Rate limits are retryable.** `CodeUnavailable` with a `Retry-After` is the signal to back off; a [`layer.Retry`](../concepts/interceptors.md) interceptor on the disk does it for you, waiting at least that long.

## Related

- The example: [examples/adapters-hosted](../../examples/adapters-hosted), and the driver reference [adapters/vercelblob/README.md](../../adapters/vercelblob/README.md).
- [Cloudinary](cloudinary.md), the other hosted store with no bucket.
- [Capabilities](../concepts/capabilities.md), [public and private files](../concepts/public-and-private.md), [errors](../concepts/errors.md) and [serving files](../concepts/serving-files.md).
- Buckets: [S3](s3.md), [GCS](gcs.md), [Azure](azure.md), [S3-compatible](s3-compatible.md).
