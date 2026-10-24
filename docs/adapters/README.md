# Storage backends

Every backend is a driver behind the same `*filesystem.Disk`, so application code does not change when the backend does: a local directory in development, S3 or R2 in production, an SFTP server for one partner's drop folder. This page compares what each driver can do and helps you choose. Each backend then has its own page with configuration, credentials, quirks and a runnable program.

| Backend | Package | Module | DSN scheme | Guide |
|---|---|---|---|---|
| Local directory | `github.com/ubgo/filesystem/local` | core | `file://` | [local.md](local.md) |
| In memory | `github.com/ubgo/filesystem/memory` | core | `mem://` | [memory.md](memory.md) |
| Amazon S3 and S3-compatible stores | `github.com/ubgo/filesystem/adapters/s3` | its own | `s3://` | [s3.md](s3.md), [s3-compatible.md](s3-compatible.md) |
| Google Cloud Storage | `github.com/ubgo/filesystem/adapters/gcs` | its own | `gs://` | [gcs.md](gcs.md) |
| Azure Blob Storage, Azurite | `github.com/ubgo/filesystem/adapters/azure` | its own | `azblob://` | [azure.md](azure.md) |
| SFTP servers | `github.com/ubgo/filesystem/adapters/sftp` | its own | `sftp://` | [sftp.md](sftp.md) |
| FTP and FTPS servers | `github.com/ubgo/filesystem/adapters/ftp` | its own | `ftp://`, `ftps://`, `ftps-implicit://` | [ftp.md](ftp.md) |
| WebDAV servers (Nextcloud, ownCloud, many NAS) | `github.com/ubgo/filesystem/adapters/webdav` | its own | `webdavs://`, `webdav://` | [webdav.md](webdav.md) |
| A table in your SQL database | `github.com/ubgo/filesystem/adapters/sqlblob` | its own | none: built from a `*sql.DB` | [sqlblob.md](sqlblob.md) |
| Google Drive | `github.com/ubgo/filesystem/adapters/gdrive` | its own | none: built with the app's OAuth token source | [gdrive.md](gdrive.md) |
| Dropbox | `github.com/ubgo/filesystem/adapters/dropbox` | its own | none: built with the app's OAuth token source | [dropbox.md](dropbox.md) |
| OneDrive and SharePoint (Microsoft Graph) | `github.com/ubgo/filesystem/adapters/onedrive` | its own | none: built with the app's OAuth token source | [onedrive.md](onedrive.md) |
| Vercel Blob | `github.com/ubgo/filesystem/adapters/vercelblob` | its own | `vercelblob://public` or `vercelblob://private`, the token from the environment | [vercelblob.md](vercelblob.md) |
| Cloudinary (raw resources; image or video opt-in) | `github.com/ubgo/filesystem/adapters/cloudinary` | its own | `cloudinary://<api_key>@<cloud>`, the secret from the environment | [cloudinary.md](cloudinary.md) |
| Supabase Storage | the S3 driver | — | `s3://` with Supabase's S3 endpoint | [supabase.md](supabase.md) |

The local and memory drivers are in the core module, which depends on the standard library only. Each cloud driver is a separate module, so the AWS, Google or Azure SDK is downloaded only by applications that import it.

## How to read the matrix

A driver declares what it supports in one `Capabilities` struct (see [capabilities](../concepts/capabilities.md)). The Disk acts on that declaration in one of three ways:

- **Native**: the backend does it in one or a few requests.
- **Fallback**: the Disk does it portably. Copy streams the bytes through your application, Move is Copy then Delete (not atomic), DeletePrefix lists and deletes key by key, DeleteKeys deletes one key at a time. Correct, but slower and more requests.
- **Refused**: there is no honest portable version, so the call fails with `CodeUnsupported`. A write that asks for metadata on SFTP, a per-object public file on Azure, or a conditional write on an S3 provider not declared to honour it never silently loses the option.

Check what a disk can do at run time with `disk.Capabilities()`; it prints as a list such as `[copy,delete-prefix,delete-keys,presign,multipart,presign-part,presign-post,metadata,content-type,headers,presign-headers,checksum-sha256]`.

## The matrix

Everything in this table is read from each driver's `Capabilities` method and exercised by the conformance suite (`filesystemtest.Run`) in that driver's tests.

| Capability | local | memory | s3 | gcs | azure | sftp |
|---|---|---|---|---|---|---|
| Copy | fallback (streams) | native | native (up to 5 GiB per object) | native | native (asynchronous, awaited); streams with `encryption_scope` | fallback (streams) |
| Move | native, atomic rename | native | fallback: copy + delete | fallback: copy + delete | fallback: copy + delete | native, atomic `posix-rename` |
| Per-object visibility | file mode (0644 / 0600 by default) | native | only with `acl=true` (AWS buckets that allow ACLs) | only with `acl=true` (fine-grained buckets) | refused (public access is a container setting) | file mode (0644 / 0600) |
| DeletePrefix | native (removes the directory) | native | native (list, then 1,000 keys per request) | fallback | fallback | native (removes the directory) |
| DeleteKeys | fallback | native | native (1,000 keys per request) | fallback | fallback | fallback |
| SignedURL | with `WithURLSigner` (`signedurl` + `fshttp`) | with `WithURLSigner` | native SigV4 | native V4, with a signing key or keyless through IAM (`signing_account`) | native SAS, with the account key or a user delegation key (Entra ID) | with `WithURLSigner` (`signedurl` + `fshttp`) |
| PublicURL | with `public_url` | with `public_url` | with `public_url` | with `public_url` | with `public_url` | with `public_url` |
| Multipart through the server | native | native | native | refused (large bodies stream through Write as resumable uploads) | native (staged blocks) | refused |
| SweepAbandoned (unfinished uploads) | native | native | opt-in with `sweep_uploads=true` (a bucket lifecycle rule is preferred) | refused (no multipart) | native (upload markers) | native (staged files a crash left) |
| FindAbandoned (the sweep's dry run, also behind the `DryRun` of `jobs.SweepAbandoned`) | native | native | with `sweep_uploads=true` | refused | native | native |
| Real directories (`MakeDir`, `DirExists`) | native | native | none: `MakeDir` writes nothing and `DirExists` is true while a key is under the prefix | none (same) | none (same) | native |
| Presigned parts (browser multipart, Uppy) | refused | refused | native | refused | refused | refused |
| Presigned POST (`PresignPost`, policy enforced by the backend) | refused | refused | native | native, with a signing key or `IAM` | refused | refused |
| Stored metadata | refused | native | native | native | native (names come back lower-cased) | refused |
| Stored content type | derived from the extension | native | native | native | native | derived from the extension |
| Stored headers (Cache-Control, Content-Disposition, Content-Encoding) | refused | native | native | native | native | refused |
| Conditional write (`IfNotExists`) | native (hard link) | native | only with `conditional_write=true` | native | native | native when the server offers `hardlink@openssh.com` |
| Per-disk encryption key | n/a | n/a | `sse`, `sse_kms_key_id` (SSE-S3, SSE-KMS); a KMS key per tenant with `Config.SSEKMSKeyFor`, per write with `RequestOptions.SSEKMSKeyID` | `kms_key` (CMEK) | `encryption_scope` | n/a |
| Backend-verified SHA-256 (`ChecksumSHA256`) | n/a (the Disk checks) | n/a (the Disk checks) | native for writes of known size up to 5 GiB | refused (GCS verifies CRC32C and MD5) | refused (Azure verifies MD5 and CRC64) | n/a (the Disk checks) |
| Other checksums the backend stores and verifies (read without downloading) | none | none | one per disk with `checksum=CRC32C` (or `CRC32`, `CRC64NVME`, `SHA1`, `SHA256`), verified on write and reported by `Stat` under `s3:checksum-<algorithm>` ([S3 step 14](s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums)) | CRC32C on every upload and MD5 on non-composite objects, read with `Driver.ObjectInfo` ([GCS checksums](gcs.md#checksums)) | per-block CRC64 with `ValidateCRC64` (not kept); a stored Content-MD5 read with `Driver.ObjectInfo` when the uploader set one ([Azure checksums](azure.md#checksums)) | none |
| Atomic replace on Write | yes (staged file, fsync, rename) | yes | yes | yes (a failed body commits nothing) | yes (blocks committed at the end) | yes (staged file, rename) |
| Escape hatch `filesystem.As[T]` | `*os.Root` | none | `*s3.Client` | `*storage.Client` | `*azblob.Client` | `*sftp.Client` |

None of the drivers in this table declares the `PublicURL` capability: for them a public URL always comes from a base you configure (`public_url` in the DSN, or `filesystem.WithPublicURL`), typically a CDN or the bucket's public domain. Two hosted drivers build public URLs themselves and declare it: [Vercel Blob](vercelblob.md) on a public store (`AccessPublic`), and [Cloudinary](cloudinary.md) for the `upload` delivery type. See [public and private files](../concepts/public-and-private.md) and [serving files](../concepts/serving-files.md).

## Choosing a backend

| You need | Use | Why |
|---|---|---|
| Development on a laptop, or a single server with a disk | [local](local.md) | No services to run; atomic writes, real file modes, signed URLs through your own handler |
| Unit and integration tests | [memory](memory.md), or `filesystemtest.NewFake` | Every capability that needs no network, nothing on disk, a movable clock |
| Production object storage on AWS | [s3](s3.md) | Every capability including presigned browser multipart uploads; batch deletes |
| Cheaper egress, or self-hosted storage | [s3](s3.md) with a provider from [s3-compatible.md](s3-compatible.md) | R2, B2, Wasabi, Spaces, SeaweedFS, Garage, MinIO through the same driver |
| Google Cloud | [gcs](gcs.md) | V4 signed URLs, conditional writes always on, resumable uploads |
| Azure | [azure](azure.md) | SAS URLs, conditional writes, server-side multipart |
| Exchanging files with a partner or a NAS | [sftp](sftp.md) | Atomic writes and moves over SSH, host key verification |
| An FTP-only partner, or a NAS speaking WebDAV | [ftp](ftp.md), [webdav](webdav.md) | Staged uploads then a rename or MOVE; FTPS and HTTPS |
| Small files kept with your rows, no object store to run | [sqlblob](sqlblob.md) | One table on the database you already have; ranged reads, compare-and-swap |
| Files in a user's own cloud drive | [gdrive](gdrive.md), [dropbox](dropbox.md), [onedrive](onedrive.md) | The app's OAuth token; no presigned URLs |

Two questions settle most choices:

1. **Will browsers upload directly to storage?** Single-request direct uploads (`upload.Presign` plus `upload.Confirm`) work on every driver with signed URLs. Browser multipart uploads through Uppy's S3 plugin (`upload.Uppy`) need presigned parts, which only the S3 driver has. See [uploads](../concepts/uploads.md) and [browser direct uploads](../recipes/browser-direct-uploads.md).
2. **How is "public" expressed?** Per object (local and SFTP file modes, S3 or GCS ACLs) or per bucket/container (most S3 providers, GCS uniform access, Azure). For the second kind, keep public and private files on two disks. See [public and private files](../concepts/public-and-private.md).

## Moving between backends

A DSN change switches the backend; keys, the Disk API and the records module stay the same. When you switch, check the matrix rows your code relies on: a call that was native on one backend may be a fallback (slower) or refused (`CodeUnsupported`) on another. Run `disk.Check(ctx)` at start-up to prove the credentials and bucket work before the first user request. Copying an existing store is covered in [move to S3](../recipes/move-to-s3.md), and writing every file to two backends in [backups and mirrors](../recipes/backups-and-mirrors.md).

## Every DSN parameter

Each backend page lists its own DSN parameters. Four more are handled by the Mux for every scheme: `prefix` (scope the disk to a key prefix), `public_url`, `visibility` (the disk's default visibility) and `readonly`. They are described in [configuration](../concepts/configuration.md), and every parameter of every scheme is in the [DSN reference](../reference/dsn.md). An unknown parameter is refused with `CodeInvalidArgument` at start-up, so a typo cannot be silently ignored.

## Next

- [Capabilities](../concepts/capabilities.md): the contract behind the matrix.
- [Write a driver](../extending/write-a-driver.md): add a backend and prove it with the same conformance suite.
- [Capabilities reference](../reference/capabilities.md): every field and the Disk method it governs.
