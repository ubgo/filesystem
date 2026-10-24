# Do I need records?

The library has two layers you can use alone. The **Disk** stores bytes under keys and knows nothing about your users. **Records** (`github.com/ubgo/filesystem/records`, its own module) keeps each file as a row in your SQL database, attached to your models, with a trash, a processing status, events and plugins. This page helps you decide whether you need the second layer, and which of its plugins to install.

## Before you start

- [Disks and drivers](../concepts/disks-and-drivers.md) explains the Disk; [Records](../records/README.md) explains the rows, attachments and the two rules that keep bytes and rows consistent.
- Records needs a `*sql.DB` on SQLite, Postgres or MySQL, opened with the driver of your choice. The module requires the core module and `github.com/ubgo/sqlb`, and never imports a database driver itself.

## Disk only, or records

| You need | Disk only | Records |
|---|---|---|
| Store and read bytes, list a prefix, sign a link | yes | yes |
| Know which user uploaded a file, its display name, size, SHA-256 and type, without a backend call | you keep your own table | a `fs_files` row per file |
| Attach files to your models (a user's avatar, a product's gallery, in order) | you keep your own join table | `fs_attachments`, with roles, positions and single-file roles |
| Trash and restore, purge, delete everything an owner leaves behind | by hand | `Trash`, `Restore`, `Purge`, owner deletion and a retention sweep |
| Replace content or change visibility while keeping the file's ID | by hand | `Replace`, `SetVisibility` |
| A processing status clients can poll, quarantine until a scan passes | no | yes |
| Per-tenant isolation in every query | a scoped disk isolates keys | the scope filters every query, and a scoped disk isolates keys |
| Folders, tags, versions, shares, permissions, quotas, audit, webhooks | no | plugins |
| Ready-made HTTP endpoints (upload, list, reorder, download, trash) | `upload.Handler` and `fshttp` | `recordshttp` |
| Scheduled maintenance (unconfirmed uploads, abandoned multipart uploads) | `jobs/diskjobs`, a module that requires the core only | `jobs`, which adds the trash, orphan and audit sweeps |

Use the Disk alone for caches, generated exports nobody browses, backups, mirrors, partner drop folders, and any service where files are not things users own and manage. Use records as soon as a user can see a list of "their files", or a file belongs to a row in your database.

## The decision in short

1. If nothing in your database refers to the file, the Disk is enough. See [Install](../start/install.md) and [concepts-disks](../../examples/concepts-disks); schedule its sweeps with `jobs/diskjobs` ([jobs-calendar](../../examples/jobs-calendar)).
2. If a row of yours refers to the file (an avatar on a user, a document on a case), use records, so the reference cannot outlive the bytes or the bytes the reference. Start with [Your first app](../start/first-app.md) and [quickstart](../../examples/quickstart).
3. If you already have your own files table, you can keep it and store only keys with the Disk; you then own the consistency rules records gives you ([Consistency](../concepts/consistency.md)). Read the records tables from your ORM instead with [ent](../integrations/ent.md), [GORM](../integrations/gorm.md) or [sqlc](../integrations/sqlc.md) if you adopt records later.
4. If you run records, pick plugins from the table below. Each one is passed to `records.WithPlugins` when the Store is opened, and adds its own tables through the same migration ledger.

## Which records plugins

| Plugin | Package | Adds | Fits | Guide | Example |
|---|---|---|---|---|---|
| Quota | `records/quota` | Storage limits per scope (and per organisation or content class), reservations for uploads in flight, usage events | every multi-tenant or freemium product | [quota](../plugins/quota.md) | [records-quota](../../examples/records-quota), [records-quota-seats](../../examples/records-quota-seats) |
| Audit | `records/audit` | Activity feed, access log, tamper-evident trail, change feed | B2B SaaS, compliance, document management | [audit](../plugins/audit.md) | [records-audit](../../examples/records-audit) |
| Folders | `records/folders` | Virtual folders, optionally one file in several (shortcuts) | drives, document management | [folders](../plugins/folders.md) | [records-folders](../../examples/records-folders) |
| Tags | `records/tags` | Tags and tag counts | media libraries, document management | [tags](../plugins/tags.md) | [records-tags](../../examples/records-tags) |
| Versions | `records/versions` | Version history and restore, kept per plan or per file | drives, document management, design tools | [versions](../plugins/versions.md) | [records-versions](../../examples/records-versions) |
| Share | `records/share` | Expiring share links, shared folders, file requests | drives, client portals | [share](../plugins/share.md) | [records-share](../../examples/records-share) |
| Permissions | `records/acl` | Read, write and admin grants (or your own levels) on files, folders and your own containers, optionally expiring, and deny grants | drives, team workspaces | [acl](../plugins/acl.md) | [records-acl](../../examples/records-acl) |
| Locks | `records/locks` | Check-out locks, with WebDAV-style tokens, and shared locks with several holders | document management, legal, engineering drawings | [locks](../plugins/locks.md) | [records-locks](../../examples/records-locks) |
| Egress | `records/egress` | Bytes served per scope per day (or hour, or tenant's day), optionally per share link, for billing | usage-billed SaaS | [egress](../plugins/egress.md) | [records-egress](../../examples/records-egress) |
| Webhooks | `records/webhooks` | Signed outbound webhooks with retries, written in the change's transaction | platforms other systems integrate with | [webhooks](../plugins/webhooks.md) | [records-webhooks](../../examples/records-webhooks) |
| Upload sessions | `records/uploadsessions` | Upload tokens kept in the database, shared by every instance | mobile apps, third-party uploads, several instances | [upload sessions](../plugins/upload-sessions.md) | [records-upload-sessions](../../examples/records-upload-sessions) |
| Variants | `contrib/imaging/variants` | Thumbnails and resized copies, eager or on first request | anything showing images | [variants](../plugins/variants.md) | [records-variants](../../examples/records-variants) |
| Dimensions | `contrib/imaging/dimensions` | Image width and height on the file | layout without layout shift, minimum-size rules | [dimensions](../plugins/dimensions.md) | [records-dimensions](../../examples/records-dimensions) |
| Media and documents | `contrib/video`, `contrib/audio`, `contrib/pdf`, `contrib/ocr`, `contrib/vision` | Metadata, posters, waveforms, page counts, extracted text, labels | media apps, search over documents | [video](../plugins/video.md), [audio](../plugins/audio.md), [pdf](../plugins/pdf.md), [ocr](../plugins/ocr.md), [vision](../plugins/vision.md) | [media](../../examples/media) |

[Plugins by app](plugins-by-app.md) groups these by kind of product.

## Pitfalls

- Change the `fs_` tables only through the Store. Reading and joining them from your own queries is fine, but a row written behind the Store's back skips the ordering that keeps rows and objects consistent.
- `upload.Sweep` deletes every object under a prefix older than a cutoff, recorded or not. With records, use `Store.SweepOrphans` for orphan objects, and point `upload.Sweep` only at a staging prefix ([Records, two rules](../records/README.md#two-rules-keep-bytes-and-rows-consistent)).
- Owner types must be registered with `WithOwnerTypes`; attaching to an unregistered type is refused.
- A plugin's tables are created by the same migration ledger as the core tables: run migrations in your deploy, not lazily on the first request ([Migrations](../records/migrations.md)).

## Related

- [Records](../records/README.md), [Files](../records/files.md), [Attachments](../records/attachments.md).
- [Write a records plugin](../extending/write-a-records-plugin.md): tables, hooks and events of your own.
- [Which upload method](which-upload-method.md) and [Serving files](serving-files.md).
