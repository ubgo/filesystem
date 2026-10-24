# Examples

Runnable programs, one per task an app actually has. Each folder is a complete `main` package with a `README.md` that explains what it shows, how to run it, a walkthrough with the commands and the output the program really produced, and its key code; and a `main_test.go` that drives the same program offline, so an example that stops working fails `task check`. The code shown in the READMEs and the [guides](../docs/README.md) is copied from these programs by docsync, so it cannot drift from what runs.

Run any of them from this directory:

```sh
cd examples
go run ./<folder>
go test ./<folder>
```

## Start here

Programs behind the getting-started guides.

| Example | What it shows |
|---|---|
| [quickstart](quickstart) | Quick start: a photo service on net/http. The program behind [Your first app](../docs/start/first-app.md): a photo service where a signed-in user uploads photos, checked by their bytes and capped in size, lists them, opens one through a short-lived signed link, moves one to the trash, restores it, and deletes it for good. |
| [start-install](start-install) | A first disk, on any backend. The program behind the install guide: a first `Disk` on a local directory, then the same code on memory or S3 by changing one DSN. |
| [start-production](start-production) | Production wiring from the environment. The program behind the production guide: the wiring a deployed app needs around the library, configured entirely from environment variables. |
| [start-testing](start-testing) | Testing code that stores files. The application code behind the testing guide, and its tests. |
| [basics](basics) | Basics: the Disk API in one program. A tour of the storage layer on its own, with no database: two disks opened from DSNs through a Mux (a local directory and a memory store), a start-up check through a Manager, a write that sniffs the content type and hashes the bytes while streaming, a refused traversal key, a scoped disk for one tenant, a copy across disks, and a signed download link served by `fshttp`. |

## Apps

Small complete applications built from several pieces.

| Example | What it shows |
|---|---|
| [mediaserver](mediaserver) | Media server: user avatars on disk and in SQLite. The file handling of a social or SaaS backend, written by hand: a user uploads an avatar, the server checks it against a policy (images only, 2 MiB, type taken from the bytes), stores it on a local disk under a random name, records it in SQLite, attaches it to the user as their single avatar, and hands out five-minute signed links to it. |
| [drive](drive) | Drive: a team drive built from the records plugins. The shape of a team drive such as Google Drive or Dropbox for a workspace: workspaces (scopes) with a 50 MiB storage quota, files attached to projects through `recordshttp`, folders, tags, version history with restore, expiring share links with a download limit, an activity feed, and image dimensions, all plugins of one records Store over a local disk and SQLite. |

## Recipes

One product feature each, as described in the [recipes](../docs/recipes/README.md).

| Example | What it shows |
|---|---|
| [recipe-account-deletion](recipe-account-deletion) | Account deletion: "delete my account" that holds up to a data-protection request. An HTTP server where deleting a user removes their row and every file only they used, in one transaction, while files they shared with a team stay for the team. |
| [recipe-avatars](recipe-avatars) | Avatars: one profile picture per user, cropped, cleaned and resized. The profile-picture feature of a social or SaaS app: a user uploads a photo, optionally with the square they chose in a cropper; the server re-encodes it (dropping EXIF and GPS), keeps exactly one avatar per user, generates the 64 and 256 px sizes right after the upload commits, and serves them as public URLs. |
| [recipe-backups-and-mirrors](recipe-backups-and-mirrors) | Backups and mirrors: move a live store to S3 without downtime. A program that moves a live store from a local directory to S3 while the app keeps writing, then keeps a dated backup copy. |
| [recipe-bulk-import](recipe-bulk-import) | Bulk import: accept a CSV now, import it in a job. `POST /imports` stores the CSV and answers `202` at once; a job run by the `jobs` scheduler streams each queued CSV, inserts the valid rows into the app's own table in the same transaction that marks the import ready, and stores the refused rows with their line numbers. |
| [recipe-browser-direct-uploads](recipe-browser-direct-uploads) | Browser direct uploads: presigned PUTs and Uppy multipart straight to S3. An HTTP server that lets a browser send files straight to an S3 bucket, so large uploads never pass through the application server: the server hands out a presigned PUT (or, for big files, signs the parts of a multipart upload for Uppy), the browser uploads, and the server confirms the stored bytes against the policy before recording the file. |
| [recipe-browser-form-uploads](recipe-browser-form-uploads) | Browser uploads through a form the bucket checks. A presigned POST ticket from `upload.Presign` with `PostForm`, a page that posts the fields and the file straight to the bucket, and `upload.ConfirmExpected`; an oversize file is refused by the bucket on a real store |
| [recipe-chat-attachments](recipe-chat-attachments) | Chat attachments: files on messages, visible to members only. The file side of a chat or messaging app: the author of a message attaches files to it, every member of the conversation can list and open them through short-lived signed links, nobody else can, and deleting the message deletes its files in the same transaction as the message row. |
| [recipe-cover-images](recipe-cover-images) | Cover images: one wide banner per owner, served as a srcset. A channel uploads one wide image; the server refuses one smaller than 1200 × 400, re-encodes it without EXIF, keeps one cover per channel, and makes a 3:1 crop around the most detailed region at three widths. |
| [recipe-email-attachments](recipe-email-attachments) | Email attachments: stream stored files into a message, or link them. A small invoice PDF is streamed from the disk into the MIME message under its original name; an export over 10 MiB becomes a seven-day signed link; the message goes to an SMTP server running in the same process. |
| [recipe-exports](recipe-exports) | Exports: "download my data" as a signed ZIP link. The "download my data" feature a privacy request or a self-service settings page needs: a user asks for an export, the server streams a ZIP (a JSON profile and a CSV of their orders) straight into storage with no temporary file, and answers with a short-lived signed link that saves the file under a readable name. |
| [recipe-import-from-url](recipe-import-from-url) | Import from URL: fetching a pasted link without SSRF. "Import from URL", "set avatar from a link", "attach from a Dropbox link", a CMS pulling an image from another site: the server fetches a URL a user typed, which is exactly what an attacker wants it to do with `http://169.254.169.254/` or `http://localhost:6379/`. |
| [recipe-mobile-uploads](recipe-mobile-uploads) | Mobile uploads: a camera-roll backup endpoint. The backend of "back up my camera roll": a phone sends up to 20 photos in one multipart request, each photo is checked and stripped of its GPS position and camera details on its own, a bad photo does not sink the others (it is listed under `failed`), and a batch retried after a lost response does not store the same photo twice. |
| [recipe-move-to-s3](recipe-move-to-s3) | Move a local store to S3: a resumable, verified copy tool. A command-line tool that moves an existing uploads directory to S3 or any S3-compatible bucket (R2, B2, Wasabi, Spaces, SeaweedFS, Garage) without losing a file, for the day a product outgrows writing uploads to the server's disk. |
| [recipe-private-downloads](recipe-private-downloads) | Private downloads: signed, short-lived links to a customer's documents. A server that hands private documents (invoices, contracts, payslips) only to the customer they belong to: the app checks ownership in the database, then redirects to a two-minute signed URL that `fshttp.Signed` serves with ranges, ETags and conditional requests. |
| [recipe-product-images](recipe-product-images) | Product images: an ordered e-commerce gallery with resized variants. The image gallery of an e-commerce catalog: a merchant uploads several photos of a product in one request, puts them in order (position 1 is the cover), and removes one for good, while the storefront gets a thumbnail grid, a product card and a zoomable large image, all from public URLs a CDN can cache. |
| [recipe-public-cdn](recipe-public-cdn) | Public CDN: publish and unpublish files between a public and a private bucket. The storage side of a CMS or site builder: an uploaded file is private (on a "vault" bucket, reachable only through signed URLs) until an editor publishes it, which moves it to a public bucket behind a CDN; unpublishing moves it back and asks the CDN to purge the old URL; and the app's own build assets are stored under content-addressed keys with `Cache-Control: public, max-age=31536000, immutable`. |
| [recipe-resumable-uploads](recipe-resumable-uploads) | Resumable uploads over tus. An upload endpoint whose uploads survive dropped connections: the client sends a file in chunks, and after a failure asks how much arrived and continues from there. `go run ./recipe-resumable-uploads shared` resumes one upload across two instances that share a session store and locker. |
| [recipe-saas-tenants](recipe-saas-tenants) | SaaS tenants: per-tenant files, quotas and buckets. The file storage of a multi-tenant SaaS product: every tenant's files are rows in its own `records` scope, stored under its own key prefix (`tenants/<id>/`), charged to its own plan's quota, optionally on a bucket of its own, and removed in one call when the tenant leaves. |
| [recipe-strip-photo-metadata](recipe-strip-photo-metadata) | Strip photo metadata before storing. A photo upload endpoint that removes what phones hide in photos before anything is stored: the GPS position (often the user's home), the camera's serial number, the capture time, XMP, comments and any payload appended after the image data. |
| [recipe-tenant-branding](recipe-tenant-branding) | Tenant branding: a logo, a favicon and an email header per tenant. Three single-file slots per tenant, each with its own dimension rules, re-encoded on upload, resized to the sizes the app renders and served from public URLs that keep working in emails. |
| [recipe-team-drive](recipe-team-drive) | Team drive: folders, versions, share links, quota and activity. The core of a team drive (the shape of Dropbox, Google Drive or a project tool's file tab): each workspace has a storage quota from its plan, files live in folders, replacing a file keeps its previous contents as versions that can be restored, files are shared outside the workspace through expiring links, and every change lands in an activity feed with who made it. |
| [recipe-virus-scanning](recipe-virus-scanning) | Virus scanning: every upload scanned before it is accepted. An upload endpoint that scans every file for malware before the client hears "201", for any product where one user's upload reaches another user's machine (shared drives, chat, support desks, client portals). |

## Concepts

One idea each, behind the [concept guides](../docs/README.md#concepts).

| Example | What it shows |
|---|---|
| [concepts-capabilities](concepts-capabilities) | Capabilities: what a backend can do, and what happens when it cannot. Backends differ: S3 copies on the server, a local directory cannot store metadata, a memory disk has no signer. |
| [concepts-composing](concepts-composing) | Composing disks: route, failover, overlay, cache and batch. A routed disk that sends each prefix to its own backend, reads that fail over to a replica, a copy-on-write overlay, reads served from a local cache, and bounded-parallel copies, each still an ordinary `*filesystem.Disk`. |
| [concepts-configuration](concepts-configuration) | Configuration: DSNs, the Mux and a Manager from the environment. How an application turns configuration into disks. |
| [concepts-consistency](concepts-consistency) | Consistency: bytes and rows without a shared transaction. An object store and a database cannot commit together, so every app that keeps file rows next to stored bytes needs ordering rules. |
| [concepts-delivery](concepts-delivery) | Delivery: CDN-signed links, proxy offload and data URIs. CloudFront signed URLs and cookies, a Cloudflare token-authentication URL, a local disk served through `fshttp.Public` with `Sendfile` so nginx sends the bytes, and a tiny image inlined with `Disk.DataURI`. |
| [concepts-disks](concepts-disks) | Disks and drivers: a tour of the Disk API. The `*filesystem.Disk` is what an application holds; a driver only moves bytes for one backend. |
| [concepts-errors](concepts-errors) | Errors: one set of codes for every backend. Every storage error is classified into a small set of codes, so application code branches the same way on local disk, S3 or SFTP. |
| [concepts-integrity](concepts-integrity) | Integrity: digests, declared sizes, ETags and content-addressed keys. The SHA-256 every write computes while it streams, a declared digest that keeps a corrupted body out, a declared size that refuses a truncated body, ETags for change checks, and content-addressed keys that store identical uploads once. |
| [concepts-interceptors](concepts-interceptors) | Interceptors: retries, timeouts, limits and policy around every operation. Retries, timeouts, concurrency limits, logging, read-only disks and policy checks apply to every operation whatever the backend. |
| [concepts-jobs](concepts-jobs) | Maintenance jobs by hand: promote, preview, your own jobs and refusals. An upload accepted on a fast disk and promoted later (`jobs.Promote`), a sweep previewed with its dry run, a function of its own wrapped as a job, and a misconfigured job and schedule refused. |
| [concepts-keys](concepts-keys) | Keys: cleaning, refusals and URL escaping. A key names one object on a disk, and every key passes the same rules before any driver sees it. |
| [concepts-security](concepts-security) | Security defaults, each one refusing something. Storage code is where traversal, disguised files, header injection and server-side request forgery get in. |
| [concepts-serving](concepts-serving) | Serving files: signed links, public files and a CDN. Stored files reach browsers in two ways: private files through links that are signed and expire, public files by a plain URL. |
| [concepts-tenancy](concepts-tenancy) | Multi-tenancy: keeping tenants apart on shared storage. A SaaS app stores every tenant's files on one bucket, and one tenant must never read, overwrite or delete another's. |
| [concepts-uploads](concepts-uploads) | Uploads: every way bytes arrive, under one policy. Every app that accepts files needs the same guarantees: a size limit enforced on the server, the type decided from the bytes rather than the client's claim, a random key instead of the client's file name, and nothing left behind when an upload is refused. |
| [concepts-upload-clients](concepts-upload-clients) | Upload clients: chunks, progress, folders, retries and the JSON contract. One file sent in three chunks out of order with a retry as Dropzone does, progress while a file streams, a dropped folder that keeps its layout, and a retry under an `Idempotency-Key` that is not stored twice. |
| [concepts-visibility](concepts-visibility) | Public and private files. Product images and avatars should be public and cacheable behind a CDN; invoices and contracts must stay private behind expiring links. |
| [jobs-scheduler](jobs-scheduler) | Maintenance jobs on a schedule. The sweeps and recounts every deployment must run, as `jobs.Schedule`s run by the in-process scheduler, with the River and Asynq adapters for several instances |
| [jobs-calendar](jobs-calendar) | Maintenance without records, on a calendar. `jobs/diskjobs` alone (no database): the upload sweep every day at 03:00 with `diskjobs.Daily`, a dry-run preview of the abandoned-upload sweep, and an application job wrapped with `diskjobs.Func` |

## Backends

One per driver, behind the [backend guides](../docs/adapters/README.md). S3, GCS and SFTP run against in-process fakes; Azure needs Azurite.

| Example | What it shows |
|---|---|
| [adapters-azure](adapters-azure) | Azure Blob Storage driver. The Azure driver stores files as block blobs in an Azure Storage container. Its block-size and concurrency, per-call options (`RequestOptions`) and front-end (`AccountName`) steps run against in-process stand-ins; the rest needs Azurite or an account. |
| [adapters-consumer-drives](adapters-consumer-drives) | Consumer drives: Google Drive, Dropbox and OneDrive. A disk on each of the customer's own drives, against in-process stand-ins for their APIs: write, read, stat and list a file, a create-only write, copies and moves, and what each drive refuses. |
| [adapters-ftp](adapters-ftp) | FTP driver. An FTP server in-process, a partner's directory opened from a DSN whose password comes from the environment, an atomic upload, a move into an archive, six uploads over a pool of three connections, and a refused conditional write. |
| [adapters-gcs](adapters-gcs) | Google Cloud Storage driver. The GCS driver stores files in a Google Cloud Storage bucket. |
| [adapters-hosted](adapters-hosted) | Vercel Blob and Cloudinary drivers. A disk on each hosted store against in-process stand-ins: write, read, stat, list, copy or move, delete, public URLs, the visibility each declares and refuses, and how a key maps to what the service stores. |
| [adapters-local](adapters-local) | Local disk driver. The local driver stores files in a directory on the machine, every operation confined to it through an `os.Root`. |
| [adapters-memory](adapters-memory) | In-memory driver. The memory driver keeps objects in a map inside the process: the backend for tests, for caches that need no persistence, and the reference implementation that supports every capability needing no network. |
| [adapters-s3](adapters-s3) | Amazon S3 driver. The S3 driver stores files in an S3 bucket on AWS or any S3-compatible store. |
| [adapters-s3-post](adapters-s3-post) | Browser form uploads straight to S3. A signed POST policy for one avatar (size, key prefix, content type, Cache-Control, expiry), the HTML form, the browser-style submit and `upload.Confirm` |
| [adapters-s3-compatible](adapters-s3-compatible) | S3-compatible providers. One starting DSN per S3-compatible store (AWS S3, Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces, SeaweedFS, Garage, MinIO), each opened through the S3 driver to print the capabilities that configuration declares. |
| [adapters-sftp](adapters-sftp) | SFTP driver. The SFTP driver stores files in a directory on an SSH server: the backend for partner exchanges, legacy hosts and anywhere a file drop is the integration. |
| [adapters-supabase](adapters-supabase) | Supabase Storage. The S3 driver on a Supabase project's S3 endpoint: the DSN (path-style, keys from the environment), then a file stored, listed, read and deleted, against an in-process fake S3. |
| [adapters-sqlblob](adapters-sqlblob) | SQL table driver. Objects as rows of a SQLite table in WAL mode: the table created with `Migrate` (or the `Schema()` statement for your own migration tool), an avatar with metadata and a `Cache-Control` header, a ranged read, swaps by ETag, a refused create-only write, the size cap, copies and listings, a query on the table through the `*sql.DB`, a move, and a signed link served by `signedurl` and `fshttp`. |
| [adapters-webdav](adapters-webdav) | WebDAV driver. A directory served over WebDAV in-process with Nextcloud's URL layout: a report stored, copied and moved on the server, metadata refused rather than dropped, and the driver's configuration logged with the password redacted. |

## Files as records

The records module and its plugins on SQLite, behind the [records guides](../docs/records/README.md).

| Example | What it shows |
|---|---|
| [records-attachments](records-attachments) | Attaching files to your own rows. How recorded files hang off the application's own rows: a post's gallery (many files, ordered), a user's avatar (a single role holding one file), reordering, detaching, replacing the file of a role with `SetAttachment`, and one file shared by two owners. |
| [records-acl](records-acl) | Permissions on files and folders. The `acl` plugin: grants that give a user or a group read, write or admin access to a file or a folder and everything in it, checked by `Can` and, behind the HTTP routes, by `acl.Authorizer`. |
| [records-audit](records-audit) | An audit log of file changes. The audit plugin of the records layer: every change to a file or an attachment writes an entry in `fs_audit`, inside the same transaction, with the actor who made it. |
| [records-dimensions](records-dimensions) | Image width and height on every file. The dimensions plugin: when a raster image is recorded, and again when its content is replaced, its width and height are read from the image header and stored in the file's metadata. |
| [records-egress](records-egress) | Egress metering. The `egress` plugin: every link the Store issues and every stream the app reports adds bytes to its scope's daily bucket; `Days` and `Total` read a period, `Report` emits daily totals for billing, `Prune` drops reported days. |
| [records-events](records-events) | Your own plugins: events and hooks. How an application takes part in what the records Store does, by installing its own plugins. |
| [records-files](records-files) | Files as database rows. The file API of the records layer: every stored object gets a row in `fs_files` with its scope, disk, key, name, type, size and digest, so an app can list, filter and link its files without asking the storage backend. |
| [records-folders](records-folders) | Folders for a drive-style app. The folders plugin of the records layer: a folder tree kept in the database, as a team drive or a document manager shows it, while the storage keys stay flat and random. |
| [records-http](records-http) | The records layer over HTTP. A JSON API over the records layer with `recordshttp`: upload files and attach them to a user, list them with links, reorder and detach them, redirect to a file, replace its content and trash it. |
| [records-lifecycle](records-lifecycle) | Trash, restore and delete for good. The end of a file's life in the records layer: the trash (`Trash`, `Restore`, counting trashed files), deleting for good (`Purge`, which deletes the object only after the transaction commits), a rollback that keeps both row and object, deleting everything a user leaves behind (`PurgeOwner`) in the same transaction that deletes the user's own row, a trash retention sweep (`SweepTrash`), and where object deletion failures go (`WithObjectErrorHandler`). |
| [records-locks](records-locks) | Check-out locks. The `locks` plugin: a user checks a document out, and until they check it in or the lock expires nobody else can replace, trash or purge it. |
| [records-migrations](records-migrations) | Creating and evolving the records schema. How the records tables are created and changed over time: `Migrate` applies what is new, once, under a lock; `MigrationSQL` renders the same changes as one script for a review or an external migration tool; a plugin ships its own migrations; `AddColumn` adds a column later on any engine; `TypesFor` gives the per-engine type names; and a migration whose statements changed after it ran is refused. |
| [records-quota](records-quota) | Storage quotas per plan. The quota plugin of the records layer: a byte and file limit per scope, taken from the scope's plan, charged in the database inside the same transaction as each upload. |
| [records-replace](records-replace) | Replacing content and changing visibility. How a recorded file changes without changing its ID: new content with `Replace` (upload) or `ReplaceObject` (bytes already stored), and a new visibility with `SetVisibility`. |
| [records-move](records-move) | Moving files between disks. Uploads land on a fast disk and a rerunnable job promotes them one at a time to the main disk with `MoveFile`, keeping each file's ID and key. |
| [records-query](records-query) | Querying and bulk changes. A role capped at N files, an owner's files as a listing filter, attachments a page at a time, facet counts for a sidebar, bulk trash with per-file results, and the read-only storage audits. |
| [records-quota-seats](records-quota-seats) | Storage seats in a team workspace. Per-uploader limits of the quota plugin: a plan total plus a seat per member, charged in one transaction with the upload |
| [records-status](records-status) | Processing status. A file recorded as processing, a worker reporting progress and the outcome, a plugin told of each change with the file in the event, failed files listed with a filter, and a client polling the status route. |
| [records-share](records-share) | Share links to one file. The share plugin of the records layer: a link to one file with a lifetime and a download limit, as "anyone with the link" sharing in a drive, a client portal or a support tool works. |
| [records-tags](records-tags) | Tagging files. The tags plugin of the records layer: free-form labels on files, normalized so `Invoice`, ` invoice ` and `INVOICE` are one tag, with listings by tag, a tag cloud, and scope-wide renames and deletes. |
| [records-upload-sessions](records-upload-sessions) | Upload sessions in the database. The `records/uploadsessions` plugin: the API issues a token for one key prefix with limits no wider than the endpoint's policy, a client holding only the token uploads with it, and every instance redeems the same tokens. |
| [records-variants](records-variants) | Image variants: thumbnails and cards. The variants plugin: named image sizes declared by the application (`thumb`, a 128×128 crop, and `card`, fitted inside 400×400), generated on first request or eagerly after an upload, served through a redirect handler, dropped when the original is replaced or changes visibility, pruned after a transform changes, and deleted with their file; `ModeProxy` streams them with ranges and 304s. |
| [imaging-uploads](imaging-uploads) | Smaller photos and size checks. `imaging.Optimize` shrinks photos losslessly first, `imaging.CheckDimensions` refuses images that are too small, the dimensions plugin stores a ThumbHash placeholder, and the variants plugin builds a srcset; `MetadataEXIFCopyright` keeps the EXIF Artist and Copyright while the GPS position goes |
| [records-versions](records-versions) | Keeping earlier versions of a file. The versions plugin of the records layer: each time a file's content is replaced, the content it replaces is kept as a numbered version, up to a cap per file. |
| [records-webhooks](records-webhooks) | Signed outbound webhooks. The `webhooks` plugin: an endpoint registered for chosen event kinds, a delivery written in the change's own transaction, signed in the Standard Webhooks format, retried after a backoff and recorded as delivered. |

## Media and documents

Plugins for images, media and untrusted formats. Several are separate Go modules so the main examples module does not depend on their codecs or SDKs.

| Example | What it shows |
|---|---|
| [imaging-transforms](imaging-transforms) | Image transforms. What `contrib/imaging` does to one image: the resize modes and a crop, EXIF orientation, a smart crop, a watermark, an animated GIF kept animated, the dominant colour, the pixel-bomb limits, a `Processor` of its own, and a golden image test. |
| [imaging-codecs](imaging-codecs) | Image codecs: WebP, AVIF and colour profiles. A Pipeline that writes WebP and AVIF, and one that converts a Display P3 photo to sRGB. A separate Go module. |
| [media](media) | Media: video, audio, streaming, PDF and SVG. Uploads of the formats that need more than sniffing, through `contrib/video`, `contrib/audio`, `contrib/pdf`, `contrib/svg` and `contrib/videoplatform`. A separate Go module, one program per folder: [video](media/video) (with an HLS ladder), [audio](media/audio) (duration limit, mono Opus, waveform), [streaming](media/streaming) (Mux through `contrib/videoplatform` with a fake API), [pdf](media/pdf) and [svg](media/svg). FFmpeg and Poppler where noted. |
| [documents](documents) | Documents: text for search and image labels. `search` stores PDF text (`contrib/pdf`), reads scans with `contrib/ocr` and converts office files with `contrib/office`; `labels` tags images with Google Cloud Vision (`contrib/vision`). A separate Go module; needs Poppler, Tesseract and LibreOffice, and a Vision API key for `labels`. |
| [pickers-import](pickers-import) | Cloud picker import. A file picked in Google Drive imported through `upload.Receive`, so the upload policy decides what is stored; the Drive API is a stand-in server. A separate Go module. |
| [remote-media](remote-media) | Remote media: link previews, an image proxy and a media cache. A link preview whose `og:image` is cached, the image served through the signed proxy, a forged proxy URL and an SVG posing as a PNG refused, and oEmbed from an allowlisted provider with a discovery link off the list refused. A separate Go module. |
| [security-archive](security-archive) | Archive uploads. Zip uploads walked under limits before they are reported stored, a clean one extracted into the user's folder, and a zip bomb, a zip-slip archive and an archive hiding a program refused. |
| [security-crypt](security-crypt) | Encryption at rest. Files encrypted before they reach the backend with a master key per tenant, a ranged read, a master key rotated without touching the chunks, and a deleted key that makes a tenant's files unreadable. |

## Maintenance

Programs behind the [maintenance](../docs/maintenance/README.md) guide.

| Example | What it shows |
|---|---|
| [maintenance](maintenance) | Maintenance: dry runs, scrubbing, migration, sync and exports. A sweep previewed before it runs, a scrub that finds an object changed behind the Store's back, every file moved from a hot disk to a cold one, a sync to a backup with a dry run first, and a user's export as a zip behind an expiring link. |
| [maintenance-notify](maintenance-notify) | Bucket notifications. Direct uploads confirmed from the bucket's own S3 events through `contrib/notify/sqs` on an in-memory queue: a refused file deleted, a foreign bucket's event and a stray message dropped, a failed event redelivered, a duplicate skipped. |

## Integrations

Mounting the handlers and wiring telemetry, behind the [integration guides](../docs/README.md#integrations). The chi, gin, echo and fiber programs are in their own module, [frameworks](frameworks), so their dependencies stay out of this one.

| Example | What it shows |
|---|---|
| [integrations-logging](integrations-logging) | Integrations: logging and metrics with log/slog and expvar. Every disk operation logged with `log/slog` through `layer.Log`, counted with `layer.Observe` into `expvar` maps, and run through the built-in interceptors in their usual order, with a backend that fails once so the retry is visible in the log. |
| [integrations-nethttp](integrations-nethttp) | Integrations: every HTTP handler on net/http and gorilla/mux. Every HTTP handler the library ships, mounted on a standard library `http.ServeMux`, and the same handler values on a gorilla/mux router: form uploads (`upload.Handler`), direct-upload tickets (`upload.PresignHandler`), Uppy multipart (`upload.Uppy`), tus resumable uploads (`contrib/tus`), signed downloads (`fshttp.Signed`), public files (`fshttp.Public`) and the records endpoints (`recordshttp`). |
| [integrations-otel](integrations-otel) | Integrations: OpenTelemetry traces and metrics. Every disk operation traced and measured with OpenTelemetry through `contrib/otel`: one span per operation, and an operation counter, a latency histogram and a bytes-written counter, labelled by disk, operation and outcome. |
| [integrations-cli](integrations-cli) | The `fs` command-line tool, end to end. Builds `cmd/fs` and runs a whole session: storing and listing on a local disk, a recursive copy and a move that fails part-way, copies and signed URLs on an S3 bucket, and the errors a script branches on. |
| [integrations-orm](integrations-orm) | Reading the records tables with sqlc, GORM and ent. Rows written by a `records.Store`, then read by each data layer and checked against what the Store answers, and the same names behind a table prefix (`WithTablePrefix`). |
| [frameworks](frameworks) | Framework examples: chi, gin, echo and fiber. Every HTTP handler the library ships, mounted on four web frameworks: chi, gin, echo and fiber. |
| [gqlgen](gqlgen) | GraphQL uploads with gqlgen. Mutations that take files as the `Upload` scalar and store them through `contrib/gqlgen` under an upload policy, including an avatar recorded and attached to the signed-in user. A separate Go module. |

## Extending

Building your own pieces, behind the [extending guides](../docs/README.md#extending).

| Example | What it shows |
|---|---|
| [extending-driver](extending-driver) | Extending: a driver for your own backend. A complete storage driver for a made-up key-value service, built the way every real driver is. |
| [extending-interceptor](extending-interceptor) | Extending: interceptors of your own. Three interceptors written from scratch: one that freezes part of a disk, one that reports slow operations, and one that enforces a byte budget from what writes actually stored. |
| [extending-records-plugin](extending-records-plugin) | Extending: a records plugin for legal holds. A records plugin written from scratch: legal holds. |
| [extending-scanner](extending-scanner) | Extending: a malware scanner of your own. A malware scanner written from scratch: a client for an HTTP scanning service (the shape of most cloud scanning APIs), plugged into an upload policy with `scan.Inspect` so every upload path scans the file before it reports success. |
