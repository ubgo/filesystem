# DSN parameters

Every parameter every DSN accepts: the four the `Mux` handles for all schemes, then each adapter's own. It is read from `mux.go` and each adapter's `opener.go`. For how DSNs fit into configuration, read [Configuration](../concepts/configuration.md).

## How a DSN is read

`mux.Open(ctx, name, dsn, opts…)` parses the DSN as a URL, picks the opener registered for its scheme, removes the Mux's own parameters, and hands the rest to the opener. Every opener refuses a parameter it does not know with `invalid_argument` (`unknown DSN parameter "endpiont" for scheme "s3"`), so a typo fails at start-up. The Mux's own errors never contain the DSN, because DSNs can carry account names and paths you would rather not log; an opener's error may name the offending part (a parameter, a host). No built-in DSN carries a secret.

| Scheme | Opener | Module |
|---|---|---|
| `file` | `local.Opener` | core (`filesystem/local`) |
| `mem` | `memory.Opener` | core (`filesystem/memory`) |
| `s3` | `s3.Opener` | `filesystem/adapters/s3` |
| `gs` | `gcs.Opener` | `filesystem/adapters/gcs` |
| `azblob` | `azure.Opener` | `filesystem/adapters/azure` |
| `sftp` | `sftp.Opener` | `filesystem/adapters/sftp` |
| `ftp`, `ftps`, `ftps-implicit` | `ftp.Opener` | `filesystem/adapters/ftp`; parameters in [FTP](../adapters/ftp.md) |
| `webdav`, `webdavs` | `webdav.Opener` | `filesystem/adapters/webdav`; parameters in [WebDAV](../adapters/webdav.md) |
| `cloudinary` | `cloudinary.Opener` | `filesystem/adapters/cloudinary` |
| `vercelblob` | `vercelblob.Opener` | `filesystem/adapters/vercelblob` |

Nothing is registered by importing a package: the application calls `mux.Register(s3.Scheme, s3.Opener)` for each scheme it uses. Registering a scheme twice is `exists`.

## Parameters every DSN accepts

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `prefix` | `filesystem.ParamPrefix` | a directory, cleaned like a key | scopes the disk to that directory, as `disk.Scope` does; keys on the disk are relative to it |
| `public_url` | `filesystem.ParamPublicURL` | an absolute `http` or `https` URL, URL-encoded | `WithPublicURL`: `PublicURL` returns this base plus the escaped key; with `prefix`, the prefix is appended to the base |
| `visibility` | `filesystem.ParamVisibility` | `public` or `private` | `WithDefaultVisibility`: the visibility of writes that do not choose one |
| `readonly` | `filesystem.ParamReadOnly` | a boolean (`true`, `false`, `1`, `0`, …) | `true` adds the `ReadOnly` interceptor: every mutation is `read_only` |

Options passed to `Open` (or to `OpenManager`) apply after these, so code can add interceptors or a signer to a configured disk.

## file (local disk)

`file:///var/app/uploads` (absolute) or `file:uploads` (relative to the working directory). The directory is created if missing.

| Parameter | Effect |
|---|---|
| none | the local driver takes no parameters beyond the Mux's |

A host (`file://uploads`, two slashes) is refused with `file DSN has a host "uploads"; use file:///absolute/path or file:relative/path`; an empty path is refused. File modes are set in code with `local.WithPermissions`, not in the DSN.

## mem (memory)

`mem://` or `mem://cache` (the host is ignored and only names it for readers). Every `Open` gets a fresh, empty driver.

| Parameter | Effect |
|---|---|
| none | the memory driver takes no parameters beyond the Mux's |

## s3 (S3 and S3-compatible stores)

`s3://bucket?region=…`. Without credential parameters, credentials come from the AWS default chain (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_PROFILE`, shared config files, instance and task roles). `profile` or the `*_env` parameters give one disk credentials of its own, so two disks in one process can use two accounts or providers. Either way the DSN only names where a secret lives, never holds one: credentials in the URL (`s3://key:secret@bucket`) are refused.

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `region` | `s3.ParamRegion` | a region, `auto` for R2 | the client's region; without it, the default chain's |
| `endpoint` | `s3.ParamEndpoint` | a URL with its scheme, URL-encoded | the S3 endpoint of a non-AWS provider (`https://<account>.r2.cloudflarestorage.com`, `http://127.0.0.1:8333`) |
| `path_style` | `s3.ParamPathStyle` | boolean | addresses the bucket in the path instead of the host name; most self-hosted stores need it |
| `acl` | `s3.ParamACL` | boolean | `s3.VisibilityACL`: canned ACLs per object, and the `SetVisibility` capability; only for AWS buckets that allow ACLs |
| `conditional_write` | `s3.ParamConditionalWrite` | boolean | `Config.ConditionalWrites`: sends `If-None-Match: *` for `IfNotExists` and declares `ConditionalWrite`; only for providers that honour it |
| `presign_headers` | `s3.ParamPresignHeaders` | boolean, default `true` | `false` sets `Config.UnverifiedSignedHeaders` and withdraws `PresignHeaders`, for stores that do not verify SigV4 signatures (a SeaweedFS started without `-s3.config` accepts a PUT that leaves a signed header out) |
| `sse` | `s3.ParamSSE` | `AES256`, `aws:kms`, … | `Config.ServerSideEncryption`, sent on every write |
| `sse_kms_key_id` | `s3.ParamSSEKMSKeyID` | a KMS key ID or ARN | `Config.SSEKMSKeyID`: the KMS key for `aws:kms` (implied when `sse` is omitted); refused together with `AES256`; sent on every write, copy and multipart upload, and signed into presigned PUTs |
| `sweep_uploads` | `s3.ParamSweepUploads` | boolean, default `false` | `true` declares `SweepAbandoned` and its dry run `FindAbandoned`: `Disk.SweepAbandoned` aborts every multipart upload in the bucket started before the cutoff (bucket-wide; needs `s3:ListBucketMultipartUploads` and `s3:AbortMultipartUpload`); a bucket lifecycle rule (`AbortIncompleteMultipartUpload`) is still the preferred way |
| `storage_class` | `s3.ParamStorageClass` | `STANDARD_IA`, `INTELLIGENT_TIERING`, … | `Config.StorageClass`, sent on every write |
| `part_size` | `s3.ParamPartSize` | bytes, an integer | `Config.PartSize`: the multipart part size for large writes; 0 or absent means the SDK default (8 MiB) |
| `prune_prefixes` | `s3.ParamPruneEmptyPrefixes` | boolean | `Config.PruneEmptyPrefixes`: a non-recursive list drops directories with no objects left, at one extra request per directory; needed on SeaweedFS |
| `profile` | `s3.ParamProfile` | a profile name | credentials (and settings) from that profile of the shared AWS files, for this disk only; assume-role and SSO profiles work; exclusive with the `*_env` parameters |
| `access_key_env` | `s3.ParamAccessKeyEnv` | an environment variable name | this disk's access key ID, read from the variable when the disk opens; needs `secret_key_env` |
| `secret_key_env` | `s3.ParamSecretKeyEnv` | an environment variable name | this disk's secret access key, read the same way; needs `access_key_env` |
| `session_token_env` | `s3.ParamSessionTokenEnv` | an environment variable name | a session token for temporary keys; optional |
| `role_arn` | `s3.ParamRoleARN` | an IAM role ARN | `RoleOptions.RoleARN`: the disk works through that role in a customer's account (bring your own bucket); the credentials above (or the default chain) are the application's own and only assume it; the opener assumes it once, so a refusal is `permission` at start-up |
| `external_id` | `s3.ParamExternalID` | the tenant's external ID | `RoleOptions.ExternalID`, the confused-deputy guard the role's trust policy checks; required with `role_arn`; never printed in errors |
| `role_session_name` | `s3.ParamRoleSessionName` | a name, e.g. the tenant ID | `RoleOptions.SessionName`, shown in the customer's CloudTrail; required with `role_arn` |
| `role_duration` | `s3.ParamRoleDuration` | a Go duration, 15m to 12h | `RoleOptions.Duration`: lifetime of each set of role credentials, renewed automatically; default `1h` (`s3.DefaultRoleDuration`) |
| `storage_classes` | `s3.ParamStorageClasses` | boolean | `Config.StorageClasses`: declares `StorageClass` |
| `tagging` | `s3.ParamTagging` | boolean | `Config.Tagging`: declares `Tags` |
| `object_lock` | `s3.ParamObjectLock` | boolean | `Config.ObjectLock`: declares `Hold`; the opener checks the bucket has Object Lock |
| `object_lock_mode` | `s3.ParamObjectLockMode` | `GOVERNANCE`, `COMPLIANCE` | `Config.ObjectLockMode`, default `GOVERNANCE`; implies `object_lock` |
| `object_lock_days` | `s3.ParamObjectLockDays` | days, an integer | `Config.ObjectLockDays`: a retention every write gets; implies `object_lock` |
| `max_retention_days` | `s3.ParamMaxRetentionDays` | days, an integer | `Config.MaxRetention`: the longest retention this disk sets, default 3650 (`s3.DefaultMaxRetention`); a later date is refused |
| `versioning` | `s3.ParamVersioning` | boolean | `Config.Versioning`: declares `Versions` and `VersionRead`; the opener checks the bucket is versioned |
| `restore` | `s3.ParamRestore` | boolean | `Config.Restore`: declares `Restore` for archive storage classes |
| `restore_tier` | `s3.ParamRestoreTier` | `Standard`, `Bulk`, `Expedited` | `Config.RestoreTier`, default `Standard` |
| `sse_c_key_env` | `s3.ParamSSECKeyEnv` | an environment variable name | `Config.SSECustomerKey`: the base64 32-byte SSE-C key, read from that variable (never the key itself in the DSN) |
| `presign_customer_key` | `s3.ParamPresignCustomerKey` | boolean | `Config.PresignCustomerKey`: signed URLs under SSE-C, each carrying the disk-wide key |
| `presign_without_checksum` | `s3.ParamPresignWithoutChecksum` | boolean | `Config.PresignWithoutChecksum`: signed URLs under `checksum`, whose uploads carry no checksum |
| `checksum` | `s3.ParamChecksum` | `CRC32`, `CRC32C`, `CRC64NVME`, `SHA1`, `SHA256` | `Config.ChecksumAlgorithm`; without it, the SDK default |
| `concurrency` | `s3.ParamConcurrency` | a positive integer | `Config.Concurrency`: the parts of one multipart write in flight at once; each holds `part_size` bytes in memory |
| `expected_bucket_owner` | `s3.ParamExpectedBucketOwner` | a 12-digit AWS account ID | `Config.ExpectedBucketOwner`: sent on every request and signed into presigned URLs, so S3 answers 403 if the bucket changed hands; set it for a bring-your-own bucket |
| `request_payer` | `s3.ParamRequestPayer` | `requester` | `Config.RequestPayer`: accepts the charges of a requester-pays bucket on every request and in presigned URLs |

Keys read through the `*_env` parameters print as a placeholder, so logging the client or the disk with `%v` does not leak them. Refused with `invalid_argument`, naming the parameter or variable and never a value: `profile` together with a `*_env` parameter, only one of `access_key_env` and `secret_key_env`, a named variable that is unset or empty, and a profile the shared files do not contain.

An empty bucket (`s3://?region=…`) is refused with `s3 DSN has no bucket`. Booleans and `part_size` that do not parse are refused naming the parameter. Provider by provider, with the values each needs: [S3-compatible stores](../adapters/s3-compatible.md).

A KMS key per tenant on one disk (`Config.SSEKMSKeyFor`, a function of the key) and a per-write key (`RequestOptions.SSEKMSKeyID`) are code, not DSN parameters; see [s3](../adapters/s3.md).

## gs (Google Cloud Storage)

`gs://bucket`. Credentials come from Application Default Credentials (`GOOGLE_APPLICATION_CREDENTIALS`, the metadata server, `gcloud auth application-default login`), never from the DSN.

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `endpoint` | `gcs.ParamEndpoint` | a URL | overrides the API endpoint, for an emulator such as fake-gcs-server; the SDK also honours `STORAGE_EMULATOR_HOST` |
| `acl` | `gcs.ParamACL` | boolean | `gcs.VisibilityACL`: per-object ACLs and the `SetVisibility` capability, for buckets with fine-grained access |
| `signing_key` | `gcs.ParamSigningKey` | a path to a service account JSON key | signs V4 URLs with the key's `client_email` and `private_key`, enabling `Presign`; without it there are no signed URLs |
| `no_auth` | `gcs.ParamNoAuth` | boolean | sends no credentials, for emulators |
| `signing_account` | `gcs.ParamSigningAccount` | a service account email, or `default` (`gcs.SigningAccountDefault`) | signs V4 URLs without a key file: each URL is signed by the IAM Credentials API (`signBlob`) as that account, enabling `Presign`. `default` is the account the process runs as (Cloud Run, GKE, GCE), read from the metadata server once when the disk opens. The runtime identity needs `roles/iam.serviceAccountTokenCreator` on the account. Exclusive with `signing_key` |
| `kms_key` | `gcs.ParamKMSKey` | a Cloud KMS key name (`projects/…/cryptoKeys/…`) | `Config.KMSKeyName`: customer-managed encryption (CMEK) for every write and copy, signed into presigned PUTs; the bucket's service agent needs the encrypter/decrypter role on the key |
| `versioned` | `gcs.ParamVersioned` | boolean | `Config.Versioned`: declares `Versions` and `VersionRead` for a bucket with object versioning on |
| `customer_key_env` | `gcs.ParamCustomerKeyEnv` | an environment variable name | `Config.CustomerKey`: the base64 customer-supplied encryption key (CSEK), read from that variable (never the key itself in the DSN) |
| `credentials_file` | `gcs.ParamCredentialsFile` | a path to a service account JSON key | authenticates with that key instead of Application Default Credentials, so two `gs://` disks in one process can act as two accounts; only service account keys are accepted; exclusive with `credentials_env` and `no_auth` |
| `credentials_env` | `gcs.ParamCredentialsEnv` | an environment variable name | the same, with the key's JSON read from the variable (never the key itself in the DSN) |

A file that is not a service account key is refused with `gs: signing_key is not a service account key`. `signing_key` together with `signing_account`, and `signing_account=default` without a metadata server (or one that reports no account), are refused with `invalid_argument`; a metadata server answering with an error is `unavailable`.

## azblob (Azure Blob Storage, Azurite)

`azblob://container`. The account name and key come from a connection string in the environment, never from the DSN.

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `connection_env` | `azure.ParamConnectionEnv` | an environment variable name | the variable holding the connection string; default `AZURE_STORAGE_CONNECTION_STRING` (`azure.DefaultConnectionEnv`) |
| `encryption_scope` | `azure.ParamEncryptionScope` | an encryption scope name | `Config.EncryptionScope`: every write, part and commit uses that scope, and presigned PUT SASes carry it; such a disk does not declare `Copy` (the copy streams through a write so the scope applies) |
| `access_tier` | `azure.ParamAccessTier` | `Hot`, `Cool`, `Cold`, `Archive` | `Config.AccessTier`: the tier new blobs get (empty means the account default); also accepted by `azure.TokenOpener` |
| `versioned` | `azure.ParamVersioned` | boolean | `Config.Versioned`: declares `Versions` and `VersionRead` for an account with blob versioning on; also accepted by `azure.TokenOpener` |
| `customer_key_env` | `azure.ParamCustomerKeyEnv` | an environment variable name | `Config.CustomerKey`: the base64 customer-provided key, read from that variable (never the key itself in the DSN); also accepted by `azure.TokenOpener` |
| `block_size` | `azure.ParamBlockSize` | bytes, an integer | `Config.BlockSize`: the size of each staged block, in `[azure.MinBlockSize, azure.MaxBlockSize]`; absent means 8 MiB (`azure.DefaultBlockSize`). A blob holds at most 50,000 blocks, so it caps one write at about 390.6 GiB by default; a body smaller than it is sent as one Put Blob. Also accepted by `azure.TokenOpener` |
| `concurrency` | `azure.ParamConcurrency` | an integer | `Config.Concurrency`: how many blocks one write stages at once; absent means 5 (`azure.DefaultConcurrency`). A write holds up to `concurrency` times `block_size` in memory. Also accepted by `azure.TokenOpener` |

The connection string must carry `AccountName` and `AccountKey` (a shared key, which also signs SAS URLs). `BlobEndpoint` selects the endpoint (Azurite: `http://127.0.0.1:10000/devstoreaccount1`); without it the endpoint is `<DefaultEndpointsProtocol, default https>://<AccountName>.blob.<EndpointSuffix, default core.windows.net>`. A missing name or key is refused with `azure: AZURE_STORAGE_CONNECTION_STRING has no AccountName and AccountKey`.

### azblob with Entra ID (`azure.TokenOpener`)

`azure.Opener` knows only shared keys. For Microsoft Entra ID, register `azure.TokenOpener(cred)` with a credential the application builds (`azidentity.NewDefaultAzureCredential`, a managed or workload identity): `mux.Register(azure.Scheme, azure.TokenOpener(cred))`. The credential is code, so the DSN does not name it.

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `account` | `azure.ParamAccount` | a storage account name | endpoint `https://<account>.blob.core.windows.net/`; with `endpoint` too, the account the SAS URLs are signed for (`Config.AccountName`) |
| `endpoint` | `azure.ParamEndpoint` | a URL | the blob endpoint, for a sovereign cloud: `https://<account>.blob.<suffix>`. Alone, any other host (a custom domain, http, an IP, a port or a path) is refused, because a user-delegation SAS is then signed for the account named by the host's first label; with `account` too, any https URL is allowed (a custom domain, Front Door, a private-endpoint name) and the SAS is signed for that account |
| `encryption_scope` | `azure.ParamEncryptionScope` | an encryption scope name | as for `azblob://` with a connection string |; `access_tier`, `versioned`, `customer_key_env`, `block_size` and `concurrency` too |

At least one of `account` and `endpoint` is required; neither, an `endpoint` that is not https when both are given, a nil credential, or `connection_env` is refused with `invalid_argument`. Both together are for an endpoint behind a front end: signed URLs keep the endpoint's host, so they work only if that front end forwards SAS requests to the account unchanged. These disks sign user delegation SAS URLs: the driver requests a user delegation key on the first `SignedURL` and reuses it for up to seven days. The identity needs the `generateUserDelegationKey` action (Storage Blob Delegator, or any Storage Blob Data role); without it `SignedURL` is `permission`.

## sftp (SFTP servers)

`sftp://user@host:port/absolute/root`; the port defaults to 22, and the path is the root directory of the disk. A password in the DSN (`user:pass@host`) is refused.

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `known_hosts` | `sftp.ParamKnownHosts` | a path to a `known_hosts` file | verifies the server's host key; required unless `insecure_ignore_host_key=true` |
| `insecure_ignore_host_key` | `sftp.ParamInsecure` | boolean | skips host key verification; only for tests, it allows a man in the middle |
| `identity` | `sftp.ParamIdentity` | a path to a private key file | public-key authentication |
| `password_env` | `sftp.ParamPasswordEnv` | an environment variable name | password authentication with the variable's value |
| `identity_passphrase_env` | `sftp.ParamIdentityPassphraseEnv` | an environment variable name | the passphrase of an encrypted `identity` key; without it an encrypted key is `invalid_argument` |
| `agent` | `sftp.ParamAgent` | boolean | authenticates with the keys of the running ssh-agent, through the socket `SSH_AUTH_SOCK` names, so keys the agent holds never touch the DSN or the disk; `true` without `SSH_AUTH_SOCK` is `invalid_argument` |

At least one of `identity`, `password_env` and `agent=true` is required. Dialling failures are `unavailable`; a refused handshake or authentication is `permission`.

## ftp, ftps, ftps-implicit (FTP and FTPS servers)

`ftp://user@host:port/absolute/root`, `ftps://` for explicit FTPS (`AUTH TLS`), or `ftps-implicit://` for implicit FTPS (TLS from the first byte); register each scheme (`ftp.Scheme`, `ftp.SchemeSecure`, `ftp.SchemeImplicit`) with `ftp.Opener`. The port defaults to 21, and to 990 for `ftps-implicit`. Both FTPS schemes check the certificate against the system roots or `ca_file`, and keep a TLS session cache so data connections resume the control connection's session (vsftpd's default `require_ssl_reuse`). A password in the DSN is refused. Full guide: [ftp](../adapters/ftp.md).

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `password_env` | `ftp.ParamPasswordEnv` | an environment variable name | the password, read from the variable; needs a user |
| `max_conns` | `ftp.ParamMaxConns` | a positive integer | `Config.MaxConns`: the size of the connection pool |
| `ca_file` | `ftp.ParamCAFile` | a path to a PEM bundle | the certificate authorities that verify the server instead of the system roots (an internal CA, a self-signed server); FTPS schemes only |

## webdavs, webdav (WebDAV servers)

`webdavs://user@host/path` (https) or `webdav://…` (http); register both schemes (`webdav.SchemeSecure`, `webdav.Scheme`) with `webdav.Opener`. A password in the DSN is refused. Full guide: [webdav](../adapters/webdav.md).

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `password_env` | `webdav.ParamPasswordEnv` | an environment variable name | the Basic authentication password, read from the variable; needs a user |
| `insecure_http` | `webdav.ParamInsecureHTTP` | boolean | `Config.AllowInsecureHTTP`: allows `webdav://` to a host other than loopback, which sends the password readable |

## cloudinary (Cloudinary)

`cloudinary://<api_key>@<cloud_name>?api_secret_env=<VAR>`, Cloudinary's own `CLOUDINARY_URL` shape without the secret; register it with `mux.Register(cloudinary.Scheme, cloudinary.Opener)`. A secret in the URL, a path, or a missing or empty secret variable is `invalid_argument`. The opener makes no request, so wrong credentials surface at the first call. Full guide: [Cloudinary](../adapters/cloudinary.md).

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `api_secret_env` | `cloudinary.ParamSecretEnv` | an environment variable name | the API secret, read from the variable; required |
| `type` | `cloudinary.ParamType` | `authenticated`, `upload`, `private` | `Config.Type`, the delivery type of stored resources; default `authenticated` |
| `resource_type` | `cloudinary.ParamResourceType` | `raw`, `image`, `video` | `Config.ResourceType`, the resource type keys are stored as; default `raw` |
| `algorithm` | `cloudinary.ParamAlgorithm` | `sha1`, `sha256` | `Config.Algorithm`, the account's signature algorithm; default `sha1` |

## vercelblob (Vercel Blob)

`vercelblob://public` or `vercelblob://private`, the host being the store's access mode; register it with `mux.Register(vercelblob.Scheme, vercelblob.Opener)`. The token is read from an environment variable, never the DSN: credentials in the URL, a path, another access mode or an empty token variable is `invalid_argument`. Full guide: [Vercel Blob](../adapters/vercelblob.md).

| Parameter | Constant | Value | Effect |
|---|---|---|---|
| `token_env` | `vercelblob.ParamTokenEnv` | an environment variable name | the store's read-write token; default `BLOB_READ_WRITE_TOKEN` (`vercelblob.DefaultTokenEnv`), the variable Vercel sets |
| `cache_max_age` | `vercelblob.ParamCacheMaxAge` | seconds, a positive integer | `Config.CacheMaxAge`: how long Vercel's CDN and browsers may cache a blob this disk writes; absent leaves Vercel's default of one month |

## Disks from the environment

`mux.OpenManager(ctx, prefix, lookup, opts…)` builds a Manager from variables named after `prefix`:

| Variable | Constant | Holds |
|---|---|---|
| `<PREFIX>DISKS` | `filesystem.EnvDisks` | the disk names, comma-separated: `media,backups` |
| `<PREFIX><NAME>_DSN` | `filesystem.EnvDSNSuffix`, built by `filesystem.EnvName(prefix, name)` | each disk's DSN; the name upper-cased, with `-` and `.` as `_` (`media-cdn` reads `STORAGE_MEDIA_CDN_DSN`) |
| `<PREFIX>DEFAULT_DISK` | `filesystem.EnvDefaultDisk` | the default disk; empty means the first listed |

```sh
STORAGE_DISKS=media,backups
STORAGE_MEDIA_DSN=s3://acme-media?region=auto&endpoint=https%3A%2F%2Facct.r2.cloudflarestorage.com&public_url=https%3A%2F%2Fcdn.acme.com&visibility=public
STORAGE_BACKUPS_DSN=file:///var/backups?readonly=true
STORAGE_DEFAULT_DISK=media
```

Errors name the variable, never its value, and an error from opening a disk names the disk too (`STORAGE_MEDIA_DSN: disk media: unknown DSN parameter "regoin" for scheme "s3"`): `STORAGE_ARCHIVE_DSN is not set (disk "archive")`, `STORAGE_DISKS lists no disks`, `STORAGE_DEFAULT_DISK names "x", which is not in STORAGE_DISKS`, and two names that map to one variable (`a-b` and `a.b`). On any failure the disks opened so far are closed. `lookup` is `os.LookupEnv` in production and a map in tests.

## Encoding

A DSN is a URL: values with `:`, `/`, `?`, `&` or `#` are percent-encoded. `url.Values` does it in Go (`q.Set(s3.ParamEndpoint, "http://127.0.0.1:9000")`, then `q.Encode()`); in shell configuration write `endpoint=https%3A%2F%2Facct.r2.cloudflarestorage.com`. Go's URL parser also accepts most unencoded endpoints in a query (`?endpoint=http://127.0.0.1:8333`), which is what the repository's `Taskfile.yml` uses for SeaweedFS, but encoding is the safe form.

## Next

- [Configuration](../concepts/configuration.md): the Manager, the Mux and `OpenManager` step by step.
- [Capabilities](capabilities.md): what each parameter turns on.
- [The fs command-line tool](../integrations/cli.md): every DSN here works as an `fs` location.
