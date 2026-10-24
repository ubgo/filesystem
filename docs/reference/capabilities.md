# Capabilities

Every bit of `filesystem.Capabilities` for every built-in driver, adapter and wrapper, read from each driver's `Capabilities()` method, with what the Disk does when a bit is false. For the model (why capabilities are declared, verified and never silently dropped), read [Capabilities](../concepts/capabilities.md); for the backends themselves, [adapters](../adapters/README.md).

`disk.Capabilities()` returns the bits at run time; `Capabilities.String()` prints the true ones, as in `[metadata,content-type,conditional-write]`.

## The bits

| Bit | Kind | The driver | When false, the Disk |
|---|---|---|---|
| `Copy` | interface `Copier` | copies inside the backend | streams the object: `Stat`, `Read`, `Write`, keeping content type, and metadata, headers and visibility where stored |
| `Move` | interface `Mover` | renames inside the backend, atomically | copies, then deletes the source; a failure between the two leaves both keys |
| `SetVisibility` | interface `VisibilitySetter` | changes one object's visibility, and `Stat` reports it | refuses `SetVisibility`, and a write whose visibility differs from the disk default, with `unsupported`; every object has the disk's default visibility |
| `DeletePrefix` | interface `PrefixDeleter` | deletes a directory's keys in few calls | lists recursively and deletes key by key |
| `DeleteKeys` | interface `KeysDeleter` | deletes many keys per request | deletes key by key, collecting failures in a `*BatchError` |
| `Presign` | interface `Presigner` | signs URLs itself, except the methods a `URLSigner` takes: `WithURLSigner` signs every download even on a presigning disk (a CDN in front of the bucket), and the methods it names when given some ([which path signs which method](../concepts/serving-files.md#which-path-signs-which-method)) | signs with the disk's `URLSigner` if it has one (`WithURLSigner`), else `unsupported` |
| `PublicURL` | interface `PublicURLer` | builds its own public URL | uses the base from `WithPublicURL` or the `public_url` DSN parameter, else `unsupported`; a configured base always wins over the driver |
| `Multipart` | interface `MultipartUploader` | creates, uploads parts through the server, lists, completes and aborts multipart uploads | refuses the multipart methods with `unsupported` |
| `PresignPart` | interface `PartPresigner`, requires `Multipart` | signs a PUT for one part, for browsers | refuses `PresignPart`; `upload.Uppy` answers 501 on every route |
| `SweepAbandoned` | interface `AbandonedSweeper` | removes upload leftovers that are not objects (unfinished multipart uploads, temporary files) older than a cutoff | refuses `SweepAbandoned` with `unsupported` |
| `FindAbandoned` | interface `AbandonedFinder`, requires `SweepAbandoned` | lists what `SweepAbandoned` would remove now, from the same scan, removing nothing (the dry run) | refuses `FindAbandoned` with `unsupported` |
| `Dirs` | interface `DirMaker` | has real directories: `MakeDir` creates an empty one, `DirExists` reports it | `MakeDir` does nothing (no marker object is written); `DirExists` lists the directory with a limit of one entry, so it exists while a key is under it |
| `Metadata` | attribute | stores `WriteOptions.Metadata` and returns it from `Stat` | refuses a write with metadata with `unsupported` |
| `ContentType` | attribute | stores the content type and returns it from `Stat` | still returns the detected or given type from `Write`; `Stat` reports what the driver derives (from the extension, at best) |
| `Headers` | attribute | stores `Cache-Control`, `Content-Disposition` and `Content-Encoding` and sends them when the object is served | refuses a write with headers with `unsupported` |
| `PresignHeaders` | attribute, requires `Presign` and `Headers` | the driver's presigned PUT pins `PresignOptions.Headers`: the backend refuses an upload without them and stores them | a presigned PUT with headers is `unsupported`, unless the disk signs through a `URLSigner` that pins them (`signedurl`) on a driver with `Headers` |
| `PresignIP` | attribute, requires `Presign` | the driver's presigned URLs carry `PresignOptions.AllowedIP` (a canonical CIDR), so the backend refuses a request from outside it | a presigned URL with `AllowedIP` is `unsupported`, unless the disk signs through a `URLSigner` that enforces it (`RestrictsIP`, as `signedurl` with `fshttp.Signed` does); never a URL that works from anywhere |
| `PresignPost` | interface `PostPresigner`, requires `Presign` | signs a browser form upload (POST policy): the backend enforces the key or prefix, size range, content type, stored headers and expiry | refuses `PresignPost` with `unsupported` |
| `PresignVisibility` | attribute, requires `SetVisibility` and `Presign` or `PresignPost` | the driver's presigned PUT and POST form pin `PresignOptions.Visibility` and `PostPolicy.Visibility`: the backend stores the upload with that visibility and refuses a request that changes it | a visibility the disk must pin is `unsupported`; on a disk without `SetVisibility` the default visibility needs no pin and is accepted; a `URLSigner` never pins one |
| `ChecksumSHA256` | attribute | the backend verifies a supplied SHA-256 on write (`WriteOptions.SHA256`) | the Disk still verifies the digest itself and refuses a mismatch before the write commits |
| `ConditionalWrite` | attribute | honours `WriteOptions.IfNotExists` atomically | refuses `IfNotExists` with `unsupported` |
| `VersionRead` | interface `VersionReader`, requires `Versions` | reads one stored version (`Disk.ReadVersion`) and makes it the current content again as a new latest version (`Disk.RestoreVersion`) | refuses both with `unsupported` |
| `StorageClass` | interface `StorageClasser` | moves an object to a storage class named by the backend (`Disk.SetStorageClass`) and reports it in `Entry.StorageClass` | refuses `SetStorageClass` with `unsupported` |

`filesystem.NewDisk` refuses a driver that declares an interface-backed bit without implementing the interface, `PresignPart` without `Multipart`, `FindAbandoned` without `SweepAbandoned`, or `VersionRead` without `Versions`, with `invalid_argument`. Implementing an interface while declaring it false is allowed: the Disk calls an optional method only when its bit is true.

## Per driver

| Bit | local | memory | s3 | gcs | azure | sftp |
|---|---|---|---|---|---|---|
| `Copy` | no | yes | yes | yes | yes | no |
| `Move` | yes | yes | no | no | no | yes |
| `SetVisibility` | yes (file modes) | yes | with `acl=true` | with `acl=true` | no | yes (file modes) |
| `DeletePrefix` | yes | yes | yes | no | no | yes |
| `DeleteKeys` | no | yes | yes (1,000 per request) | no | no | no |
| `Presign` | no | no | yes (SigV4) | with `signing_key` | with a shared key (SAS) | no |
| `PublicURL` | no | no | no | no | no | no |
| `Multipart` | yes | yes | yes | no | yes | no |
| `PresignPart` | no | no | yes | no | no | no |
| `SweepAbandoned` | yes | yes | with `sweep_uploads=true` | no | yes | yes, except with `WriteInPlace` |
| `FindAbandoned` | yes | yes | with `sweep_uploads=true` | no | yes | yes, except with `WriteInPlace` |
| `Dirs` | yes | yes | no | no | no | yes |
| `Metadata` | no | yes | yes | yes | yes | no |
| `ContentType` | no | yes | yes | yes | yes | no |
| `Headers` | no | yes | yes | yes | yes | no |
| `PresignHeaders` | no | no | yes, unless `presign_headers=false` | when signing is configured | no | no |
| `PresignIP` | no | no | no | no | yes (SAS `sip`, IPv4 ranges only) | no |
| `PresignPost` | no | no | yes | with a signing key, `SignBytes` or IAM | no | no |
| `PresignVisibility` | no | no | with `acl=true` (not with `UnverifiedSignedHeaders`) | with `acl=true` and signing configured | no | no |
| `ChecksumSHA256` | no | no | yes (single-request writes up to 5 GiB) | no | no | no |
| `ConditionalWrite` | yes | yes | with `conditional_write=true` | yes | yes | when the server offers `hardlink@openssh.com`, or with `WriteInPlace` (an exclusive create) |
| `Append` | yes | yes | no | no | yes, unless `RetainFor` is set | no |
| `SetModTime` | yes | yes | no | no | no | no |
| `Tags` | no | yes | with `tagging=true` | no | yes (blob index tags) | no |
| `Hold` | no | no | with `object_lock=true` | yes | yes | no |
| `Versions` | no | with `memory.WithVersions()` | with `versioning=true` | with `versioned=true` | with `versioned=true` | no |
| `VersionRead` | no | with `memory.WithVersions()` | with `versioning=true` | with `versioned=true` | with `versioned=true`, not with a customer key or encryption scope | no |
| `StorageClass` | no | yes | with `storage_classes=true` | yes | yes (access tiers) | no |
| `Restore` | no | no | with `restore=true` | no | yes (archive tier) | no |
| `CompareAndSwap` | where the OS reports inode change times (not Windows) | yes | no | yes | yes | no |
| `CaseInsensitive` | probed at `New` | no | no | no | no | probed at `New` |

The adapters below are each a module of their own, read from each driver's `Capabilities()` method the same way:

| Bit | ftp | webdav | sqlblob | gdrive | dropbox | onedrive | vercelblob | cloudinary |
|---|---|---|---|---|---|---|---|---|
| `Copy` | no | yes | yes | yes | yes | no | yes | no |
| `Move` | yes | yes | yes | yes | yes | yes | no | yes |
| `SetVisibility` | no | no | yes | no | no | no | no | no |
| `DeletePrefix` | yes | yes | yes | yes | yes | yes | no | yes |
| `DeleteKeys` | no | no | yes | no | no | no | yes | yes |
| `Presign` | no | no | no | no | no | no | no | no |
| `PublicURL` | no | no | no | no | no | no | when `Config.Access` is `AccessPublic` | when `Config.Type` is `TypeUpload` |
| `Multipart` | no | no | no | no | no | no | no | no |
| `PresignPart` | no | no | no | no | no | no | no | no |
| `SweepAbandoned` | yes, except with `WriteInPlace` | yes, except with `WriteInPlace` | no | no | no | no | no | no |
| `FindAbandoned` | yes, except with `WriteInPlace` | yes, except with `WriteInPlace` | no | no | no | no | no | no |
| `Dirs` | yes | yes | no | no | no | no | no | no |
| `Metadata` | no | no | yes | no | no | no | no | no |
| `ContentType` | no | no | yes | yes | no | no | yes | no |
| `Headers` | no | no | yes | no | no | no | no | no |
| `PresignHeaders` | no | no | no | no | no | no | no | no |
| `PresignIP` | no | no | no | no | no | no | no | no |
| `PresignPost` | no | no | no | no | no | no | no | no |
| `PresignVisibility` | no | no | no | no | no | no | no | no |
| `ChecksumSHA256` | no | no | no | no | no | no | no | no |
| `ConditionalWrite` | no | no | yes | no | yes | yes | no | no |
| `Append` | no | no | no | no | no | no | no | no |
| `SetModTime` | no | no | no | no | no | no | no | no |
| `Tags` | no | no | no | no | no | no | no | no |
| `Hold` | no | no | no | no | no | no | no | no |
| `Versions` | no | no | no | no | no | no | no | no |
| `VersionRead` | no | no | no | no | no | no | no | no |
| `StorageClass` | no | no | no | no | no | no | no | no |
| `Restore` | no | no | no | no | no | no | no | no |
| `CompareAndSwap` | no | no | yes | no | yes | yes | yes | no |
| `CaseInsensitive` | probed at `New` | probed at `New` | no | no | yes | yes | no | no |

Notes, driver by driver:

- **local:** visibility is the file mode's world-read bit (`local.WithPermissions` sets the modes). Writes go to a staging file under `.filesystem` and are renamed into place, so readers never see a partial object; by default the file and then the changed directories are synced (`local.WithDurability`). `Native()` returns the `*os.Root`.
- **memory:** everything that needs no network. No signer of its own; give it one with `WithURLSigner` and `fshttp.Signed`.
- **s3:** `SetVisibility` only in `s3.VisibilityACL` mode (`acl=true`), for AWS buckets that still allow ACLs; the default `VisibilityNone` leaves visibility to the bucket policy. `ConditionalWrite` only with `conditional_write=true` (AWS since 2024, R2, recent MinIO, SeaweedFS), because a store that ignores `If-None-Match` would turn `IfNotExists` into a silent overwrite; `CreateMultipart` refuses `IfNotExists` even then. `Native()` returns the `*s3.Client`.
- **gcs:** `Presign` only with a service account key (`signing_key`), V4 signing. `SetVisibility` only with fine-grained ACLs (`acl=true`); uniform bucket-level access, the GCS default, has none. Large writes use resumable uploads inside `Write`, not the `Multipart` interface. `Native()` returns the `*storage.Client`.
- **azure:** `Presign` only with a shared-key credential (the opener always has one), as SAS URLs. Visibility is a container setting, never per blob. Multipart is block-based; unfinished uploads keep a marker under `.filesystem-multipart`. `Native()` returns the `*azblob.Client`.
- **sftp:** atomic writes with the `posix-rename@openssh.com` extension, which the driver requires unless `Config.AllowNonAtomicWrites` accepts a server without it (replacing a key then removes the old file and renames the new one in two steps); conditional writes with `hardlink@openssh.com`, claimed only when the server offers it. A write-only drop folder that refuses `mkdir` inside `Root` can stage in a directory outside it on the same filesystem (`Config.StagingPath`, writes stay atomic) or skip staging (`Config.WriteInPlace` with `AllowNonAtomicWrites`: a reader can see a file while it arrives and a failed replace leaves the key empty; `IfNotExists` stays atomic through an exclusive create, and nothing is swept). `Native()` returns the `*sftp.Client`.
- **ftp, webdav:** writes are staged under a reserved directory and renamed into place; leftovers there are what `SweepAbandoned` removes. For a write-only drop folder, `Config.StagingPath` stages in a directory outside `Root` (no key reserved, writes still atomic), and `Config.WriteInPlace` with `Config.AllowNonAtomicWrites` writes straight to each key with no staging, so a reader can see a file while it arrives `SweepAbandoned` and `FindAbandoned` are not declared, and the disk reports itself case-sensitive since probing would write into `Root`. Case sensitivity is probed when the driver is opened. Neither stores metadata, headers or visibility, and neither signs URLs.
- **sqlblob:** every object is a row of one table, so everything that needs no network is native, including visibility, metadata, headers and compare-and-swap; objects are capped at `sqlblob.DefaultMaxObjectSize` (16 MiB) unless `Config.MaxObjectSize` says otherwise. No signer of its own.
- **gdrive, dropbox, onedrive:** the customer's own drive, opened with the application's OAuth token source. Dropbox and OneDrive compare paths without case and honour conditional writes; Google Drive does neither. None signs URLs.
- **vercelblob, cloudinary:** the two drivers that build public URLs themselves, and only for public content: Vercel Blob when the store is public, Cloudinary for `upload` resources (`authenticated` ones are read through signed requests the driver makes).

Of the core and cloud drivers, none has `PublicURL`: their public URLs come from a configured base (`public_url=` or `WithPublicURL`), typically a CDN in front of the bucket. A configured base also wins over the Vercel Blob and Cloudinary drivers' own URLs.

## Wrapper disks

| Disk | Capabilities |
|---|---|
| `disk.Scope(name, prefix)`, or `prefix=` in a DSN | the inner driver's, minus `SweepAbandoned` and `FindAbandoned` (leftovers are backend-wide); `MakeDir` and `DirExists` work under the prefix; keys gain the prefix on the way in and lose it on the way out |
| `filesystem.Mirror(name, primary, mirrors…)` | the primary's, minus `PresignPart`, `SweepAbandoned` and `FindAbandoned` (leftovers are backend-wide); `MakeDir` creates the directory on the primary and then on every mirror; `SignedURL` with `MethodPut` is refused with `unsupported`, because a client uploading straight to the primary would bypass the mirrors |
| `filesystemtest.NewFake(t, name)` | the memory driver's |

Interceptors never change capabilities: they wrap operations, not the driver.

## What capabilities are required

| Feature | Needs |
|---|---|
| `upload.Uppy` (browser multipart) | `Multipart` and `PresignPart`: S3-compatible disks only |
| `upload.PresignHandler`, `upload.Presign` (direct PUT) | `Presign`, or a `URLSigner` with `fshttp.Signed` mounted |
| A public direct upload (`upload.TicketRequest.Visibility` under `Policy.AllowPublic`, `PresignOptions.Visibility`, `PostPolicy.Visibility`) | `PresignVisibility`: S3 and GCS in ACL mode; never through a `URLSigner` |
| `records.Store.URL` for private files | `Presign` or a `URLSigner` |
| `records.Store.URL` for public files | a public URL base; otherwise it falls back to a signed link |
| `WriteOptions.Visibility` other than the disk default, `records.Store.SetVisibility` on one disk | `SetVisibility`; otherwise use separate public and private disks (`records.WithVisibilityDisks`) |
| `WriteOptions.IfNotExists` | `ConditionalWrite` |
| `WriteOptions.Metadata`, `WriteOptions.Headers` | `Metadata`, `Headers` |
| `fshttp.Public` serving a file | `SetVisibility` and a public object, or a disk whose default visibility is public |

## Next

- [Capabilities](../concepts/capabilities.md): the model, and how to check before calling.
- [DSN parameters](dsn.md): the parameters that turn capabilities on.
- [Write a driver](../extending/write-a-driver.md): declaring capabilities for a backend of your own.
