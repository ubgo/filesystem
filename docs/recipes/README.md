# Recipes

A recipe is a complete solution to one product need: the problem, the design in a few sentences, the full working code in steps, the HTTP contract with transcripts from a real run, the edge cases it handles, and what to add before production. Each one is backed by a runnable program under [examples/](../../examples) named `recipe-<name>`, with an offline test that the repository's test suite runs, so the code on these pages compiles and works today.

Read the [concepts](../concepts/uploads.md) first if you want the why behind the pieces; come here when you know what you are building.

## Run any recipe

```sh
git clone https://github.com/ubgo/filesystem
cd filesystem/examples
go run ./recipe-avatars          # each server prints its address (localhost:8101 to 8123)
go test ./recipe-avatars         # the offline test: no network, no Docker
```

Recipes that talk to S3 start an in-process fake S3 by default and read a real DSN from an environment variable named on their page. The fake does not check signatures or every header; each page says what it leaves unverified.

## By kind of app

### Social and community

| Recipe | What it builds |
|---|---|
| [Avatars](avatars.md) | One profile picture per user: crop, re-encode without EXIF, 64 and 256 px sizes generated after upload, the old one purged |
| [Strip photo metadata](strip-photo-metadata.md) | GPS, camera details and appended payloads removed from every photo before it is stored, with the gaps where processing cannot run |
| [Import from a URL](import-from-url.md) | "Paste a link" imports with the SSRF guard: loopback, private networks and cloud metadata refused at dial time |
| [Public files behind a CDN](public-cdn.md) | Public and private files on separate disks, immutable cache headers, and making a public file private everywhere |
| [Cover and banner images](cover-images.md) | One wide header image per profile or channel: refused when too small, cropped around its subject, served as a srcset at 600, 1200 and 1800 px |

### SaaS and B2B

| Recipe | What it builds |
|---|---|
| [SaaS tenants](saas-tenants.md) | Every query scoped to the tenant, plan-based quotas, a bucket of its own for the customers who ask, offboarding |
| [Private downloads](private-downloads.md) | Invoices and contracts behind an ownership check and short-lived signed links, with key rotation, ranges and 304s |
| [Exports](exports.md) | "Download my data": a ZIP streamed into storage without a temporary file, one per user per day, a signed link, and expiry |
| [Account deletion](account-deletion.md) | Erasing a user's files in the transaction that deletes the user, keeping what they shared, retrying what failed after the commit |
| [Virus scanning](virus-scanning.md) | ClamAV on every upload path before success is reported, failing closed when the scanner is down |
| [Bulk import](bulk-import.md) | "Import from a CSV": accepted at once with a 202, parsed by a background job that is safe to rerun, refused rows returned as a report |
| [Tenant branding](tenant-branding.md) | A logo, a favicon and an email header per tenant, each slot with its own shape rules and sizes, on public URLs that work in email |
| [Email attachments](email-attachments.md) | Stored files streamed into outbound email, or replaced by a seven-day signed link when too large to attach |

### E-commerce and marketplaces

| Recipe | What it builds |
|---|---|
| [Product images](product-images.md) | An ordered gallery per product with a cover, thumbnail, card and zoom sizes, reordering that refuses a stale list |
| [Browser direct uploads](browser-direct-uploads.md) | Large files sent from the browser straight to the bucket (presigned PUT, Uppy multipart) and confirmed by the server |

### Chat and messaging

| Recipe | What it builds |
|---|---|
| [Chat attachments](chat-attachments.md) | Files on messages, readable by conversation members only, deleted with their message |
| [Resumable uploads](resumable-uploads.md) | tus uploads that survive a dropped connection, checked by the same policy when the last byte arrives |

### Mobile backends

| Recipe | What it builds |
|---|---|
| [Mobile uploads](mobile-uploads.md) | Choosing the upload path, then "back up 20 photos" with per-photo failures, idempotent retries and metadata stripping |
| [Resumable uploads](resumable-uploads.md) | The tus server that iOS, Android and Flutter tus clients talk to |

### Internal tools and collaboration

| Recipe | What it builds |
|---|---|
| [Team drive](team-drive.md) | Workspaces with quotas, folders, version history with restore, share links and an activity feed |

### Operations

| Recipe | What it builds |
|---|---|
| [Move a local store to S3](move-to-s3.md) | Copying an existing local directory to a bucket: resumable, concurrent, verified, then a DSN switch |
| [Backups and mirrors](backups-and-mirrors.md) | Writing to two backends at once, backfilling, switching the primary without downtime, and scheduled backup copies |

## By library feature

If you know the piece and want to see it used:

| Feature | Recipes |
|---|---|
| `records.WithSingleRoles`, `SetAttachment` | [Avatars](avatars.md), [Cover images](cover-images.md), [Tenant branding](tenant-branding.md) |
| `Store.Reorder`, gallery roles | [Product images](product-images.md) |
| `Store.PurgeOwner`, `SweepTrash` | [Account deletion](account-deletion.md), [Chat attachments](chat-attachments.md) |
| `recordshttp.Handler` | [Chat attachments](chat-attachments.md), [Product images](product-images.md) |
| `imaging.Sanitize`, `imaging.Process` | [Strip photo metadata](strip-photo-metadata.md), [Avatars](avatars.md), [Mobile uploads](mobile-uploads.md) |
| `variants` plugin | [Avatars](avatars.md), [Product images](product-images.md), [Cover images](cover-images.md) (densities and `SrcSet`), [Tenant branding](tenant-branding.md) (`Generate` per role) |
| `imaging.CheckDimensions`, `upload.LimitsHandler` | [Cover images](cover-images.md), [Tenant branding](tenant-branding.md), [Mobile uploads](mobile-uploads.md) |
| `Store.SetStatus`, `jobs.Func`, `jobs.Scheduler` | [Bulk import](bulk-import.md) |
| `Disk.Read` and `Disk.SignedURL` outside HTTP | [Email attachments](email-attachments.md) |
| `upload.Presign`, `upload.Confirm`, `upload.Uppy` | [Browser direct uploads](browser-direct-uploads.md) |
| `contrib/tus` | [Resumable uploads](resumable-uploads.md) |
| `upload.FromURL`, `upload.Fetch` | [Import from a URL](import-from-url.md) |
| Per-file results for bulk uploads (`Partial`, SHA-256 dedupe) | [Mobile uploads](mobile-uploads.md) |
| `upload.Handler` | [Strip photo metadata](strip-photo-metadata.md), [Virus scanning](virus-scanning.md) |
| `scan.Inspect`, `upload.Inspectors` | [Virus scanning](virus-scanning.md) |
| `signedurl`, `fshttp.Signed`, `Store.DownloadURL` | [Private downloads](private-downloads.md), [Chat attachments](chat-attachments.md), [Exports](exports.md), [Email attachments](email-attachments.md) |
| `WithPublicURL`, `fshttp.Public`, `WithVisibilityDisks` | [Public files behind a CDN](public-cdn.md), [Avatars](avatars.md) |
| `quota`, `folders`, `versions`, `share`, `audit` plugins | [Team drive](team-drive.md), [SaaS tenants](saas-tenants.md) |
| `Disk.Scope`, `Mux.OpenManager` | [SaaS tenants](saas-tenants.md) |
| `filesystem.Mirror`, `CopyAcross` | [Backups and mirrors](backups-and-mirrors.md), [Move a local store to S3](move-to-s3.md) |
| `upload.Sweep` | [Exports](exports.md), [Browser direct uploads](browser-direct-uploads.md) |

## Bigger programs

- [examples/drive](../../examples/drive): a team drive with every records plugin on one Store, larger than the [team drive](team-drive.md) recipe.
- [examples/mediaserver](../../examples/mediaserver): the avatar flow written by hand, without variants or cropping.

## Related

- [Your first app](../start/first-app.md): the library from zero, before any recipe.
- [Uploads](../concepts/uploads.md): policies, processors, inspectors and every upload path.
- [Files as records](../records/README.md): the Store, attachments and plugins the recipes build on.
- [Going to production](../start/production.md): what every deployment needs, whatever the recipe.
