# Google Cloud Storage driver

The GCS driver stores files in a Google Cloud Storage bucket. This program opens a bucket from a Config and from a DSN, writes public and private objects on a fine-grained bucket and flips one with `SetVisibility`, issues V4 signed URLs from a service account key, stores a pre-compressed gzip object and reads back exactly the bytes it stored, and shows that GCS offers no multipart API through this driver. It then walks through the object-store features the fake can model: a storage class per disk, Object Versioning with a restore, a retention date, a content-type override on a signed link, a Cloud KMS key per disk, a customer-supplied key (CSEK), lifecycle and replication settings, and a token scoped to one prefix. It uses the core `filesystem` package and `github.com/ubgo/filesystem/adapters/gcs` (built on `cloud.google.com/go/storage`). The guide [Google Cloud Storage](../../docs/adapters/gcs.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-gcs
go test ./adapters-gcs
```

It runs against an in-process fake GCS ([fake-gcs-server](https://github.com/fsouza/fake-gcs-server)) on a random local port, with a throwaway 2048-bit RSA key standing in for a service account key, so it needs no project, credentials, Docker or network. The fake and everything in it go away when the program exits. The program has no switch to a real bucket: against real GCS the client comes from Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS` or `gcloud auth application-default login`) and the DSN is just `gs://<bucket>`, plus `acl=true` and `signing_key=<path to key file>` when wanted. The driver's own conformance suite runs against a real bucket with `FILESYSTEM_GCS_TEST_DSN`, as the guide describes.

## What it does

```text
capabilities: [copy,set-visibility,presign,presign-post,metadata,content-type,headers,presign-headers,presign-visibility,conditional-write,hold,storage-class,compare-and-swap]
avatars/u1.png: image/png, public
invoices/42.pdf: application/pdf, private
after SetVisibility: private
signed GET: algorithm=GOOG4-RSA-SHA256 disposition=attachment
signed PUT: client must send Content-Type: image/png
gzip object: stored 38 bytes, read 38, identical=true, cache-control="private, max-age=60"
multipart: unsupported
after DeletePrefix: exists=false
matching SHA-256: stored, entry sha256=2ce40867277e...
wrong SHA-256: invalid_argument, stored=false
GCS CRC32C matches local=true, MD5 matches local=true, generation set=true
via DSN: [copy,metadata,content-type,headers,conditional-write,hold,storage-class,compare-and-swap]
storage class: NEARLINE
versions: 2, older reads "draft", after RestoreVersion the live object reads "draft"
retention: mode Unlocked on write, extended to 30 days: true
signed GET override: response-content-type=text/plain
kms: presigned PUT signs the key: true, client sends projects/example-project/locations/eu/keyRings/files/cryptoKeys/media
per call: invoices/42.pdf stored as NEARLINE
per call: thumbs/42.png stored as STANDARD
per call: presigned PUT signs the key true and the class true, client sends projects/example-project/locations/eu/keyRings/tenants/cryptoKeys/acme
per call: unknown class invalid_argument, stored false
csek: read "for your eyes only", key printed as gcs.CustomerKey{[REDACTED]}, presign declared: false
lifecycle: Delete,SetStorageClass,AbortIncompleteMultipartUpload; logs/ move to ARCHIVE after 90 days
replication: ASYNC_TURBO
delegated media/users/42 with inRole:roles/storage.objectViewer,inRole:roles/storage.objectCreator
token prints as: gcs.ScopedToken{AccessToken: [REDACTED], Bucket: "media", Prefix: "users/42"}
```

1. The Config disk declares `set-visibility` (per-object ACLs) and `presign` (a signing key) on top of what every GCS disk has.
2. A PNG written public and a PDF written with no visibility (private by default) read back their visibility from each object's ACL; `SetVisibility` then makes the avatar private.
3. A V4 signed GET forces a download with `response-content-disposition=attachment`; a signed PUT is bound to `image/png`, which the client must send.
4. A gzip object with `Content-Encoding: gzip` reads back as the same 38 compressed bytes, not decompressed by GCS on the way out.
5. `CreateMultipart` is `unsupported` (large bodies stream through `Write` as resumable uploads), and `DeletePrefix` still works by listing and deleting key by key.
6. A write with the right `WriteOptions.SHA256` is stored; one with a wrong digest is refused with `invalid_argument` and leaves nothing at the key. `ObjectInfo` then reads the CRC32C and MD5 GCS computed over the stored bytes, and both match the ones computed locally.
7. The same bucket opened from a DSN with no `acl` and no `signing_key` declares neither of those capabilities.
8. A disk with `StorageClass: NEARLINE` writes into that class, which `Stat` reports (`features.go`, `storageClass`).
9. On a bucket with Object Versioning, the same key is written twice; `Versions` lists both generations newest first, `ReadVersion` reads the older one and `RestoreVersion` makes it the live object again.
10. `RetainFor` gives every write a retention date (mode `Unlocked`), and `SetHold` extends one object's date, which `Hold` reads back.
11. A signed GET whose `PresignOptions.ContentType` is signed as `response-content-type`, so a file stored as bytes opens as text.
12. A disk with `KMSKeyName` names the key on every write, and a presigned PUT signs `x-goog-encryption-kms-key-name`, which the client sends from `SignedRequest.Header`.
13. `requestopts.go` chooses the KMS key and class per call with `gcs.WithRequestOptions`, bound to `invoices/42.pdf`: the invoice is stored `NEARLINE` with the tenant's key, while a thumbnail written with the same context keeps the bucket's `STANDARD`, because the options apply only to the key they are bound to. A presigned PUT for the invoice signs both the key and the class, which the client sends from `SignedRequest.Header`, and an unknown class is `invalid_argument` before anything is stored.
14. A customer-supplied key (`gcs.NewCustomerKey`) goes on every request, prints as a placeholder and switches signed URLs off.
15. Lifecycle rules (`SetLifecycle`, `Lifecycle`) and turbo replication (`SetReplication`, `Replication`) are set and read back, against a small stand-in for the bucket metadata API (fake-gcs-server keeps neither).
16. `DelegatePrefix` exchanges the application's token for one whose access boundary reaches only `users/42/`, through a stand-in for Google's STS; the token prints redacted.

## The code

The driver built from a `*storage.Client` the application made, on a fine-grained bucket with a signing key, so `SetVisibility` and `Presign` are both declared.

<!-- ds:block id=openwithconfig-pquvryxh -->
**openWithConfig** · [`examples/adapters-gcs/main.go:127-138`](main.go#L127-L138)

```go
func openWithConfig(client *storage.Client, privateKey []byte) (*filesystem.Disk, error) {
	drv, err := gcs.New(gcs.Config{
		Client:     client,
		Bucket:     bucketName,
		Visibility: gcs.VisibilityACL,
		Signing:    &storage.SignedURLOptions{GoogleAccessID: serviceAccount, PrivateKey: privateKey},
	})
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk("media", drv)
}
```
<!-- /ds:block hash=a09895 -->

The DSN form. Here it points at the fake with `endpoint` and `no_auth`; against real GCS it is `gs://media`.

<!-- ds:block id=openwithdsn-tfh28zpd -->
**openWithDSN** · [`examples/adapters-gcs/main.go:144-153`](main.go#L144-L153)

```go
func openWithDSN(ctx context.Context, fakeURL string) (*filesystem.Disk, error) {
	mux := filesystem.NewMux()
	if err := mux.Register(gcs.Scheme, gcs.Opener); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set(gcs.ParamEndpoint, fakeURL+"/storage/v1/")
	q.Set(gcs.ParamNoAuth, "true")
	return mux.Open(ctx, "media-dsn", gcs.Scheme+"://"+bucketName+"?"+q.Encode())
}
```
<!-- /ds:block hash=f3a470 -->

Public and private objects, visibility read back from the ACL, and a change of visibility.

<!-- ds:block id=publicandprivate-jtfcpswk -->
**publicAndPrivate** · [`examples/adapters-gcs/main.go:159-182`](main.go#L159-L182)

```go
func publicAndPrivate(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.WriteBytes(ctx, "avatars/u1.png", pngHeader, filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "invoices/42.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	for _, k := range []string{"avatars/u1.png", "invoices/42.pdf"} {
		e, err := disk.Stat(ctx, k)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: %s, %s\n", e.Key, e.ContentType, e.Visibility)
	}
	if err := disk.SetVisibility(ctx, "avatars/u1.png", filesystem.VisibilityPrivate); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after SetVisibility: %s\n", e.Visibility)
	return nil
}
```
<!-- /ds:block hash=9e7c46 -->

V4 signed links: a download forced to an attachment and an upload bound to one content type.

<!-- ds:block id=signedlinks-9xy3dhdu -->
**signedLinks** · [`examples/adapters-gcs/main.go:191-211`](main.go#L191-L211)

```go
func signedLinks(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	get, err := disk.SignedURL(ctx, "invoices/42.pdf", filesystem.PresignOptions{
		Method: filesystem.MethodGet, Expires: linkLifetime, ContentDisposition: "attachment",
	})
	if err != nil {
		return err
	}
	put, err := disk.SignedURL(ctx, "uploads/new.png", filesystem.PresignOptions{
		Method: filesystem.MethodPut, Expires: linkLifetime, ContentType: "image/png",
	})
	if err != nil {
		return err
	}
	u, err := url.Parse(get.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET: algorithm=%s disposition=%s\n", u.Query().Get("X-Goog-Algorithm"), u.Query().Get("response-content-disposition"))
	fmt.Fprintf(out, "signed PUT: client must send Content-Type: %s\n", put.Header.Get("Content-Type"))
	return nil
}
```
<!-- /ds:block hash=84a4e4 -->

A pre-compressed object read back exactly as stored, with its `Cache-Control` kept.

<!-- ds:block id=gzipasstored-ywtqnjxk -->
**gzipAsStored** · [`examples/adapters-gcs/main.go:217-243`](main.go#L217-L243)

```go
func gzipAsStored(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(bytes.Repeat([]byte("log line\n"), 100)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_, err := disk.WriteBytes(ctx, "logs/app.log.gz", buf.Bytes(), filesystem.WriteOptions{
		ContentType: "text/plain",
		Headers:     filesystem.HTTPHeaders{ContentEncoding: "gzip", CacheControl: "private, max-age=60"},
	})
	if err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "logs/app.log.gz")
	if err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "logs/app.log.gz")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "gzip object: stored %d bytes, read %d, identical=%t, cache-control=%q\n", buf.Len(), len(got), bytes.Equal(got, buf.Bytes()), e.Headers.CacheControl)
	return nil
}
```
<!-- /ds:block hash=aa5803 -->

What GCS does not offer through this driver, and the prefix delete that still works.

<!-- ds:block id=nomultipart-mhyeku4p -->
**noMultipart** · [`examples/adapters-gcs/main.go:249-261`](main.go#L249-L261)

```go
func noMultipart(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	_, err := disk.CreateMultipart(ctx, "big.bin", filesystem.WriteOptions{})
	fmt.Fprintf(out, "multipart: %s\n", filesystem.CodeOf(err))
	if err := disk.DeletePrefix(ctx, "avatars"); err != nil {
		return err
	}
	ok, err := disk.Exists(ctx, "avatars/u1.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "after DeletePrefix: exists=%t\n", ok)
	return nil
}
```
<!-- /ds:block hash=7a7d29 -->

The two layers of integrity checking: the Disk's SHA-256 check on the way in, and the CRC32C and MD5 GCS keeps with the object.

<!-- ds:block id=checksums-yggargx8 -->
**checksums** · [`examples/adapters-gcs/main.go:270-295`](main.go#L270-L295)

```go
func checksums(ctx context.Context, out io.Writer, disk *filesystem.Disk, drv *gcs.Driver) error {
	body := []byte("order 42: 3 items")
	sum := sha256.Sum256(body)
	e, err := disk.WriteBytes(ctx, "orders/42.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "matching SHA-256: stored, entry sha256=%s...\n", e.SHA256[:12])

	wrong := sha256.Sum256([]byte("something else"))
	_, err = disk.WriteBytes(ctx, "orders/43.txt", body, filesystem.WriteOptions{SHA256: hex.EncodeToString(wrong[:])})
	stored, existsErr := disk.Exists(ctx, "orders/43.txt")
	if existsErr != nil {
		return existsErr
	}
	fmt.Fprintf(out, "wrong SHA-256: %s, stored=%t\n", filesystem.CodeOf(err), stored)

	info, err := drv.ObjectInfo(ctx, "orders/42.txt")
	if err != nil {
		return err
	}
	md := md5.Sum(body)
	crcOK := info.CRC32C == crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli))
	fmt.Fprintf(out, "GCS CRC32C matches local=%t, MD5 matches local=%t, generation set=%t\n", crcOK, bytes.Equal(info.MD5, md[:]), info.VersionID != "")
	return nil
}
```
<!-- /ds:block hash=372afa -->

## What the example cannot show

fake-gcs-server models objects, versions, storage classes on write and retention dates. These need a real bucket, so they stay in the guide, described from the driver's source and its tests (`adapters/gcs/advanced_test.go` checks the requests the driver sends):

- **Legal (temporary) holds**: the fake accepts `SetHold{Legal: true}` but does not keep the hold, so `Hold` reads it back as false.
- **WORM enforcement**: the fake deletes an object under an unexpired retention date, where GCS refuses a delete or overwrite under a hold or retention (`CodePermission`). The example shows the retention date, not the refusal. A `RetentionLocked` date cannot be shortened on GCS; the fake does not model the lock.
- **`SetStorageClass`**: the fake answers the rewrite without changing the class, so only a class set on write is shown.
- **Encryption itself** (CMEK and CSEK): the fake ignores the KMS key and serves a CSEK object without its key, so the example shows the request settings, not encrypted storage.
- **Lifecycle and replication taking effect**: the stand-in stores the settings; nothing is expired, archived or replicated.
- **A downscoped token being enforced**: the stand-in STS returns a fixed token; that GCS refuses a request outside the prefix needs a real project.
- **Not GCS features**: restore from an archive tier (every GCS class, Archive included, is read directly, so `Restore` is not declared), object tags (GCS has none; use metadata), one KMS key per tenant on a shared disk and bring-your-own bucket through an assumed role (both S3-only; see [examples/adapters-s3](../adapters-s3)).

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the fake GCS, a throwaway signing key, Config and DSN disks, ACLs, signed URLs, gzip, refusals |
| `features.go` | The object-store feature steps: storage class, versions, retention, content-type override, KMS key, CSEK, lifecycle, replication, a scoped token |
| `requestopts.go` | A KMS key and storage class for one call with `gcs.RequestOptions`; code: [`writeInvoice`](ds:block?id=writeinvoice-e3n7q5kw), [`requestOptions`](ds:block?id=requestoptions-f8w2m6tz) |
| `main_test.go` | Runs the program against a fresh fake and checks each line the guide quotes |

## Next

- The guide: [Google Cloud Storage](../../docs/adapters/gcs.md), and the driver reference [adapters/gcs/README.md](../../adapters/gcs/README.md).
- Other backends: [S3](../adapters-s3), [Azure](../adapters-azure), [local](../adapters-local), [memory](../adapters-memory), [SFTP](../adapters-sftp).
- [Public and private files](../concepts-visibility) covers visibility across backends.
