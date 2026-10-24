# Vercel Blob and Cloudinary drivers

Two hosted stores that are not buckets: Vercel Blob, the file store of a Vercel project, and Cloudinary, the media service, where the driver keeps exact bytes as raw resources. This program opens a `*filesystem.Disk` on each and walks what an application does with them: write, read, stat, list, copy or move, delete, public URLs, the visibility each declares and refuses, how a key maps to what the service stores (a Vercel pathname, a Cloudinary public ID), the capabilities each has and lacks, and how a wrong credential, a rate limit and a size limit come back as filesystem error codes. It uses the core `filesystem` package, `github.com/ubgo/filesystem/adapters/vercelblob` and `github.com/ubgo/filesystem/adapters/cloudinary`. The guides [Vercel Blob](../../docs/adapters/vercelblob.md) and [Cloudinary](../../docs/adapters/cloudinary.md) explain each step.

## Run it

```sh
cd examples
go run ./adapters-hosted
go test ./adapters-hosted
```

It runs against in-process fakes of both HTTP APIs (`internal/fakes`, cut down from the fakes in the drivers' own tests), on random local ports, so it needs no account, credentials, Docker or network. The Cloudinary fake checks every request's signature with the secret, as Cloudinary does; the Vercel fake checks the bearer token on the API and, for a private store, on the blob host. The program has no switch to the real services: against them, leave `APIURL`, `BlobURL` and `DeliveryURL` empty and read `BLOB_READ_WRITE_TOKEN`, `CLOUDINARY_API_KEY` and `CLOUDINARY_API_SECRET` from the environment.

## What it does

```text
== Vercel Blob
public URL: https://storeabc123.public.blob.vercel-storage.com/avatars/u1.png
public URL: https://storeabc123.public.blob.vercel-storage.com/docs/annual%20report.pdf
config prints as: vercelblob.Config{Access: "public", Token: [redacted]}
stat: avatars/u1.png 29 bytes image/png etag=e1
range read: "PNG"
list avatars: 2 entries after a server-side copy
after delete: exists=false (ok)
public store capabilities: [copy,delete-keys,public-url,content-type,compare-and-swap]
private store capabilities: [copy,delete-keys,content-type,compare-and-swap]
private store PublicURL: unsupported
private write on the public store: unsupported
IfNotExists: unsupported
IfMatch with the current etag: ok; with the same etag again: conflict
wrong token: permission
rate limited: unavailable (retry after 30s)
over the size limit: too_large
== Cloudinary
config prints as: cloudinary.Config{CloudName: "acme", APIKey: "123456789012345", APISecret: [redacted], Type: ""}
write entry type "image/png", stat type "", 29 bytes, etag=version 1
read back identical: true
after move and copy: avatars/user-1-copy.png, avatars/user-1.png
after DeletePrefix: 0 resources; Admin API calls so far: 6
stored as: raw/upload/docs/annual~20report~20~28v2~29.pdf
listed as: docs/annual report (v2).pdf
public URL path: /acme/raw/upload/docs/annual~20report~20~28v2~29.pdf
brand/ holds 3 resources, the driver lists: brand/terms.txt
upload type capabilities: [move,delete-prefix,delete-keys,public-url]
authenticated type capabilities: [move,delete-prefix,delete-keys]
authenticated PublicURL: unsupported
SignedURL: unsupported
private write on the upload type: unsupported
IfNotExists: unsupported
wrong secret: permission
Admin API limit: unavailable (retry after 30s)
over the raw-upload limit: too_large
```

Vercel Blob:

1. From the token alone, with no request, the driver derives the store's blob host: the store ID (`StoreAbc123`, the fourth `_` field of `vercel_blob_rw_<store>_<secret>`) lower-cased, then the access mode. A public blob's URL is that host plus the escaped key, so a space becomes `%20`. `Config` prints with the token redacted.
2. On a private store a PNG is written (its type sniffed from the bytes, since Vercel keeps a content type), stat'ed through the API, read as a byte range, copied on the server (`PUT ?fromUrl=`), listed and deleted.
3. Only the public store declares `public-url`. Neither store declares `set-visibility`: a store's access mode is fixed when it is created, so the public store's disk is opened with `WithDefaultVisibility(VisibilityPublic)` and a write asking for private is refused with `unsupported` rather than silently stored public. `IfNotExists` is refused; `IfMatch` is a real compare-and-swap, and a stale ETag is `conflict`.
4. A wrong token is `permission`; a `rate_limited` answer is `unavailable` with its `Retry-After`; a body over the plan's limit is `too_large`.

Cloudinary:

5. A PNG is stored as a raw resource: the write's entry carries the sniffed `image/png`, but `Stat` reports no type, because raw resources keep none. The ETag is Cloudinary's version number. The bytes read back unchanged (no transformation). `Move` is a server-side rename; `Copy` works but the Disk streams it, since Cloudinary has no copy; `DeletePrefix` removes the folder. Six Admin API calls were spent on the way (`Stat`, the two stats inside `Move`, the `List`, the prefix delete, the stat before the stream copy), each counting against Cloudinary's hourly limit.
6. A key with spaces and parentheses becomes the public ID `docs/annual~20report~20~28v2~29.pdf` (every byte outside `[A-Za-z0-9._-]` is `~XX`); listings decode it back to the key, and the `upload` type's delivery URL carries the encoded ID under `/raw/upload/`.
7. Of three resources other tools put under `brand/`, the driver lists one: a raw resource whose public ID decodes. A raw ID with a space (not written by the driver) is skipped, and an image resource is never seen, because the driver reads and writes raw resources only.
8. The `authenticated` type (the default) has no public URL; neither type has signed URLs, per-object visibility or `IfNotExists`.
9. A wrong secret fails the request's signature (`permission`); the Admin API's 420 is `unavailable` with its `Retry-After`; a body over the raw-upload limit is `too_large`.

## The code

### Vercel Blob

What the driver derives from the token, with no request.

<!-- ds:block id=verceltokenandurls-7qeurunm -->
**vercelTokenAndURLs** · [`examples/adapters-hosted/vercel.go:52-71`](vercel.go#L52-L71)

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

Opening a disk on a store; a public store's disk defaults writes to public.

<!-- ds:block id=openvercel-5wv46szv -->
**openVercel** · [`examples/adapters-hosted/vercel.go:78-93`](vercel.go#L78-L93)

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

Write, stat, a range read, a server-side copy, list and delete.

<!-- ds:block id=vercelbasics-unjm7x3q -->
**vercelBasics** · [`examples/adapters-hosted/vercel.go:98-131`](vercel.go#L98-L131)

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

What each store declares and refuses.

<!-- ds:block id=vercelvisibility-4dq65ubj -->
**vercelVisibility** · [`examples/adapters-hosted/vercel.go:137-156`](vercel.go#L137-L156)

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

Error answers mapped to filesystem codes.

<!-- ds:block id=vercelerrors-d9h25xtq -->
**vercelErrors** · [`examples/adapters-hosted/vercel.go:161-179`](vercel.go#L161-L179)

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

### Cloudinary

Opening a disk for each delivery type.

<!-- ds:block id=opencloudinary-xc9n9csz -->
**openCloudinary** · [`examples/adapters-hosted/cloudinary.go:61-78`](cloudinary.go#L61-L78)

```go
func openCloudinary(secret string, typ cloudinary.Type, fakeURL string) (*filesystem.Disk, error) {
	drv, err := cloudinary.New(cloudinary.Config{
		CloudName:   cloudName,
		APIKey:      cloudAPIKey,
		APISecret:   secret,
		Type:        typ,
		APIURL:      fakeURL + "/v1_1",
		DeliveryURL: fakeURL,
	})
	if err != nil {
		return nil, err
	}
	var opts []filesystem.Option
	if typ == cloudinary.TypeUpload {
		opts = append(opts, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	}
	return filesystem.NewDisk("cloudinary-"+string(typ), drv, opts...)
}
```
<!-- /ds:block hash=7344a0 -->

Raw resources: no content type kept, exact bytes, a rename, a streamed copy and a prefix delete.

<!-- ds:block id=cloudinarybasics-uesjzuv9 -->
**cloudinaryBasics** · [`examples/adapters-hosted/cloudinary.go:84-115`](cloudinary.go#L84-L115)

```go
func cloudinaryBasics(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	w, err := disk.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{})
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "write entry type %q, stat type %q, %d bytes, etag=version %s\n", w.ContentType, e.ContentType, e.Size, e.ETag)
	got, err := disk.ReadAll(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "read back identical: %t\n", bytes.Equal(got, pngHeader))
	if _, err := disk.Move(ctx, "avatars/u1.png", "avatars/user-1.png"); err != nil {
		return err
	}
	if _, err := disk.Copy(ctx, "avatars/user-1.png", "avatars/user-1-copy.png"); err != nil {
		return err
	}
	page, err := disk.ListPage(ctx, "avatars", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after move and copy: %s\n", keysOf(page))
	if err := disk.DeletePrefix(ctx, "avatars"); err != nil {
		return err
	}
	fmt.Fprintf(out, "after DeletePrefix: %d resources; Admin API calls so far: %d\n", len(fake.PublicIDs()), fake.AdminCalls())
	return nil
}
```
<!-- /ds:block hash=6b70c8 -->

How a key maps to a public ID and a delivery URL.

<!-- ds:block id=cloudinarypublicids-bpnyfrv5 -->
**cloudinaryPublicIDs** · [`examples/adapters-hosted/cloudinary.go:121-138`](cloudinary.go#L121-L138)

```go
func cloudinaryPublicIDs(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	const key = "docs/annual report (v2).pdf"
	if _, err := disk.WriteBytes(ctx, key, []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Fprintf(out, "stored as: %s\n", strings.Join(fake.PublicIDs(), ", "))
	page, err := disk.ListPage(ctx, "docs", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "listed as: %s\n", keysOf(page))
	u, err := disk.PublicURL(ctx, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "public URL path: %s\n", strings.TrimPrefix(u, fake.URL))
	return nil
}
```
<!-- /ds:block hash=c66d3a -->

What the driver sees of resources it did not write.

<!-- ds:block id=cloudinaryforeign-6f7n7a4w -->
**cloudinaryForeign** · [`examples/adapters-hosted/cloudinary.go:145-155`](cloudinary.go#L145-L155)

```go
func cloudinaryForeign(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	fake.Seed("raw", "authenticated", "brand/terms.txt", []byte("terms"))
	fake.Seed("raw", "authenticated", "brand/My Logo.txt", []byte("logo"))
	fake.Seed("image", "authenticated", "brand/logo", pngHeader)
	page, err := disk.ListPage(ctx, "brand", filesystem.ListOptions{})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "brand/ holds 3 resources, the driver lists: %s\n", keysOf(page))
	return nil
}
```
<!-- /ds:block hash=ce93f1 -->

Capabilities and refusals.

<!-- ds:block id=cloudinaryrefusals-vcaxnkhw -->
**cloudinaryRefusals** · [`examples/adapters-hosted/cloudinary.go:160-172`](cloudinary.go#L160-L172)

```go
func cloudinaryRefusals(ctx context.Context, out io.Writer, public, private *filesystem.Disk) error {
	fmt.Fprintf(out, "upload type capabilities: %s\n", public.Capabilities())
	fmt.Fprintf(out, "authenticated type capabilities: %s\n", private.Capabilities())
	_, err := private.PublicURL(ctx, "brand/terms.txt")
	fmt.Fprintf(out, "authenticated PublicURL: %s\n", outcome(err))
	_, err = private.SignedURL(ctx, "brand/terms.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: cloudinary.DownloadExpiry})
	fmt.Fprintf(out, "SignedURL: %s\n", outcome(err))
	_, err = public.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	fmt.Fprintf(out, "private write on the upload type: %s\n", outcome(err))
	_, err = private.WriteBytes(ctx, "x.txt", []byte("x"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "IfNotExists: %s\n", outcome(err))
	return nil
}
```
<!-- /ds:block hash=c5809c -->

Error answers mapped to filesystem codes.

<!-- ds:block id=cloudinaryerrors-2xs8hhxw -->
**cloudinaryErrors** · [`examples/adapters-hosted/cloudinary.go:178-192`](cloudinary.go#L178-L192)

```go
func cloudinaryErrors(ctx context.Context, out io.Writer, disk *filesystem.Disk, fake *fakes.Cloudinary) error {
	wrong, err := openCloudinary("not-the-secret", cloudinary.TypeAuthenticated, fake.URL)
	if err != nil {
		return err
	}
	_, err = wrong.WriteBytes(ctx, "a.txt", []byte("a"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "wrong secret: %s\n", outcome(err))
	fake.FailNext(fakes.Failure{Status: statusRateLimited, Code: "Rate Limit Exceeded", RetryAfter: int(retryHint.Seconds())})
	_, err = disk.Stat(ctx, "brand/terms.txt")
	fmt.Fprintf(out, "Admin API limit: %s\n", outcome(err))
	fake.MaxBytes = cloudUploadSize
	_, err = disk.WriteBytes(ctx, "big.bin", make([]byte, cloudUploadSize+1), filesystem.WriteOptions{})
	fmt.Fprintf(out, "over the raw-upload limit: %s\n", outcome(err))
	return nil
}
```
<!-- /ds:block hash=8034fd -->

## What the example cannot show

- **Vercel's CDN cache**: a read of a public blob after an overwrite or a delete can return the old bytes for up to a minute on Vercel; the fake serves the current bytes. Private stores are read with `cache=0`.
- **Real limits**: the fakes enforce a 1 KiB limit to show the error; the real ones are the plan's (Cloudinary raw uploads: 10 MB free, 100 MB paid) and Cloudinary's Admin API budget (500 calls an hour on the free plan).
- **Exact character rules**: which characters Vercel escapes in a pathname's URL and which ones Cloudinary refuses in a public ID; the driver's `~XX` encoding avoids every character Cloudinary documents as refused.

## Files

| File | What it holds |
|---|---|
| `main.go` | `run`, which walks both drivers, and the error printer |
| `vercel.go` | The Vercel Blob steps: token and URLs, a public and a private store, refusals, errors |
| `cloudinary.go` | The Cloudinary steps: both delivery types, public IDs, foreign resources, refusals, errors |
| `internal/fakes` | The in-process Vercel Blob API and blob host, and the Cloudinary Upload and Admin APIs |
| `main_test.go` | Runs the program against fresh fakes and checks each line the guides quote |

## Next

- The guides: [Vercel Blob](../../docs/adapters/vercelblob.md) and [Cloudinary](../../docs/adapters/cloudinary.md); the driver references [adapters/vercelblob/README.md](../../adapters/vercelblob/README.md) and [adapters/cloudinary/README.md](../../adapters/cloudinary/README.md).
- Bucket backends: [S3](../adapters-s3), [GCS](../adapters-gcs), [Azure](../adapters-azure).
- [Capabilities](../concepts-capabilities) and [public and private files](../concepts-visibility) across backends.
