# Cloudinary

The Cloudinary driver stores files as Cloudinary raw resources: exact bytes under a public ID, for applications that already keep their media in Cloudinary and want documents and other files in the same account behind the same `*filesystem.Disk`. It speaks the Upload and Admin REST APIs over `net/http`, with no dependency beyond the library.

Every snippet on this page is cut from [examples/adapters-hosted](../../examples/adapters-hosted), which runs the driver against an in-process fake of both APIs that checks every request's signature, and every transcript is from running it.

## When to choose it over a bucket

Choose this driver when the account already lives in Cloudinary and you want the application's other files (PDFs, exports, the originals of images) in the same place, under the same credentials and the same billing, through the portable Disk API. Choose a bucket ([S3](s3.md), [GCS](gcs.md), [Azure](azure.md) or an [S3-compatible](s3-compatible.md) service) for general file storage: buckets have no hourly limit on listing and stat calls, no 10 MB or 100 MB ceiling on a single write, and they offer expiring signed links, per-object visibility, content types and conditional writes, none of which this driver can give. And if what you want is Cloudinary's image pipeline (resizing, format conversion, face cropping on delivery), that is not what this driver does: it stores raw resources, never transformed. Use Cloudinary's SDK or URL API for the pipeline, or the library's [image processing](../plugins/image-processing.md) and [variants](../plugins/variants.md) with any backend.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/cloudinary`, a module of its own (`go get github.com/ubgo/filesystem/adapters/cloudinary`). Its reference is [adapters/cloudinary/README.md](../../adapters/cloudinary/README.md).
- The cloud name, API key and API secret are on the Cloudinary dashboard. Check Settings, Security for the account's signature algorithm (SHA-1 unless changed; set `Config.Algorithm` to `cloudinary.SHA256` otherwise).
- Raw resources are stored byte for byte and never transformed. For image variants, keep using the library's [image processing](../plugins/image-processing.md) and store the results through this driver, or use Cloudinary's own image pipeline outside it.
- Tested against an in-process fake that checks every signature. What only a real account shows: the exact characters Cloudinary refuses in a public ID, and whether the download endpoint answers ranged reads with 206 (the driver cuts a full answer to the range either way).

## Step 1 — Open a disk

```go
drv, err := cloudinary.New(cloudinary.Config{
	CloudName: "acme",
	APIKey:    os.Getenv("CLOUDINARY_API_KEY"),
	APISecret: os.Getenv("CLOUDINARY_API_SECRET"),
	Type:      cloudinary.TypeAuthenticated,
})
if err != nil {
	return err
}
disk, err := filesystem.NewDisk("cloudinary", drv)
```

`TypeAuthenticated` (the default) keeps resources readable only through signed requests; `TypePrivate` keeps the original signed while transformed versions are public; `TypeUpload` gives them public delivery URLs (`https://res.cloudinary.com/<cloud>/raw/upload/<public id>`), which the driver then returns from `PublicURL`. The secret signs every Upload API request and is the Admin API's Basic password; `Config` prints and logs it redacted (`cloudinary.Config{CloudName: "acme", APIKey: "123456789012345", APISecret: [redacted], Type: ""}`). `New` makes no request, so a wrong secret shows up on the first operation.

The example opens one disk per delivery type; `APIURL` and `DeliveryURL` point at the fake, and against Cloudinary they stay empty. The `upload` type's disk defaults writes to public, since every resource of that type has a public URL:

<!-- ds:block id=opencloudinary-xc9n9csz -->
**openCloudinary** · [`examples/adapters-hosted/cloudinary.go:61-78`](../../examples/adapters-hosted/cloudinary.go#L61-L78)

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

From a DSN, the shape is Cloudinary's own `CLOUDINARY_URL` without the secret, which never goes in a DSN:

```go
mux.Register(cloudinary.Scheme, cloudinary.Opener)
disk, err := mux.Open(ctx, "media", "cloudinary://123456789012345@acme?api_secret_env=CLOUDINARY_API_SECRET&type=upload")
```

`api_secret_env` (required) names the variable holding the secret; `type`, `resource_type` and `algorithm` set the matching `Config` fields. A secret in the URL, a path, an unknown parameter or an empty variable is `CodeInvalidArgument`.

## Step 2 — Write, read, move and delete

A write is one signed upload streaming the body as a multipart form, with `overwrite` and `invalidate`, so the key is replaced and cached copies are purged. Reads go through Cloudinary's signed download endpoint with a five-minute expiry (`DownloadExpiry`), so they are fresh right after an overwrite and work for both delivery types. `Stat` and `List` use the Admin API.

<!-- ds:block id=cloudinarybasics-uesjzuv9 -->
**cloudinaryBasics** · [`examples/adapters-hosted/cloudinary.go:84-115`](../../examples/adapters-hosted/cloudinary.go#L84-L115)

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

```text
write entry type "image/png", stat type "", 29 bytes, etag=version 1
read back identical: true
after move and copy: avatars/user-1-copy.png, avatars/user-1.png
after DeletePrefix: 0 resources; Admin API calls so far: 6
```

- Raw resources keep no content type. The entry a write returns carries the type the Disk sniffed, but `Stat` and `List` report none, so store the type yourself (a [records](../records/README.md) row does) if anything serves the file later.
- The ETag is Cloudinary's version number, which changes on every overwrite.
- `Move` is a signed `rename` that overwrites the destination. `Copy` works, but the Disk streams it (stat, read, write), because Cloudinary has no server-side copy of a resource.
- `DeletePrefix` is an Admin API delete by prefix, repeated while Cloudinary reports it partial; `DeleteKeys` sends 100 public IDs per Admin API request. `Delete` of a missing key succeeds.
- Six Admin API calls went on that short walk, each counted against Cloudinary's hourly limit.

## Step 3 — Understand public IDs and resource types

A key becomes a public ID. Cloudinary refuses `? & # \ % < > +` and other characters in public IDs, so every byte outside `[A-Za-z0-9._-]` is written `~XX` (and `~` itself `~7E`), with `/` kept as the folder separator; listings decode it back. The extension stays in the public ID, as Cloudinary does for raw resources.

<!-- ds:block id=cloudinarypublicids-bpnyfrv5 -->
**cloudinaryPublicIDs** · [`examples/adapters-hosted/cloudinary.go:121-138`](../../examples/adapters-hosted/cloudinary.go#L121-L138)

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

```text
stored as: raw/upload/docs/annual~20report~20~28v2~29.pdf
listed as: docs/annual report (v2).pdf
public URL path: /acme/raw/upload/docs/annual~20report~20~28v2~29.pdf
```

Cloudinary keeps resources apart by resource type (`image`, `video`, `raw`) and delivery type (`upload`, `authenticated`, and others). The driver reads and writes resources of its configured resource type (`raw` by default) and delivery type only, so with the defaults a PNG written through it is a raw resource, not an image Cloudinary can transform. Of resources other tools put in the same folder, it lists the raw ones whose public ID it could have written; the rest are skipped, so a listing never returns a key the disk cannot read back:

<!-- ds:block id=cloudinaryforeign-6f7n7a4w -->
**cloudinaryForeign** · [`examples/adapters-hosted/cloudinary.go:145-155`](../../examples/adapters-hosted/cloudinary.go#L145-L155)

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

```text
brand/ holds 3 resources, the driver lists: brand/terms.txt
```

### Image and video resources

To let Cloudinary transform what the disk stores (thumbnails, format conversion, video streaming), set `Config.ResourceType` to `ResourceImage` or `ResourceVideo`. That trades away the exact-bytes guarantee, so it is opt-in:

```go
drv, err := cloudinary.New(cloudinary.Config{
	CloudName: "acme", APIKey: key, APISecret: secret,
	Type:         cloudinary.TypePrivate,   // originals signed, derived versions public
	ResourceType: cloudinary.ResourceImage, // uploads become images Cloudinary can transform
})
```

- Cloudinary treats a public ID's extension as a delivery format, so a read asks for the format the key's extension names and `PublicURL` repeats it (`photos/a.jpg` is delivered from `.../image/upload/photos/a.jpg.jpg`). The bytes are the uploaded ones only when the extension matches the uploaded format; `a.png` holding a JPEG reads back converted. A key without an extension reads the original.
- Cloudinary refuses an upload it cannot parse as that type, so keep non-media files on a raw disk (a second disk over the same account works).
- `auto` is not offered: Cloudinary would choose the type per upload, and every later call must name the type the resource became.
- The fake checks that every request names the configured type; how Cloudinary converts or refuses real files has not been exercised against an account.

## Step 4 — Capabilities and visibility

`Move`, `DeleteKeys`, `DeletePrefix`, and `PublicURL` for `TypeUpload`. There is no server-side `Copy` (the Disk streams), no signed URLs (Cloudinary's signed delivery URLs do not expire without its paid token-based authentication), no conditional writes, and no content type, per-object visibility, metadata or headers.

<!-- ds:block id=cloudinaryrefusals-vcaxnkhw -->
**cloudinaryRefusals** · [`examples/adapters-hosted/cloudinary.go:160-172`](../../examples/adapters-hosted/cloudinary.go#L160-L172)

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

```text
upload type capabilities: [move,delete-prefix,delete-keys,public-url]
authenticated type capabilities: [move,delete-prefix,delete-keys]
authenticated PublicURL: unsupported
SignedURL: unsupported
private write on the upload type: unsupported
IfNotExists: unsupported
```

Visibility is the delivery type, fixed per disk: the `upload` type is public, the `authenticated` type private. A write that asks for the other visibility is refused rather than stored with the wrong one. An application with both kinds of file opens two disks on the same account, one per type. A private file reaches a browser through your own handler (`fshttp`), since the driver has no expiring link to hand out.

## Step 5 — Handle errors

404 `CodeNotFound`; 401 and 403 (a bad signature or key) `CodePermission`; "File size too large" `CodeTooLarge`; 420 (the Admin API's hourly limit), 429 and 5xx `CodeUnavailable`, with the `Retry-After` Cloudinary sent (`filesystem.RetryAfterOf`); other 400 `CodeInvalidArgument`. A failed request's error never carries the URL's query, where a signed read's API key, signature and expiry are, so logs cannot replay it.

<!-- ds:block id=cloudinaryerrors-2xs8hhxw -->
**cloudinaryErrors** · [`examples/adapters-hosted/cloudinary.go:178-192`](../../examples/adapters-hosted/cloudinary.go#L178-L192)

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

```text
wrong secret: permission
Admin API limit: unavailable (retry after 30s)
over the raw-upload limit: too_large
```

## Try it

```sh
cd examples
go run ./adapters-hosted
```

The part after `== Cloudinary` is the walk-through above; `go test ./adapters-hosted` checks every quoted line.

## Pitfalls

- **The secret signs everything.** It is the Upload API's signing key and the Admin API's password: whoever has it can upload, rename and delete every resource. Read it from the environment, never from source or a client bundle; `Config` redacts it, and the driver sends it only to the API host. The API key alone is not a secret, but the pair is.
- **`upload` means public forever.** A `TypeUpload` resource's delivery URL needs no signature and never expires. Use `TypeAuthenticated` (the default) for anything private.
- **The Admin API is rate-limited per hour.** `Stat`, `List`, `Exists`, `Move`, a streamed `Copy`, `DeleteKeys` and `DeletePrefix` spend it (500 calls an hour on the free plan). An application that stats on every request, or lists to render a page, will hit it; keep file facts in your own database (a [records](../records/README.md) row) and read the bytes with `Read`, which uses the Upload API's download endpoint.
- **Writes have a hard size ceiling.** A body above the plan's raw-upload limit (10 MB on the free plan, 100 MB on paid ones) is `CodeTooLarge`, after it was sent: chunked uploads are not implemented. Set `WriteOptions.MaxSize` or the upload `Policy.MaxSize` below the limit so the request is refused before it starts.
- **Public IDs are not keys.** In the Cloudinary console a key shows up encoded (`docs/annual~20report~20~28v2~29.pdf`). Resources uploaded by other tools with IDs the driver would not write are invisible to `List`, and resources of another resource type than the disk's (`image` or `video` on a raw disk) are never seen at all.
- **No content type comes back.** Serve raw resources with the type you stored elsewhere, or the browser gets a generic one.
- **No create-only write, no compare-and-swap.** Every write overwrites; use a fresh key per upload (`upload.RandomKey`) where two writers could pick the same one.
- **Fresh reads, cached public URLs.** Reads through the disk use the signed download endpoint and are fresh right after an overwrite. Writes and deletes pass `invalidate`, so Cloudinary purges cached delivery copies, but a public URL handed out earlier keeps pointing at the same public ID: after a `Move` it no longer resolves.

## Related

- The example: [examples/adapters-hosted](../../examples/adapters-hosted), and the driver reference [adapters/cloudinary/README.md](../../adapters/cloudinary/README.md).
- [Vercel Blob](vercelblob.md), the other hosted store with no bucket.
- [Image processing](../plugins/image-processing.md) and [variants](../plugins/variants.md), for resized copies on any backend.
- [Capabilities](../concepts/capabilities.md), [public and private files](../concepts/public-and-private.md), [errors](../concepts/errors.md) and [serving files](../concepts/serving-files.md).
- Buckets: [S3](s3.md), [GCS](gcs.md), [Azure](azure.md), [S3-compatible](s3-compatible.md).
