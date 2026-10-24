# adapters/azure

`github.com/ubgo/filesystem/adapters/azure` is the `filesystem.Driver` for Azure Blob Storage (block blobs) and Azurite, its emulator, on `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`. It is its own module, so only applications that import it download the Azure SDK.

The guide is [docs/adapters/azure.md](../../docs/adapters/azure.md).

```sh
go get github.com/ubgo/filesystem/adapters/azure
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Client` | `*azblob.Client` | required | The account client, built by the application (shared key, Entra ID, SAS) |
| `Container` | `string` | required | The container |
| `SharedKey` | `*azblob.SharedKeyCredential` | nil | Signs SAS URLs; with neither this nor `UserDelegation`, no `Presign` |
| `EncryptionScope` | `string` | empty | The encryption scope sent on every write, staged block and commit, and carried by presigned PUTs (`ses`); a disk with one does not declare `Copy` |
| `EncryptionScopeFor` | `func(key string) string` | nil | The encryption scope per blob (by tenant prefix, under a scoped disk's prefix) on every write, staged block, commit, append and presigned PUT; `""` falls back to `EncryptionScope`; like it, turns off `Copy` and `VersionRead` |
| `Clock` | `func() time.Time` | `time.Now` | Dates SAS expiries and `ExpiresAt`, user delegation key validity and `RetainFor` policies; Retry-After waits stay on the wall clock |
| `CopyPollInterval` | `time.Duration` | `DefaultCopyPollInterval` (50 ms) | How often `Copy` checks a pending server-side copy |
| `UserDelegation` | `bool` | false | Signs user delegation SAS URLs for an Entra ID client, with a key the driver fetches through `Client` and caches; exclusive with `SharedKey` |
| `AccountName` | `string` | empty (the endpoint host's first label) | The account a user delegation SAS is signed for, when `Client`'s endpoint is a custom domain, Front Door or a private-endpoint name; the key is then fetched through a second client sending to `Client`'s host; needs `UserDelegation` and `Credential`, and 3 to 24 lower-case letters and digits |
| `Credential` | `azcore.TokenCredential` | nil | The credential `Client` was built with; required with `AccountName` and used for nothing else |
| `DelegationClientOptions` | `*azblob.ClientOptions` | nil (SDK defaults) | Options of `AccountName`'s key client (transport, retries, audience), usually `Client`'s; copied, never modified; only with `AccountName` |
| `CustomerKey` | `CustomerKey` | zero | A customer-provided key (CPK) sent on every read, write, Stat, staged block, commit and append (HTTPS only); redacted when printed; exclusive with `EncryptionScope`; turns off `Presign` and `Copy` |
| `AccessTier` | `blob.AccessTier` | empty (account default) | The tier every write and completed multipart upload gets |
| `RehydrateTier`, `RehydratePriority` | `blob.AccessTier`, `blob.RehydratePriority` | Hot, Standard | Where and how fast `Restore` rehydrates an archived blob |
| `RetainFor` | `time.Duration` | 0 | An immutability policy this long after every blob the disk creates (writes, completed multipart uploads, copies, restored versions), set right after it is stored; a failed policy request returns an error saying the blob was stored (container needs version-level immutability); turns off `Presign` (a SAS upload cannot pin it) and `Append` (a protected append blob refuses appends) |
| `RetentionMode` | `blob.ImmutabilityPolicySetting` | Unlocked | Mode of the policies the driver sets |
| `Versioned` | `bool` | false | Declares `Versions`: the account has blob versioning on |
| `ValidateCRC64` | `bool` | false | Sends a CRC64 with every staged block and append, which Azure checks |
| `BlockSize` | `int64` | `DefaultBlockSize` (8 MiB) | The size of each block `Write` stages, in [`MinBlockSize` (1 MiB), `MaxBlockSize` (4000 MiB)]. A block blob holds at most `MaxBlocks` (50,000) blocks, so this caps one `Write`: about 390.6 GiB at the default, about 190.7 TiB at the maximum |
| `Concurrency` | `int` | `DefaultConcurrency` (5) | How many blocks one `Write` stages at once; a `Write` holds up to `Concurrency` times `BlockSize` in memory (40 MiB at the defaults) |

`New` returns `CodeInvalidArgument` without `Client` or `Container`, for `SharedKey` with `UserDelegation`, `AccountName` without `UserDelegation` or `Credential` or not a valid account name, `Credential` or `DelegationClientOptions` without `AccountName`, `CustomerKey` with `EncryptionScope` or `EncryptionScopeFor`, a negative `RetainFor` or `CopyPollInterval`, an unknown `RetentionMode`, a `BlockSize` outside [`MinBlockSize`, `MaxBlockSize`] (zero means the default) and a negative `Concurrency`.

## DSN

`azblob://<container>?<parameters>`, registered with `mux.Register(azure.Scheme, azure.Opener)`. The account comes from a connection string in the environment, never from the DSN: a shared-key one, or a SAS one.

| Constant | Parameter | Default | Meaning |
|---|---|---|---|
| `ParamConnectionEnv` | `connection_env` | `DefaultConnectionEnv` (`AZURE_STORAGE_CONNECTION_STRING`) | the variable holding the connection string |
| `ParamEncryptionScope` | `encryption_scope` | none | `EncryptionScope`; also accepted by `TokenOpener` |
| `ParamAccessTier` | `access_tier` | none | `AccessTier` (Hot, Cool, Cold, Archive); also accepted by `TokenOpener` |
| `ParamVersioned` | `versioned` | false | `Versioned`; also accepted by `TokenOpener` |
| `ParamCustomerKeyEnv` | `customer_key_env` | none | `CustomerKey` from the base64 key in that environment variable (never the key itself in the DSN); also accepted by `TokenOpener` |
| `ParamBlockSize` | `block_size` | `DefaultBlockSize` | `BlockSize` in bytes; also accepted by `TokenOpener` |
| `ParamConcurrency` | `concurrency` | `DefaultConcurrency` | `Concurrency`; also accepted by `TokenOpener` |

From a shared-key connection string the opener reads `AccountName` and `AccountKey`, `BlobEndpoint` (used as is when present), and otherwise builds `<DefaultEndpointsProtocol>://<AccountName>.blob.<EndpointSuffix>` with defaults `https` and `core.windows.net`. A missing container, an unknown parameter, an unknown access tier, a malformed `versioned`, `block_size` or `concurrency`, a customer key variable that is not base64 of 32 bytes, and a connection string with neither name and key nor a SAS are `CodeInvalidArgument`. Disks opened this way can sign SAS URLs. A SAS connection string (`SharedAccessSignature=...` with `BlobEndpoint` or `AccountName`, and no `AccountKey`) authenticates every request with that token, the least-privilege setup, and declares no `Presign`, since there is no key to sign with; the disk stops working when the token expires.

`TokenOpener(cred azcore.TokenCredential)` returns an opener for Entra ID: `azblob://<container>?account=<name>` (endpoint `https://<name>.blob.core.windows.net/`) or `?endpoint=https://<account>.blob.<suffix>` for a sovereign cloud (`ParamAccount`, `ParamEndpoint`; alone, any other endpoint host, port or path is refused, since the SAS takes the account from the host). Both together (`?account=<name>&endpoint=https://files.example.com`) open an endpoint behind a custom domain, Front Door or a private-endpoint name: the endpoint may be any https URL, and `AccountName` signs for the account. The credential is built by the application (for example with `azidentity`), so this module needs no identity library. Its disks sign user delegation SAS URLs. A missing container, a nil credential, neither `account` nor `endpoint`, a non-https endpoint, an invalid account name with an endpoint, or an unknown parameter are `CodeInvalidArgument`.

## Capabilities

Always: `Multipart`, `Metadata`, `ContentType`, `Headers`, `ConditionalWrite`, `CompareAndSwap` (If-Match on the commit), `SweepAbandoned`, `FindAbandoned`. Without `RetainFor`: `Append` (append blobs, at most `MaxAppendBlock` per call). Always: `Tags` (blob index tags), `Hold` (legal hold and immutability policy), `Restore` (rehydration from Archive). Without `EncryptionScope` or `CustomerKey`: `Copy` (server-side). With `SharedKey` or `UserDelegation`, and neither `CustomerKey` nor `RetainFor`: `Presign`. Always: `StorageClass` (the access tier). With `Versioned`: `Versions`, and `VersionRead` unless `CustomerKey` or `EncryptionScope` is set. Never: `SetVisibility` (public access is a container setting), `PresignPart` (Put Block returns no ETag), `Move`, `DeletePrefix`, `DeleteKeys` (Disk fallbacks), `PublicURL`.

## Tiers, versions and ObjectInfo

`Disk.SetStorageClass` sets the access tier (Hot, Cool, Cold, Archive; anything else is `CodeInvalidArgument`) in place, and `Entry.StorageClass` reports it. `Disk.ReadVersion` and `Disk.RestoreVersion` take a `Version.ID`; a restore is a server-side copy of that version over the key, so a disk with `CustomerKey` or `EncryptionScope` does not declare `VersionRead`. `*azure.Driver` also offers `ObjectInfo(ctx, key)` (archive status, rehydrate priority, version, stored MD5, hold, policy mode), which cleans the key like the Disk but bypasses a scoped disk's prefix and is reached by keeping the `*azure.Driver` passed to `NewDisk`. A read of an archived or rehydrating blob is `CodeConflict` matching `azure.ErrArchived`.

## Behaviour

- `Write` stages blocks of `BlockSize`, `Concurrency` at a time, and commits the block list at the end; a body smaller than one block is read whole and sent as one Put Blob (one of exactly one block is staged and committed). Either way a failing body produces no blob. `IfNotExists` is `If-None-Match: *` on the commit.
- Reads send `Accept-Encoding: identity`, so gzip-encoded blobs read back as stored. A ranged read past the end is an empty read.
- Metadata names are returned lower-cased (Azure may change their case).
- `List` uses the flat listing (recursive) or the hierarchy listing with `/`; the cursor is Azure's marker.
- `Copy` starts a server-side copy and polls every `CopyPollInterval` (50 ms by default) until it finishes.
- `Presign` is a service SAS (shared key) or a user delegation SAS (`UserDelegation`): GET grants read (with optional content type and disposition), PUT grants create and write and needs the `x-ms-blob-type: BlockBlob` header from `SignedRequest.Header`. HTTPS only, except for `http://` endpoints. Azure signs no request headers, so a PUT does not pin `Content-Type`: the client must send the one in `SignedRequest.Header`, and `Stat` reports what was stored, so `upload.Confirm` refuses an upload whose stored type differs from its bytes. For the same reason `PresignHeaders` is never declared: a signed PUT with `PresignOptions.Headers` is `CodeUnsupported`.
- User delegation: the key is requested for `MaxUserDelegationKeyLifetime` less five minutes (Azure's limit, with room for clock skew), starting five minutes back, and reused while it lasts until the URL's expiry less an hour; a URL is clamped to its key's expiry (`SignedRequest.ExpiresAt` reports it), so a link asking for close to seven days may end up to about an hour early; concurrent signers share one request, and a failed request is not cached. The identity needs the `generateUserDelegationKey` action (Storage Blob Delegator, or any Storage Blob Data role), otherwise `Presign` is `CodePermission`. The account name is the first label of the endpoint's host, so the endpoint must be `<account>.blob.<suffix>`, unless `AccountName` names it: the driver then fetches the key through a second client whose URL's host is `<AccountName>.invalid` (a reserved name that never resolves, so a missed rewrite fails) (azblob v1.8.2 has no public way to bind a key to another account; the SDK takes it from that first label) and whose requests are rewritten to `Client`'s host, so the key request and the signed URLs still go through the configured front end. Such URLs work only if that front end forwards SAS requests to the account unchanged; Azurite cannot show this, so confirm it against the real front end.
- `RequestOptions` set Azure-only settings for the calls on one key: attach them with `WithRequestOptions(ctx, disk, key, o)` (bound to the key the driver sees, under a scoped disk's prefix; any other key ignores them). `EncryptionScope` overrides `EncryptionScopeFor` and `EncryptionScope` (one tenant's customer-managed scope from a disk without a per-key function; exclusive with `CustomerKey`), `AccessTier` overrides `AccessTier`, `Tags` are stored in the same request as the blob, `LegalHold` and `RetainUntil` are set right after it (as `RetainFor`; `RetainUntil` replaces its expiry), and `RehydrateTier` and `RehydratePriority` override `Config`'s for `Restore` and `SetStorageClass`. `Write`, `CompleteMultipart`, `Copy` and `RestoreVersion` honour every write field, except that a server-side copy refuses `EncryptionScope` (`CodeUnsupported`); a multipart upload takes them from `CreateMultipart`'s context and records them in its marker, so its parts and completion use them whatever context they get; `Append` honours `EncryptionScope` and `Tags` (when it creates the blob); a presigned PUT honours `EncryptionScope`; a field a call cannot honour is `CodeUnsupported`, an unknown tier or priority `CodeInvalidArgument`, both before any request. Without them, every call behaves as before.
- Multipart: parts are staged blocks whose block ID is the part's ETag; completion commits them in order with the options recorded at creation. Each upload keeps a marker blob under `MultipartDir` (`.filesystem-multipart`), a reserved prefix (`CodeInvalidKey`) skipped by listings. `UploadPart` reads a non-seekable body into memory. `AbortMultipart` deletes the marker; Azure discards uncommitted blocks after seven days.
- `SweepAbandoned(ctx, before)` handles uploads created before `before` (a marker records only when its upload started). When the target blob does not exist and every uncommitted block on it is the upload's own, it discards the blocks by committing an empty list (`If-None-Match: *`, tagged with the upload ID) and deleting that empty blob (`If-Match` its ETag, on a context that survives cancellation, for up to 30 seconds); the empty blob is visible at the key between the two, and if the delete fails the marker is kept so the next sweep deletes the tagged blob; otherwise the blocks are left to Azure's seven-day expiry, because a commit would rewrite a stored blob or destroy another writer's blocks. Then it deletes the marker, after which the upload is `CodeNotFound`. Other blobs under the prefix and stored blobs are never touched. `FindAbandoned(ctx, before)` lists the same uploads (ID, start time, key from the marker) and changes nothing.
- `Native()` returns the `*azblob.Client` for `filesystem.As[*azblob.Client]`.

## CORS

`azure.SetCORS(ctx, client, rules...)` replaces the CORS rules of the account's blob service (account-wide, at most five); `azure.DirectUploadCORS(origins...)` is the recommended rule for browsers using this driver's signed URLs. A setup step with admin credentials, not part of the Disk. See the guide.

`azure.DelegatePrefix(azure.DelegateOptions{...})` (its `Clock` dates the expiry, `time.Now` by default) signs a `ScopedSAS` for one directory (`sr=d`, hierarchical-namespace accounts) or, with `WholeContainer`, one container, read-only unless `Write`, with an expiry, printed redacted. For its `UserDelegation`, `drv.UserDelegationCredential(ctx, until)` returns the driver's own cached key and its expiry, bound to `AccountName` when set, so a prefix SAS behind a custom domain signs for the right account (`CodeUnsupported` without `UserDelegation`). `PresignOptions.AllowedIP` becomes the SAS `sip` range (`PresignIP`, IPv4 only). Lifecycle and object replication policies are management-plane settings outside this adapter. See the guide.

## Checksums

`ChecksumSHA256` is not declared: Azure verifies MD5 and CRC64, not SHA-256; the Disk checks `WriteOptions.SHA256` itself.

## Errors

| Azure | Code |
|---|---|
| `BlobNotFound`, `ContainerNotFound`, 404 | `CodeNotFound` |
| `BlobImmutableDueToPolicy`, and any other refusal whose code names an immutability policy or legal hold, whatever the operation | `CodePermission` |
| `BlobArchived`, `BlobBeingRehydrated` | `CodeConflict`, matching `azure.ErrArchived` |
| `InvalidBlobType` (appending to a block blob) | `CodeConflict` |
| `ConditionNotMet`, `BlobNotFound`, `InvalidBlockList` on a write with `IfMatch` | `CodeConflict` |
| `ConditionNotMet`, `BlobAlreadyExists`, 409, 412 | `CodeExists` |
| `InvalidBlockList`, `InvalidBlockID` | `CodeInvalidArgument` |
| 401, 403 | `CodePermission` |
| 429, 5xx | `CodeUnavailable` (with the response's `Retry-After`, when present, on the error: `filesystem.RetryAfterOf`) |
| other | `CodeUnknown` |

## Tests

There is no in-process Azure fake. `task test:azure` starts Azurite in Docker and runs the driver's tests and the conformance suite against it; without `FILESYSTEM_AZURE_TEST_CONNECTION` the server tests skip. Set that variable to a real account's connection string to run them against Azure. `delegation_test.go` needs no server: it fakes Get User Delegation Key over TLS and checks the bearer token, the `skoid`/`sktid`/`ske` fields, key reuse and renewal, and `TokenOpener`. That Azure accepts the signature, and which role assignments a real identity needs, can only be checked against a real account (Azurite does not issue user delegation keys over its path-style endpoint). `advanced_test.go` needs no server either: a scripted Blob service records what the driver sends for customer-provided keys, legal holds, immutability policies, tiers, rehydration, versions and swap conflicts, none of which Azurite implements; their effect in Azure needs a real account.
