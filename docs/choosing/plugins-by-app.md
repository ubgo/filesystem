# Plugins by app

The library is a set of pieces, and most products need the same handful of them. This page names, for six common kinds of application, the adapters to store on, the plugins to install, and the recipes to copy from. Each row links the guide and the runnable example, so you can start from a program that already works and remove what you do not need.

## Before you start

- Every kind of app below uses the same core: a `Disk` per backend, an `upload.Policy` per endpoint, and signed or public URLs to serve. If those are new, follow the [learning path](../README.md#learning-path) to step 5 first.
- The plugins marked records need the records layer; [Do I need records?](do-i-need-records.md) explains when it is worth it.

## At a glance

| App | Store on | Records plugins | Other modules | Start from |
|---|---|---|---|---|
| B2B SaaS | S3, GCS or Azure; a bucket per customer for those who ask | quota, audit, acl, webhooks, egress | `contrib/crypt` for per-tenant keys, `scan` for malware | [SaaS tenants](../recipes/saas-tenants.md) |
| Social | S3 or R2 behind a CDN | variants, dimensions, quota | `contrib/video`, `contrib/audio`, `contrib/remote`, `contrib/svg` | [Avatars](../recipes/avatars.md), [Mobile uploads](../recipes/mobile-uploads.md) |
| E-commerce | S3 or R2 behind a CDN | variants, dimensions | `contrib/imaging` | [Product images](../recipes/product-images.md) |
| Document management | S3, GCS or Azure; WebDAV or SFTP for a customer's NAS | folders, tags, versions, share, acl, locks, audit | `contrib/pdf`, `contrib/office`, `contrib/ocr`, `contrib/archive` | [Team drive](../recipes/team-drive.md) |
| Media | S3 (Uppy multipart needs it) behind a signing CDN | variants, quota, egress | `contrib/video`, `contrib/audio`, `contrib/videoplatform`, `contrib/tus`, `contrib/cloudfront` or `contrib/cloudflare` | [Resumable uploads](../recipes/resumable-uploads.md) |
| Chat | S3, GCS or Azure | variants, quota, share | `contrib/remote` for link previews, `scan` | [Chat attachments](../recipes/chat-attachments.md) |

## B2B SaaS

Many customers in one system, where the one bug you cannot afford is one customer seeing another's file.

| Need | Use | Guide | Example |
|---|---|---|---|
| Every read, list and delete per tenant | a scoped disk and the records scope | [Multi-tenancy](../concepts/multi-tenancy.md) | [concepts-tenancy](../../examples/concepts-tenancy) |
| Storage limits per plan | `records/quota` | [quota](../plugins/quota.md) | [records-quota](../../examples/records-quota) |
| Who did what, for the customer's admin | `records/audit` | [audit](../plugins/audit.md) | [records-audit](../../examples/records-audit) |
| Permissions inside a workspace | `records/acl` | [acl](../plugins/acl.md) | [records-acl](../../examples/records-acl) |
| Tell the customer's systems about new files | `records/webhooks` | [webhooks](../plugins/webhooks.md) | [records-webhooks](../../examples/records-webhooks) |
| Bill bandwidth | `records/egress` | [egress](../plugins/egress.md) | [records-egress](../../examples/records-egress) |
| Encryption with a key per tenant | `contrib/crypt` | [crypt](../plugins/crypt.md) | [security-crypt](../../examples/security-crypt) |
| Logo, favicon and email header per tenant | single roles and variants | [Tenant branding](../recipes/tenant-branding.md) | [recipe-tenant-branding](../../examples/recipe-tenant-branding) |
| CSV imports, exports, closing an account | jobs, exports, owner deletion | [Bulk import](../recipes/bulk-import.md), [Exports](../recipes/exports.md), [Account deletion](../recipes/account-deletion.md) | [recipe-bulk-import](../../examples/recipe-bulk-import), [recipe-exports](../../examples/recipe-exports), [recipe-account-deletion](../../examples/recipe-account-deletion) |

## Social

User photos and videos, viewed far more often than they are uploaded.

| Need | Use | Guide | Example |
|---|---|---|---|
| Profile pictures and banners | single roles, variants | [Avatars](../recipes/avatars.md), [Cover images](../recipes/cover-images.md) | [recipe-avatars](../../examples/recipe-avatars), [recipe-cover-images](../../examples/recipe-cover-images) |
| Uploads from the phone's camera roll | tus or form posts with idempotent retries | [Mobile uploads](../recipes/mobile-uploads.md) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| No GPS position of the user's home in a public photo | re-encoding in `Process` | [Strip photo metadata](../recipes/strip-photo-metadata.md) | [recipe-strip-photo-metadata](../../examples/recipe-strip-photo-metadata) |
| Thumbnails, crops, srcset | `contrib/imaging/variants` | [variants](../plugins/variants.md), [Image transforms](../plugins/image-transforms.md) | [records-variants](../../examples/records-variants), [imaging-transforms](../../examples/imaging-transforms) |
| Video and voice notes | `contrib/video`, `contrib/audio` | [video](../plugins/video.md), [audio](../plugins/audio.md) | [media](../../examples/media) |
| Link previews and an image proxy | `contrib/remote` | [remote](../plugins/remote.md) | [remote-media](../../examples/remote-media) |
| Images pasted from a link | `upload.FromURL` | [Import from URL](../recipes/import-from-url.md) | [recipe-import-from-url](../../examples/recipe-import-from-url) |
| Public media at low cost | R2 and a public CDN | [Public CDN](../recipes/public-cdn.md), [S3-compatible stores](../adapters/s3-compatible.md) | [recipe-public-cdn](../../examples/recipe-public-cdn) |

## E-commerce

A catalogue of product photos, shown at many sizes on every device, plus documents for orders.

| Need | Use | Guide | Example |
|---|---|---|---|
| Ordered product galleries | attachments with positions, variants | [Product images](../recipes/product-images.md) | [recipe-product-images](../../examples/recipe-product-images) |
| Width and height for layout | `contrib/imaging/dimensions` | [dimensions](../plugins/dimensions.md) | [records-dimensions](../../examples/records-dimensions) |
| Catalogue images from a CDN | public URLs | [Public CDN](../recipes/public-cdn.md) | [recipe-public-cdn](../../examples/recipe-public-cdn) |
| Product imports from a spreadsheet | a job over a stored CSV | [Bulk import](../recipes/bulk-import.md) | [recipe-bulk-import](../../examples/recipe-bulk-import) |
| Invoices by email | stream from the disk, or a signed link | [Email attachments](../recipes/email-attachments.md) | [recipe-email-attachments](../../examples/recipe-email-attachments) |

## Document management

Contracts, drawings and records that people organise, share, lock and keep for years.

| Need | Use | Guide | Example |
|---|---|---|---|
| A Drive-like file manager | folders (one file in several folders with `WithShortcuts`), tags, versions, share, audit | [Team drive](../recipes/team-drive.md) | [recipe-team-drive](../../examples/recipe-team-drive), [drive](../../examples/drive) |
| Permissions and check-out | `records/acl` (deny grants with `WithDeny`), `records/locks` (shared locks for co-editing with `WithSharedLocks`) | [acl](../plugins/acl.md), [locks](../plugins/locks.md) | [records-acl](../../examples/records-acl), [records-locks](../../examples/records-locks) |
| Text for search, previews, page counts | `contrib/pdf`, `contrib/office`, `contrib/ocr` | [pdf](../plugins/pdf.md), [office](../plugins/office.md), [ocr](../plugins/ocr.md) | [media](../../examples/media) (PDF) |
| Zip uploads unpacked safely | `contrib/archive` | [archive](../plugins/archive.md) | [security-archive](../../examples/security-archive) |
| Malware scanning before anyone opens a file | `scan`, quarantine | [Virus scanning](../recipes/virus-scanning.md), [Processing status](../records/processing-status.md) | [recipe-virus-scanning](../../examples/recipe-virus-scanning) |
| Downloads only the right person may fetch | short signed links | [Private downloads](../recipes/private-downloads.md) | [recipe-private-downloads](../../examples/recipe-private-downloads) |
| A customer's NAS or Nextcloud | the webdav or sftp adapter | [webdav](../adapters/webdav.md), [sftp](../adapters/sftp.md) | [adapters-webdav](../../examples/adapters-webdav), [adapters-sftp](../../examples/adapters-sftp) |

## Media

Large video and audio files, uploaded once, streamed many times.

| Need | Use | Guide | Example |
|---|---|---|---|
| Multi-GB browser uploads | `upload.Uppy` on S3 | [Browser direct uploads](../recipes/browser-direct-uploads.md) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Uploads that resume | `contrib/tus` | [Resumable uploads](../recipes/resumable-uploads.md) | [recipe-resumable-uploads](../../examples/recipe-resumable-uploads) |
| Posters, metadata, transcoding, adaptive streaming | `contrib/video` (an HLS ladder on your own storage with `Tool.HLS`), `contrib/audio`, `contrib/videoplatform` | [video](../plugins/video.md), [HLS ladder](../plugins/video.md#an-hls-ladder-on-your-own-storage), [audio](../plugins/audio.md) | [media](../../examples/media) |
| Paid content behind a CDN | `contrib/cloudfront` or `contrib/cloudflare` | [Serving files, step 11](../concepts/serving-files.md#step-11--signed-cdn-links) | [concepts-delivery](../../examples/concepts-delivery) |
| Bandwidth per customer | `records/egress` | [egress](../plugins/egress.md) | [records-egress](../../examples/records-egress) |

## Chat

Files in messages: many small images, some documents, seen only by the people in the conversation.

| Need | Use | Guide | Example |
|---|---|---|---|
| Files attached to messages, visible to members only | attachments, signed links | [Chat attachments](../recipes/chat-attachments.md) | [recipe-chat-attachments](../../examples/recipe-chat-attachments) |
| Thumbnails in the thread | variants | [variants](../plugins/variants.md) | [records-variants](../../examples/records-variants) |
| Previews of links people paste | `contrib/remote` | [remote](../plugins/remote.md) | [remote-media](../../examples/remote-media) |
| Uploads from phones | tus or idempotent form posts | [Mobile uploads](../recipes/mobile-uploads.md) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Scanning before delivery | `scan`, quarantine | [Virus scanning](../recipes/virus-scanning.md) | [recipe-virus-scanning](../../examples/recipe-virus-scanning) |

## The decision in short

1. Find the row for your kind of app and run its starting recipe's example as it is.
2. Remove the plugins you do not need; each is one value passed to `records.WithPlugins` when the Store is opened, and owns its own tables.
3. Add the cross-cutting pieces every deployment needs: interceptors for retries and metrics, scheduled maintenance jobs, and the production checklist ([Going to production](../start/production.md)).

## Pitfalls

- Plugins that call external programs need them installed where the app runs: FFmpeg and ffprobe for video and audio, Poppler for PDF, LibreOffice for office documents, Tesseract for OCR. Their guides say which and how they are sandboxed.
- Browser multipart uploads need the s3 driver. If you pick GCS or Azure for a media app, use tus or presigned PUTs instead ([Which upload method](which-upload-method.md)).
- Public media with a year-long cache must stay public. For anything that can later be made private or deleted, keep the cache short or purge the CDN on the events the variants guide lists.

## Related

- [Which adapter](which-adapter.md), [Which upload method](which-upload-method.md), [Serving files](serving-files.md), [Do I need records?](do-i-need-records.md).
- [Recipes](../recipes/README.md): every recipe with its program.
