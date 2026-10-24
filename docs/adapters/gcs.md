# Google Cloud Storage

The GCS driver stores files in a Google Cloud Storage bucket. It offers server-side copy, V4 signed URLs, conditional writes on every bucket, stored metadata and headers, and per-object ACLs on buckets that use fine-grained access. Large bodies stream through `Write` as resumable uploads; there is no separate multipart API.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/gcs`, a module of its own built on `cloud.google.com/go/storage`. Its reference is [adapters/gcs/README.md](../../adapters/gcs/README.md).
- The runnable program is [`examples/adapters-gcs`](../../examples/adapters-gcs/). It runs every snippet on this page against an in-process fake GCS ([fake-gcs-server](https://github.com/fsouza/fake-gcs-server)), so `go run ./adapters-gcs` from `examples/` needs no project.
- You need a bucket and an identity with `roles/storage.objectUser` on it (or `storage.objects.create`, `get`, `list`, `delete` and `update`). The driver never creates buckets.

## 1. Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/gcs
```

## 2. Open a disk in code

Build the `*storage.Client` yourself (`storage.NewClient(ctx)` uses Application Default Credentials) and pass it in. This example uses a fine-grained bucket and a signing key, so both `SetVisibility` and `Presign` are declared:

<!-- ds:block id=openwithconfig-pquvryxh -->
**openWithConfig** · [`examples/adapters-gcs/main.go:127-138`](../../examples/adapters-gcs/main.go#L127-L138)

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

Every `Config` field:

| Field | Default | What it does |
|---|---|---|
| `Client` | required | The `*storage.Client`. `New` returns `CodeInvalidArgument` without it. |
| `Bucket` | required | The bucket name. |
| `Visibility` | `VisibilityNone` | `VisibilityNone`: uniform bucket-level access, IAM on the bucket decides who reads, `SetVisibility` is not declared. `VisibilityACL`: fine-grained access, objects get the `publicRead` or `private` predefined ACL. Any other value is `CodeInvalidArgument`. |
| `PrivateACL` | `ACLPrivate` | The predefined ACL `VisibilityACL` gives private objects: a write, a private presigned PUT or form (and one that asks no visibility, so a cross-project upload still grants the bucket's owner), and the base ACL of a copy. `ACLProjectPrivate`, `ACLBucketOwnerRead` or `ACLBucketOwnerFullControl`; public ACLs, unknown values and a `PrivateACL` without `VisibilityACL` are `CodeInvalidArgument`. |
| `Signing` | nil | `*storage.SignedURLOptions` with `GoogleAccessID` and at most one of `PrivateKey` or `SignBytes`. With neither, URLs are signed keylessly through the IAM Credentials API as `GoogleAccessID` (see [credentials](#4-credentials)). Nil means no `Presign` capability. No `GoogleAccessID`, or both a key and a function, is `CodeInvalidArgument`. The driver sets method, expiry and the other per-URL fields itself. |
| `KMSKeyName` | empty | The Cloud KMS key, `projects/P/locations/L/keyRings/R/cryptoKeys/K`, this disk's objects are encrypted with (CMEK). |
| `KMSKeyFor` | nil | `func(key string) string` picking the CMEK per object (per tenant prefix) on a disk shared by many tenants; `""` falls back to `KMSKeyName`. Exclusive with `CustomerKey`. See "One key per tenant" below. |
| `Clock` | `time.Now` | The time the driver dates from: `RetainFor` retention dates and a form upload's signing date and expiry. Signed URLs are signed by the SDK at the wall clock whatever it says. |
| `IAM` | nil | An `*iamcredentials.Service` keyless signing calls `signBlob` through, with the context of each `SignedURL` call; the opener sets it for `signing_account`. Without it the SDK makes the call itself, with no deadline of yours. |
| `ChunkSize` | 0 (`DefaultChunkSize`, 16 MiB) | Resumable-upload chunk size, and the size up to which a body is read into memory before uploading (step 7). Negative is `CodeInvalidArgument`. |

Check: `disk.Capabilities()` prints `[copy,set-visibility,presign,presign-post,metadata,content-type,headers,presign-headers,presign-visibility,conditional-write,hold,storage-class,compare-and-swap]`. Without `Visibility` and `Signing` it is `[copy,metadata,content-type,headers,conditional-write,hold,storage-class,compare-and-swap]`.

## 3. Open a disk from a DSN

The DSN form is `gs://<bucket>?<parameters>`:

```text
gs://my-bucket
gs://my-bucket?acl=true&signing_key=/run/secrets/gcs-signer.json
```

Every parameter the opener understands, read from `adapters/gcs/opener.go`:

| Parameter | Value | Default | Sets |
|---|---|---|---|
| `endpoint` | URL | Google's API | the client's endpoint, for an emulator such as fake-gcs-server (the SDK also honours `STORAGE_EMULATOR_HOST`) |
| `acl` | boolean | `false` | `Visibility: VisibilityACL` |
| `signing_key` | path of a service account JSON key file | none | `Signing`, from the file's `client_email` and `private_key` |
| `no_auth` | boolean | `false` | a client that sends no credentials, for emulators |
| `kms_key` | a Cloud KMS key name | none | `KMSKeyName`: this disk's customer-managed encryption key (CMEK) |
| `signing_account` | a service account email, or `default` | none | keyless `Signing` as that account, through IAM `signBlob`; `default` is the account the process runs as, read from the metadata server when the disk opens. Exclusive with `signing_key` |
| `credentials_file` | path of a service account JSON key | Application Default Credentials | the account the disk's client (and its `signBlob` calls) authenticates as; only service account keys. Exclusive with `credentials_env` and `no_auth` |
| `credentials_env` | an environment variable name | Application Default Credentials | the same, from a service account JSON key held in that variable, so the key never sits in a file or the DSN |

Plus the Mux's own `prefix`, `public_url`, `visibility` and `readonly` (see [configuration](../concepts/configuration.md)).

The program opens the same bucket on the fake with `endpoint` and `no_auth`:

<!-- ds:block id=openwithdsn-tfh28zpd -->
**openWithDSN** · [`examples/adapters-gcs/main.go:144-153`](../../examples/adapters-gcs/main.go#L144-L153)

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

Validation, all `CodeInvalidArgument` at open time: no bucket (`gs://?no_auth=true`), an unknown parameter, a boolean that does not parse, a `signing_key` file that cannot be read or lacks `client_email` or `private_key`, `signing_key` together with `signing_account`, `signing_account=default` where there is no metadata server (or it reports no account), and Application Default Credentials that the SDK cannot use. A metadata server that answers with an error is `CodeUnavailable`. The key file's path, not its content, is in the DSN, so the DSN can be logged.

### Customer-managed encryption keys per disk

GCS encrypts every object at rest, with Google-managed keys unless the bucket has a default Cloud KMS key. A disk can choose its own key (CMEK):

```text
gs://tenant-a?kms_key=projects/acme/locations/europe-west1/keyRings/files/cryptoKeys/tenant-a
```

- The key is named on every request that creates data: `Write` (the `kmsKeyName` of the upload), `Copy` (the rewrite's `destinationKmsKeyName`), and the presigned PUT, which signs `x-goog-encryption-kms-key-name` so the client must send it (it is in `SignedRequest.Header`).
- The project's Cloud Storage service agent needs `roles/cloudkms.cryptoKeyEncrypterDecrypter` on the key, and the key must be in the bucket's location; otherwise GCS refuses the write.
- A key name is not a secret, so it is safe in a DSN. Customer-supplied keys (CSEK) are the other option, `Config.CustomerKey` (see "Holds, retention, storage classes and versions" in step 7); the two are exclusive.
- The tests check that the key is sent on each path (and signed into the URL) against fake-gcs-server; that GCS encrypts with it needs a real bucket and key.

The example opens a disk with a key and signs a direct upload; the key is in the signed headers and in `SignedRequest.Header`:

<!-- ds:block id=kmskey-ydr9m4vx -->
**kmsKey** · [`examples/adapters-gcs/features.go:172-191`](../../examples/adapters-gcs/features.go#L172-L191)

```go
func kmsKey(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "cmek", false, gcs.Config{KMSKeyName: kmsKeyName, Signing: f.signing})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	put, err := disk.SignedURL(ctx, "direct.bin", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: linkLifetime})
	if err != nil {
		return err
	}
	u, err := url.Parse(put.URL)
	if err != nil {
		return err
	}
	signed := strings.Contains(u.Query().Get("X-Goog-SignedHeaders"), "x-goog-encryption-kms-key-name")
	fmt.Fprintf(out, "kms: presigned PUT signs the key: %t, client sends %s\n", signed, put.Header.Get("X-Goog-Encryption-Kms-Key-Name"))
	return nil
}
```
<!-- /ds:block hash=65316a -->

Check: `kms: presigned PUT signs the key: true, client sends projects/example-project/locations/eu/keyRings/files/cryptoKeys/media`.

### One key per tenant on a shared disk

One disk (one client, one bucket) can serve many tenants that each bring their own CMEK. `Config.KMSKeyFor` gets the key as the driver sees it, under a scoped disk's prefix, and returns that tenant's key name, or `""` for `KMSKeyName`:

```go
drv, err := gcs.New(gcs.Config{
	Client: client, Bucket: "shared-files",
	KMSKeyFor: func(key string) string {
		tenant, _, _ := strings.Cut(strings.TrimPrefix(key, "tenants/"), "/")
		return tenantKeys[tenant] // "" outside every tenant: KMSKeyName, or the bucket default
	},
})
```

It is called on every write, copy, storage-class rewrite, version restore, presigned PUT and form upload (with the form's key, `prefix/${filename}`), so decide by prefix and keep it fast and free of I/O. Reads need no key: GCS decrypts with the key the object was written with. Bring-your-own bucket through an assumed role remains an S3 feature.

### A key or class per call

When the key is not a function of the object's name (a tenant's key read from the database, one customer moved to its own key), choose it per call with `gcs.RequestOptions`, as the s3 adapter's `SSEKMSKeyID` does. The options ride on the context and apply only to the one key they are bound to, so a context reused for a thumbnail or another tenant's file never inherits them:

```go
ctx, err := gcs.WithRequestOptions(ctx, disk, "invoices/42.pdf", gcs.RequestOptions{
	KMSKeyName:   tenant.KMSKey, // overrides KMSKeyFor and KMSKeyName for this key only
	StorageClass: gcs.ClassNearline,
})
if err != nil {
	return err
}
_, err = disk.WriteBytes(ctx, "invoices/42.pdf", pdf, filesystem.WriteOptions{ContentType: "application/pdf"})
```

They apply to a write, a copy's destination, a storage-class rewrite, a version restore, a presigned PUT (both are signed, so the client must send them from `SignedRequest.Header`) and a form for that key (for a prefix form, bind them to `prefix/${filename}`). A KMS key on a `CustomerKey` disk or an unknown class is `invalid_argument` before anything is written or signed.

**Retention, a hold or Custom-Time at write.** `RetainUntil`, `TemporaryHold` and `CustomTime` are set in the same request that creates the object (a write, or a copy's destination), so a record is never stored unprotected, as it is between a write and a later `Disk.SetHold`. `RetainUntil` uses `Config.RetentionMode` and only lengthens `Config.RetainFor` (the later date wins); a date not after now is `invalid_argument`. `CustomTime` is the date bucket lifecycle rules count `daysSinceCustomTime` from, such as an invoice's due date; GCS never lets it move back. GCS cannot pin any of the three in a signature, so a presigned PUT or form asked for one is `unsupported` rather than an upload that lands unprotected:

```go
ctx, err := gcs.WithRequestOptions(ctx, disk, "invoices/42.pdf", gcs.RequestOptions{
	RetainUntil:   time.Now().AddDate(7, 0, 0), // bucket needs object retention enabled
	TemporaryHold: true,                        // released later with Disk.SetHold
	CustomTime:    invoice.DueDate,             // lifecycle: delete 30 days after due
})
```

Try it: [`examples/adapters-gcs`](../../examples/adapters-gcs) runs this section in `requestopts.go` ([`writeInvoice`](ds:block?id=writeinvoice-e3n7q5kw)): `go run ./adapters-gcs` from `examples/`, then look for `per call: invoices/42.pdf stored as NEARLINE`.


## 4. Credentials

The opener's client uses Application Default Credentials, in the SDK's order:

1. `GOOGLE_APPLICATION_CREDENTIALS`, the path of a service account key (or a workload identity federation configuration).
2. The user credentials written by `gcloud auth application-default login`, for development.
3. The attached service account on GCE, GKE (Workload Identity), Cloud Run and Cloud Functions, through the metadata server.

Application Default Credentials are one account per process. When two disks must act as two accounts (a tenant's bucket reached through that tenant's service account, say), give a disk its own service account key with `credentials_file` or `credentials_env`:

```text
gs://tenant-a-files?credentials_env=TENANT_A_SA_KEY
gs://tenant-b-files?credentials_file=/run/secrets/tenant-b-sa.json
```

Only service account keys are accepted (`"type": "service_account"`), since a key file of another credential type can make the client fetch and run whatever the file names; `credentials_env` keeps the key out of the filesystem and the DSN alike. The same credentials sign `signing_account` URLs through IAM.

Signing URLs is separate from authenticating requests: a V4 signature needs a private key, a signing function, or Google to sign for you. On a laptop or with a key file, `signing_key` (or `Signing.PrivateKey`) is enough. On Cloud Run or GKE there is usually no key file, so sign keylessly:

```text
gs://my-bucket?signing_account=default
gs://my-bucket?signing_account=files-signer@my-project.iam.gserviceaccount.com
```

`signing_account` makes each `SignedURL` call the IAM Credentials API (`signBlob`) through the storage client, authenticated with Application Default Credentials; Google signs with the named account's own key, which never leaves Google. In code, the same is a `Signing` with `GoogleAccessID` and no key or function. What it needs:

- The identity the process runs as needs `roles/iam.serviceAccountTokenCreator` (the `iam.serviceAccounts.signBlob` permission) on the signing account, even when it is the same account. Without it, `SignedURL` is `CodePermission`.
- The IAM Credentials API (`iamcredentials.googleapis.com`) must be enabled in the project.
- `default` reads the account from the metadata server once, when the disk opens, so a disk that cannot sign fails at start-up rather than on the first link. On GKE, the pod's Kubernetes service account must be bound to a Google service account through Workload Identity; a pod using a Kubernetes identity directly has no service account email to sign as, so name one.
- Each signed URL costs one network round trip and counts against the IAM Credentials quota; cache links you hand out repeatedly.
- The opener calls IAM itself, with the context of the `SignedURL` call, so a request's deadline or cancellation stops a slow `signBlob`, and the URL is always signed by Google as the account you name, even when Application Default Credentials are a key file for another account (that account then needs the token-creator role on the one you name). In code, set `Config.IAM` (an `iamcredentials.Service` built with your credentials) next to a keyless `Signing` for the same behaviour; without it the SDK calls IAM with no deadline of yours and, given a key-file client, signs with that key, which is only valid when the account you name is the key's own.

The repository's tests cover this path against a fake IAM endpoint and a fake metadata server: the URL carries the signature the fake returned over the V4 string to sign, and it verifies against the signing key. What they cannot show without a real Google Cloud project is that Google's IAM accepts the call from a real runtime identity, that the metadata server of Cloud Run or GKE answers as the fake does, and that GCS accepts the resulting URL; on a real deployment, open the disk with `signing_account` and fetch one signed URL once to confirm the roles.

## 5. Visibility: uniform access or ACLs

**Uniform bucket-level access (the default).** The GCS default and Google's recommendation. Who reads is decided by IAM on the bucket; the driver sends no ACL and does not declare `SetVisibility`. Writes use the disk's default visibility, and a write asking for another is `CodeUnsupported`. To serve public files, grant `allUsers` the `roles/storage.objectViewer` role on a separate public bucket and open it with `visibility=public&public_url=https://storage.googleapis.com/<bucket>` (or your CDN). See [public and private files](../concepts/public-and-private.md).

**Fine-grained access (`acl=true`).** Each write sets the `publicRead` or `private` predefined ACL, `SetVisibility` adds or removes the `allUsers` reader entry, and `Stat` reports `Visibility` from the object's ACL. A copy gets the bucket's default ACL, so in this mode `Copy` reads the source's visibility and applies it to the copy (two extra requests). On a bucket with uniform access, GCS rejects ACL requests, so leave `acl` off there.

**Another private ACL.** `private` grants the object's owner alone. When the disk writes into a bucket another project owns (a partner's inbox, a central archive), that owner cannot manage what it receives; set `Config.PrivateACL` to another non-public predefined ACL (a `gcs.PredefinedACL`, one of `gcs.PrivateACLValues`) and every private write, private direct upload and copy uses it, while public objects keep `publicRead`:

```go
drv, err := gcs.New(gcs.Config{
	Client:     client,
	Bucket:     "partner-inbox",
	Visibility: gcs.VisibilityACL,
	PrivateACL: gcs.ACLBucketOwnerFullControl, // signed as bucket-owner-full-control on a presigned PUT or form
})
```

`SetVisibility` to private still only removes the public grant and leaves the rest of the object's ACL as it is.

**Visibility on direct uploads.** A presigned PUT or form sends no ACL unless asked (or unless `Config.PrivateACL` is set, which it then pins), so it gets the bucket's default object ACL. In ACL mode with a signing account the disk declares `PresignVisibility`: `PresignOptions.Visibility` is signed as `x-goog-acl` (`public-read`, or `private` / `Config.PrivateACL`'s XML name such as `bucket-owner-full-control`), which the client sends from `SignedRequest.Header` and cannot change, and `PostPolicy.Visibility` pins an `acl` form field. Under uniform access a visibility is `unsupported` rather than an upload GCS rejects. For browser tickets, `upload.Policy.AllowPublic` decides whether a client may ask for public ([Uploads](../concepts/uploads.md#public-direct-uploads)).

<!-- ds:block id=publicandprivate-jtfcpswk -->
**publicAndPrivate** · [`examples/adapters-gcs/main.go:159-182`](../../examples/adapters-gcs/main.go#L159-L182)

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

Check: `avatars/u1.png: image/png, public`, `invoices/42.pdf: application/pdf, private`, then `after SetVisibility: private`.

## 6. Signed URLs

`SignedURL` issues V4 signed URLs (`X-Goog-Algorithm=GOOG4-RSA-SHA256`), computed locally when `Signing.PrivateKey` is set. The lifetime is required and at most 7 days.

<!-- ds:block id=signedlinks-9xy3dhdu -->
**signedLinks** · [`examples/adapters-gcs/main.go:191-211`](../../examples/adapters-gcs/main.go#L191-L211)

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

Check: `signed GET: algorithm=GOOG4-RSA-SHA256 disposition=attachment` and `signed PUT: client must send Content-Type: image/png`.

- A GET can override the response's content type and disposition (`response-content-type`, `response-content-disposition`). For active content (HTML, SVG, XML, script) the Disk forces `attachment` when you pass the content type.
- A PUT signs the content type: the client must send exactly the headers in `SignedRequest.Header`, or GCS refuses the upload.
- A PUT can also pin the headers stored with the object: pass `PresignOptions.Headers` (Cache-Control, Content-Disposition, Content-Encoding) and the URL signs them (they appear in `X-Goog-SignedHeaders`), `SignedRequest.Header` returns them, GCS refuses an upload without them and stores them with the object. That is the `PresignHeaders` capability, declared whenever `Signing` is set. The repository's tests check that the headers are signed and returned; fake-gcs-server does not verify signatures, so that GCS refuses an upload without them and stores them is not covered by any test in the repository: check it once against a real bucket.
- Browser uploads from another origin need a CORS configuration on the bucket (`gcloud storage buckets update --cors-file`), outside this library.
- The fake GCS server does not check signatures, so the program checks the URL's shape only; the signing itself is the SDK's.

A download link that overrides the stored content type, so a file stored as bytes opens as text in the browser:

<!-- ds:block id=responsecontenttype-y8jngg64 -->
**responseContentType** · [`examples/adapters-gcs/features.go:152-167`](../../examples/adapters-gcs/features.go#L152-L167)

```go
func responseContentType(ctx context.Context, out io.Writer, f fixture) error {
	req, err := f.disk.SignedURL(ctx, "export.dat", filesystem.PresignOptions{
		Method:      filesystem.MethodGet,
		Expires:     linkLifetime,
		ContentType: "text/plain", // signed as response-content-type
	})
	if err != nil {
		return err
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed GET override: response-content-type=%s\n", u.Query().Get("response-content-type"))
	return nil
}
```
<!-- /ds:block hash=36e2ef -->

Check: `signed GET override: response-content-type=text/plain`.


### Presigned POST forms

A presigned POST is a form whose policy GCS enforces itself: the size range, the key (exact, or any key under a prefix), the Content-Type, the stored headers and the expiry. It needs the same signing setup as signed URLs (a key, `SignBytes`, or keyless signing with `Config.IAM`, which the opener sets).

```go
post, err := disk.PresignPost(ctx, filesystem.PostPolicy{KeyPrefix: "uploads/42", ContentType: "image/png", MaxSize: 2 << 20, Expires: 10 * time.Minute})
// The browser posts post.Fields, then the file in a "file" field, last, to post.URL.
```

- The form posts to `https://storage.googleapis.com/<bucket>/` (`gcs.PostHost`, GCS's XML API). The driver builds the V4 policy itself rather than with the SDK's generator, because the SDK always pins one exact key and cannot express a prefix (`["starts-with", "$key", "prefix/"]`); the policy also pins `content-length-range`, the bucket, the Content-Type and stored headers when set, and the disk's KMS key.
- It is signed with RSA-SHA256 over the base64 policy, by the disk's key, `SignBytes`, or Google through `signBlob` (keyless). A keyless `Signing` without `Config.IAM` cannot sign a policy built here, so such a disk does not declare `PresignPost`.
- With `KeyPrefix`, the key field is `prefix/${filename}`, which GCS replaces with the uploaded file's name; the client may also set any key under the prefix.
- The tests verify the policy's conditions and the signature against the signing key, offline; fake-gcs-server takes no POST forms, so that GCS refuses an upload outside the policy needs a real bucket. The browser needs CORS for `POST` ([CORS](#12-cors-for-browser-uploads)), and the upload is still the client's: run `upload.Confirm`.

## 7. Writes, resumable uploads and gzip objects

`Write` reads up to one chunk (`ChunkSize`, 16 MiB by default) before sending anything:

- A body that fits is uploaded only after it was read completely, so a body that fails half way never reaches GCS.
- A larger body streams as a resumable upload in chunks of `ChunkSize`. GCS commits it only when the final chunk is sent; if the body fails, the driver cancels the upload and nothing is stored (`TestLargeFailingBodyIsNotCommitted`).
- Memory per concurrent write is up to one chunk. Lower `ChunkSize` (GCS's minimum is 256 KiB) if many large uploads run at once.

There is no multipart API: GCS's resumable uploads are driven by the SDK inside `Write`, and the Go client does not expose GCS's XML multipart API. `CreateMultipart` is `CodeUnsupported`, so `upload.Uppy` cannot be used with GCS; single-request presigned uploads (`upload.Presign` and `upload.Confirm`) work.

Stored headers and gzip. GCS stores `Cache-Control`, `Content-Disposition` and `Content-Encoding` with the object. For an object stored with `Content-Encoding: gzip`, GCS normally decompresses it on the way out ("decompressive transcoding"), which would change its bytes and size. The driver detects that and reopens the read with `ReadCompressed`, so you get the bytes exactly as stored; only such objects pay the second request.

<!-- ds:block id=gzipasstored-ywtqnjxk -->
**gzipAsStored** · [`examples/adapters-gcs/main.go:217-243`](../../examples/adapters-gcs/main.go#L217-L243)

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

Check: `gzip object: stored 38 bytes, read 38, identical=true, cache-control="private, max-age=60"`.

A client built with `storage.WithJSONReads()` cannot read compressed objects as stored; with such a client, reading a gzip object fails with an error instead of returning different bytes. The default client (XML reads) has no such limit.

### Checksums

Two layers check that the bytes stored are the bytes sent, and they answer different questions.

| Layer | Algorithm | Who checks | When to use it |
|---|---|---|---|
| The Disk | SHA-256 (`WriteOptions.SHA256`) | The library, while streaming, on every backend | The client or an upstream step declared a digest (a direct upload's confirm, an import manifest, a copy from another backend). |
| GCS | CRC32C on every upload, MD5 on non-composite objects | The Go SDK sends a CRC32C and GCS refuses a body that does not match; full reads are checked against the stored CRC32C | Always on; nothing to configure. |
| `Driver.ObjectInfo` | Reads the stored CRC32C and MD5 | Your code | Compare a stored object against a checksum you already hold without downloading it: a scrub job, a migration check, a backup audit. |

GCS does not verify SHA-256 on its side, so the driver does not declare `ChecksumSHA256`; the Disk still hashes every body and refuses a mismatch with `CodeInvalidArgument` before the object is committed, so nothing is ever stored at the key. The returned `Entry.SHA256` is the digest the Disk computed, which a records row keeps for deduplication.

The program [examples/adapters-gcs](../../examples/adapters-gcs) shows all three: a write with the right digest, one with a wrong digest, and `ObjectInfo` compared against digests computed locally.

<!-- ds:block id=checksums-yggargx8 -->
**checksums** · [`examples/adapters-gcs/main.go:270-295`](../../examples/adapters-gcs/main.go#L270-L295)

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

Check: `matching SHA-256: stored`, `wrong SHA-256: invalid_argument, stored=false` and `GCS CRC32C matches local=true, MD5 matches local=true`.

- `ObjectInfo` is a driver method, past the Disk: keep the `*gcs.Driver` you built the disk from. It cleans the key like the Disk does but does not add a scoped disk's prefix, so pass `disk.DriverKey(key)` when the disk is scoped.
- Composite objects (made by GCS's compose call, which this driver does not use, or by `gsutil` parallel composite uploads) have no MD5: `info.MD5` is empty, compare the CRC32C instead.
- A CRC32C is an error check, not a security check: two different bodies can share one. Use SHA-256 (the Disk's `Entry.SHA256`, or a records row's `sha256` column) where an attacker could pick the bytes.
- `WriteOptions.SHA256` is not accepted on multipart uploads (`CodeUnsupported`), and GCS has none through this driver anyway.

### Holds, retention, storage classes and versions

The disk reads and sets protection through `Disk.Hold` and `Disk.SetHold`: `Legal` is the object's temporary hold (any bucket), `RetainUntil` its retention date (a bucket with object retention enabled). `Config.RetainFor` gives every object the disk creates (a write, a copy, a restored version, a storage-class rewrite of an object without one) a retention date, in `RetentionUnlocked` (can be shortened by someone with the override permission) or `RetentionLocked` (WORM, only extended). While either holds, a delete or overwrite is `CodePermission`. Signed URLs and form uploads cannot pin a retention, so a disk with `RetainFor` declares neither `Presign` nor `PresignPost`. `Disk.SetStorageClass` rewrites only while both the generation and the metageneration it read are current, so neither a new version nor a metadata change made meanwhile is lost (`CodeConflict`).

```go
drv, err := gcs.New(gcs.Config{Client: client, Bucket: "invoices", StorageClass: gcs.ClassNearline, RetainFor: 7 * 365 * 24 * time.Hour, Versioned: true})
disk, err := filesystem.NewDisk("invoices", drv)
err = disk.SetHold(ctx, "2026/0042.pdf", filesystem.Hold{Legal: true})       // litigation hold
err = disk.SetStorageClass(ctx, "2025/0001.pdf", string(gcs.ClassArchive))  // rewrite in place
versions, err := disk.Versions(ctx, "2026/0042.pdf")                          // newest first
err = disk.RestoreVersion(ctx, "2026/0042.pdf", versions[1].ID)            // bring back the previous one
info, err := drv.ObjectInfo(ctx, "2026/0042.pdf")                             // CRC32C, MD5, generation
```

The example runs the parts fake-gcs-server models. A disk whose writes go to a colder class:

<!-- ds:block id=storageclass-pj8rpapy -->
**storageClass** · [`examples/adapters-gcs/features.go:69-83`](../../examples/adapters-gcs/features.go#L69-L83)

```go
func storageClass(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "cold-store", false, gcs.Config{StorageClass: gcs.ClassNearline})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "logs/2026-09.log", []byte("september"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	e, err := disk.Stat(ctx, "logs/2026-09.log")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "storage class: %s\n", e.StorageClass)
	return nil
}
```
<!-- /ds:block hash=b8e960 -->

Check: `storage class: NEARLINE`.

Versions on a bucket with Object Versioning, including bringing an older generation back:

<!-- ds:block id=bucketversions-qwpnag3j -->
**bucketVersions** · [`examples/adapters-gcs/features.go:88-120`](../../examples/adapters-gcs/features.go#L88-L120)

```go
func bucketVersions(ctx context.Context, out io.Writer, f fixture) error {
	_, disk, err := newBucketDisk(f, "history", true, gcs.Config{Versioned: true})
	if err != nil {
		return err
	}
	for _, body := range []string{"draft", "final"} {
		if _, err := disk.WriteBytes(ctx, "contract.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	vs, err := disk.Versions(ctx, "contract.txt") // newest first; an ID is a generation
	if err != nil {
		return err
	}
	rc, err := disk.ReadVersion(ctx, "contract.txt", vs[1].ID)
	if err != nil {
		return err
	}
	old, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return err
	}
	if err := disk.RestoreVersion(ctx, "contract.txt", vs[1].ID); err != nil {
		return err
	}
	live, err := disk.ReadAll(ctx, "contract.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "versions: %d, older reads %q, after RestoreVersion the live object reads %q\n", len(vs), old, live)
	return nil
}
```
<!-- /ds:block hash=3e78d4 -->

Check: `versions: 2, older reads "draft", after RestoreVersion the live object reads "draft"`.

A retention date on every write, then extended for one object:

<!-- ds:block id=retention-b9yuyv4w -->
**retention** · [`examples/adapters-gcs/features.go:125-147`](../../examples/adapters-gcs/features.go#L125-L147)

```go
func retention(ctx context.Context, out io.Writer, f fixture) error {
	drv, disk, err := newBucketDisk(f, "records", false, gcs.Config{RetainFor: time.Hour})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "contracts/acme.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	info, err := drv.ObjectInfo(ctx, "contracts/acme.pdf")
	if err != nil {
		return err
	}
	until := time.Now().AddDate(0, 0, 30).UTC().Truncate(time.Second)
	if err := disk.SetHold(ctx, "contracts/acme.pdf", filesystem.Hold{RetainUntil: until}); err != nil {
		return err
	}
	h, err := disk.Hold(ctx, "contracts/acme.pdf")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "retention: mode %s on write, extended to 30 days: %t\n", info.RetentionMode, h.RetainUntil.Equal(until))
	return nil
}
```
<!-- /ds:block hash=74b209 -->

Check: `retention: mode Unlocked on write, extended to 30 days: true`.

A customer-supplied key, and what it switches off:

<!-- ds:block id=customerkey-7w6wh6d9 -->
**customerKey** · [`examples/adapters-gcs/features.go:196-215`](../../examples/adapters-gcs/features.go#L196-L215)

```go
func customerKey(ctx context.Context, out io.Writer, f fixture) error {
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 bytes from the secret manager, never from code
	key, err := gcs.NewCustomerKey(raw)
	if err != nil {
		return err
	}
	_, disk, err := newBucketDisk(f, "sealed", false, gcs.Config{CustomerKey: key, Signing: f.signing})
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "secret.txt", []byte("for your eyes only"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	got, err := disk.ReadAll(ctx, "secret.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "csek: read %q, key printed as %v, presign declared: %t\n", got, key, disk.Capabilities().Presign)
	return nil
}
```
<!-- /ds:block hash=9985da -->

Check: `csek: read "for your eyes only", key printed as gcs.CustomerKey{[REDACTED]}, presign declared: false`.

Not in the example, because fake-gcs-server does not model them: the legal (temporary) hold, which it does not keep; a delete or overwrite refused while a hold or retention applies, which it allows; `SetStorageClass`, whose rewrite it answers without changing the class; and CSEK decryption, since it serves the object without the key. The driver's tests check the requests for each against the fake (`adapters/gcs/advanced_test.go`); the enforcement needs a real bucket.

GCS has no object tags (use metadata) and no restore step (every class, Archive included, is read directly), so `Tags` and `Restore` are not declared. Compare-and-swap (`WriteOptions.IfMatch`) is a generation precondition: the ETag is checked against the current object, and the write is conditioned on that generation, so a writer that got in between makes it `CodeConflict`. `Config.CustomerKey` (`gcs.NewCustomerKey`, 32 bytes) encrypts every object with a customer-supplied key; GCS keeps only its hash, the key never prints, and signed URLs are switched off because they would need the key on the client. Holds, retention enforcement and CSEK decryption are only checked against a real bucket; the fake-gcs-server tests check the requests the driver sends.

## 8. Listing, deleting, copying

- `List` uses the bucket's object iterator with GCS's page token as the cursor, and sorts each page by key (GCS returns objects before prefixes).
- Keys ending in `/` (directory markers created by the console or other tools) are not listed.
- `Delete` of a missing object succeeds.
- `Copy` is server-side (`CopierFrom`) and keeps metadata and headers. `Move` is Copy then Delete, not atomic.
- No bulk delete: `DeletePrefix` and `DeleteKeys` are Disk fallbacks, one `Delete` per key.

<!-- ds:block id=nomultipart-mhyeku4p -->
**noMultipart** · [`examples/adapters-gcs/main.go:249-261`](../../examples/adapters-gcs/main.go#L249-L261)

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

Check: `multipart: unsupported` and `after DeletePrefix: exists=false`.

## 9. Conditional writes and errors

`IfNotExists` is always available: the driver writes with the `DoesNotExist` precondition, and GCS answers 412 when the object exists, which is `CodeExists`.

| GCS error | Code |
|---|---|
| `storage.ErrObjectNotExist`, `storage.ErrBucketNotExist`, 404 | `CodeNotFound` |
| 412 | `CodeExists` |
| 401, 403 | `CodePermission` |
| 429, 5xx | `CodeUnavailable`, carrying the response's `Retry-After` when it sent one, so `layer.Retry` waits at least that long and `fshttp` passes it on |
| anything else | `CodeUnknown` |

The SDK retries idempotent requests on 429 and 5xx by itself. If you also add `layer.Retry`, both retry; the driver's tests turn the SDK's retries off (`client.SetRetry(storage.WithPolicy(storage.RetryNever))`) to see each failure once, and you may do the same when `layer.Retry` should own the policy. See [errors](../concepts/errors.md).

## 10. The native client

`filesystem.As[*storage.Client](disk)` returns the client for bucket settings, object holds, retention and anything else the Disk does not cover.

## 11. Test locally

`task test` runs the driver's tests and the conformance suite (in both visibility modes) against fake-gcs-server in process; no Docker is needed. To run the suite against a real bucket, set `FILESYSTEM_GCS_TEST_DSN` (for example `gs://scratch-bucket`) with Application Default Credentials available, then:

```sh
cd adapters/gcs
FILESYSTEM_GCS_TEST_DSN=gs://scratch-bucket go test -count=1 -run TestConformanceAgainstARealBucket -v ./...
```

Each case runs under a random prefix that is deleted afterwards. There is no Taskfile task for GCS, because there is no local emulator worth running beyond the in-process fake.

## 12. CORS for browser uploads

A browser that uploads or downloads directly with a signed URL or a presigned POST calls the bucket from your site's origin, so the bucket must allow that origin (CORS). `gcs.SetCORS` sets the bucket's rules, and `gcs.DirectUploadCORS` is the recommended rule:

```go
client, _ := filesystem.As[*storage.Client](disk)
err := gcs.SetCORS(ctx, client, "acme-media", gcs.DirectUploadCORS("https://app.example.com"))
```

- The recommended rule allows `GET`, `HEAD`, `PUT` and `POST` from the origins you name, the headers a signed request may carry (`Content-Type`, the pinned `Cache-Control`, `Content-Disposition`, `Content-Encoding`, and `x-goog-encryption-kms-key-name`), exposes `ETag`, and lets browsers cache the preflight for an hour. Name your origins: `*` lets any site use links your users hold.
- GCS has one header list (`responseHeader`) that both allows request headers and exposes response headers, so a rule's `AllowedHeaders` and `ExposeHeaders` are merged into it.
- `SetCORS` replaces the bucket's whole CORS configuration. It is a setup step, not part of the Disk: run it from a deploy script with credentials allowed `storage.buckets.update` (`gcloud storage buckets update --cors-file` does the same from a shell).
- fake-gcs-server accepts the update without keeping it, so the tests check the request the driver sends; that GCS answers the preflight needs a real bucket.

## 13. Bucket setup: lifecycle, replication, object ACLs, scoped tokens

These are adapter functions built from typed structs, like `SetCORS`: setup steps run with admin credentials, not Disk capabilities. Each has a reader.

```go
err := gcs.SetLifecycle(ctx, client, "acme-media",
	gcs.LifecycleRule{Action: gcs.LifecycleDelete, AgeDays: 1, Prefixes: []string{"tmp/"}},
	gcs.LifecycleRule{Action: gcs.LifecycleSetStorageClass, StorageClass: "ARCHIVE", AgeDays: 90, Prefixes: []string{"logs/"}},
	gcs.LifecycleRule{Action: gcs.LifecycleAbortUploads, AgeDays: 7},
)
rules, err := gcs.Lifecycle(ctx, client, "acme-media")
```

- A GCS lifecycle rule has one action, so expiring and archiving the same prefix takes two rules. `SetLifecycle` replaces all of them; an age of zero is refused, so no rule fires the moment an object is written.
- A stored rule with any other condition (live or noncurrent only, newer versions, storage classes, suffixes, dates) makes `Lifecycle` fail with `CodeUnsupported` naming the rule's position, rather than return it with the condition dropped: written back, "delete noncurrent versions after 30 days" would delete every object after 30 days.
- Which regions a bucket spans is fixed when it is created (a dual-region location). `SetReplication(ctx, client, bucket, gcs.ReplicationTurbo)` turns on turbo replication (every object copied within fifteen minutes, dual-region only) and `gcs.ReplicationDefault` turns it off; `Replication` reads the mode back.
- `SetObjectACL(ctx, client, bucket, key, gcs.ObjectACL{Grants})` replaces an object's ACL with `ACLGrant{Entity, Role}` entries (`user-…`, `group-…`, `domain-…`; reader or owner); `ObjectGrants` reads it back. A grant to `allUsers` or `allAuthenticatedUsers` is refused unless `AllowPublic` is set. `key` is the bucket key (`disk.DriverKey(key)`). Buckets with uniform bucket-level access, the default for new buckets, refuse object ACLs.
- `DelegatePrefix(ctx, rootTokenSource, gcs.DelegateOptions{Bucket, Prefix, Access})` exchanges the application's token for a downscoped one whose credential access boundary (`PrefixBoundary`) allows only objects named `prefix/…` and listings under `prefix/`. `Access` picks the roles kept: `DelegateRead` (the zero value) keeps `storage.objectViewer`, to read objects and their metadata and list them; `DelegateCreate` adds `storage.objectCreator`, to upload new objects, which cannot overwrite or delete because GCS needs delete permission to replace an object; `DelegateReadWrite` keeps `storage.objectUser`, to read, list, create, overwrite, update metadata and delete. None keeps `storage.objectAdmin`, whose `storage.objects.setIamPolicy` would let the holder change ACLs and make objects public on a fine-grained bucket. It returns a `ScopedToken` with its expiry that prints redacted; `TokenSource()` feeds `option.WithTokenSource`. Prefixes containing quotes or backslashes are refused, so the condition cannot be broken out of.
- `PresignOptions.AllowedIP` is not supported by GCS signed URLs: `SignedURL` answers `CodeUnsupported`.

### Run them against a stand-in

fake-gcs-server accepts lifecycle and replication updates but keeps neither, so the example sends them to a small stand-in for the bucket metadata API that stores them the way GCS does (a PATCH merges the fields, a GET returns them); the SDK's encoding and the adapter's read-back checks both run:

<!-- ds:block id=bucketlifecycle-jwr74z2b -->
**bucketLifecycle** · [`examples/adapters-gcs/features.go:221-245`](../../examples/adapters-gcs/features.go#L221-L245)

```go
func bucketLifecycle(ctx context.Context, out io.Writer, _ fixture) error {
	client, stop, err := bucketConfigClient(ctx)
	if err != nil {
		return err
	}
	defer stop()
	err = gcs.SetLifecycle(ctx, client, bucketName,
		gcs.LifecycleRule{Action: gcs.LifecycleDelete, AgeDays: 1, Prefixes: []string{"tmp/"}},
		gcs.LifecycleRule{Action: gcs.LifecycleSetStorageClass, StorageClass: "ARCHIVE", AgeDays: 90, Prefixes: []string{"logs/"}},
		gcs.LifecycleRule{Action: gcs.LifecycleAbortUploads, AgeDays: 7},
	)
	if err != nil {
		return err
	}
	rules, err := gcs.Lifecycle(ctx, client, bucketName)
	if err != nil {
		return err
	}
	actions := make([]string, len(rules))
	for i, r := range rules {
		actions[i] = r.Action
	}
	fmt.Fprintf(out, "lifecycle: %s; logs/ move to %s after %d days\n", strings.Join(actions, ","), rules[1].StorageClass, rules[1].AgeDays)
	return nil
}
```
<!-- /ds:block hash=cce835 -->

Check: `lifecycle: Delete,SetStorageClass,AbortIncompleteMultipartUpload; logs/ move to ARCHIVE after 90 days`.

<!-- ds:block id=bucketreplication-ue44rg6x -->
**bucketReplication** · [`examples/adapters-gcs/features.go:250-265`](../../examples/adapters-gcs/features.go#L250-L265)

```go
func bucketReplication(ctx context.Context, out io.Writer, _ fixture) error {
	client, stop, err := bucketConfigClient(ctx)
	if err != nil {
		return err
	}
	defer stop()
	if err := gcs.SetReplication(ctx, client, bucketName, gcs.ReplicationTurbo); err != nil {
		return err
	}
	mode, err := gcs.Replication(ctx, client, bucketName)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "replication: %s\n", mode)
	return nil
}
```
<!-- /ds:block hash=346a2c -->

Check: `replication: ASYNC_TURBO`. Nothing is deleted, archived or replicated by the stand-in; that GCS applies the rules needs a real bucket.

A token scoped to one user's folder, exchanged through a stand-in for Google's STS that records the access boundary:

<!-- ds:block id=delegateprefix-ftu27q44 -->
**delegatePrefix** · [`examples/adapters-gcs/features.go:271-290`](../../examples/adapters-gcs/features.go#L271-L290)

```go
func delegatePrefix(ctx context.Context, out io.Writer, _ fixture) error {
	sts := &fakeSTS{}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: sts}) // only for the stand-in STS
	root := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "root-token", Expiry: time.Now().Add(time.Hour)})
	tok, err := gcs.DelegatePrefix(ctx, root, gcs.DelegateOptions{Bucket: bucketName, Prefix: "users/42", Access: gcs.DelegateCreate})
	if err != nil {
		return err
	}
	var sent struct {
		AccessBoundary struct {
			AccessBoundaryRules []struct{ AvailablePermissions []string }
		}
	}
	if err := json.Unmarshal([]byte(sts.boundary), &sent); err != nil {
		return err
	}
	fmt.Fprintf(out, "delegated %s/%s with %s\n", tok.Bucket, tok.Prefix, strings.Join(sent.AccessBoundary.AccessBoundaryRules[0].AvailablePermissions, ","))
	fmt.Fprintf(out, "token prints as: %s\n", redactExpiry(tok.String()))
	return nil
}
```
<!-- /ds:block hash=ba02fd -->

Check: `delegated media/users/42 with inRole:roles/storage.objectViewer,inRole:roles/storage.objectCreator` and `token prints as: gcs.ScopedToken{AccessToken: [REDACTED], Bucket: "media", Prefix: "users/42"}` (the example drops the expiry from the printed line). That GCS refuses a request outside the prefix needs a real project.

## Pitfalls

- **`acl=true` on a uniform-access bucket** makes every write fail. Most buckets are uniform; leave `acl` off unless the bucket was created with fine-grained access.
- **No signing on Cloud Run or GKE.** Application Default Credentials there have no private key; without `Signing`, `SignedURL` is `CodeUnsupported`. Use `signing_account=default` and grant the runtime account `roles/iam.serviceAccountTokenCreator` on itself.
- **Expecting multipart.** GCS has no multipart API through this driver; browser multipart (Uppy) needs S3.
- **Memory under many concurrent large uploads**: each write buffers up to one chunk.

## Next

- [Serving files](../concepts/serving-files.md): signed URLs, public URLs and fshttp.
- [Public and private files](../concepts/public-and-private.md).
- [Browser direct uploads](../recipes/browser-direct-uploads.md).
- [Comparison of every backend](README.md).
