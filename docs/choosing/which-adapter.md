# Which adapter

Every backend sits behind the same `*filesystem.Disk`, so the choice of adapter changes a DSN or a constructor, not your application code. What changes is what the disk can do natively, what it does through a portable fallback, and what it refuses with `CodeUnsupported`. This page helps you pick: first by where the files must live, then by the capabilities your features need, then by who runs the storage.

## Before you start

- The full per-driver capability table is [Capabilities reference](../reference/capabilities.md), and the comparison with every fallback spelled out is [Storage backends](../adapters/README.md). This page summarises the rows that decide most choices; read those pages for the rest.
- A disk reports what it can do at run time: `disk.Capabilities()` prints as `[copy,delete-prefix,presign,…]`. `disk.Check(ctx)` proves the credentials and bucket work at start-up. Both are shown in [Capabilities](../concepts/capabilities.md) and run by [concepts-capabilities](../../examples/concepts-capabilities).

## By where the files must live

| The files live | Adapter | Module | Opened with | Guide | Example |
|---|---|---|---|---|---|
| On the server's own disk (development, a single server) | local | core | `file://` | [local](../adapters/local.md) | [adapters-local](../../examples/adapters-local) |
| Nowhere: tests and caches | memory | core | `mem://` | [memory](../adapters/memory.md) | [adapters-memory](../../examples/adapters-memory) |
| In Amazon S3 | s3 | `adapters/s3` | `s3://` | [s3](../adapters/s3.md) | [adapters-s3](../../examples/adapters-s3) |
| In R2, B2, Wasabi, Spaces, SeaweedFS, Garage, MinIO or Supabase Storage | s3 with the provider's endpoint | `adapters/s3` | `s3://` | [S3-compatible stores](../adapters/s3-compatible.md), [supabase](../adapters/supabase.md) | [adapters-s3-compatible](../../examples/adapters-s3-compatible), [adapters-supabase](../../examples/adapters-supabase) |
| In Google Cloud Storage | gcs | `adapters/gcs` | `gs://` | [gcs](../adapters/gcs.md) | [adapters-gcs](../../examples/adapters-gcs) |
| In Azure Blob Storage | azure | `adapters/azure` | `azblob://` | [azure](../adapters/azure.md) | [adapters-azure](../../examples/adapters-azure) |
| On a partner's or a NAS's SSH server | sftp | `adapters/sftp` | `sftp://` | [sftp](../adapters/sftp.md) | [adapters-sftp](../../examples/adapters-sftp) |
| On an FTP or FTPS server | ftp | `adapters/ftp` | `ftp://`, `ftps://`, `ftps-implicit://` | [ftp](../adapters/ftp.md) | [adapters-ftp](../../examples/adapters-ftp) |
| On Nextcloud, ownCloud or a NAS speaking WebDAV | webdav | `adapters/webdav` | `webdav://`, `webdavs://` | [webdav](../adapters/webdav.md) | [adapters-webdav](../../examples/adapters-webdav) |
| In a table of the database you already run | sqlblob | `adapters/sqlblob` | built from a `*sql.DB` | [sqlblob](../adapters/sqlblob.md) | [adapters-sqlblob](../../examples/adapters-sqlblob) |
| In the customer's own Google Drive, Dropbox or OneDrive | gdrive, dropbox, onedrive | one module each | built with the app's OAuth token source | [gdrive](../adapters/gdrive.md), [dropbox](../adapters/dropbox.md), [onedrive](../adapters/onedrive.md) | [adapters-consumer-drives](../../examples/adapters-consumer-drives) |
| In Vercel Blob or Cloudinary | vercelblob, cloudinary | one module each | `vercelblob://`, `cloudinary://`, or built with a token or API credentials | [vercelblob](../adapters/vercelblob.md), [cloudinary](../adapters/cloudinary.md) | [adapters-hosted](../../examples/adapters-hosted) |

The local and memory drivers are in the core module, which depends on the standard library only. Every other adapter is a module of its own, so an application downloads the AWS, Google or Azure SDK only when it imports that adapter.

## By the features you need

These rows come from each driver's `Capabilities()` method. "Signer" means the disk has no presigning of its own and signs through a `URLSigner` you give it (`signedurl` with `fshttp.Signed` mounted, see [Serving files](../concepts/serving-files.md#step-1--signed-urls-on-any-backend)).

| You need | local | memory | s3 | gcs | azure | sftp | ftp | webdav | sqlblob | gdrive, dropbox, onedrive | vercelblob | cloudinary |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| Signed download links | signer | signer | native | with `signing_key`, or keyless through IAM (`signing_account`) | native (SAS) | signer | signer | signer | signer | signer | signer | signer |
| Browsers uploading straight to storage (presigned PUT) | through your signed handler | through your signed handler | native | with `signing_key` | native | through your signed handler | through your signed handler | through your signed handler | through your signed handler | through your signed handler | through your signed handler | through your signed handler |
| Browser multipart (Uppy, `PresignPart`) | no | no | yes | no | no | no | no | no | no | no | no | no |
| A form the bucket checks (`PresignPost`) | no | no | yes | with a signing key | no | no | no | no | no | no | no | no |
| Public URLs built by the driver (`PublicURL`) | from a base | from a base | from a base | from a base | from a base | from a base | from a base | from a base | from a base | from a base | when the store is public | for `upload` resources |
| Per-object public or private | file mode | yes | with `acl=true` | with `acl=true` | no (a container setting) | file mode | no | no | yes | no | no | no |
| Stored metadata and HTTP headers | no | yes | yes | yes | yes | no | no | no | yes | no | no | no |
| Create only if absent (`IfNotExists`) | yes | yes | with `conditional_write=true` | yes | yes | when the server offers `hardlink@openssh.com` | no | no | yes | dropbox, onedrive | no | no |
| Server-side copy | streams | yes | yes | yes | yes | streams | streams | yes | yes | gdrive, dropbox | yes | streams |
| Atomic move | yes | yes | copy then delete | copy then delete | copy then delete | yes | yes | yes | yes | yes | copy then delete | yes |
| Real directories (`MakeDir`) | yes | yes | no | no | no | yes | yes | yes | no | no | no | no |
| Object versions, holds, storage classes | no | classes; versions with `memory.WithVersions()` | with `versioning=true`, `object_lock=true`, `storage_classes=true` | holds and classes; versions with `versioned=true` | holds and access tiers; versions with `versioned=true` | no | no | no | no | no | no | no |

"From a base" means `PublicURL` returns `base + "/" + key` from `WithPublicURL` or the `public_url` DSN parameter, and is refused when no base is set. The Vercel Blob and Cloudinary drivers are the two that build public URLs themselves, and only when the store or resource type is public (`vercelblob.go` and `cloudinary.go` set `PublicURL` from the access mode). "Streams" means the Disk falls back to reading and writing the object through your process; "copy then delete" is not atomic, so a failure in between leaves both keys.

## By who runs it, and what it costs

| You want | Choose | Why |
|---|---|---|
| Nothing to operate | S3, GCS or Azure | Managed, durable, every capability that needs presigning |
| Low egress cost for public media | R2 through the s3 driver | The [S3-compatible guide](../adapters/s3-compatible.md) notes R2 charges no egress |
| Storage you host yourself | SeaweedFS or Garage through the s3 driver; SFTP or WebDAV on a NAS | The s3 driver works against any S3 API; set `conditional_write=true` only when the store honours `If-None-Match` |
| One database to back up and no object store | sqlblob | Objects are rows; the default cap is 16 MiB per object (`sqlblob.DefaultMaxObjectSize`), and every write holds its body in memory |
| Files that belong to the customer, not to you | gdrive, dropbox, onedrive | Saved into the user's own drive with their OAuth token; no presigned URLs, no metadata |
| Your app already runs on Vercel or keeps media in Cloudinary | vercelblob, cloudinary | The files sit next to what you already run; mind the rate limits the [cloudinary guide](../adapters/cloudinary.md) lists |
| Local development and CI | local, memory | No services; the memory driver is what `filesystemtest.NewFake` uses |

## The decision in short

1. If you are writing tests, use memory (or `filesystemtest.NewFake`). If you are on a laptop or one server, use local.
2. If browsers upload large files in parallel parts with Uppy, you need S3 or an S3-compatible store: only the s3 driver declares `PresignPart`.
3. If the bucket itself must enforce the size and type of a browser upload, you need `PresignPost`: S3, or GCS with a signing key.
4. If you are on AWS, Google Cloud or Azure already, use that cloud's adapter.
5. If egress is your largest bill, put public media on R2 behind its CDN, through the s3 driver.
6. If a partner or a device speaks only SFTP, FTP or WebDAV, use that adapter for that one disk, and keep your own files on an object store; a [Manager](../concepts/configuration.md) holds both.
7. If the files must end up in the user's own drive, use gdrive, dropbox or onedrive with the user's token.
8. If you want per-object public and private on one bucket, check the per-object row above. Where it says no, keep two disks, one public and one private ([Public and private files](../concepts/public-and-private.md)).

## Pitfalls

- A DSN that works on one backend can refuse an option on another. A write with metadata on SFTP, or `IfNotExists` on an S3 provider not declared to honour it, fails with `CodeUnsupported` instead of silently dropping the option. Run your integration tests against the backend you deploy on.
- An unknown DSN parameter is refused with `CodeInvalidArgument` at start-up, so a typo cannot be ignored ([DSN reference](../reference/dsn.md)).
- A scoped disk (`prefix=`) and a mirror lose the leftover sweep (`SweepAbandoned`) because leftovers are backend-wide; a mirror also refuses presigned PUTs, so a client cannot bypass the mirrors ([Capabilities reference, wrapper disks](../reference/capabilities.md#wrapper-disks)).

## Related

- [Storage backends](../adapters/README.md): the full matrix with fallbacks, and every adapter page.
- [Capabilities](../concepts/capabilities.md): why capabilities are declared and never silently dropped.
- [Write a driver](../extending/write-a-driver.md): a backend of your own, proven by the conformance suite.
- [Move to S3](../recipes/move-to-s3.md): changing backend with files already stored.
