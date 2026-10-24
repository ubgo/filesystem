# Feature index

Every feature `github.com/ubgo/filesystem` ships, grouped by area, with the guide that teaches it and the runnable program that shows it working. Use it to answer "can it do X?" and to jump straight to the page and section that explains how.

How to read a row:

- **Feature** is the capability in plain words.
- **Package** is the import path under `github.com/ubgo/`; a path starting with `filesystem/adapters/`, `filesystem/contrib/`, `filesystem/records`, `filesystem/jobs` or `filesystem/cmd/fs` lives in a Go module of its own, so `go get` it separately (see [Install](../start/install.md)). Where a feature spans several adapters the list is shortened, as in `filesystem/adapters/s3, gcs, azure`.
- **Guide** links to the section of the guide that covers the feature. Some features are covered inside a broader section rather than with a page of their own; the guide is still the best place to start.
- **Example** links to a program under [examples/](../../examples) that the test suite compiles and runs; run it from `examples/` with `go run ./<folder>`, after reading the folder's README for any service it needs. A dash means no example program exercises the feature yet; the guide and the package README still show the API.

Features the library deliberately leaves out, and the ones still in progress, are not listed. For backend-by-backend support of each capability see [Capabilities](capabilities.md); for every error code see [Error codes](errors.md).

## Contents

- [Core object operations](#core-object-operations) (30)
- [Keys and paths](#keys-and-paths) (15)
- [Disks, adapters and composition](#disks-adapters-and-composition) (33)
- [Visibility and access control](#visibility-and-access-control) (17)
- [URLs and delivery](#urls-and-delivery) (19)
- [Uploads](#uploads) (30)
- [Validation](#validation) (15)
- [File records and metadata](#file-records-and-metadata) (26)
- [Image processing](#image-processing) (28)
- [Video and audio](#video-and-audio) (10)
- [Documents and previews](#documents-and-previews) (6)
- [Security](#security) (21)
- [Quotas, usage and billing](#quotas-usage-and-billing) (11)
- [Lifecycle and retention](#lifecycle-and-retention) (19)
- [Integrity](#integrity) (12)
- [Observability](#observability) (7)
- [Events, hooks and background work](#events-hooks-and-background-work) (10)
- [Testing](#testing) (6)
- [HTTP serving](#http-serving) (16)
- [Developer experience](#developer-experience) (14)
- [Sharing](#sharing) (9)
- [Collaboration](#collaboration) (6)
- [Search and indexing](#search-and-indexing) (3)
- [Mobile backends](#mobile-backends) (6)
- [Social and user-generated content](#social-and-user-generated-content) (10)
- [SaaS and B2B](#saas-and-b2b) (7)
- [Compliance](#compliance) (4)

## Core object operations

Writing, reading, listing, copying and deleting objects on any disk.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Write bytes or a string | `filesystem` | [Disks and drivers: Step 1 — Write and read](../concepts/disks-and-drivers.md#step-1--write-and-read) | [concepts-disks](../../examples/concepts-disks) |
| Streaming write from an io.Reader | `filesystem` | [Disks and drivers: Step 1 — Write and read](../concepts/disks-and-drivers.md#step-1--write-and-read) | [concepts-disks](../../examples/concepts-disks) |
| Read a whole file | `filesystem` | [Disks and drivers: Step 1 — Write and read](../concepts/disks-and-drivers.md#step-1--write-and-read) | [concepts-disks](../../examples/concepts-disks) |
| Streaming read | `filesystem` | [Disks and drivers: Step 1 — Write and read](../concepts/disks-and-drivers.md#step-1--write-and-read) | [concepts-disks](../../examples/concepts-disks) |
| Delete (idempotent) | `filesystem` | [Disks and drivers: Step 6 — Delete](../concepts/disks-and-drivers.md#step-6--delete) | [concepts-disks](../../examples/concepts-disks) |
| Exists | `filesystem` | [Disks and drivers: Step 3 — Ask about a key](../concepts/disks-and-drivers.md#step-3--ask-about-a-key) | [concepts-disks](../../examples/concepts-disks) |
| Stat | `filesystem` | [Disks and drivers: Step 3 — Ask about a key](../concepts/disks-and-drivers.md#step-3--ask-about-a-key) | [concepts-disks](../../examples/concepts-disks) |
| List a directory | `filesystem` | [Disks and drivers: Step 4 — List](../concepts/disks-and-drivers.md#step-4--list) | [concepts-disks](../../examples/concepts-disks) |
| Recursive listing and fs.WalkDir | `filesystem` | [Disks and drivers: Step 4 — List](../concepts/disks-and-drivers.md#step-4--list) | [concepts-disks](../../examples/concepts-disks) |
| Paginated listing with a cursor | `filesystem` | [Disks and drivers: Step 4 — List](../concepts/disks-and-drivers.md#step-4--list) | [concepts-disks](../../examples/concepts-disks) |
| Copy within a disk | `filesystem` | [Disks and drivers: Step 5 — Copy and move](../concepts/disks-and-drivers.md#step-5--copy-and-move) | [concepts-disks](../../examples/concepts-disks) |
| Move and rename | `filesystem` | [Disks and drivers: Step 5 — Copy and move](../concepts/disks-and-drivers.md#step-5--copy-and-move) | [concepts-disks](../../examples/concepts-disks) |
| Batch delete with per-key errors | `filesystem` | [Errors: Step 5 — Batch failures](../concepts/errors.md#step-5--batch-failures) | [concepts-errors](../../examples/concepts-errors) |
| Delete a directory (prefix) | `filesystem` | [Disks and drivers: Step 6 — Delete](../concepts/disks-and-drivers.md#step-6--delete) | [concepts-disks](../../examples/concepts-disks) |
| Create a directory | `filesystem` | [Keys: Step 2 — Directories](../concepts/keys.md#step-2--directories) | [concepts-keys](../../examples/concepts-keys) |
| Directory exists | `filesystem` | [Keys: Step 2 — Directories](../concepts/keys.md#step-2--directories) | [concepts-keys](../../examples/concepts-keys) |
| Ranged read | `filesystem` | [Disks and drivers: Step 2 — Read part of an object](../concepts/disks-and-drivers.md#step-2--read-part-of-an-object) | [concepts-disks](../../examples/concepts-disks) |
| Append | `filesystem` | [Disks and drivers: Step 9 — Append, conditional reads and object state](../concepts/disks-and-drivers.md#step-9--append-conditional-reads-and-object-state) | [concepts-disks](../../examples/concepts-disks) |
| Copy and move between disks | `filesystem` | [Configuration: Step 5 — The Manager](../concepts/configuration.md#step-5--the-manager) | [concepts-configuration](../../examples/concepts-configuration) |
| Create-only write (if not exists) | `filesystem` | [Consistency: One object](../concepts/consistency.md#one-object) | [concepts-consistency](../../examples/concepts-consistency) |
| Compare-and-swap write by ETag | `filesystem` | [Consistency: One object](../concepts/consistency.md#one-object) | [adapters-sqlblob](../../examples/adapters-sqlblob) |
| Write options and stored HTTP headers | `filesystem` | [Uploads: Stored HTTP headers](../concepts/uploads.md#stored-http-headers) | [adapters-s3](../../examples/adapters-s3) |
| Custom object metadata | `filesystem` | [Capabilities: Step 4 — Check before calling](../concepts/capabilities.md#step-4--check-before-calling) | [concepts-capabilities](../../examples/concepts-capabilities) |
| Object tags | `filesystem/adapters/s3` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [adapters-s3](../../examples/adapters-s3) |
| context.Context on every call | `filesystem` | [Disks and drivers: Conventions every call follows](../concepts/disks-and-drivers.md#conventions-every-call-follows) | [concepts-disks](../../examples/concepts-disks) |
| Capability discovery | `filesystem` | [Capabilities: Step 1 — What each driver declares](../concepts/capabilities.md#step-1--what-each-driver-declares) | [concepts-capabilities](../../examples/concepts-capabilities) |
| Native client escape hatch (filesystem.As) | `filesystem` | [Disks and drivers: Step 8 — Check at start-up, and reach the native client](../concepts/disks-and-drivers.md#step-8--check-at-start-up-and-reach-the-native-client) | [concepts-disks](../../examples/concepts-disks) |
| Set modification time | `filesystem` | [Disks and drivers: Step 9 — Append, conditional reads and object state](../concepts/disks-and-drivers.md#step-9--append-conditional-reads-and-object-state) | [concepts-disks](../../examples/concepts-disks) |
| Conditional reads (If-None-Match, If-Modified-Since) | `filesystem` | [Disks and drivers: Step 9 — Append, conditional reads and object state](../concepts/disks-and-drivers.md#step-9--append-conditional-reads-and-object-state) | [concepts-disks](../../examples/concepts-disks) |
| Content type sniffed on write | `filesystem/sniff` | [Security: Step 2 — Content type from the bytes](../concepts/security.md#step-2--content-type-from-the-bytes) | [concepts-security](../../examples/concepts-security) |

## Keys and paths

How keys are cleaned, checked and generated.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Key normalisation (CleanKey) | `filesystem` | [Keys: Step 1 — What CleanKey does](../concepts/keys.md#step-1--what-cleankey-does) | [concepts-keys](../../examples/concepts-keys) |
| Traversal refused, never cleaned | `filesystem` | [Security: Step 1 — Traversal](../concepts/security.md#step-1--traversal) | [concepts-security](../../examples/concepts-security) |
| Root prefix per disk (scoped disks) | `filesystem` | [Multi-tenancy: Step 1 — A scoped disk per tenant](../concepts/multi-tenancy.md#step-1--a-scoped-disk-per-tenant) | [concepts-tenancy](../../examples/concepts-tenancy) |
| Generated random storage keys | `filesystem/upload` | [Keys: Choosing keys](../concepts/keys.md#choosing-keys) | [concepts-uploads](../../examples/concepts-uploads) |
| Custom key strategy | `filesystem/upload` | [Uploads: Key layouts and name collisions](../concepts/uploads.md#key-layouts-and-name-collisions) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Original filename kept apart from the key | `filesystem/records` | [Files: Who uploaded it, when, and the original filename](../records/files.md#who-uploaded-it-when-and-the-original-filename) | [records-files](../../examples/records-files) |
| Filename sanitisation | `filesystem/upload` | [Security: Step 3 — File names](../concepts/security.md#step-3--file-names) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Unicode NFC key normalisation | `filesystem/contrib/keynorm` | [Keys: Unicode spellings](../concepts/keys.md#unicode-spellings) | [concepts-keys](../../examples/concepts-keys) |
| Letter-case semantics | `filesystem` | [Keys: Letter case, reserved names and portable keys](../concepts/keys.md#letter-case-reserved-names-and-portable-keys) | [adapters-sftp](../../examples/adapters-sftp) |
| Control characters and NUL refused | `filesystem` | [Keys: Step 1 — What CleanKey does](../concepts/keys.md#step-1--what-cleankey-does) | [concepts-keys](../../examples/concepts-keys) |
| Key length limits | `filesystem` | [Keys: Step 1 — What CleanKey does](../concepts/keys.md#step-1--what-cleankey-does) | [concepts-keys](../../examples/concepts-keys) |
| Reserved names on local disks | `filesystem/local` | [Keys: Letter case, reserved names and portable keys](../concepts/keys.md#letter-case-reserved-names-and-portable-keys) | [concepts-keys](../../examples/concepts-keys) |
| Name collision policy (auto-suffix) | `filesystem/upload` | [Uploads: Key layouts and name collisions](../concepts/uploads.md#key-layouts-and-name-collisions) | [concepts-uploads](../../examples/concepts-uploads) |
| Sharded key layout | `filesystem/upload` | [Uploads: Key layouts and name collisions](../concepts/uploads.md#key-layouts-and-name-collisions) | [concepts-uploads](../../examples/concepts-uploads) |
| Escaping characters a backend cannot store | `filesystem` | [Keys: Characters a backend cannot store](../concepts/keys.md#characters-a-backend-cannot-store) | [concepts-keys](../../examples/concepts-keys) |

## Disks, adapters and composition

Backends, named disks, configuration, and disks built from other disks.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Named disks (Manager) | `filesystem` | [Configuration: Step 5 — The Manager](../concepts/configuration.md#step-5--the-manager) | [concepts-configuration](../../examples/concepts-configuration) |
| Default disk | `filesystem` | [Configuration: Step 5 — The Manager](../concepts/configuration.md#step-5--the-manager) | [concepts-configuration](../../examples/concepts-configuration) |
| Local directory adapter | `filesystem/local` | [Local disk](../adapters/local.md) | [adapters-local](../../examples/adapters-local) |
| S3 and S3-compatible adapter | `filesystem/adapters/s3` | [Amazon S3](../adapters/s3.md) | [adapters-s3](../../examples/adapters-s3) |
| Google Cloud Storage adapter | `filesystem/adapters/gcs` | [Google Cloud Storage](../adapters/gcs.md) | [adapters-gcs](../../examples/adapters-gcs) |
| Azure Blob Storage adapter | `filesystem/adapters/azure` | [Azure Blob Storage](../adapters/azure.md) | [adapters-azure](../../examples/adapters-azure) |
| Azure block size and concurrency for large blobs | `filesystem/adapters/azure` | [Azure Blob Storage: Large blobs: block size and concurrency](../adapters/azure.md#large-blobs-block-size-and-concurrency) | [adapters-azure](../../examples/adapters-azure) |
| In-memory adapter | `filesystem/memory` | [In memory](../adapters/memory.md) | [adapters-memory](../../examples/adapters-memory) |
| SFTP, FTP and FTPS adapters | `filesystem/adapters/sftp`, `filesystem/adapters/ftp` | [SFTP](../adapters/sftp.md) | [adapters-sftp](../../examples/adapters-sftp) |
| Google Drive, Dropbox and OneDrive adapters | `filesystem/adapters/gdrive`, `dropbox`, `onedrive` | [Google Drive](../adapters/gdrive.md) | [adapters-consumer-drives](../../examples/adapters-consumer-drives) |
| SQL database adapter (sqlblob) | `filesystem/adapters/sqlblob` | [SQL table (sqlblob)](../adapters/sqlblob.md) | [adapters-sqlblob](../../examples/adapters-sqlblob) |
| Scoped sub-disk | `filesystem` | [Multi-tenancy: Step 1 — A scoped disk per tenant](../concepts/multi-tenancy.md#step-1--a-scoped-disk-per-tenant) | [concepts-tenancy](../../examples/concepts-tenancy) |
| Read-only disk | `filesystem/layer` | [Interceptors: Step 6 — ReadOnly](../concepts/interceptors.md#step-6--readonly) | [concepts-interceptors](../../examples/concepts-interceptors) |
| Route by prefix (mount) | `filesystem` | [Composing disks: Step 1 — Route by prefix](../concepts/composing-disks.md#step-1--route-by-prefix) | [concepts-composing](../../examples/concepts-composing) |
| Mirror writes to a second disk | `filesystem` | [Backups and mirrors: Step 2: Mirror the app's writes](../recipes/backups-and-mirrors.md#step-2-mirror-the-apps-writes) | [recipe-backups-and-mirrors](../../examples/recipe-backups-and-mirrors) |
| Read failover | `filesystem` | [Composing disks: Step 2 — Fail reads over](../concepts/composing-disks.md#step-2--fail-reads-over) | [concepts-composing](../../examples/concepts-composing) |
| Move every file to another disk | `filesystem/jobs` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Moving every file to another disk](../maintenance/README.md#moving-every-file-to-another-disk) | [maintenance](../../examples/maintenance) |
| Interceptors around every operation | `filesystem/layer` | [Interceptors: Step 1 — The operation model](../concepts/interceptors.md#step-1--the-operation-model) | [concepts-interceptors](../../examples/concepts-interceptors) |
| Retry with backoff | `filesystem/layer` | [Interceptors: Step 3 — Retry](../concepts/interceptors.md#step-3--retry) | [concepts-interceptors](../../examples/concepts-interceptors) |
| Timeouts | `filesystem/layer` | [Interceptors: Step 4 — Timeout](../concepts/interceptors.md#step-4--timeout) | [concepts-interceptors](../../examples/concepts-interceptors) |
| Concurrency and bandwidth limits | `filesystem/layer` | [Interceptors: Step 5 — Limit](../concepts/interceptors.md#step-5--limit) | [concepts-interceptors](../../examples/concepts-interceptors) |
| Read cache | `filesystem` | [Composing disks: Step 4 — Cache reads](../concepts/composing-disks.md#step-4--cache-reads) | [concepts-composing](../../examples/concepts-composing) |
| DSN configuration (Mux) | `filesystem` | [Configuration: Step 2 — Open a disk from a DSN](../concepts/configuration.md#step-2--open-a-disk-from-a-dsn) | [concepts-configuration](../../examples/concepts-configuration) |
| Disks from the environment | `filesystem` | [Configuration: Step 4 — A Manager from the environment](../concepts/configuration.md#step-4--a-manager-from-the-environment) | [concepts-configuration](../../examples/concepts-configuration) |
| Overlay (copy-on-write) | `filesystem` | [Composing disks: Step 3 — Overlay copy on write](../concepts/composing-disks.md#step-3--overlay-copy-on-write) | [concepts-composing](../../examples/concepts-composing) |
| Fault injection | `filesystem/layer` | [Interceptors: Step 10b — Inject faults in tests](../concepts/interceptors.md#step-10b--inject-faults-in-tests) | [concepts-interceptors](../../examples/concepts-interceptors) |
| A disk as an io/fs.FS | `filesystem` | [Serving files: A disk as an io/fs.FS](../concepts/serving-files.md#a-disk-as-an-iofsfs) | [concepts-serving](../../examples/concepts-serving) |
| Per-tenant disks opened at run time | `filesystem` | [SaaS tenants: Step 2: Disks from the environment](../recipes/saas-tenants.md#step-2-disks-from-the-environment) | [recipe-saas-tenants](../../examples/recipe-saas-tenants) |
| Hedged reads | `filesystem/layer` | [Interceptors: Step 10c — Hedge slow reads](../concepts/interceptors.md#step-10c--hedge-slow-reads) | [concepts-interceptors](../../examples/concepts-interceptors) |
| Vercel Blob and Cloudinary adapters | `filesystem/adapters/vercelblob`, `cloudinary` | [Vercel Blob](../adapters/vercelblob.md) | [adapters-hosted](../../examples/adapters-hosted) |
| Per-call KMS key, storage class, tags and hold on GCS and Azure | `filesystem/adapters/gcs`, `azure` | [Google Cloud Storage: A key or class per call](../adapters/gcs.md#a-key-or-class-per-call) | [adapters-gcs](../../examples/adapters-gcs) |
| Azure behind a custom domain, Front Door or a private endpoint | `filesystem/adapters/azure` | [Azure Blob Storage: Behind a custom domain or Front Door](../adapters/azure.md#behind-a-custom-domain-or-front-door) | [adapters-azure](../../examples/adapters-azure) |
| Your own table names or a table prefix for records | `filesystem/records`, `filesystem/contrib/ent`, `gorm`, `sqlc` | [Migrations: Step 6 — Other table names](../records/migrations.md#step-6--other-table-names) | [integrations-orm](../../examples/integrations-orm) |

## Visibility and access control

Public and private files, authorisation and permissions.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Public or private per file | `filesystem` | [Public and private files: Step 1 — Per-object visibility](../concepts/public-and-private.md#step-1--per-object-visibility) | [concepts-visibility](../../examples/concepts-visibility) |
| Default visibility per disk | `filesystem` | [Public and private files: Step 2 — Per-disk visibility](../concepts/public-and-private.md#step-2--per-disk-visibility) | [concepts-visibility](../../examples/concepts-visibility) |
| Change visibility later | `filesystem`, `filesystem/records` | [Replace and visibility: Step 3 — Change visibility on one disk](../records/replace-and-visibility.md#step-3--change-visibility-on-one-disk) | [records-replace](../../examples/records-replace) |
| Separate public and private disks | `filesystem/records` | [Replace and visibility: Step 4 — Separate public and private disks](../records/replace-and-visibility.md#step-4--separate-public-and-private-disks) | [recipe-public-cdn](../../examples/recipe-public-cdn) |
| Authorisation hook on downloads | `filesystem/records/recordshttp` | [HTTP endpoints: Step 2 — Write the two hooks](../records/http-endpoints.md#step-2--write-the-two-hooks) | [records-http](../../examples/records-http) |
| Permission rules on files | `filesystem/records/acl` | [Permissions](../plugins/acl.md) | [records-acl](../../examples/records-acl) |
| Per-object ACLs on S3 and GCS | `filesystem/adapters/s3`, `filesystem/adapters/gcs` | [Amazon S3: 5. Visibility: bucket policy or object ACLs](../adapters/s3.md#5-visibility-bucket-policy-or-object-acls) | [adapters-gcs](../../examples/adapters-gcs) |
| Tenant isolation by prefix and guard | `filesystem`, `filesystem/layer` | [Multi-tenancy: Step 3 — A guard for code that builds keys](../concepts/multi-tenancy.md#step-3--a-guard-for-code-that-builds-keys) | [concepts-tenancy](../../examples/concepts-tenancy) |
| Uploader recorded on each file | `filesystem/records` | [Files: Who uploaded it, when, and the original filename](../records/files.md#who-uploaded-it-when-and-the-original-filename) | [records-files](../../examples/records-files) |
| Folder-level permissions | `filesystem/records/acl` | [Permissions: Step 2 — Grant](../plugins/acl.md#step-2--grant) | [records-acl](../../examples/records-acl) |
| Share with named users | `filesystem/records/acl` | [Permissions: Step 2 — Grant](../plugins/acl.md#step-2--grant) | [records-acl](../../examples/records-acl) |
| Temporary credentials scoped to a prefix | `filesystem/adapters/s3`, `gcs`, `azure` | [Azure Blob Storage: 12. Scoped SAS and IP-restricted URLs](../adapters/azure.md#12-scoped-sas-and-ip-restricted-urls) | [adapters-s3](../../examples/adapters-s3) |
| Delegated S3 credentials in GovCloud, China and on SSE-KMS buckets | `filesystem/adapters/s3` | [Amazon S3: 15. Bucket setup: lifecycle, replication, object ACLs, scoped credentials](../adapters/s3.md#15-bucket-setup-lifecycle-replication-object-acls-scoped-credentials) | [adapters-s3](../../examples/adapters-s3) |
| Your own permission levels and containers (projects, teams) | `filesystem/records/acl` | [Permissions: Your own permission model](../plugins/acl.md#your-own-permission-model) | [records-acl](../../examples/records-acl) |
| Expiring grants | `filesystem/records/acl` | [Permissions: Your own permission model](../plugins/acl.md#your-own-permission-model) | [records-acl](../../examples/records-acl) |
| Public direct uploads (the presigned PUT or form pins the visibility) | `filesystem`, `filesystem/upload` | [Uploads: Public direct uploads](../concepts/uploads.md#public-direct-uploads) | [concepts-uploads](../../examples/concepts-uploads) |
| Deny grants (everyone except one person) | `filesystem/records/acl` | [Permissions: Deny grants](../plugins/acl.md#deny-grants) | [records-acl](../../examples/records-acl) |

## URLs and delivery

Signed links, public URLs, CDNs and image URLs.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Public URL | `filesystem` | [Serving files: Step 7 — Public files](../concepts/serving-files.md#step-7--public-files) | [concepts-serving](../../examples/concepts-serving) |
| Temporary signed URL | `filesystem`, `filesystem/signedurl` | [Serving files: Step 1 — Signed URLs on any backend](../concepts/serving-files.md#step-1--signed-urls-on-any-backend) | [concepts-serving](../../examples/concepts-serving) |
| CDN base URL | `filesystem` | [Serving files: Step 8 — A CDN in front](../concepts/serving-files.md#step-8--a-cdn-in-front) | [concepts-serving](../../examples/concepts-serving) |
| Download filename and disposition | `filesystem/fshttp` | [Serving files: Step 5 — Active content is always a download](../concepts/serving-files.md#step-5--active-content-is-always-a-download) | [concepts-serving](../../examples/concepts-serving) |
| Response content-type override on signed links | `filesystem/adapters/s3` | [Amazon S3: 7. Signed URLs](../adapters/s3.md#7-signed-urls) | [adapters-s3](../../examples/adapters-s3) |
| App-routed download URL | `filesystem/fshttp`, `filesystem/records/recordshttp` | [HTTP endpoints: Step 6 — Read, replace and trash a file](../records/http-endpoints.md#step-6--read-replace-and-trash-a-file) | [records-http](../../examples/records-http) |
| Signed URLs for local and memory disks | `filesystem/signedurl` | [Serving files: Step 1 — Signed URLs on any backend](../concepts/serving-files.md#step-1--signed-urls-on-any-backend) | [concepts-serving](../../examples/concepts-serving) |
| Cache-busting immutable URLs | `filesystem/fshttp` | [Serving files: Step 9 — Cache-Control](../concepts/serving-files.md#step-9--cache-control) | [concepts-serving](../../examples/concepts-serving) |
| CDN signed URLs and cookies (CloudFront, Cloudflare) | `filesystem/contrib/cloudfront`, `filesystem/contrib/cloudflare` | [Serving files: Step 11 — Signed CDN links](../concepts/serving-files.md#step-11--signed-cdn-links) | [concepts-delivery](../../examples/concepts-delivery) |
| CDN signs downloads on a disk that presigns (CloudFront over S3, Cloudflare over R2 or GCS) | `filesystem` | [Serving files: Which path signs which method](../concepts/serving-files.md#which-path-signs-which-method) | [concepts-serving](../../examples/concepts-serving) |
| Transform URLs (why variants are named, not signed) | `filesystem/contrib/imaging/variants` | [Image variants: Why there are no signed transform URLs](../plugins/variants.md#why-there-are-no-signed-transform-urls) | [records-variants](../../examples/records-variants) |
| Variant URLs | `filesystem/contrib/imaging/variants` | [Image variants: Step 3 — Get a variant](../plugins/variants.md#step-3--get-a-variant) | [records-variants](../../examples/records-variants) |
| Expiry bounds on signed links | `filesystem` | [Security: Step 6 — Signed URLs](../concepts/security.md#step-6--signed-urls) | [concepts-security](../../examples/concepts-security) |
| IP-restricted signed links | `filesystem`, `filesystem/signedurl` | [Serving files: Limiting a link to an IP range](../concepts/serving-files.md#limiting-a-link-to-an-ip-range) | [concepts-serving](../../examples/concepts-serving) |
| Custom domain per tenant | `filesystem` | [Multi-tenancy: A domain per tenant](../concepts/multi-tenancy.md#a-domain-per-tenant) | [concepts-tenancy](../../examples/concepts-tenancy) |
| Data URIs for tiny files | `filesystem` | [Serving files: Step 13 — Tiny files as data URIs](../concepts/serving-files.md#step-13--tiny-files-as-data-uris) | [concepts-delivery](../../examples/concepts-delivery) |
| Responsive srcset markup | `filesystem/contrib/imaging/variants` | [Image variants: Responsive images: srcset](../plugins/variants.md#responsive-images-srcset) | [imaging-uploads](../../examples/imaging-uploads) |
| Format negotiation from Accept | `filesystem/contrib/imaging/variants` | [Image variants: Focal points and format negotiation](../plugins/variants.md#focal-points-and-format-negotiation) | [records-variants](../../examples/records-variants) |
| Tokens in a header or cookie, or another signing scheme | `filesystem/fshttp` | [Serving files: Step 3 — Refusals say nothing](../concepts/serving-files.md#step-3--refusals-say-nothing) | [concepts-serving](../../examples/concepts-serving) |

## Uploads

Every way bytes arrive: forms, direct to the bucket, resumable, imports.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Server-side upload handler | `filesystem/upload` | [Uploads: Step 3 — The form handler](../concepts/uploads.md#step-3--the-form-handler) | [concepts-uploads](../../examples/concepts-uploads) |
| Streaming uploads with no temp files | `filesystem/upload` | [Uploads: Step 2 — Receive: the core of every server-side path](../concepts/uploads.md#step-2--receive-the-core-of-every-server-side-path) | [concepts-uploads](../../examples/concepts-uploads) |
| Direct-to-bucket presigned PUT | `filesystem/upload` | [Uploads: Step 4 — Direct uploads: Presign, then Confirm](../concepts/uploads.md#step-4--direct-uploads-presign-then-confirm) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Presigned POST forms | `filesystem`, `filesystem/adapters/s3`, `filesystem/adapters/gcs` | [Uploads: Form uploads the bucket checks](../concepts/uploads.md#form-uploads-the-bucket-checks) | [recipe-browser-form-uploads](../../examples/recipe-browser-form-uploads) |
| Direct-upload confirmation | `filesystem/upload` | [Uploads: Step 4 — Direct uploads: Presign, then Confirm](../concepts/uploads.md#step-4--direct-uploads-presign-then-confirm) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Server-side multipart uploads | `filesystem` | [Disks and drivers: Step 7 — Multipart uploads](../concepts/disks-and-drivers.md#step-7--multipart-uploads) | [adapters-s3](../../examples/adapters-s3) |
| Browser multipart uploads for Uppy | `filesystem/upload` | [Uploads: Step 5 — Browser multipart uploads with Uppy](../concepts/uploads.md#step-5--browser-multipart-uploads-with-uppy) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| Resumable uploads (tus) | `filesystem/contrib/tus` | [Resumable uploads](../recipes/resumable-uploads.md) | [recipe-resumable-uploads](../../examples/recipe-resumable-uploads) |
| Resumable uploads across several instances | `filesystem/contrib/tus` | [Resumable uploads: For production](../recipes/resumable-uploads.md#for-production) | [recipe-resumable-uploads](../../examples/recipe-resumable-uploads) |
| Upload disk chosen per request or by visibility | `filesystem/records/recordshttp` | [HTTP endpoints: Choose the disk per request](../records/http-endpoints.md#choose-the-disk-per-request) | [records-http](../../examples/records-http) |
| Custom chunked uploads | `filesystem/upload` | [Upload clients: Step 2 — Chunked uploads](../concepts/upload-clients.md#step-2--chunked-uploads) | [concepts-upload-clients](../../examples/concepts-upload-clients) |
| Upload progress callback | `filesystem/upload` | [Upload clients: Step 3 — Progress on the server](../concepts/upload-clients.md#step-3--progress-on-the-server) | [concepts-upload-clients](../../examples/concepts-upload-clients) |
| Abort and sweep abandoned multipart uploads | `filesystem` | [Uploads: Leftovers that are not objects](../concepts/uploads.md#leftovers-that-are-not-objects) | [jobs-scheduler](../../examples/jobs-scheduler) |
| Upload from a remote URL (SSRF-safe) | `filesystem/upload` | [Uploads: Step 7 — Imports from a URL](../concepts/uploads.md#step-7--imports-from-a-url) | [recipe-import-from-url](../../examples/recipe-import-from-url) |
| Upload from base64 and data URIs | `filesystem/upload` | [Uploads: Base64 bodies and data URIs](../concepts/uploads.md#base64-bodies-and-data-uris) | [concepts-uploads](../../examples/concepts-uploads) |
| Cache then promote | `filesystem/upload` | [Uploads: Cache, then promote](../concepts/uploads.md#cache-then-promote) | [concepts-uploads](../../examples/concepts-uploads) |
| Upload sessions with expiry | `filesystem/upload`, `filesystem/records/uploadsessions` | [Uploads: Upload sessions: a token valid for a window](../concepts/uploads.md#upload-sessions-a-token-valid-for-a-window) | [concepts-uploads](../../examples/concepts-uploads) |
| Several files per request | `filesystem/upload` | [Uploads: Step 3 — The form handler](../concepts/uploads.md#step-3--the-form-handler) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Folder uploads with relative paths | `filesystem/upload` | [Upload clients: Step 4 — Folder uploads](../concepts/upload-clients.md#step-4--folder-uploads) | [concepts-upload-clients](../../examples/concepts-upload-clients) |
| Idempotent uploads (Idempotency-Key) | `filesystem/upload` | [Upload clients: Step 5 — Retries without duplicates](../concepts/upload-clients.md#step-5--retries-without-duplicates) | [concepts-upload-clients](../../examples/concepts-upload-clients) |
| Upload rate limiting (pre-read hook) | `filesystem/upload` | [Uploads: Limits per user, plan or request](../concepts/uploads.md#limits-per-user-plan-or-request) | [concepts-uploads](../../examples/concepts-uploads) |
| GraphQL Upload scalar | `filesystem/contrib/gqlgen` | [GraphQL uploads with gqlgen](../integrations/graphql.md) | [gqlgen](../../examples/gqlgen) |
| Cloud picker imports (Drive, Dropbox, OneDrive) | `filesystem/contrib/pickers` | [Cloud picker imports](../plugins/pickers.md) | [pickers-import](../../examples/pickers-import) |
| Accept fast, store later | `filesystem/jobs` | [Uploads: Accept fast, store later](../concepts/uploads.md#accept-fast-store-later) | [records-move](../../examples/records-move) |
| Body size cap at the transport | `filesystem/upload` | [Uploads: Sizes: empty files, a request's total, and the body cap](../concepts/uploads.md#sizes-empty-files-a-requests-total-and-the-body-cap) | [recipe-avatars](../../examples/recipe-avatars) |
| Upload pipeline (process and inspect) | `filesystem/upload` | [Uploads: Step 8 — Process and Inspect](../concepts/uploads.md#step-8--process-and-inspect) | [concepts-uploads](../../examples/concepts-uploads) |
| Dry run for abandoned multipart uploads | `filesystem` | [Uploads: Leftovers that are not objects](../concepts/uploads.md#leftovers-that-are-not-objects) | [concepts-uploads](../../examples/concepts-uploads) |
| Storage key from the request (the tenant in the key) | `filesystem/upload` | [Uploads: Key layouts and name collisions](../concepts/uploads.md#key-layouts-and-name-collisions) | [concepts-uploads](../../examples/concepts-uploads) |
| Imports through an egress proxy, with your own headers | `filesystem/upload` | [Uploads: Step 7 — Imports from a URL](../concepts/uploads.md#step-7--imports-from-a-url) | [concepts-uploads](../../examples/concepts-uploads) |
| Record a multipart upload in the complete request | `filesystem/upload` | [Uploads: Step 5 — Browser multipart uploads with Uppy](../concepts/uploads.md#step-5--browser-multipart-uploads-with-uppy) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |

## Validation

Size, type, extension and media limits enforced on the server.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Maximum file size, counted while streaming | `filesystem/upload` | [Uploads: Step 1 — The policy](../concepts/uploads.md#step-1--the-policy) | [concepts-uploads](../../examples/concepts-uploads) |
| MIME type allow-list | `filesystem/upload` | [Uploads: Step 1 — The policy](../concepts/uploads.md#step-1--the-policy) | [concepts-uploads](../../examples/concepts-uploads) |
| Magic-byte type detection | `filesystem/sniff` | [Security: Step 2 — Content type from the bytes](../concepts/security.md#step-2--content-type-from-the-bytes) | [concepts-security](../../examples/concepts-security) |
| Custom detector for formats Go does not know (DICOM, STL, Parquet) | `filesystem/sniff`, `filesystem/upload` | [Security: Formats the detection does not know](../concepts/security.md#formats-the-detection-does-not-know) | [concepts-security](../../examples/concepts-security) |
| Extension allow-list and consistency | `filesystem/upload` | [Security: Step 3 — File names](../concepts/security.md#step-3--file-names) | [concepts-security](../../examples/concepts-security) |
| Minimum size and empty-file rejection | `filesystem/upload` | [Uploads: Sizes: empty files, a request's total, and the body cap](../concepts/uploads.md#sizes-empty-files-a-requests-total-and-the-body-cap) | [concepts-uploads](../../examples/concepts-uploads) |
| Image dimension limits | `filesystem/contrib/imaging` | [Image processing: Checking dimensions on upload](../plugins/image-processing.md#checking-dimensions-on-upload) | [imaging-uploads](../../examples/imaging-uploads) |
| Aspect-ratio limits | `filesystem/contrib/imaging` | [Image processing: Checking dimensions on upload](../plugins/image-processing.md#checking-dimensions-on-upload) | [imaging-uploads](../../examples/imaging-uploads) |
| Maximum files per role | `filesystem/records` | [Attachments: A role with at most N files](../records/attachments.md#a-role-with-at-most-n-files) | [records-query](../../examples/records-query) |
| Video and audio duration and resolution limits | `filesystem/contrib/video`, `filesystem/contrib/audio` | [Video metadata, posters, transcodes and streaming: Options](../plugins/video.md#options) | [media/video](../../examples/media/video) |
| Custom validator chain (inspectors) | `filesystem/upload` | [Uploads: Step 8 — Process and Inspect](../concepts/uploads.md#step-8--process-and-inspect) | [recipe-virus-scanning](../../examples/recipe-virus-scanning) |
| File extension from the detected type | `filesystem/sniff` | [Security: The extension comes from the detected type](../concepts/security.md#the-extension-comes-from-the-detected-type) | [recipe-avatars](../../examples/recipe-avatars) |
| Machine-readable error codes | `filesystem`, `filesystem/upload` | [Errors: The codes](../concepts/errors.md#the-codes) | [concepts-errors](../../examples/concepts-errors) |
| Total request size limit | `filesystem/upload` | [Uploads: Limits per user, plan or request](../concepts/uploads.md#limits-per-user-plan-or-request) | [concepts-uploads](../../examples/concepts-uploads) |
| Limits per user, plan or request | `filesystem/upload` | [Uploads: Limits per user, plan or request](../concepts/uploads.md#limits-per-user-plan-or-request) | [gqlgen](../../examples/gqlgen) |

## File records and metadata

Files as rows in your database, attached to your models.

| Feature | Package | Guide | Example |
|---|---|---|---|
| File records on SQLite, Postgres and MySQL | `filesystem/records` | [Files](../records/files.md) | [records-files](../../examples/records-files) |
| Opaque public file IDs | `filesystem/records` | [Files: Opaque public file IDs](../records/files.md#opaque-public-file-ids) | [records-files](../../examples/records-files) |
| Extracted media metadata (analysers) | `filesystem/contrib/imaging/dimensions`, `video`, `pdf` | [Image dimensions](../plugins/dimensions.md) | [records-dimensions](../../examples/records-dimensions) |
| Alt text, caption and title | `filesystem/records` | [Files: Alt text, caption and title](../records/files.md#alt-text-caption-and-title) | [records-files](../../examples/records-files) |
| Custom properties on a file | `filesystem/records` | [Events and hooks: Step 4 — Stamp and guard files](../records/events-and-hooks.md#step-4--stamp-and-guard-files) | [records-events](../../examples/records-events) |
| Polymorphic attachments to any owner | `filesystem/records` | [Attachments: Step 2 — Attach files](../records/attachments.md#step-2--attach-files) | [records-attachments](../../examples/records-attachments) |
| Named roles (collections) | `filesystem/records` | [Attachments: Step 1 — Register owner types and single roles](../records/attachments.md#step-1--register-owner-types-and-single-roles) | [records-attachments](../../examples/records-attachments) |
| Ordering within a role | `filesystem/records` | [Attachments: Step 4 — Reorder](../records/attachments.md#step-4--reorder) | [records-attachments](../../examples/records-attachments) |
| Single-file roles (SetAttachment) | `filesystem/records` | [Attachments: Step 6 — One file per role: SetAttachment](../records/attachments.md#step-6--one-file-per-role-setattachment) | [records-attachments](../../examples/records-attachments) |
| Shared objects referenced by several files | `filesystem/records` | [Files: Deduplicated uploads and shared objects](../records/files.md#deduplicated-uploads-and-shared-objects) | [records-files](../../examples/records-files) |
| Trash and restore | `filesystem/records` | [Lifecycle: trash, restore, purge: Step 2 — Trash and restore](../records/lifecycle.md#step-2--trash-and-restore) | [records-lifecycle](../../examples/records-lifecycle) |
| Virtual folders | `filesystem/records/folders` | [Folders](../plugins/folders.md) | [records-folders](../../examples/records-folders) |
| Tags | `filesystem/records/tags` | [Tags](../plugins/tags.md) | [records-tags](../../examples/records-tags) |
| Version history and restore | `filesystem/records/versions` | [Versions](../plugins/versions.md) | [records-versions](../../examples/records-versions) |
| Processing status | `filesystem/records` | [Processing status](../records/processing-status.md) | [records-status](../../examples/records-status) |
| Focal point and crop hints | `filesystem/contrib/imaging/focal` | [Image variants: Focal points and format negotiation](../plugins/variants.md#focal-points-and-format-negotiation) | [records-variants](../../examples/records-variants) |
| Media library queries and facets | `filesystem/records` | [Files: Step 8 — Narrow a listing with filters](../records/files.md#step-8--narrow-a-listing-with-filters) | [records-query](../../examples/records-query) |
| Bulk trash, restore, purge and visibility | `filesystem/records` | [Lifecycle: trash, restore, purge: Bulk changes](../records/lifecycle.md#bulk-changes) | [records-query](../../examples/records-query) |
| Usage tracking that blocks deleting a used file | `filesystem/records` | [Files: Where a file is used](../records/files.md#where-a-file-is-used) | [records-files](../../examples/records-files) |
| Orphan reconciliation with dry run | `filesystem/records` | [Lifecycle: trash, restore, purge: Audit storage against the rows](../records/lifecycle.md#audit-storage-against-the-rows) | [jobs-scheduler](../../examples/jobs-scheduler) |
| Read helpers for ent, GORM and sqlc | `filesystem/contrib/ent`, `filesystem/contrib/gorm`, `filesystem/contrib/sqlc` | [GORM](../integrations/gorm.md) | [integrations-orm](../../examples/integrations-orm) |
| Attachment data embedded on the owner's row | `filesystem/records` | [Files: A file on its owner's row](../records/files.md#a-file-on-its-owners-row) | [records-files](../../examples/records-files) |
| Uploaded-at time and source | `filesystem/records` | [Files: Who uploaded it, when, and the original filename](../records/files.md#who-uploaded-it-when-and-the-original-filename) | [records-files](../../examples/records-files) |
| The actor from your own session type | `filesystem/records` | [Files: Who uploaded it, when, and the original filename](../records/files.md#who-uploaded-it-when-and-the-original-filename) | [records-files](../../examples/records-files) |
| Listings sorted by name, size or age | `filesystem/records` | [Files: Step 7 — List a scope, page by page](../records/files.md#step-7--list-a-scope-page-by-page) | [records-files](../../examples/records-files) |
| Case-insensitive folder names | `filesystem/records/folders` | [Folders: Case-insensitive names](../plugins/folders.md#case-insensitive-names) | [records-folders](../../examples/records-folders) |

## Image processing

Variants, codecs, crops, placeholders and image safety.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Named image variants | `filesystem/contrib/imaging/variants` | [Image variants: Step 1 — Declare the variants](../plugins/variants.md#step-1--declare-the-variants) | [records-variants](../../examples/records-variants) |
| Resize modes (fit, fill, pad, crop, limit) | `filesystem/contrib/imaging` | [Image transforms: Step 1 — Pick a resize mode](../plugins/image-transforms.md#step-1--pick-a-resize-mode) | [imaging-transforms](../../examples/imaging-transforms) |
| EXIF orientation applied | `filesystem/contrib/imaging` | [Image transforms: Step 2 — Let EXIF orientation turn the photo upright](../plugins/image-transforms.md#step-2--let-exif-orientation-turn-the-photo-upright) | [recipe-strip-photo-metadata](../../examples/recipe-strip-photo-metadata) |
| Photo metadata stripping | `filesystem/contrib/imaging` | [Strip photo metadata](../recipes/strip-photo-metadata.md) | [recipe-strip-photo-metadata](../../examples/recipe-strip-photo-metadata) |
| WebP output | `filesystem/contrib/imaging/webp` | [Image processing: Codec modules: WebP, AVIF, HEIC](../plugins/image-processing.md#codec-modules-webp-avif-heic) | [imaging-codecs](../../examples/imaging-codecs) |
| AVIF output | `filesystem/contrib/imaging/avif` | [Image processing: Codec modules: WebP, AVIF, HEIC](../plugins/image-processing.md#codec-modules-webp-avif-heic) | [imaging-codecs](../../examples/imaging-codecs) |
| HEIC input | `filesystem/contrib/imaging/heic` | [Image processing: Codec modules: WebP, AVIF, HEIC](../plugins/image-processing.md#codec-modules-webp-avif-heic) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Lossless optimisation | `filesystem/contrib/imaging` | [Image processing: Making uploads smaller](../plugins/image-processing.md#making-uploads-smaller) | [imaging-uploads](../../examples/imaging-uploads) |
| Eager variants after upload | `filesystem/contrib/imaging/variants` | [Image variants: Step 2 — Upload, and let the eager variant appear](../plugins/variants.md#step-2--upload-and-let-the-eager-variant-appear) | [records-variants](../../examples/records-variants) |
| On-the-fly variants on first request | `filesystem/contrib/imaging/variants` | [Image variants: Step 3 — Get a variant](../plugins/variants.md#step-3--get-a-variant) | [records-variants](../../examples/records-variants) |
| Variants from a background job | `filesystem/jobs`, `filesystem/contrib/imaging/variants` | [Image variants: Generating from a job](../plugins/variants.md#generating-from-a-job) | [records-variants](../../examples/records-variants) |
| Regenerate missing variants | `filesystem/contrib/imaging/variants` | [Image variants: Step 6 — Change a transform, then prune](../plugins/variants.md#step-6--change-a-transform-then-prune) | [records-variants](../../examples/records-variants) |
| Responsive width sets | `filesystem/contrib/imaging/variants` | [Image variants: Responsive images: srcset](../plugins/variants.md#responsive-images-srcset) | [imaging-uploads](../../examples/imaging-uploads) |
| Blur placeholders (ThumbHash) | `filesystem/contrib/imaging` | [Image dimensions: Step 5 — A placeholder while the image loads (ThumbHash)](../plugins/dimensions.md#step-5--a-placeholder-while-the-image-loads-thumbhash) | [imaging-uploads](../../examples/imaging-uploads) |
| Dominant colour | `filesystem/contrib/imaging` | [Image transforms: Step 6 — A placeholder colour](../plugins/image-transforms.md#step-6--a-placeholder-colour) | [imaging-transforms](../../examples/imaging-transforms) |
| Smart crop | `filesystem/contrib/imaging` | [Image transforms: Step 3 — Crop around what matters: gravity, smart crop, focal point](../plugins/image-transforms.md#step-3--crop-around-what-matters-gravity-smart-crop-focal-point) | [imaging-transforms](../../examples/imaging-transforms) |
| Focal-point crop | `filesystem/contrib/imaging/focal` | [Image variants: Focal points and format negotiation](../plugins/variants.md#focal-points-and-format-negotiation) | [records-variants](../../examples/records-variants) |
| Watermarks | `filesystem/contrib/imaging` | [Image transforms: Step 4 — Draw a watermark](../plugins/image-transforms.md#step-4--draw-a-watermark) | [imaging-transforms](../../examples/imaging-transforms) |
| Animated GIF handling | `filesystem/contrib/imaging` | [Image transforms: Step 5 — Keep an animated GIF animated](../plugins/image-transforms.md#step-5--keep-an-animated-gif-animated) | [imaging-transforms](../../examples/imaging-transforms) |
| Colour profiles converted to sRGB | `filesystem/contrib/imaging/icc` | [Image processing: Colour profiles, SVG and golden tests](../plugins/image-processing.md#colour-profiles-svg-and-golden-tests) | [imaging-codecs](../../examples/imaging-codecs) |
| Pixel-bomb limits | `filesystem/contrib/imaging` | [Image transforms: Step 7 — Bound what is decoded](../plugins/image-transforms.md#step-7--bound-what-is-decoded) | [imaging-uploads](../../examples/imaging-uploads) |
| Pluggable image processor | `filesystem/contrib/imaging` | [Image transforms: Step 8 — Plug in a processor of your own](../plugins/image-transforms.md#step-8--plug-in-a-processor-of-your-own) | [imaging-transforms](../../examples/imaging-transforms) |
| SVG rasterisation | `filesystem/contrib/svg/raster` | [Accepting SVG safely: Step 3 — Rasterise SVG for thumbnails and previews](../plugins/svg.md#step-3--rasterise-svg-for-thumbnails-and-previews) | [media/svg](../../examples/media/svg) |
| User-chosen crop coordinates | `filesystem/contrib/imaging` | [Avatars: Step 4: The crop](../recipes/avatars.md#step-4-the-crop) | [recipe-avatars](../../examples/recipe-avatars) |
| Re-encode images on upload | `filesystem/contrib/imaging` | [Image processing: Making uploads smaller](../plugins/image-processing.md#making-uploads-smaller) | [recipe-strip-photo-metadata](../../examples/recipe-strip-photo-metadata) |
| Device-density sizes (1x, 2x, 3x) | `filesystem/contrib/imaging/variants` | [Image variants: Device densities: 1x, 2x, 3x](../plugins/variants.md#device-densities-1x-2x-3x) | [records-variants](../../examples/records-variants) |
| Derived files on another disk (variants, posters, previews, OCR text) | `filesystem/contrib/imaging/variants`, `video`, `pdf`, `ocr` | [Image variants: Variants on another disk](../plugins/variants.md#variants-on-another-disk) | [records-variants](../../examples/records-variants) |
| Keep the EXIF Artist and Copyright fields | `filesystem/contrib/imaging` | [Image processing: Keeping copyright and comments](../plugins/image-processing.md#keeping-copyright-and-comments) | [imaging-uploads](../../examples/imaging-uploads) |

## Video and audio

Metadata, posters, transcodes, waveforms and streaming platforms.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Video poster frame | `filesystem/contrib/video` | [Video metadata, posters, transcodes and streaming: Step 2 — The records plugin](../plugins/video.md#step-2--the-records-plugin) | [media/video](../../examples/media/video) |
| Video metadata | `filesystem/contrib/video` | [Video metadata, posters, transcodes and streaming: Step 2 — The records plugin](../plugins/video.md#step-2--the-records-plugin) | [media/video](../../examples/media/video) |
| Transcode to web MP4 | `filesystem/contrib/video` | [Video metadata, posters, transcodes and streaming: Transcoding to a web MP4](../plugins/video.md#transcoding-to-a-web-mp4) | [media/video](../../examples/media/video) |
| Adaptive streaming through Mux or Cloudflare Stream | `filesystem/contrib/videoplatform` | [Video metadata, posters, transcodes and streaming: Adaptive streaming through Mux or Cloudflare Stream](../plugins/video.md#adaptive-streaming-through-mux-or-cloudflare-stream) | [media/streaming](../../examples/media/streaming) |
| Storyboards for scrubbing | `filesystem/contrib/video` | [Video metadata, posters, transcodes and streaming: Storyboards for scrubbing](../plugins/video.md#storyboards-for-scrubbing) | [media/video](../../examples/media/video) |
| Audio metadata | `filesystem/contrib/audio` | [Audio metadata, waveforms and voice notes: Step 2 — The records plugin](../plugins/audio.md#step-2--the-records-plugin) | [media/audio](../../examples/media/audio) |
| Audio waveforms | `filesystem/contrib/audio` | [Audio metadata, waveforms and voice notes: Step 2 — The records plugin](../plugins/audio.md#step-2--the-records-plugin) | [media/audio](../../examples/media/audio) |
| Audio transcoding (Opus, AAC) | `filesystem/contrib/audio` | [Audio metadata, waveforms and voice notes: Step 1 — The upload policy](../plugins/audio.md#step-1--the-upload-policy) | [media/audio](../../examples/media/audio) |
| Video platform adapter | `filesystem/contrib/videoplatform` | [Video metadata, posters, transcodes and streaming: Adaptive streaming through Mux or Cloudflare Stream](../plugins/video.md#adaptive-streaming-through-mux-or-cloudflare-stream) | [media/streaming](../../examples/media/streaming) |
| HLS ladder (adaptive streaming) on your own storage | `filesystem/contrib/video` | [Video metadata, posters, transcodes and streaming: An HLS ladder on your own storage](../plugins/video.md#an-hls-ladder-on-your-own-storage) | [media/video](../../examples/media/video) |

## Documents and previews

PDFs, office files, text extraction and OCR.

| Feature | Package | Guide | Example |
|---|---|---|---|
| PDF preview image | `filesystem/contrib/pdf` | [PDF page counts and previews: Step 2 — The records plugin](../plugins/pdf.md#step-2--the-records-plugin) | [media/pdf](../../examples/media/pdf) |
| PDF metadata | `filesystem/contrib/pdf` | [PDF page counts and previews: Step 3 — Document information, text and office files](../plugins/pdf.md#step-3--document-information-text-and-office-files) | [media/pdf](../../examples/media/pdf) |
| Office document previews | `filesystem/contrib/office` | [Office document previews and text](../plugins/office.md) | [documents/search](../../examples/documents/search) |
| Inline viewing only for safe types | `filesystem/fshttp`, `filesystem/sniff` | [Serving files: Step 5 — Active content is always a download](../concepts/serving-files.md#step-5--active-content-is-always-a-download) | [concepts-serving](../../examples/concepts-serving) |
| PDF text extraction | `filesystem/contrib/pdf` | [PDF page counts and previews: Step 3 — Document information, text and office files](../plugins/pdf.md#step-3--document-information-text-and-office-files) | [documents/search](../../examples/documents/search) |
| OCR (Tesseract) | `filesystem/contrib/ocr` | [Text from images and scans (OCR)](../plugins/ocr.md) | [documents/search](../../examples/documents/search) |

## Security

Defaults and plugins that keep stored and served content safe.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Separate origin for user content | `filesystem` | [Production: Step 8 — A separate origin for user content](../start/production.md#step-8--a-separate-origin-for-user-content) | [start-production](../../examples/start-production) |
| nosniff on every served file | `filesystem/fshttp` | [Security: Step 4 — Serving user content](../concepts/security.md#step-4--serving-user-content) | [concepts-serving](../../examples/concepts-serving) |
| Risky types forced to download | `filesystem/fshttp`, `filesystem/sniff` | [Serving files: Step 5 — Active content is always a download](../concepts/serving-files.md#step-5--active-content-is-always-a-download) | [concepts-serving](../../examples/concepts-serving) |
| SVG sanitisation | `filesystem/contrib/svg` | [Accepting SVG safely](../plugins/svg.md) | [media/svg](../../examples/media/svg) |
| Virus scanning (ClamAV) | `filesystem/scan` | [Virus scanning](../recipes/virus-scanning.md) | [recipe-virus-scanning](../../examples/recipe-virus-scanning) |
| Quarantine until scanned | `filesystem/records` | [Processing status: Quarantine until scanned](../records/processing-status.md#quarantine-until-scanned) | [records-events](../../examples/records-events) |
| Archive bomb limits | `filesystem/contrib/archive` | [Accepting archives safely: What the walk refuses](../plugins/archive.md#what-the-walk-refuses) | [security-archive](../../examples/security-archive) |
| Zip-slip protection | `filesystem/contrib/archive` | [Accepting archives safely: What the walk refuses](../plugins/archive.md#what-the-walk-refuses) | [security-archive](../../examples/security-archive) |
| Server-side encryption with a KMS key per disk | `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: Server-side encryption per disk](../adapters/s3.md#server-side-encryption-per-disk) | [adapters-gcs](../../examples/adapters-gcs) |
| Customer-provided encryption keys | `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [adapters-s3](../../examples/adapters-s3) |
| Client-side encryption | `filesystem/contrib/crypt` | [Encrypting files at rest](../plugins/crypt.md) | [security-crypt](../../examples/security-crypt) |
| Short default expiry for signed links | `filesystem` | [Security: Step 6 — Signed URLs](../concepts/security.md#step-6--signed-urls) | [concepts-security](../../examples/concepts-security) |
| Moderation hook and hash lists | `filesystem/scan`, `filesystem/upload` | [Write a scanner: Moderation: hash lists](../extending/write-a-scanner.md#moderation-hash-lists) | [extending-scanner](../../examples/extending-scanner) |
| SSRF-safe remote fetch | `filesystem/upload` | [Security: Step 5 — Server-side request forgery](../concepts/security.md#step-5--server-side-request-forgery) | [recipe-import-from-url](../../examples/recipe-import-from-url) |
| Secrets redacted from logs | `filesystem/signedurl` | [Security: Step 11 — Secrets stay out of logs and errors](../concepts/security.md#step-11--secrets-stay-out-of-logs-and-errors) | [concepts-security](../../examples/concepts-security) |
| Filename header-injection safety | `filesystem/upload`, `filesystem/fshttp` | [Security: Step 3 — File names](../concepts/security.md#step-3--file-names) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Sandboxing CSP on served files | `filesystem/fshttp` | [Security: Step 4 — Serving user content](../concepts/security.md#step-4--serving-user-content) | [concepts-serving](../../examples/concepts-serving) |
| Executable deny-list | `filesystem/upload` | [Uploads: Step 1 — The policy](../concepts/uploads.md#step-1--the-policy) | [concepts-security](../../examples/concepts-security) |
| Archive inspection | `filesystem/contrib/archive` | [Accepting archives safely: Step 1 — Inspect uploads](../plugins/archive.md#step-1--inspect-uploads) | [security-archive](../../examples/security-archive) |
| Direct uploads re-validated on confirm | `filesystem/upload` | [Uploads: Step 4 — Direct uploads: Presign, then Confirm](../concepts/uploads.md#step-4--direct-uploads-presign-then-confirm) | [recipe-browser-direct-uploads](../../examples/recipe-browser-direct-uploads) |
| One KMS key per tenant on a shared S3 disk | `filesystem/adapters/s3` | [Amazon S3: One KMS key per tenant on a shared disk](../adapters/s3.md#one-kms-key-per-tenant-on-a-shared-disk) | [adapters-s3](../../examples/adapters-s3) |

## Quotas, usage and billing

Limits per tenant and per user, usage events and egress.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Per-user quota | `filesystem/records/quota` | [Quota: Per-uploader limits: seats in a workspace](../plugins/quota.md#per-uploader-limits-seats-in-a-workspace) | [records-quota-seats](../../examples/records-quota-seats) |
| Per-tenant quota | `filesystem/records/quota` | [Quota](../plugins/quota.md) | [records-quota](../../examples/records-quota) |
| Usage aggregation | `filesystem/records` | [Quota: Step 3 — Upload; past the limit is refused](../plugins/quota.md#step-3--upload-past-the-limit-is-refused) | [records-quota](../../examples/records-quota) |
| Plan-based file size | `filesystem/upload` | [Uploads: Limits per user, plan or request](../concepts/uploads.md#limits-per-user-plan-or-request) | [concepts-uploads](../../examples/concepts-uploads) |
| Egress metering | `filesystem/records/egress` | [Egress metering](../plugins/egress.md) | [records-egress](../../examples/records-egress) |
| Quota reservations for uploads in flight | `filesystem/records/quota` | [Quota: Reserve room for uploads in flight](../plugins/quota.md#reserve-room-for-uploads-in-flight) | [records-quota](../../examples/records-quota) |
| Usage events for billing | `filesystem/records/quota` | [Quota: Usage events for billing](../plugins/quota.md#usage-events-for-billing) | [records-quota](../../examples/records-quota) |
| File count limits | `filesystem/records/quota` | [Quota: Step 1 — Return a scope's limit](../plugins/quota.md#step-1--return-a-scopes-limit) | [records-quota](../../examples/records-quota) |
| Organisation-wide and content-class counters | `filesystem/records/quota` | [Quota: Other counters: organisations, owner types, content classes](../plugins/quota.md#other-counters-organisations-owner-types-content-classes) | [records-quota](../../examples/records-quota) |
| Egress by the hour or in each tenant's time zone | `filesystem/records/egress` | [Egress metering: Other buckets: hours, or each tenant's own day](../plugins/egress.md#other-buckets-hours-or-each-tenants-own-day) | [records-egress](../../examples/records-egress) |
| Egress per share link or other subject | `filesystem/records/egress` | [Egress metering: Per share link: a breakdown by subject](../plugins/egress.md#per-share-link-a-breakdown-by-subject) | [records-egress](../../examples/records-egress) |

## Lifecycle and retention

Trash, purge, retention, holds, archive tiers and sweeps.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Purge everything an owner leaves behind | `filesystem/records` | [Lifecycle: trash, restore, purge: Step 4 — Delete what an owner leaves behind](../records/lifecycle.md#step-4--delete-what-an-owner-leaves-behind) | [recipe-account-deletion](../../examples/recipe-account-deletion) |
| Objects deleted after the commit | `filesystem/records`, `filesystem/jobs` | [Consistency: Step 3 — Purge deletes the object after the commit](../concepts/consistency.md#step-3--purge-deletes-the-object-after-the-commit) | [concepts-consistency](../../examples/concepts-consistency) |
| Abandoned upload cleanup | `filesystem`, `filesystem/upload`, `filesystem/jobs` | [Uploads: Step 9 — Sweep what nobody finished](../concepts/uploads.md#step-9--sweep-what-nobody-finished) | [jobs-scheduler](../../examples/jobs-scheduler) |
| Temporary files with a TTL | `filesystem`, `filesystem/records` | [Lifecycle: trash, restore, purge: Temporary and view-once files](../records/lifecycle.md#temporary-and-view-once-files) | [records-lifecycle](../../examples/records-lifecycle) |
| Trash emptied on a schedule | `filesystem/records`, `filesystem/jobs` | [Lifecycle: trash, restore, purge: Step 5 — Empty the trash on a schedule](../records/lifecycle.md#step-5--empty-the-trash-on-a-schedule) | [jobs-scheduler](../../examples/jobs-scheduler) |
| View-once and expiring files | `filesystem/records` | [Lifecycle: trash, restore, purge: Temporary and view-once files](../records/lifecycle.md#temporary-and-view-once-files) | [records-lifecycle](../../examples/records-lifecycle) |
| Retention rules | `filesystem/records` | [Lifecycle: trash, restore, purge: Retention rules](../records/lifecycle.md#retention-rules) | [records-lifecycle](../../examples/records-lifecycle) |
| Legal holds | `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [adapters-s3](../../examples/adapters-s3) |
| WORM retention lock | `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [adapters-gcs](../../examples/adapters-gcs) |
| Storage classes and tiering | `filesystem`, `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [migrating-awssdk](../../examples/migrating-awssdk) |
| Restore from archive tiers | `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [adapters-s3](../../examples/adapters-s3) |
| Read a bucket version | `filesystem`, `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 14. Object features: tags, holds, versions, classes, archives, SSE-C, checksums](../adapters/s3.md#14-object-features-tags-holds-versions-classes-archives-sse-c-checksums) | [adapters-s3](../../examples/adapters-s3) |
| Erase everything of a subject | `filesystem/records` | [Account deletion](../recipes/account-deletion.md) | [recipe-account-deletion](../../examples/recipe-account-deletion) |
| Export everything of a subject | `filesystem/jobs` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Exporting a subject's files](../maintenance/README.md#exporting-a-subjects-files) | [maintenance](../../examples/maintenance) |
| Replace content, keep the file's identity | `filesystem/records` | [Replace and visibility: Step 1 — Replace a file's content with an upload](../records/replace-and-visibility.md#step-1--replace-a-files-content-with-an-upload) | [records-replace](../../examples/records-replace) |
| Dry runs before sweeps | `filesystem/records`, `filesystem/upload`, `filesystem/jobs` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Dry runs](../maintenance/README.md#dry-runs) | [maintenance](../../examples/maintenance) |
| Bucket lifecycle rules | `filesystem/adapters/s3`, `filesystem/adapters/gcs` | [Amazon S3: 15. Bucket setup: lifecycle, replication, object ACLs, scoped credentials](../adapters/s3.md#15-bucket-setup-lifecycle-replication-object-acls-scoped-credentials) | [adapters-s3](../../examples/adapters-s3) |
| Retention per tenant or plan | `filesystem/records` | [Lifecycle: trash, restore, purge: Retention by plan](../records/lifecycle.md#retention-by-plan) | [records-lifecycle](../../examples/records-lifecycle) |
| Version history per plan or file | `filesystem/records/versions` | [Versions: Other history models](../plugins/versions.md#other-history-models) | [records-versions](../../examples/records-versions) |

## Integrity

Checksums, deduplication, atomic writes and scrubbing.

| Feature | Package | Guide | Example |
|---|---|---|---|
| SHA-256 computed while streaming | `filesystem`, `filesystem/upload` | [Integrity: Step 1 — Every write is hashed while it streams](../concepts/integrity.md#step-1--every-write-is-hashed-while-it-streams) | [concepts-disks](../../examples/concepts-disks) |
| Checksum verified by the backend | `filesystem/adapters/s3` | [Amazon S3: Checksums S3 verifies](../adapters/s3.md#checksums-s3-verifies) | [adapters-s3](../../examples/adapters-s3) |
| Client-supplied checksum checked | `filesystem/upload` | [Uploads: Declared size and checksum](../concepts/uploads.md#declared-size-and-checksum) | [concepts-uploads](../../examples/concepts-uploads) |
| ETags exposed | `filesystem` | [Integrity: Step 4 — ETags for "has it changed?"](../concepts/integrity.md#step-4--etags-for-has-it-changed) | [concepts-serving](../../examples/concepts-serving) |
| Content-addressed keys | `filesystem/upload` | [Files: Deduplicated uploads and shared objects](../records/files.md#deduplicated-uploads-and-shared-objects) | [records-files](../../examples/records-files) |
| Deduplication by hash | `filesystem/records` | [Files: Deduplicated uploads and shared objects](../records/files.md#deduplicated-uploads-and-shared-objects) | [records-files](../../examples/records-files) |
| Several checksum algorithms | `filesystem/adapters/s3`, `gcs`, `azure` | [Google Cloud Storage: Checksums](../adapters/gcs.md#checksums) | [adapters-gcs](../../examples/adapters-gcs) |
| Integrity scrubbing | `filesystem/jobs` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Integrity scrubbing](../maintenance/README.md#integrity-scrubbing) | [maintenance](../../examples/maintenance) |
| Atomic local writes | `filesystem/local` | [Local disk: 5. Atomic, conditional writes and moves](../adapters/local.md#5-atomic-conditional-writes-and-moves) | [adapters-local](../../examples/adapters-local) |
| Durability options on local disks | `filesystem/local` | [Local disk: 5. Atomic, conditional writes and moves](../adapters/local.md#5-atomic-conditional-writes-and-moves) | [adapters-local](../../examples/adapters-local) |
| Declared size verified | `filesystem/upload` | [Uploads: Declared size and checksum](../concepts/uploads.md#declared-size-and-checksum) | [concepts-uploads](../../examples/concepts-uploads) |
| Other digests beside SHA-256 (MD5, CRC32C, BLAKE3) | `filesystem` | [Integrity: Other digests beside SHA-256](../concepts/integrity.md#other-digests-beside-sha-256) | [concepts-integrity](../../examples/concepts-integrity) |

## Observability

Logs, metrics, traces, audit trails and health checks.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Structured logging (slog) | `filesystem/layer` | [Logging and metrics](../integrations/logging-and-metrics.md) | [integrations-logging](../../examples/integrations-logging) |
| Metrics | `filesystem/layer`, `filesystem/contrib/otel` | [Logging and metrics: Step 4 — Count operations](../integrations/logging-and-metrics.md#step-4--count-operations) | [integrations-logging](../../examples/integrations-logging) |
| OpenTelemetry tracing | `filesystem/contrib/otel` | [OpenTelemetry](../integrations/otel.md) | [integrations-otel](../../examples/integrations-otel) |
| Access log with a tamper-evident hash chain | `filesystem/records/audit` | [Audit log: Reads, tamper evidence and a change feed](../plugins/audit.md#reads-tamper-evidence-and-a-change-feed) | [records-audit](../../examples/records-audit) |
| Typed errors on every adapter | `filesystem` | [Errors](../concepts/errors.md) | [concepts-errors](../../examples/concepts-errors) |
| Disk health check | `filesystem` | [Production: Step 10 — Health checks](../start/production.md#step-10--health-checks) | [start-production](../../examples/start-production) |
| Audit chain key rotation and the request's source | `filesystem/records/audit` | [Audit log: Rotating the chain key, and logging where changes came from](../plugins/audit.md#rotating-the-chain-key-and-logging-where-changes-came-from) | [records-audit](../../examples/records-audit) |

## Events, hooks and background work

Hooks, events, jobs, webhooks and bucket notifications.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Before and after upload stages | `filesystem/upload` | [Uploads: Step 8 — Process and Inspect](../concepts/uploads.md#step-8--process-and-inspect) | [concepts-uploads](../../examples/concepts-uploads) |
| Hooks on delete and purge | `filesystem/records` | [Events and hooks](../records/events-and-hooks.md) | [records-events](../../examples/records-events) |
| Variants ready and failed events | `filesystem/records`, `filesystem/contrib/imaging/variants` | [Image variants: Processing status and events](../plugins/variants.md#processing-status-and-events) | [records-variants](../../examples/records-variants) |
| Job runner (in-process, River, Asynq) | `filesystem/jobs`, `filesystem/jobs/river`, `filesystem/jobs/asynq` | [Maintenance jobs: Step 5 — Other runners](../concepts/maintenance-jobs.md#step-5--other-runners) | [jobs-scheduler](../../examples/jobs-scheduler) |
| Signed outbound webhooks | `filesystem/records/webhooks` | [Webhooks](../plugins/webhooks.md) | [records-webhooks](../../examples/records-webhooks) |
| Bucket notifications (SQS, Pub/Sub, Event Grid) | `filesystem/contrib/notify` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Bucket notifications](../maintenance/README.md#bucket-notifications) | [maintenance-notify](../../examples/maintenance-notify) |
| Object and row consistency | `filesystem/records` | [Consistency: Bytes and rows: the two rules](../concepts/consistency.md#bytes-and-rows-the-two-rules) | [concepts-consistency](../../examples/concepts-consistency) |
| Typed event payloads | `filesystem/records` | [Events and hooks: The events](../records/events-and-hooks.md#the-events) | [records-events](../../examples/records-events) |
| Global webhook endpoints, secret rotation, your own signer and payload | `filesystem/records/webhooks` | [Webhooks: Other models: global endpoints, secret rotation, signers and payloads](../plugins/webhooks.md#other-models-global-endpoints-secret-rotation-signers-and-payloads) | [records-webhooks](../../examples/records-webhooks) |
| Maintenance without records, on a calendar | `filesystem/jobs/diskjobs` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Without records, and on a calendar](../maintenance/README.md#without-records-and-on-a-calendar) | [jobs-calendar](../../examples/jobs-calendar) |

## Testing

Fakes, assertions, the conformance suite and test clocks.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Fake disk | `filesystem/filesystemtest` | [Test your app: Step 1 — A fake disk and its assertions](../start/testing.md#step-1--a-fake-disk-and-its-assertions) | [start-testing](../../examples/start-testing) |
| Assertion helpers | `filesystem/filesystemtest` | [Test your app: Step 1 — A fake disk and its assertions](../start/testing.md#step-1--a-fake-disk-and-its-assertions) | [start-testing](../../examples/start-testing) |
| Fake multipart requests and images | `filesystem/filesystemtest` | [Test your app: Step 6 — HTTP handlers](../start/testing.md#step-6--http-handlers) | [start-testing](../../examples/start-testing) |
| Driver conformance suite | `filesystem/filesystemtest` | [Test your app: Step 7 — The conformance suite, for a driver of your own](../start/testing.md#step-7--the-conformance-suite-for-a-driver-of-your-own) | [extending-driver](../../examples/extending-driver) |
| Injectable clock | `filesystem/records` (and its plugins), `filesystem/memory`, `filesystem/signedurl`, `filesystem` (`CacheOptions.Now`), `filesystem/upload`, `filesystem/layer` | [Test your app: Step 5 — Move the clock instead of sleeping](../start/testing.md#step-5--move-the-clock-instead-of-sleeping) | [start-testing](../../examples/start-testing) |
| Golden image tests | `filesystem/contrib/imaging` | [Image transforms: Step 9 — Test transforms with golden images](../plugins/image-transforms.md#step-9--test-transforms-with-golden-images) | [imaging-transforms](../../examples/imaging-transforms) |

## HTTP serving

Download handlers, ranges, zips, framework adapters and endpoints.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Download handler | `filesystem/fshttp` | [Serving files: Step 2 — Download](../concepts/serving-files.md#step-2--download) | [concepts-serving](../../examples/concepts-serving) |
| Range requests | `filesystem/fshttp` | [Serving files: Step 2 — Download](../concepts/serving-files.md#step-2--download) | [concepts-serving](../../examples/concepts-serving) |
| Conditional GET (ETag, 304) | `filesystem/fshttp` | [Serving files: Step 2 — Download](../concepts/serving-files.md#step-2--download) | [concepts-serving](../../examples/concepts-serving) |
| Cache-Control by visibility | `filesystem/fshttp` | [Serving files: Step 9 — Cache-Control](../concepts/serving-files.md#step-9--cache-control) | [concepts-serving](../../examples/concepts-serving) |
| Streaming zip downloads | `filesystem/fshttp` | [Serving files: Step 10 — Many files as one zip](../concepts/serving-files.md#step-10--many-files-as-one-zip) | [concepts-serving](../../examples/concepts-serving) |
| Adapters for gin, echo, fiber and chi | `filesystem/contrib/gin`, `echo`, `fiber`, `chi` | [Framework adapters: gin, echo, fiber and chi](../integrations/framework-adapters.md) | [frameworks](../../examples/frameworks) |
| Presign endpoint | `filesystem/upload` | [net/http, gorilla and chi: Step 5 — Direct uploads: tickets and Uppy](../integrations/net-http.md#step-5--direct-uploads-tickets-and-uppy) | [integrations-nethttp](../../examples/integrations-nethttp) |
| Variant endpoint | `filesystem/contrib/imaging/variants` | [Image variants: Step 4 — Serve variants over HTTP](../plugins/variants.md#step-4--serve-variants-over-http) | [records-variants](../../examples/records-variants) |
| CORS for browser uploads | `filesystem/adapters/s3`, `gcs`, `azure` | [Amazon S3: 13. CORS for browser uploads](../adapters/s3.md#13-cors-for-browser-uploads) | [recipe-browser-form-uploads](../../examples/recipe-browser-form-uploads) |
| HEAD requests | `filesystem/fshttp` | [Serving files: HEAD requests](../concepts/serving-files.md#head-requests) | [concepts-serving](../../examples/concepts-serving) |
| Sendfile offload to the proxy | `filesystem/fshttp` | [Serving files: Step 12 — Let the proxy send the bytes](../concepts/serving-files.md#step-12--let-the-proxy-send-the-bytes) | [concepts-delivery](../../examples/concepts-delivery) |
| Upload endpoint JSON contract | `filesystem/upload` | [Upload clients: Step 1 — The JSON contract](../concepts/upload-clients.md#step-1--the-json-contract) | [concepts-uploads](../../examples/concepts-uploads) |
| Your own error envelope (problem+json) on every handler | `filesystem/fshttp`, `filesystem/upload`, `filesystem/records/recordshttp`, `filesystem/contrib/gin`, `echo`, `fiber`, `chi` | [Errors: Your own error envelope](../concepts/errors.md#your-own-error-envelope) | [concepts-errors](../../examples/concepts-errors) |
| Serve from your own handler with ranges, 304 and the security headers | `filesystem/fshttp` | [Serving files: Serving from your own handler](../concepts/serving-files.md#serving-from-your-own-handler) | [concepts-serving](../../examples/concepts-serving) |
| Your own success envelope on the upload and records handlers and share links | `filesystem/upload`, `filesystem/records/recordshttp`, `filesystem/records/share` | [Uploads: Your own success envelope](../concepts/uploads.md#your-own-success-envelope) | [concepts-uploads](../../examples/concepts-uploads) |
| Variant proxy with ranges and conditional requests | `filesystem/contrib/imaging/variants` | [Image variants: Proxy instead of redirect](../plugins/variants.md#proxy-instead-of-redirect) | [records-variants](../../examples/records-variants) |

## Developer experience

The driver interface, configuration, the CLI and helpers.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Six-method driver interface | `filesystem` | [Write a driver](../extending/write-a-driver.md) | [extending-driver](../../examples/extending-driver) |
| Optional capability interfaces | `filesystem` | [Write a driver: Optional interfaces](../extending/write-a-driver.md#optional-interfaces) | [extending-driver](../../examples/extending-driver) |
| Typed configuration | `filesystem` | [Disks and drivers: Conventions every call follows](../concepts/disks-and-drivers.md#conventions-every-call-follows) | [concepts-configuration](../../examples/concepts-configuration) |
| The fs command-line tool | `filesystem/cmd/fs` | [The fs command-line tool](../integrations/cli.md) | [integrations-cli](../../examples/integrations-cli) |
| Public local directory served by the app | `filesystem/local`, `filesystem/fshttp` | [Serving files: Step 7 — Public files](../concepts/serving-files.md#step-7--public-files) | [concepts-serving](../../examples/concepts-serving) |
| Standard library types throughout | `filesystem` | [Disks and drivers: Conventions every call follows](../concepts/disks-and-drivers.md#conventions-every-call-follows) | [concepts-disks](../../examples/concepts-disks) |
| MIME helpers | `filesystem/sniff` | [Security: MIME helpers](../concepts/security.md#mime-helpers) | [concepts-security](../../examples/concepts-security) |
| Human-readable size values | `filesystem` | [Configuration: Sizes in configuration](../concepts/configuration.md#sizes-in-configuration) | [concepts-configuration](../../examples/concepts-configuration) |
| Runnable examples per app archetype | `examples` | [Recipes](../recipes/README.md) | [drive](../../examples/drive) |
| Record migrations on three engines | `filesystem/records` | [Migrations](../records/migrations.md) | [records-migrations](../../examples/records-migrations) |
| Sync one disk to another | `filesystem`, `filesystem/cmd/fs` | [Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events: Syncing two disks](../maintenance/README.md#syncing-two-disks) | [maintenance](../../examples/maintenance) |
| Local temp copy of a stored file | `filesystem` | [Disks and drivers: Step 10 — A local file for tools that need a path](../concepts/disks-and-drivers.md#step-10--a-local-file-for-tools-that-need-a-path) | [concepts-disks](../../examples/concepts-disks) |
| Concurrent batch helper | `filesystem` | [Composing disks: Step 5 — Batch per-key work](../concepts/composing-disks.md#step-5--batch-per-key-work) | [concepts-composing](../../examples/concepts-composing) |
| A fixed content-type table for local disks | `filesystem/local` | [Local disk: 3. Open a disk in code](../adapters/local.md#3-open-a-disk-in-code) | [adapters-local](../../examples/adapters-local) |

## Sharing

Share links, passwords, limits, folders and file requests.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Share links | `filesystem/records/share` | [Share links: Step 2 — Create a link](../plugins/share.md#step-2--create-a-link) | [records-share](../../examples/records-share) |
| Link expiry | `filesystem/records/share` | [Share links: Step 2 — Create a link](../plugins/share.md#step-2--create-a-link) | [records-share](../../examples/records-share) |
| Password-protected links | `filesystem/records/share` | [Share links: Passwords](../plugins/share.md#passwords) | [records-share](../../examples/records-share) |
| Download-count limits | `filesystem/records/share` | [Share links: Step 2 — Create a link](../plugins/share.md#step-2--create-a-link) | [records-share](../../examples/records-share) |
| Revoke a link | `filesystem/records/share` | [Share links: Step 4 — List and revoke](../plugins/share.md#step-4--list-and-revoke) | [records-share](../../examples/records-share) |
| Shared folders and collections | `filesystem/records/share` | [Share links: Share a folder or a collection](../plugins/share.md#share-a-folder-or-a-collection) | [records-share](../../examples/records-share) |
| Link statistics | `filesystem/records/share` | [Share links: Link statistics](../plugins/share.md#link-statistics) | [records-share](../../examples/records-share) |
| File request (upload-only) links | `filesystem/records/share` | [Share links: Request files: upload-only links](../plugins/share.md#request-files-upload-only-links) | [records-share](../../examples/records-share) |
| Your own access check, delivery page and upload policy on links | `filesystem/records/share` | [Share links: Your own link policy](../plugins/share.md#your-own-link-policy) | [records-share](../../examples/records-share) |

## Collaboration

Concurrency control, locks and change feeds.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Save only what you read (optimistic concurrency) | `filesystem/records` | [Replace and visibility: Save only what you read](../records/replace-and-visibility.md#save-only-what-you-read) | [records-replace](../../examples/records-replace) |
| Check-out locks | `filesystem/records/locks` | [Check-out locks](../plugins/locks.md) | [records-locks](../../examples/records-locks) |
| Change feed in commit order | `filesystem/records/audit` | [Audit log: Reads, tamper evidence and a change feed](../plugins/audit.md#reads-tamper-evidence-and-a-change-feed) | [records-audit](../../examples/records-audit) |
| WebDAV-style lock tokens | `filesystem/records/locks` | [Check-out locks: Who holds a lock: tokens and your own rule](../plugins/locks.md#who-holds-a-lock-tokens-and-your-own-rule) | [records-locks](../../examples/records-locks) |
| Shared locks (several holders, each with a token) | `filesystem/records/locks` | [Check-out locks: Shared locks: several holders](../plugins/locks.md#shared-locks-several-holders) | [records-locks](../../examples/records-locks) |
| One file in several folders (shortcuts) | `filesystem/records/folders` | [Folders: One file in several folders: shortcuts](../plugins/folders.md#one-file-in-several-folders-shortcuts) | [records-folders](../../examples/records-folders) |

## Search and indexing

Filename search, facets and auto-tagging.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Filename search | `filesystem/records` | [Files: Step 8 — Narrow a listing with filters](../records/files.md#step-8--narrow-a-listing-with-filters) | [records-query](../../examples/records-query) |
| Facet counts | `filesystem/records` | [Files: Facet counts](../records/files.md#facet-counts) | [records-query](../../examples/records-query) |
| Auto-tagging images | `filesystem/contrib/vision` | [Auto-tagging images](../plugins/vision.md) | [documents/labels](../../examples/documents/labels) |

## Mobile backends

Uploads from phones on flaky networks.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Resumable uploads over flaky networks | `filesystem/contrib/tus` | [Resumable uploads: Step 3: The handler](../recipes/resumable-uploads.md#step-3-the-handler) | [recipe-resumable-uploads](../../examples/recipe-resumable-uploads) |
| Long-lived upload URLs within a bound | `filesystem/upload` | [Uploads: Declared size and checksum](../concepts/uploads.md#declared-size-and-checksum) | [concepts-security](../../examples/concepts-security) |
| Client downscale contract | `filesystem/upload` | [Mobile uploads: Downscale on the phone](../recipes/mobile-uploads.md#downscale-on-the-phone) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Offline queue idempotency | `filesystem/upload` | [Upload clients: Step 5 — Retries without duplicates](../concepts/upload-clients.md#step-5--retries-without-duplicates) | [concepts-upload-clients](../../examples/concepts-upload-clients) |
| Upload status polling | `filesystem/records` | [Processing status: Step 6 — Poll from a client](../records/processing-status.md#step-6--poll-from-a-client) | [records-status](../../examples/records-status) |
| Background-upload friendly responses | `filesystem/upload` | [Mobile uploads: Pick the path](../recipes/mobile-uploads.md#pick-the-path) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |

## Social and user-generated content

Avatars, posts, attachments and remote media.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Avatar flow | `filesystem/records`, `filesystem/contrib/imaging` | [Avatars](../recipes/avatars.md) | [recipe-avatars](../../examples/recipe-avatars) |
| Default avatar (initials or identicon) | `filesystem/contrib/imaging/placeholder` | [Avatars: When there is no avatar](../recipes/avatars.md#when-there-is-no-avatar) | [recipe-avatars](../../examples/recipe-avatars) |
| Cover and banner images | `filesystem/records`, `filesystem/contrib/imaging/variants` | [Cover and banner images: The design](../recipes/cover-images.md#the-design) | [recipe-cover-images](../../examples/recipe-cover-images) |
| Multi-media posts | `filesystem/records` | [Product images](../recipes/product-images.md) | [recipe-product-images](../../examples/recipe-product-images) |
| Message attachments | `filesystem/records/recordshttp` | [Chat attachments](../recipes/chat-attachments.md) | [recipe-chat-attachments](../../examples/recipe-chat-attachments) |
| Link previews with cached images | `filesystem/contrib/remote` | [Caching remote media: Step 2 — Link previews](../plugins/remote.md#step-2--link-previews) | [remote-media](../../examples/remote-media) |
| Signed image proxy for external images | `filesystem/contrib/remote` | [Caching remote media: Step 3 — The image proxy](../plugins/remote.md#step-3--the-image-proxy) | [remote-media](../../examples/remote-media) |
| Remote media cache | `filesystem/contrib/remote` | [Caching remote media: Step 1 — Make the cache](../plugins/remote.md#step-1--make-the-cache) | [remote-media](../../examples/remote-media) |
| Takedown removes variants and versions | `filesystem/records` | [Lifecycle: trash, restore, purge: Takedown](../records/lifecycle.md#takedown) | [records-lifecycle](../../examples/records-lifecycle) |
| oEmbed previews from providers you allow | `filesystem/contrib/remote` | [Caching remote media: oEmbed from providers you allow](../plugins/remote.md#oembed-from-providers-you-allow) | [remote-media](../../examples/remote-media) |

## SaaS and B2B

Tenants, customer buckets, exports and partner drops.

| Feature | Package | Guide | Example |
|---|---|---|---|
| A bucket or prefix per tenant | `filesystem` | [SaaS tenants](../recipes/saas-tenants.md) | [recipe-saas-tenants](../../examples/recipe-saas-tenants) |
| Bring your own bucket (assume a role) | `filesystem/adapters/s3` | [Amazon S3: Bring your own bucket: a role in the customer's account](../adapters/s3.md#bring-your-own-bucket-a-role-in-the-customers-account) | [adapters-s3](../../examples/adapters-s3) |
| Generated exports and reports | `filesystem/jobs`, `filesystem` | [Exports](../recipes/exports.md) | [recipe-exports](../../examples/recipe-exports) |
| Bulk import intake | `filesystem/upload`, `filesystem/jobs` | [Bulk import: Step 2: Accept and queue](../recipes/bulk-import.md#step-2-accept-and-queue) | [recipe-mobile-uploads](../../examples/recipe-mobile-uploads) |
| Tenant branding assets | `filesystem/records` | [Tenant branding: Step 1: The slots](../recipes/tenant-branding.md#step-1-the-slots) | [recipe-tenant-branding](../../examples/recipe-tenant-branding) |
| Files as outbound email attachments | `filesystem` | [Email attachments: Step 3: Stream the file into the message](../recipes/email-attachments.md#step-3-stream-the-file-into-the-message) | [recipe-email-attachments](../../examples/recipe-email-attachments) |
| Partner SFTP and FTP drops | `filesystem/adapters/sftp` | [SFTP](../adapters/sftp.md) | [adapters-sftp](../../examples/adapters-sftp) |

## Compliance

Residency, per-tenant keys, replication and audit retention.

| Feature | Package | Guide | Example |
|---|---|---|---|
| Data residency per tenant | `filesystem` | [Multi-tenancy: A region per tenant (data residency)](../concepts/multi-tenancy.md#a-region-per-tenant-data-residency) | [concepts-tenancy](../../examples/concepts-tenancy) |
| Per-tenant encryption keys and rotation | `filesystem/contrib/crypt` | [Encrypting files at rest: Step 2 — Rotate keys](../plugins/crypt.md#step-2--rotate-keys) | [security-crypt](../../examples/security-crypt) |
| Cross-region replication | `filesystem/adapters/s3`, `filesystem/adapters/gcs` | [Amazon S3: 15. Bucket setup: lifecycle, replication, object ACLs, scoped credentials](../adapters/s3.md#15-bucket-setup-lifecycle-replication-object-acls-scoped-credentials) | [adapters-s3](../../examples/adapters-s3) |
| Audit retention floor | `filesystem/records/audit` | [Audit log: Step 7 — Keep it for as long as you need](../plugins/audit.md#step-7--keep-it-for-as-long-as-you-need) | [jobs-scheduler](../../examples/jobs-scheduler) |

## Next

- [Guides](../README.md): every guide, in the order to read them.
- [Recipes](../recipes/README.md): complete product features built from the pieces above.
- [Examples](../../examples): every runnable program.
