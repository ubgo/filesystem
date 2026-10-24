# Records: files as rows

The `records` module stores every file as a database row and attaches it to any row of your application: a user's avatar, a post's gallery, a message's attachments, a product's images. It is built on a `filesystem.Disk` for the bytes and on your own `*sql.DB` for the rows.

You need it when your application has to answer questions about files without touching storage: which files does this post have, in what order, who uploaded this, how much does this workspace store, what is in the trash. If all you do is put bytes under a key and read them back, a `filesystem.Disk` alone is enough and this module adds nothing.

## What it gives you

- A `fs_files` row per file: scope (tenant), disk, key, display name, content type, size, SHA-256, visibility, metadata, who uploaded it, when, and whether it is in the trash. See [Files](files.md).
- A `fs_attachments` row per link between a file and an owner (`user`/`usr_1`) under a role (`avatar`, `gallery`), ordered by position, with single roles that hold at most one file. See [Attachments](attachments.md).
- A trash, purge, owner deletion and a retention sweep, with the object always deleted after the row, never before. See [Lifecycle](lifecycle.md).
- New content and new visibility for an existing file, keeping its ID and attachments. See [Replace and visibility](replace-and-visibility.md).
- A processing status per file (ready, processing, failed) that workers and plugins report and clients poll. See [Processing status](processing-status.md).
- Events and hooks so your code (and plugins) take part in every change, inside the change's transaction. See [Events and hooks](events-and-hooks.md).
- A migration ledger that creates and evolves the schema on SQLite, Postgres and MySQL. See [Migrations](migrations.md).
- An HTTP endpoint set for the common routes, with your authentication plugged in. See [HTTP endpoints](http-endpoints.md).
- Plugins for the features most products add next: [permissions](../plugins/acl.md), [check-out locks](../plugins/locks.md), [audit](../plugins/audit.md), [folders](../plugins/folders.md), [quota](../plugins/quota.md), [share links](../plugins/share.md), [tags](../plugins/tags.md), [versions](../plugins/versions.md), [image variants](../plugins/variants.md), [image dimensions](../plugins/dimensions.md), [egress metering](../plugins/egress.md), [webhooks](../plugins/webhooks.md) and [upload sessions](../plugins/upload-sessions.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

`records` is its own module. It requires the core `github.com/ubgo/filesystem` module and `github.com/ubgo/sqlb` (the SQL builder it renders every statement with), and nothing else: it never imports a database driver. You open the `*sql.DB` with the driver of your choice and pass it in.

## The pieces

| Piece | What it is |
|---|---|
| `records.Store` | The entry point. Holds the `*sql.DB`, the `sqlb.Dialect`, the `filesystem.Manager` the bytes live in, and the installed plugins. Safe for concurrent use. |
| `records.File` | One stored file: `ID` (`fil_…`), `Scope`, `Disk`, `Key`, `Filename`, `ContentType`, `Size`, `SHA256`, `Visibility`, `Metadata`, `CreatedAt`, `DeletedAt` (set while trashed), `UploadedBy`, `Checksums` (extra digests by algorithm, nil for none). |
| `records.Owner` | The application row a file is attached to: `{Type: "post", ID: "post_1"}`. Types must be registered with `WithOwnerTypes`. |
| `records.Attachment` | A file linked to an owner under a role, with a `Position` from 1. ID `att_…`. |
| Scope | The tenant, workspace or user a file belongs to. Opaque to the library (`""` is fine for an app without tenants), and every query filters by it, so one scope can never read another's files. |
| `records.Plugin` | Anything with a `Name()`. What else it implements (`Migrator`, `FileHooks`, `ReplaceHooks`, `Versioner`, `EventHooks`) decides how it takes part. |

## Two rules keep bytes and rows consistent

An object store and a database never share a transaction, so the Store orders every change:

- **Object first, row second.** A file is recorded only after its object is stored: `CreateFile` checks the object exists, and `Upload` deletes the object again if the row cannot be inserted or an enclosing transaction rolls back.
- **Row first, object last.** `Purge` deletes the rows inside the transaction and the object only after the commit, so a rollback never leaves a row pointing at deleted bytes.

What can still be left behind is an orphan object (a row insert failed and its cleanup failed too, or a post-commit deletion failed), never a row without bytes. `Store.SweepOrphans(ctx, disk, prefix, olderThan)` removes them: it deletes the objects under a prefix that no file row names and no plugin holds (kept versions, image variants), older than a cutoff, so it never deletes a recorded file. `upload.Sweep` is different: it deletes every object under a prefix older than a cutoff, recorded or not, so point it only at a staging prefix where unrecorded uploads wait, never at the prefix your recorded files live under. For failed post-commit deletions, `WithObjectErrorHandler` hands you the key to retry. [Consistency](../concepts/consistency.md) explains the ordering in depth.

Reads are open, writes are closed: you may `SELECT` and `JOIN` the `fs_` tables from your own queries (the typed columns `records.FileID`, `records.AttachmentOwnerID` and the rest are exported for that), but change them only through the Store, where these rules live.

## Engines

| Engine | Dialect | Driver (yours to import) | DSN shape |
|---|---|---|---|
| SQLite | `sqlb.SQLiteDialect{}` | `modernc.org/sqlite` (driver name `sqlite`) | `file:/var/lib/app/app.db?_pragma=busy_timeout(5000)`, and `db.SetMaxOpenConns(1)`: SQLite has one writer, a second connection only waits on the lock |
| Postgres | `sqlb.PostgresDialect{}` | `github.com/jackc/pgx/v5/stdlib` (driver name `pgx`) | `postgres://app:secret@localhost:5432/app?sslmode=disable` |
| MySQL | `sqlb.MySQLDialect{}` | `github.com/go-sql-driver/mysql` (driver name `mysql`) | `app:secret@tcp(127.0.0.1:3306)/app?parseTime=true&loc=UTC`; `parseTime=true` is required, because the Store scans timestamps into `time.Time`; MySQL 8.0 or later, because `Attach` and `Purge` lock the file row with `FOR SHARE` and `FOR UPDATE` |

The test suite runs every records package on all three (`task test:engines`). Any other dialect makes `records.New` fail with `unsupported`. Times are stored in UTC at microsecond precision, the finest every engine keeps, so a value read back equals the value written.

## Store options

| Option | What it does | Default |
|---|---|---|
| `WithOwnerTypes(types...)` | Registers the owner types files can attach to. `Attach` refuses any other type with `invalid_argument`, so a typo is an error, not an attachment to nothing. Types match `^[a-z][a-z0-9_.]{0,63}$`. | none: attaching needs at least one |
| `WithSingleRoles(roles...)` | Roles that hold at most one file per owner (`avatar`, `cover`, `logo`), enforced by the database even under concurrent requests. See [Attachments](attachments.md). | none |
| `WithVisibilityDisks(public, private)` | Keeps public and private files on two different disks; `Upload` with an empty disk name picks one, `SetVisibility` moves the object. See [Replace and visibility](replace-and-visibility.md). | off |
| `WithPlugins(plugins...)` | Installs plugins. Names must be unique and match `^[a-z][a-z0-9_.]{0,63}$`; `records` is reserved for the core. | none |
| `WithObjectErrorHandler(fn)` | Receives object deletions that failed after a commit, when no caller is left to return them to. See [Lifecycle](lifecycle.md). | dropped (the object is left for a sweep) |
| `WithIDs(fn)` | Replaces the ID generator. It receives `fil_`, `att_` or a plugin's own prefix (plugins create their rows' IDs with `Store.NewID`) and must return a unique string starting with it, at most 191 bytes. Single-role attachment IDs are derived and ignore it; event IDs and audit rows stay random by design. | prefix plus 128 random bits, lowercase base32 |
| `WithClock(now)` | Replaces `time.Now`, for tests. Plugins built without their own `WithClock`, and the `recordshttp` and share handlers, read it too. | `time.Now` |
| `WithMigrationLockWait(d)` | How long `Migrate` waits on MySQL for another instance's migration lock. See [Migrations](migrations.md). | `DefaultMigrationLockWait` (60s) |

The actor of a change is not an option: it travels in the context. Set `ctx = records.WithActor(ctx, "user:usr_1")` once per request, in the middleware that authenticates; the Store records it as `File.UploadedBy` (up to `MaxActorBytes`, 191) and the [audit plugin](../plugins/audit.md) logs it.

## Transactions

`Store.Tx(ctx, fn)` runs `fn` in one database transaction. Every Store call made with the `ctx` that `fn` receives joins it, and so can your own statements on the `*sql.Tx` it passes, so "delete the post row and purge its files" commits or rolls back as one. A `Tx` inside a `Tx` joins the outer one; only the outermost commits. Calls made outside any `Tx` run in their own. `AfterCommit` and `OnRollback` register work for the end of the transaction; [Lifecycle](lifecycle.md) and [Events and hooks](events-and-hooks.md) show both.

## Next

- [Files](files.md): wire a Store, record and upload files, list them, hand out links.
- [Attachments](attachments.md): galleries, avatars, ordering.
- [Migrations](migrations.md): creating the tables in production.
- The module's [reference README](../../records/README.md) lists every exported name.
