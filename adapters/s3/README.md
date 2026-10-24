# adapters/s3

`github.com/ubgo/filesystem/adapters/s3` is the `filesystem.Driver` for Amazon S3 and every S3-compatible store (Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces, SeaweedFS, Garage, MinIO), on `aws-sdk-go-v2`. Uploads go through the SDK's `feature/s3/transfermanager`, so bodies of unknown length stream as multipart uploads without being buffered whole. It is its own module, so only applications that import it download the AWS SDK.

The guide is [docs/adapters/s3.md](../../docs/adapters/s3.md); provider DSNs are in [docs/adapters/s3-compatible.md](../../docs/adapters/s3-compatible.md).

```sh
go get github.com/ubgo/filesystem/adapters/s3
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Client` | `*s3.Client` | required | Built by the application with its region, endpoint and credentials |
| `Bucket` | `string` | required | The bucket |
| `Visibility` | `VisibilityMode` | `VisibilityNone` | `VisibilityNone` (bucket policy decides, no `SetVisibility`) or `VisibilityACL` (per-object canned ACLs; presigned PUTs and forms then pin an asked visibility as `x-amz-acl` / the `acl` field, `PresignVisibility`) |
| `ConditionalWrites` | `bool` | `false` | The provider honours `If-None-Match: *` on PUT; turns on `ConditionalWrite` |
| `SSEKMSKeyID` | `string` | empty | The KMS key for SSE-KMS; implies `aws:kms`, refused with `AES256` |
| `SSEKMSKeyFor` | `func(key string) string` | nil | The KMS key per object (by tenant prefix, under a scoped disk's prefix); `""` falls back to `SSEKMSKeyID`; same rules |
| `ServerSideEncryption` | `types.ServerSideEncryption` | none | Sent on every write, copy and multipart upload |
| `StorageClass` | `types.StorageClass` | none | Sent on every write, copy and multipart upload |
| `PartSize` | `int64` | 0 (SDK default, 8 MiB) | Transfer-manager part size |
| `Concurrency` | `int` | 0 (SDK default, 5) | Parts of one upload in flight at once; each holds `PartSize` bytes |
| `ExpectedBucketOwner` | `string` | none | 12-digit account ID sent on every request (`x-amz-expected-bucket-owner`, signed into presigned URLs); S3 refuses the call if another account owns the bucket |
| `RequestPayer` | `types.RequestPayer` | none | `requester`, sent on every request and signed into presigned URLs, for requester-pays buckets |
| `PrivateACL` | `CannedACL` | `CannedPrivate` | The canned ACL `VisibilityACL` gives private objects (`CannedBucketOwnerFullControl` for writes into another account's bucket); public ACLs refused |
| `Clock` | `func() time.Time` | `time.Now` | Dates `ObjectLockDays` retentions, the `MaxRetention` check and hold checks, and signs presigned URLs, parts and forms (their `ExpiresAt` too); Retry-After waits stay on the wall clock |
| `PruneEmptyPrefixes` | `bool` | `false` | Non-recursive List drops directories with no objects (SeaweedFS and other filer-backed stores) |
| `SweepUploads` | `bool` | `false` | Declares `SweepAbandoned` and `FindAbandoned`: a bucket-wide abort (and its dry run) of multipart uploads initiated before a cutoff |
| `UnverifiedSignedHeaders` | `bool` | `false` | The store does not verify SigV4 signatures; withdraws `PresignHeaders` |
| `StorageClasses` | `bool` | `false` | The store keeps per-object storage classes; declares `StorageClass` |
| `Tagging` | `bool` | `false` | The store keeps object tags; declares `Tags` |
| `ObjectLock` | `bool` | `false` | The bucket has Object Lock; declares `Hold`, and deletes refuse held objects |
| `ObjectLockMode` | `types.ObjectLockMode` | `GOVERNANCE` | The mode of every retention this disk sets |
| `ObjectLockDays` | `int` | 0 | A retention of this many days on every write and copy; needs `ObjectLock` |
| `MaxRetention` | `time.Duration` | `DefaultMaxRetention` (ten years) | The longest retention this disk sets; a later date is `CodeInvalidArgument` |
| `Versioning` | `bool` | `false` | The bucket is versioned; declares `Versions` and `VersionRead` |
| `Restore` | `bool` | `false` | Objects may be in archive classes; declares `Restore` |
| `RestoreTier` | `types.Tier` | `Standard` | The retrieval tier of `Restore` |
| `SSECustomerKey` | `CustomerKey` | none | SSE-C key sent on every object request; exclusive with SSE-S3/KMS; withdraws every presign capability unless `PresignCustomerKey` |
| `PresignCustomerKey` | `bool` | `false` | Under SSE-C, declare `Presign` and `PresignPart` anyway; each `SignedRequest.Header` then holds the disk-wide key |
| `PresignWithoutChecksum` | `bool` | `false` | Under a `ChecksumAlgorithm`, declare `Presign` anyway; presigned PUTs then carry no checksum |
| `ChecksumAlgorithm` | `types.ChecksumAlgorithm` | SDK default | CRC32, CRC32C, CRC64NVME, SHA1 or SHA256, stored and verified by S3 |

`New` returns `CodeInvalidArgument` without `Client` or `Bucket`, for an unknown visibility mode, lock mode, restore tier or checksum algorithm, for a negative `PartSize`, `Concurrency` or `ObjectLockDays`, an `ExpectedBucketOwner` that is not a 12-digit account ID, a `RequestPayer` other than `requester`, a public or unknown `PrivateACL`, for `ObjectLockDays` without `ObjectLock`, and for an SSE-C key together with `ServerSideEncryption` or `SSEKMSKeyID`. `New` sends no request; `(*Driver).CheckBucket(ctx)` verifies that the bucket has the versioning and Object Lock the Config declares (the Opener calls it). `VisibilityModeValues` lists the modes.

## DSN

`s3://<bucket>?<parameters>`, registered with `mux.Register(s3.Scheme, s3.Opener)`. Without credential parameters the client uses the AWS default chain (environment, shared files, web identity, container and instance roles). With them the disk has credentials of its own: `profile` picks a shared-config profile, or `access_key_env` and `secret_key_env` (plus `session_token_env`) name environment variables holding a key pair. A secret is never in the DSN itself: a `user:password@` part is refused, and errors name the variable, never its value.

| Constant | Parameter | Value | Sets |
|---|---|---|---|
| `ParamRegion` | `region` | region, `auto` for R2 | the client's region |
| `ParamEndpoint` | `endpoint` | URL | the client's `BaseEndpoint` |
| `ParamPathStyle` | `path_style` | boolean | path-style addressing |
| `ParamACL` | `acl` | boolean | `VisibilityACL` |
| `ParamPresignHeaders` | `presign_headers` | boolean, default `true` | `false` sets `UnverifiedSignedHeaders` |
| `ParamSweepUploads` | `sweep_uploads` | boolean | `SweepUploads` (declares `SweepAbandoned` and `FindAbandoned`) |
| `ParamConditionalWrite` | `conditional_write` | boolean | `ConditionalWrites` |
| `ParamSSE` | `sse` | `AES256`, `aws:kms` | `ServerSideEncryption` |
| `ParamSSEKMSKeyID` | `sse_kms_key_id` | KMS key ID, ARN or alias ARN | `SSEKMSKeyID` (implies `sse=aws:kms`) |
| `ParamStorageClass` | `storage_class` | storage class | `StorageClass` |
| `ParamPartSize` | `part_size` | bytes | `PartSize` |
| `ParamConcurrency` | `concurrency` | count | `Concurrency` |
| `ParamExpectedBucketOwner` | `expected_bucket_owner` | 12-digit account ID | `ExpectedBucketOwner` |
| `ParamRequestPayer` | `request_payer` | `requester` | `RequestPayer` |
| `ParamPruneEmptyPrefixes` | `prune_prefixes` | boolean | `PruneEmptyPrefixes` |
| `ParamProfile` | `profile` | profile name | the shared-config profile for this disk; exclusive with the `*_env` parameters |
| `ParamAccessKeyEnv` | `access_key_env` | variable name | the access key ID, read from that variable; needs `secret_key_env` |
| `ParamSecretKeyEnv` | `secret_key_env` | variable name | the secret access key, read from that variable; needs `access_key_env` |
| `ParamSessionTokenEnv` | `session_token_env` | variable name | a session token for temporary keys; optional |
| `ParamRoleARN` | `role_arn` | IAM role ARN | work through that role (bring your own bucket); the credentials above only assume it |
| `ParamExternalID` | `external_id` | string | `RoleOptions.ExternalID`; required with `role_arn` |
| `ParamRoleSessionName` | `role_session_name` | string | `RoleOptions.SessionName`; required with `role_arn` |
| `ParamRoleDuration` | `role_duration` | Go duration | `RoleOptions.Duration`; default `DefaultRoleDuration` (1h) |
| `ParamStorageClasses` | `storage_classes` | boolean | `StorageClasses` |
| `ParamTagging` | `tagging` | boolean | `Tagging` |
| `ParamObjectLock` | `object_lock` | boolean | `ObjectLock`, checked against the bucket |
| `ParamObjectLockMode` | `object_lock_mode` | `GOVERNANCE`, `COMPLIANCE` | `ObjectLockMode`; implies `object_lock` |
| `ParamObjectLockDays` | `object_lock_days` | days | `ObjectLockDays`; implies `object_lock` |
| `ParamVersioning` | `versioning` | boolean | `Versioning`, checked against the bucket |
| `ParamRestore` | `restore` | boolean | `Restore` |
| `ParamRestoreTier` | `restore_tier` | `Standard`, `Bulk`, `Expedited` | `RestoreTier` |
| `ParamSSECKeyEnv` | `sse_c_key_env` | variable name | `SSECustomerKey`, a base64 32-byte key read from that variable |
| `ParamChecksum` | `checksum` | algorithm | `ChecksumAlgorithm` |
| `ParamMaxRetentionDays` | `max_retention_days` | days | `MaxRetention` |
| `ParamPresignCustomerKey` | `presign_customer_key` | boolean | `PresignCustomerKey` |
| `ParamPresignWithoutChecksum` | `presign_without_checksum` | boolean | `PresignWithoutChecksum` |

A missing bucket, an unknown parameter, a malformed boolean or number, credentials in the URL, `profile` together with a `*_env` parameter, only one of `access_key_env` and `secret_key_env`, a named variable that is unset or empty, an unknown profile and an unloadable AWS config are `CodeInvalidArgument`. Static keys from the `*_env` parameters are held behind a pointer and format as a placeholder, so `%v` or `%+v` of the client, its options or the disk does not print them.

## Capabilities

Always: `Copy`, `DeletePrefix`, `DeleteKeys`, `Multipart`, `Metadata`, `ContentType`, `Headers`. Without an SSE-C key, or with `PresignCustomerKey`: `Presign`, `PresignPart` (and `PresignHeaders` as below). Without an SSE-C key: `PresignPost`. Unless `ChecksumAlgorithm` is another algorithm than SHA256: `ChecksumSHA256`. With `StorageClasses`: `StorageClass`. With `Tagging`: `Tags`. With `ObjectLock`: `Hold`. With `Versioning`: `Versions` and `VersionRead`. With `Restore`: `Restore`. With `Presign`, unless `UnverifiedSignedHeaders` (`presign_headers=false`): `PresignHeaders`. With `SweepUploads` (`sweep_uploads=true`): `SweepAbandoned` and `FindAbandoned`. With `VisibilityACL`: `SetVisibility`. With `ConditionalWrites`: `ConditionalWrite`. Never: `Move` (the Disk copies then deletes), `PublicURL` (use `public_url` / `WithPublicURL`).

## Behaviour

- `Stat` in ACL mode reads the object's ACL (one extra request); public means `AllUsers` may read.
- A ranged `Read` past the end is an empty read.
- `List` uses `ListObjectsV2` (cursor = continuation token), skips keys ending in `/`, and sorts entries by key.
- `Copy` is `CopyObject` (5 GiB limit); in ACL mode the source's visibility is carried over.
- `DeleteKeys` and `DeletePrefix` send `DeleteObjects` with up to 1,000 keys; per-key failures come back in a `*filesystem.BatchError`.
- `Presign` is SigV4; a PUT signs the content type and any `PresignOptions.Headers` (Cache-Control, Content-Disposition, Content-Encoding), returns them in `SignedRequest.Header`, and S3 stores them with the object. A store that verifies signatures refuses the upload without them; one that does not (SeaweedFS without `-s3.config`) must be opened with `presign_headers=false`, which withdraws `PresignHeaders`. `PresignPart` signs one part's PUT.
- `PresignPost` builds a SigV4 POST policy (size range, exact key or `starts-with` prefix, Content-Type, stored headers, the disk's encryption, storage class, tags and object-lock mode, retain-until date and legal hold), each pinned value also a form field; objects get the bucket's default ACL. A presigned PUT signs the same per-write settings. A checksum cannot be signed before the body exists, so with a `ChecksumAlgorithm` no presign capability is declared unless `PresignWithoutChecksum` opts in to `Presign` and `PresignHeaders` with PUTs that carry no checksum; parts and forms stay off, and a PUT under a per-call algorithm is `CodeUnsupported`.
- `ChecksumSHA256`: a write with `WriteOptions.SHA256` and a `SizeHint` up to 5 GiB is one `PutObject` signed with that digest as its payload hash and carrying `x-amz-checksum-sha256`, so S3 refuses a mismatching body; other writes rely on the Disk's own check.
- `CreateMultipart` with `IfNotExists` is `CodeUnsupported`; `AbortMultipart` of an unknown upload succeeds.
- `Native()` returns the `*s3.Client` for `filesystem.As[*s3.Client]`.

## Object features

- `s3.WithRequestOptions(ctx, disk, key, s3.RequestOptions{...})` returns `(ctx, error)` and carries what `WriteOptions` has no field for to the calls on that key (the destination of a copy) made with that context, through the Disk; they apply to that exact key only, resolved through `disk.DriverKey` (so a scoped disk's prefix is added for the caller; an invalid key is the error): `Tags`, `StorageClass`, `LegalHold`, `RetainUntil` and `ChecksumAlgorithm` for a write, copy or multipart upload, and `RestoreTier` for `Restore`. A field whose capability the disk lacks is `CodeUnsupported`.
- `Tags`/`SetTags` are `GetObjectTagging`/`PutObjectTagging` (`DeleteObjectTagging` for an empty map); a copy keeps the source's tags unless `RequestOptions.Tags` replaces them.
- `Hold` reads the legal hold and retention from one HEAD; `SetHold` is `PutObjectLegalHold` then `PutObjectRetention` (removing one sends the governance bypass header). `Delete`, `DeleteKeys`, `DeletePrefix`, `Write`, `Copy` onto it, `SetStorageClass`, `RestoreVersion`, `CreateMultipart` and `CompleteMultipart` HEAD the key first and refuse a held one with `CodePermission` (the batch deletes 16 HEADs at a time); retention beyond `MaxRetention` is `CodeInvalidArgument`.
- `Versions` is `ListObjectVersions` (versions and delete markers of that exact key, newest first); `ReadVersion` is a versioned `GetObject`; `RestoreVersion` copies the version onto the key after a HEAD that turns a delete marker into `CodeNotFound`.
- `SetStorageClass` copies the object onto itself in the new class; `Stat` sets `Entry.StorageClass`.
- `Restore` is `RestoreObject` at the configured tier; `RestoreAlreadyInProgress` is success.
- `Stat` adds S3 state with no `Entry` field to `Metadata` under `s3:` keys (`MetaVersionID`, `MetaRestore`, `MetaRestoreExpiry`, `MetaChecksumPrefix`+algorithm); writes drop `s3:` keys from `WriteOptions.Metadata`.
- An empty `x-amz-object-lock-retain-until-date` (RustFS, after a retention is removed) is ignored instead of failing the HEAD.

## Sweeping abandoned uploads

With `SweepUploads`, `SweepAbandoned(ctx, before)` lists the bucket's unfinished multipart uploads and aborts those initiated before `before`, skipping any that finished meanwhile. It is bucket-wide and off by default; a bucket lifecycle rule (`AbortIncompleteMultipartUpload`) is the preferred way to do the same. `FindAbandoned(ctx, before)` runs the same listing and returns those uploads (ID, key, initiation time) without aborting any.

## Encryption

`ServerSideEncryption` and `SSEKMSKeyID` are sent on `Write`, `Copy`, `CreateMultipart`, signed into presigned PUTs (the client sends them from `SignedRequest.Header`) and pinned in presigned POSTs, so every object of the disk is encrypted the same way. `SSEKMSKeyFor` picks the key per object instead (one key per tenant on a shared disk, decided from the key's prefix), and `RequestOptions.SSEKMSKeyID` per call; precedence is call, resolver, `SSEKMSKeyID`. A KMS key with `AES256` or `SSECustomerKey` is `CodeInvalidArgument` (in `New` for the resolver, before any request for a call key).

`SSECustomerKey` (SSE-C, built with `NewCustomerKey` from 32 bytes) is sent on every HEAD, GET, PUT, copy (source and destination), multipart create, part and complete, and, only with `PresignCustomerKey`, signed into presigned GETs, PUTs and parts, whose `SignedRequest.Header` then carries the key. A `CustomerKey` formats as a placeholder with every verb, alone or inside a `Config` or `Driver`.

## Bring your own bucket (assume role)

`NewRoleClient(ctx, appCfg, RoleOptions{RoleARN, ExternalID, SessionName, Duration})` returns an `*s3.Client` that signs with a role in a customer's account, assumed with the application's credentials and renewed a minute before expiry (`aws.CredentialsCache` over `stscreds`); it assumes once before returning, so a refusal is `CodePermission` at opening. `RoleCredentials(stsClient, opts)` returns just the provider. `ExternalID` and `SessionName` are required (the external ID guards against the confused-deputy problem); `Duration` defaults to `DefaultRoleDuration`. Optional session settings narrow or label the session: `Policy` (an inline session policy, for example limited to the tenant's prefix), `PolicyARNs` (managed session policies), `Tags` and `TransitiveTagKeys` (session tags for attribute-based access control; the trust policy must allow `sts:TagSession`), and `SourceIdentity` (recorded in the customer's CloudTrail; needs `sts:SetSourceIdentity`). The DSN form is `role_arn`, `external_id`, `role_session_name`, `role_duration`. The provider prints only the role and the session name under every verb. Tested against STS and S3 fakes only.

## CORS

`s3.SetCORS(ctx, client, bucket, rules...)` replaces the bucket's CORS configuration (`PutBucketCors`); `s3.DirectUploadCORS(origins...)` is the recommended rule for browsers using this driver's signed URLs. A setup step with admin credentials, not part of the Disk. See the guide.

`s3.SetLifecycle`/`s3.Lifecycle` (expiry, multipart cleanup, transitions per prefix), `s3.SetReplication`/`s3.Replication` (cross-region replication; needs versioning and an IAM role) and `s3.SetObjectACL`/`s3.ObjectGrants` (typed canned ACLs and explicit grants on a bucket key; public ones need `AllowPublic`) are setup functions of the same kind, each read back by its pair. `s3.DelegatePrefix(ctx, stsClient, s3.DelegateOptions{...})` issues `ScopedCredentials`: an STS session whose policy (`s3.SessionPolicy`) allows only one prefix, with an expiry, printed redacted; `Partition` (default: the role ARN's, else `aws`) makes the ARNs work in GovCloud and China, and `KMSKeyARNs` adds `kms:Decrypt` (and with `Write` `kms:GenerateDataKey`) on exactly those keys for SSE-KMS buckets. S3 presigned URLs cannot carry `PresignOptions.AllowedIP` (`CodeUnsupported`). See the guide.

## Errors

| S3 | Code |
|---|---|
| `NoSuchKey`, `NotFound`, `NoSuchUpload`, 404 | `CodeNotFound` |
| `PreconditionFailed`, 412 | `CodeExists` |
| `AccessDenied`, 403 | `CodePermission` |
| `InvalidPart`, `InvalidPartOrder`, `EntityTooSmall`, `InvalidStorageClass`, `BadDigest`, checksum mismatches | `CodeInvalidArgument` |
| `InvalidObjectState` (archived, not restored) | `CodeConflict` with `ErrArchived` in the chain (as Azure's archive tier; never retried) |
| 405 from `ReadVersion` / `RestoreVersion` (delete marker) | `CodeNotFound` |
| `SlowDown`, 429, 5xx | `CodeUnavailable` (with the response's `Retry-After`, when present, on the error: `filesystem.RetryAfterOf`) |
| other | `CodeUnknown` |

## Tests

`go test ./...` runs the conformance suite (default mode, and ACL mode with tags, storage classes and a versioned bucket) against an in-process gofakes3 behind a proxy that adds ACLs, `If-None-Match`, `Cache-Control`, tags, Object Lock, storage classes and restores, stored checksums and versioned HEAD and copy, plus fault injection for every operation. `task test:s3` (SeaweedFS) and `task test:rustfs` (RustFS) run the suite and the `*AgainstARealStore` feature tests in Docker; set `FILESYSTEM_S3_TEST_DSN` to run them against any other store.
