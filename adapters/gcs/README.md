# adapters/gcs

`github.com/ubgo/filesystem/adapters/gcs` is the `filesystem.Driver` for Google Cloud Storage, on `cloud.google.com/go/storage`. It is its own module, so only applications that import it download the Google Cloud SDK.

The guide is [docs/adapters/gcs.md](../../docs/adapters/gcs.md).

```sh
go get github.com/ubgo/filesystem/adapters/gcs
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Client` | `*storage.Client` | required | Built by the application |
| `Bucket` | `string` | required | The bucket |
| `Visibility` | `VisibilityMode` | `VisibilityNone` | `VisibilityNone` (uniform bucket-level access) or `VisibilityACL` (fine-grained, `publicRead` / `private` predefined ACLs) |
| `PrivateACL` | `PredefinedACL` | empty (`ACLPrivate`) | The predefined ACL `VisibilityACL` gives private objects on a write, a presigned PUT or form (also one asking no visibility) and a copy: `ACLProjectPrivate`, `ACLBucketOwnerRead` or `ACLBucketOwnerFullControl` (for writes into a bucket another project owns); one of `PrivateACLValues`, needs `VisibilityACL` |
| `Signing` | `*storage.SignedURLOptions` | nil | `GoogleAccessID` plus at most one of `PrivateKey` or `SignBytes`; with neither, URLs are signed keylessly through IAM `signBlob` as `GoogleAccessID`; enables `Presign` |
| `IAM` | `*iamcredentials.Service` | nil | The IAM Credentials client keyless signing calls, with the caller's context; see Behaviour |
| `KMSKeyName` | `string` | empty | The Cloud KMS key (CMEK) for every write, copy and presigned PUT |
| `KMSKeyFor` | `func(key string) string` | nil | The CMEK per object (by tenant prefix, under a scoped disk's prefix) on every write, copy, class rewrite, version restore, presigned PUT and form; `""` falls back to `KMSKeyName` |
| `Clock` | `func() time.Time` | `time.Now` | Dates `RetainFor` retentions and a form's (`PresignPost`) signing date and expiry; signed URLs are signed by the SDK at the wall clock |
| `ChunkSize` | `int` | 0 (`DefaultChunkSize`, 16 MiB) | Resumable-upload chunk size, and the size read into memory before a write starts |
| `StorageClass` | `StorageClass` | empty (bucket default) | The class every write gets (`ClassStandard`, `ClassNearline`, `ClassColdline`, `ClassArchive`) |
| `RetainFor` | `time.Duration` | 0 | A retention date this long after every object the disk creates: writes, copies, restored versions, and storage-class rewrites of an object without one (bucket needs object retention enabled); turns off `Presign` and `PresignPost`, which cannot pin it |
| `RetentionMode` | `RetentionMode` | `RetentionUnlocked` | `RetentionUnlocked` or `RetentionLocked` (WORM: only extended, never shortened) |
| `Versioned` | `bool` | false | Declares `Versions`: the bucket has Object Versioning on |
| `CustomerKey` | `CustomerKey` | zero | A customer-supplied key (CSEK) for every read, write, Stat and copy; redacted when printed; exclusive with `KMSKeyName`; turns off `Presign` and `PresignPost` |

`New` returns `CodeInvalidArgument` without `Client` or `Bucket`, for an unknown visibility mode, a negative `ChunkSize`, a `Signing` without `GoogleAccessID` or with both `PrivateKey` and `SignBytes`, `CustomerKey` with `KMSKeyName` or `KMSKeyFor`, a negative `RetainFor`, an unknown `RetentionMode`, and a `PrivateACL` outside `PrivateACLValues` or without `VisibilityACL`.

## DSN

`gs://<bucket>?<parameters>`, registered with `mux.Register(gcs.Scheme, gcs.Opener)`. Requests are authenticated with Application Default Credentials, or with the service account key `credentials_file` or `credentials_env` names, so two disks in one process can act as two accounts.

| Constant | Parameter | Value | Sets |
|---|---|---|---|
| `ParamEndpoint` | `endpoint` | URL | the API endpoint (emulators); the SDK also honours `STORAGE_EMULATOR_HOST` |
| `ParamACL` | `acl` | boolean | `VisibilityACL` |
| `ParamSigningKey` | `signing_key` | path of a service account JSON key | `Signing` from its `client_email` and `private_key` |
| `ParamNoAuth` | `no_auth` | boolean | no credentials (emulators) |
| `ParamKMSKey` | `kms_key` | Cloud KMS key name | `KMSKeyName` |
| `ParamSigningAccount` | `signing_account` | service account email, or `default` (`SigningAccountDefault`) | keyless `Signing` as that account; `default` reads the runtime's account from the metadata server once, at open |
| `ParamStorageClass` | `storage_class` | class name | `StorageClass` |
| `ParamVersioned` | `versioned` | boolean | `Versioned` |
| `ParamCustomerKeyEnv` | `customer_key_env` | environment variable name | `CustomerKey` from the base64 key in that variable (never the key itself in the DSN) |
| `ParamCredentialsFile` | `credentials_file` | path of a service account JSON key | the client's (and `signing_account`'s signBlob) credentials instead of ADC; only service account keys |
| `ParamCredentialsEnv` | `credentials_env` | environment variable name | the same, from a service account JSON key held in that variable |

A missing bucket, an unknown parameter, a malformed boolean, an unreadable or invalid key file, a `credentials_file` or `credentials_env` that is not a service account key, two of `credentials_file`, `credentials_env` and `no_auth`, `signing_key` together with `signing_account`, and `signing_account=default` without a metadata server (or one reporting no account) are `CodeInvalidArgument`; a metadata server that fails is `CodeUnavailable`; missing or unusable default credentials are `CodePermission`.

## Capabilities

Always: `Copy`, `Metadata`, `ContentType`, `Headers`, `ConditionalWrite`, `CompareAndSwap` (generation preconditions), `Hold` (temporary hold and retention date). With `VisibilityACL`: `SetVisibility`, and with `Signing` as well, `PresignVisibility`. With `Signing`, no `CustomerKey` and no `RetainFor`: `Presign` and `PresignHeaders`; with a key, `SignBytes`, or `IAM` as well: `PresignPost`. With `Versioned`: `Versions` and `VersionRead`. Always: `StorageClass`. Never: `Tags` (GCS has no object tags; use metadata), `Restore` (every class is online), `Move`, `DeletePrefix`, `DeleteKeys` (Disk fallbacks), `Multipart` and `PresignPart` (resumable uploads happen inside `Write`), `PublicURL`.

## Per-call options

`RequestOptions{KMSKeyName, StorageClass, RetainUntil, TemporaryHold, CustomTime}`, attached with `gcs.WithRequestOptions(ctx, disk, key, o)`, override the disk's `Config` for the operations on that exact key (cleaned and under a scoped disk's prefix, as `disk.DriverKey` gives it); a call on any other key ignores them. `KMSKeyName` overrides `KMSKeyFor` and `KMSKeyName` on a write, copy, storage-class rewrite, version restore, presigned PUT (signed) and form (pinned); `StorageClass` (one of `StorageClassValues`) overrides `Config.StorageClass` on a write and sets a copy's class, and a presigned PUT or form then signs it as `x-goog-storage-class`. A KMS key on a `CustomerKey` disk or an unknown class is `CodeInvalidArgument` before anything is written or signed. `RetainUntil` (in `RetentionMode`, the later of it and `RetainFor`'s date wins, so a call never shortens a disk's retention; not after now is `CodeInvalidArgument`), `TemporaryHold` and `CustomTime` (the date lifecycle rules count `daysSinceCustomTime` from) are set in the request that creates the object, on a write or a copy's destination, so there is no window before a later `SetHold`; a presigned PUT or form asked for any of them is `CodeUnsupported`, since GCS cannot pin them in a signature. Without options nothing changes. It mirrors the s3 adapter's `RequestOptions`.

## Storage classes, versions and ObjectInfo

`Disk.SetStorageClass` is an in-place rewrite conditioned on the generation it read (`CodeConflict` if the object changed); `Entry.StorageClass` reports the class. `Disk.ReadVersion` and `Disk.RestoreVersion` take a `Version.ID`, the generation (an ID that is not one is `CodeNotFound`). `*gcs.Driver` also offers `ObjectInfo(ctx, key)` (generation, CRC32C, MD5, hold, retention mode, event-based hold), which cleans the key like the Disk but bypasses a scoped disk's prefix and is reached by keeping the `*gcs.Driver` passed to `NewDisk`.

## Behaviour

- `Write` reads up to one chunk first: a body that fits is sent only once read completely; a larger one is a resumable upload committed with its last chunk. A failing body commits nothing.
- `Read` of an object stored with `Content-Encoding: gzip` returns the stored bytes: a decompressed response is reopened with `ReadCompressed`. A client built with `storage.WithJSONReads` fails on such objects instead.
- A ranged `Read` past the end is an empty read.
- `List` uses GCS's page token as the cursor, skips keys ending in `/`, and sorts entries by key.
- `Copy` is server-side; in ACL mode the source's visibility is applied to the copy.
- `SetVisibility` adds or removes the `allUsers` reader entry.
- `Presign` is V4; a PUT signs the content type and any `PresignOptions.Headers` (Cache-Control, Content-Disposition, Content-Encoding, listed in `X-Goog-SignedHeaders` and returned in `SignedRequest.Header`), which GCS then requires and stores; a GET can set `response-content-type` and `response-content-disposition`.
- In ACL mode a visibility asked for (`PresignOptions.Visibility`, `PostPolicy.Visibility`) is signed as `x-goog-acl` (`public-read` or `private`) on a PUT and pinned as the `acl` field of a form, so the client cannot change it (`PresignVisibility`); under uniform access it is `CodeUnsupported`. Nothing is signed when none is asked.
- `PresignPost` builds and signs a V4 POST policy itself (size range, exact key or `starts-with` prefix, Content-Type, stored headers, KMS key, the call's `RequestOptions`, an asked visibility) for `https://storage.googleapis.com/<bucket>/` (`PostHost`); declared when `Signing` has a key or `SignBytes`, or with `IAM` for keyless signing.
- `ChecksumSHA256` is not declared: GCS verifies CRC32C and MD5, not SHA-256; the Disk checks `WriteOptions.SHA256` itself.
- Keyless signing (a `Signing` with only `GoogleAccessID`, or `signing_account`) makes one IAM Credentials `signBlob` call per URL, whose caller needs `roles/iam.serviceAccountTokenCreator` on that account; a refusal is `CodePermission`. With `Config.IAM` (the opener always sets it) the call uses the `SignedURL` caller's context, so its deadline or cancellation stops it, and Google signs as `GoogleAccessID` even when the storage client holds a key for another account. Without it the SDK calls IAM with `context.Background()` and, for a key-file client, signs with that key, which is only valid when `GoogleAccessID` is the key's account.
- `Native()` returns the `*storage.Client` for `filesystem.As[*storage.Client]`.

## CORS

`gcs.SetCORS(ctx, client, bucket, rules...)` replaces the bucket's CORS configuration; `gcs.DirectUploadCORS(origins...)` is the recommended rule for browsers using this driver's signed URLs. A setup step with admin credentials, not part of the Disk. See the guide.

`gcs.SetLifecycle`/`gcs.Lifecycle` (delete, change class or abort uploads by age and prefix), `gcs.SetReplication`/`gcs.Replication` (turbo replication on dual-region buckets) and `gcs.SetObjectACL`/`gcs.ObjectGrants` (object ACL entities; public grants need `AllowPublic`) are setup functions of the same kind, each read back by its pair. `gcs.DelegatePrefix(ctx, root, gcs.DelegateOptions{...})` issues a `ScopedToken`: a downscoped token whose access boundary (`gcs.PrefixBoundary`) allows only one prefix at the chosen `DelegateAccess` (read, create or read-write; never ACL or IAM changes), with an expiry, printed redacted. GCS signed URLs cannot carry `PresignOptions.AllowedIP` (`CodeUnsupported`). See the guide.

## Errors

| GCS | Code |
|---|---|
| `ErrObjectNotExist`, `ErrBucketNotExist`, 404 | `CodeNotFound` |
| 412 | `CodeExists` |
| 401, 403 | `CodePermission` |
| 429, 5xx | `CodeUnavailable` (with the response's `Retry-After`, when present, on the error: `filesystem.RetryAfterOf`) |
| other | `CodeUnknown` |

## Tests

`go test ./...` runs the conformance suite (both visibility modes) against an in-process fake-gcs-server, plus a fault proxy for every request. `keyless_test.go` signs through a fake IAM Credentials endpoint (the URL's signature must verify against the key the fake signs with) and resolves `signing_account=default` from a fake metadata server (`GCE_METADATA_HOST`); a real IAM grant and Cloud Run or GKE metadata server can only be checked on Google Cloud. `advanced_test.go` covers holds, retention, storage classes, versions, customer-supplied keys and checksums; where the fake ignores a field (temporary holds, a rewrite's class, CSEK) it checks the request the driver sends, so enforcement by GCS (a held object refusing deletion, decryption with the key) needs a real bucket. Set `FILESYSTEM_GCS_TEST_DSN` (with Application Default Credentials) to run the suite against a real bucket.
