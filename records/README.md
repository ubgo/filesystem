# github.com/ubgo/filesystem/records

Stores files as database rows and attaches them to any row of the application: a user's avatar, a post's gallery, a message's attachments. Bytes live on a `filesystem.Disk`; rows live in the application's own `*sql.DB` on SQLite, Postgres or MySQL. The guides start at [docs/records](../docs/records/README.md); this page is the reference.

## Install

```sh
go get github.com/ubgo/filesystem/records
```

A module of its own. It requires the core module `github.com/ubgo/filesystem` and `github.com/ubgo/sqlb`, and no database driver: the application opens the `*sql.DB` (`modernc.org/sqlite`, `github.com/jackc/pgx/v5/stdlib`, `github.com/go-sql-driver/mysql` with `parseTime=true`) and passes it in. `deps_test.go` enforces the dependency list.

## Wire it

<!-- ds:block id=newstore-c3s67q5f -->
**newStore** · [`examples/records-files/main.go:90-99`](../examples/records-files/main.go#L90-L99)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=a22ffb -->

`records.New(db, dialect, disks, opts...)` takes the database, one of `sqlb.SQLiteDialect{}`, `sqlb.PostgresDialect{}`, `sqlb.MySQLDialect{}`, and the `filesystem.Manager` holding the disks files are stored on. `Migrate` creates the tables; call it on every start. A full walk-through is [Files](../docs/records/files.md).

## Consistency rules

- **Object first, row second.** `CreateFile` records only an object that exists; `Upload` deletes its object when the row cannot be inserted or an enclosing `Tx` rolls back.
- **Row first, object last.** `Purge`, `ReplaceObject`, `SetVisibility` (between disks) and plugins delete objects only after the transaction commits.
- What can be left behind is an orphan object, never a row without bytes. Failed post-commit deletions go to `WithObjectErrorHandler`; `SweepOrphans` deletes orphans after checking each object against the rows and the plugins that own objects.
- Reads are open, writes are closed: SELECT and JOIN the `fs_` tables freely; change them only through the Store.

## Options

| Option | What it does | Default |
|---|---|---|
| `WithOwnerTypes(types...)` | Owner types files can attach to; any other is `invalid_argument`. Pattern `^[a-z][a-z0-9_.]{0,63}$`. | none |
| `WithForeignOwnerTypes(types...)` | Owner types spelled the way another framework stores them in a shared database (`User`, `Admin::User`, `App\Models\Post`), registered beside `WithOwnerTypes`'. Pattern `^[A-Za-z][A-Za-z0-9_.:\\-]{0,63}$`: no `/`, spaces, quotes or control characters. | none |
| `WithActorFunc(fn)` | Replaces `ActorFrom` as the way the Store and its plugins name who a request acts for (`Store.Actor`), for an app that keeps its user under its own context key; when `fn` returns `""` the context's `WithActor` value is read. | `ActorFrom` |
| `WithSingleRoles(roles...)` | Roles holding at most one file per owner, enforced by a derived primary key even under concurrency. | none |
| `WithVisibilityDisks(public, private)` | Public and private files on two disks; `Upload` with disk `""` chooses, `SetVisibility` moves. Both must be in the manager. | off |
| `WithPlugins(plugins...)` | Installs plugins; unique names, `records` reserved. | none |
| `WithObjectErrorHandler(fn)` | Receives object deletions that failed after a commit. | dropped |
| `WithIDs(fn)` | ID generator; receives `IDPrefixFile` (`fil_`), `IDPrefixAttachment` (`att_`) or a plugin's own prefix (`acl_`, `fld_`, `ver_`, `shr_`, `sev_`, `whe_`, `whd_`, `qrs_`, through `Store.NewID`), returns a unique string with that prefix, at most 191 bytes. Not used for single-role attachments, event IDs or audit rows (sequence-prefixed by design). | prefix + 128 random bits, base32 |
| `WithClock(now)` | Replaces `time.Now`, for the Store and every plugin without its own `WithClock` (acl, folders, versions, locks, share, quota, webhooks) and the `recordshttp` and share handlers. | `time.Now` |
| `WithSourceIP()` | Keeps `Source.IP` (personal data) when a file is recorded; without it the IP is dropped. | off |
| `WithRetention(Retention{OwnerType, Role, MaxAge}...)` | Retention rules for `SweepRetention`, in every scope; the owner type must be registered, `Role` `""` is every role, `MaxAge` positive. Rules per tenant go to `SweepRetentionIn`. | none |
| `WithScanFailure(ScanFailureKeep\|ScanFailurePurge)` | What `FailScan` does with a file whose scan failed: keep it quarantined with the verdict, or purge it. | `ScanFailureKeep` |
| `WithTablePrefix(prefix)`, `WithTableNames(fn)` | Every table and index of the core and its plugins under another name: `WithTablePrefix("app_")` makes `fs_files` `app_fs_files`; `WithTableNames` maps each `fs_` name with the application's function (pure, injective, an identifier of at most `MaxObjectNameBytes`, checked by `Migrate`). Queries, migrations and the ledger all follow; `Store.TableName(name)` gives the mapped name for hand-written SQL. Pick it before the first `Migrate`. | the `fs_` names |
| `WithFileOrder(name, Ordering)`, `KeysetOrder(key, dir, parse, format)` | An order of your own for `Files` (`FileQuery.Order: name`): any expression over `fs_files` as a keyset on (key, id), such as `sqlb.Lower(FileFilename)` for a case-insensitive name sort. The cursor holds the key as the database computed it; `name` is `[a-z][a-z0-9_]*`, not built in, added once. | the built-in `FileOrderValues` |
| `WithMigrationLockWait(d)` | MySQL migration lock wait, whole seconds; 0 fails at once; negative is `invalid_argument`. | `DefaultMigrationLockWait` (60s) |

`WithActor(ctx, actor)` / `ActorFrom(ctx)` carry the actor of a request in the context; `Store.Actor(ctx)` reads it (or `WithActorFunc`'s lookup), and it becomes `File.UploadedBy` (at most `MaxActorBytes`, 191) and the share plugin's viewers and depositors.

`Action` (`ActionValues`: upload, list, reorder, detach, read, replace, trash, restore, purge, set_attachment, set_visibility, set_text) and `ActionTarget{Scope, Owner, Role, FileID, File, Change}` are the vocabulary of authorization rules: `recordshttp.Action` and `recordshttp.Target` are aliases of them, and permission plugins such as acl decide on them without importing the HTTP layer. `Change{Visibility, Text, Previous}` is the change a visibility, text or set-attachment request asks for, read before the rule is asked, so "only admins publish" can be written.

## Go API

Files ([guide](../docs/records/files.md)):

| Method | What it does |
|---|---|
| `CreateFile(ctx, NewFile)` | Records an existing object; size, type, digest and visibility default to the disk's. `NewFile.Checksums` (extra digests by algorithm, at most `MaxChecksums`, each hex of at most `MaxChecksumBytes`) land on `File.Checksums`. Runs `FileHooks`, emits `file.created`. |
| `Upload(ctx, scope, disk, body, filename, upload.Policy, opts...)` | `upload.Receive` then `CreateFile`, keeping the digests `Policy.Checksums` asked for as `File.Checksums` (`Replace` sets the new content's); deletes the object on failure or rollback. `Dedup()` records the file on an existing live file's object with the same SHA-256, disk and visibility in the scope (deleting the upload's copy); `Expires(at)` and `ViewOnce()` make it temporary. |
| `WithSource(ctx, Source{Channel, Client, IP})`, `SourceFrom(ctx)` | Where files recorded with `ctx` came from, kept as `File.Source` (`NewFile.Source` overrides); widths `MaxSourceChannelBytes`, `MaxSourceClientBytes`, `MaxSourceIPBytes`, wider is `invalid_argument`. |
| `File(ctx, scope, id)` | One file, trashed or not; `not_found` otherwise. |
| `Files(ctx, FileQuery{Scope, Trashed, Limit, Cursor, Filters, Order})` | A page newest first, keyset on `(created_at, id)`; `Limit` 0 is `DefaultPageSize` (50), at most `MaxPageSize` (1000). `Order` (`FileOrderValues`) sorts by `OrderOldest`, `OrderName`, `OrderNameDesc`, `OrderLargest` or `OrderSmallest` instead, or by a `WithFileOrder` name, each a keyset on (key, id); a cursor is valid only for the order that issued it. |
| `SetText(ctx, scope, id, Text{Alt, Caption, Title})`, `File.Text()` | A file's alt text, caption and title, kept in `Metadata` under `MetaAlt`, `MetaCaption`, `MetaTitle` and limited to `MaxAltChars`, `MaxCaptionChars`, `MaxTitleChars` on every write path. `SetText` replaces all three (`""` removes one), keeps the other metadata, and refuses with `unavailable` if the metadata changed concurrently. `BeforeReplace` hooks are held to the same limits. |
| `AttachedTo(scope, owner, role)` | A `FileQuery.Filters` condition keeping an owner's files, under one role or any (`""`). |
| `Facets(ctx, fq, facet)` | Counts (files and bytes) of what `fq` selects by `FacetContentType` or `FacetVisibility`; folders plugin: `FolderFacet`. |
| `SetStatus(ctx, scope, id, Status, detail)` | Records where a file's processing stands (`StatusReady`, `StatusProcessing`, `StatusFailed`, `StatusQuarantined`) with a short note; emits `file.status_changed` with the file. `NewFile.Status` sets it at creation; filter with `sqlb.Eq(FileStatus, StatusFailed)`. It never moves a file out of `StatusQuarantined` (`ErrQuarantined`) and never quarantines a public file (`ErrPublicQuarantine`). |
| `Quarantine()` (upload option), `PassScan(ctx, scope, id)`, `FailScan(ctx, scope, id, detail)`, `Scan(ctx, scope, id, scan.Scanner)`, `File.Quarantined()` | Quarantine until scanned ([guide](../docs/records/processing-status.md#quarantine-until-scanned)): a quarantined file gets no link (`URL`, `DownloadURL`), stream (`Served`), view (`Viewed`), share or public visibility (`ErrQuarantined`, `conflict`) until `PassScan` releases it; `FailScan` keeps it quarantined with the verdict or purges it (`WithScanFailure`); `Scan` reads the object, scans it and records the verdict, failing closed; `PassScan` runs `ReleaseHooks` and emits `file.status_changed` then `file.released`; `Dedup` never reuses a quarantined object; `QuarantineContent()` quarantines a file with its replacement's new content. |
| `FileBySHA256(ctx, scope, sum)` | The newest live file of the scope with that content digest (hex, either case), or `not_found`: the deduplication lookup, on the `(scope, sha256)` index. A malformed digest is `invalid_argument`. |
| `URL(ctx, f, ttl)` | Public URL for a public file when the disk has one, else a signed GET. |
| `References(ctx, disk, key)` | How many file rows name an object (shared objects). Every deletion of an object (purge, replace, move, `DeleteObjectAfterCommit`) keeps it while another row or an `ObjectOwner` plugin still names it; an in-place visibility change of a shared object is `conflict`. |
| `UsagesOf(ctx, UsageQuery{Scope, FileID, Limit, Cursor})` | Every attachment of a file (owner and role), newest first, keyset-paginated. |
| `Embed(f)`, `Unembed(raw)` | A file as versioned JSON (`EmbedVersion`) to keep on its owner's row, and back; a newer version is `unsupported`. |
| `DownloadURL(ctx, f, ttl)` | A signed GET that saves under the file's name (`AttachmentDisposition`). |

Attachments ([guide](../docs/records/attachments.md)):

| Method | What it does |
|---|---|
| `Attach(ctx, scope, fileID, owner, role)` | Links a live file at the last position; `exists` when already attached or a single role is occupied. |
| `Attachment(ctx, scope, id)` | One attachment. |
| `Attachments(ctx, scope, owner, role)` | `[]AttachedFile` in position order, trashed files left out. |
| `Reorder(ctx, scope, owner, role, ids)` | New order; `ids` must list every attachment `Attachments` returns (live files) exactly once. Trashed files' attachments keep their relative order after them. |
| `AttachmentsPage(ctx, AttachmentQuery{Scope, Owner, Role, Limit, Cursor})` | `Attachments` a page at a time, keyset on (position, id). |
| `WithRoleLimit(role, max)` | At most `max` files per owner under `role`; past it `Attach` is `quota_exceeded`, enforced by derived IDs under concurrency. |
| `Detach(ctx, scope, id)` | Removes the link; unknown IDs succeed. |
| `SetAttachment(ctx, scope, fileID, owner, role, Previous)` | Makes the file the role's only one; detached files are kept, trashed or purged (`PreviousKeep`, `PreviousTrash`, `PreviousPurge`) unless still used elsewhere. |

Lifecycle ([guide](../docs/records/lifecycle.md)):

| Method | What it does |
|---|---|
| `Trash(ctx, scope, id)`, `Restore(ctx, scope, id)` | Set or clear `DeletedAt`; no-ops emit nothing. A trashed public file's object is hidden (moved to the private disk with `WithVisibilityDisks`, else made private in place where the disk can) and shown again by `Restore`. |
| `Purge(ctx, scope, id, opts...)` | Rows in a transaction (after `BeforePurge`), object after the commit, with the file's row locked so a racing `Attach` cannot leave an attachment behind. Outside a `Tx` it returns an object deletion error; inside one the handler gets it. `IfTrashed(bool)` purges only if the file is still in that trash state (else `unavailable`). |
| `PurgeOwner(ctx, scope, owner)` | Detaches every attachment of the owner and purges the files only it used; returns the count. |
| `PurgeUnattached(ctx, scope, uploadedBy, filters...)` | Purges the files of the scope that `uploadedBy` recorded and no attachment uses (live or trashed), which `PurgeOwner` cannot find; `filters` narrow it; `""` is refused. Returns the count. |
| `PurgeScope(ctx, scope)` | Offboards a tenant: purges every file of the scope (live and trashed) through `Purge`, 100 IDs at a time, each in its own transaction, then runs every `ScopePurger` plugin in one transaction. Stops at the first file that cannot be purged; rerun to continue. Leaves the audit log. Refuses `""`. Returns the files purged. |
| `TrashMany`, `RestoreMany`, `PurgeMany(ctx, scope, ids)`, `SetVisibilityMany(ctx, scope, ids, v)` | One change for up to `MaxBulk` files, each in its own transaction, one `BulkResult` per distinct ID; refused inside a `Tx`. |
| `FindOrphans(ctx, disk, prefix, olderThan, found)` | What `SweepOrphans` would delete, deleting nothing. |
| `MissingObjects(ctx, disk, missing)` | File rows whose object the disk does not have; reports only. |
| `Viewed(ctx, scope, id)`, `File.Expired(now)` | Asked before serving: false for an expired or trashed file; a view-once file is served exactly once (its first view sets `ExpiresAt`). |
| `SweepExpired(ctx, now)` | Purges every file whose `ExpiresAt` has passed, each in its own transaction; returns the count. |
| `SweepRetention(ctx, now, dryRun)` | Detaches attachments older than their `WithRetention` rule and purges files left with none; `dryRun` reports the same `RetentionReport{Detached, Purged}` changing nothing. |
| `SweepRetentionIn(ctx, scope, rules, now, dryRun)` | `SweepRetention` with the given rules in one scope only: retention by plan ("free 30 days, pro 1 year"), the app passing each tenant's rules. Rules are checked as `WithRetention`'s; a bad one sweeps nothing. |
| `SweepTrash(ctx, cutoff)` | Purges every file in every scope trashed before `cutoff`, each in its own transaction; returns the count. A file that cannot be purged does not stop it: the error joins one error per failed file. |
| `FindTrash(ctx, cutoff, found)` | `SweepTrash`'s dry run: calls `found` with every file `SweepTrash` would purge for the same cutoff, changing nothing; a real sweep may purge fewer (a plugin can refuse), never more. |
| `SweepOrphans(ctx, disk, prefix, olderThan)` | Deletes the objects under `prefix` that no file row names and no `ObjectOwner` plugin holds, older than `olderThan`; returns the count. Never deletes a recorded file. |

Replace and visibility ([guide](../docs/records/replace-and-visibility.md)):

| Method | What it does |
|---|---|
| `Replace(ctx, scope, id, body, filename, upload.Policy, opts...)` | New content under the file's visibility; same ID. `IfUnchanged(sha256)` replaces only the content the caller read, else `records.ErrChanged` (`conflict`), checked before the upload and again under the row lock. |
| `ChangeOption`: `IfUnchanged(sha256)`, `IfTrashed(bool)`, `QuarantineContent()` | Narrow `Replace`, `ReplaceObject`, `Trash`, `Restore`, `SetVisibility` and `Purge` to the state the caller read, re-checked with the row locked inside the change's transaction (an object change already made is undone). `IfUnchanged` refuses with `ErrChanged`; `IfTrashed` is `Purge`'s only (`unavailable` on mismatch, `invalid_argument` on any other call); `QuarantineContent` is a replacement's only (`invalid_argument` on any other call). `PurgeOption` is an alias. |
| `ReplaceObject(ctx, scope, id, NewContent, opts...)` | An existing object on the file's disk becomes its content, and gets the file's visibility where the disk can set it. |
| `MoveFile(ctx, scope, id, to)` | Moves one live file's object to disk `to` (copy checked against the row, row and `FileDiskMover` plugins in one transaction, old object deleted after the commit); rerunnable; emits `file.moved` with `Detail` from/to. |
| `RenameDisk(ctx, from, to)` | Points every record on disk `from` at disk `to` (a name the `Manager` has), in every scope and one transaction: `fs_files.disk`, then each `DiskRenamer` plugin. Rows only; the objects must already be reachable on `to`. Returns the file rows changed. |
| `SetVisibility(ctx, scope, id, v)` | In place (needs the disk's `SetVisibility` capability) or by moving between the `WithVisibilityDisks` disks. A trashed file changes only its row; `Restore` applies it. |

Transactions and plugins ([events and hooks](../docs/records/events-and-hooks.md)):

| Method | What it does |
|---|---|
| `Tx(ctx, fn(ctx, *sql.Tx) error)` | One transaction; Store calls with its `ctx` join it; nested calls join the outer one. |
| `AfterCommit(ctx, fn)`, `OnRollback(ctx, fn)` | Work after a commit (at once without a transaction) or after a rollback. |
| `DeleteObjectAfterCommit(ctx, f)` | Deletes `f.Disk`/`f.Key` after the commit, failures to the handler. |
| `Conn(ctx)`, `Exec`, `One`, `Each`, `Build`, `Dialect()`, `Disks()` | What plugins run statements and reach bytes with; `Build` renders a statement with the Store's table names for a plugin that runs it another way (a locking read). |
| `TableName(name)` | The name a default table or index name (`TableNameFiles`, `acl.TableNameGrants`) has in this Store's database (`WithTableNames`); `name` itself by default. |
| `Hold(ctx, HeldObject)`, `Release(ctx, HeldObject)`, `HeldBytes(ctx, scope)` | A plugin reports an object it keeps beside a file (a version, a variant) to every `UsageHooks` plugin, in install order, first error wins; `HeldBytes` sums every `HeldCounter`. |
| `Emit(ctx, plugin, Event)` | A plugin's own event (`<name>.<action>`, with `Subject` and `Detail`) to every `EventHooks` plugin, inside the change's transaction; a core kind, another plugin's kind or no transaction is `invalid_argument`. |

Migrations ([guide](../docs/records/migrations.md)):

| Name | What it does |
|---|---|
| `Migrate(ctx)` | Applies new migrations under a cross-process lock; returns `owner/name` of each. |
| `Migrations()` | Every migration in apply order. |
| `MigrationSQL()` | Every migration as one script, with the `fs_migrations` ledger and its inserts, so a later `Migrate` sees the script's migrations as applied. |
| `NewDDL(engine)` with `CreateTable(Table{…})`, `CreateIndex`, `DropIndex`, `AddColumn`, `ModifyColumn`, `Raw`, then `Statements()` | A migration's statements built with `github.com/ubgo/sqlb/ddl` from typed columns (`Column(col, spelling)`, `Key(col)`), re-runnable on every engine. |
| `TypesFor(engine)` | Each engine's spellings of the types `fs_` tables use. |

Helpers: `IsUniqueViolation(err)`, `EncodeCursor(at, id)` / `DecodeCursor(c)`, `AttachmentDisposition(name)`.

## Plugin interfaces

| Interface | Methods |
|---|---|
| `Plugin` | `Name() string` |
| `Migrator` | `Migrations(engine) ([]Migration, error)` |
| `FileHooks` | `BeforeCreate(ctx, s, *File)`, `AfterCreate(ctx, s, File)`, `BeforePurge(ctx, s, File)` |
| `ReplaceHooks` | `BeforeReplace(ctx, s, current, *next)` |
| `ReleaseHooks` | `BeforeRelease(ctx, s, *f)`: a quarantined file being released by `PassScan`, inside its transaction; may set metadata only |
| `Versioner` | `KeepVersion(ctx, s, previous) (bool, error)` |
| `EventHooks` | `OnEvent(ctx, s, Event)` |
| `ServeHooks` | `OnServed(ctx, s, Served)`: told of every read a handler reports with `Store.Served` (egress metering, an access log), outside any transaction |
| `ObjectOwner` | `OwnedObjects(ctx, s, disk, keys) (map[string]bool, error)`: the plugin's own objects, which `SweepOrphans` keeps |
| `UsageHooks` | `OnHold(ctx, s, HeldObject)`, `OnRelease(ctx, s, HeldObject)`: told of objects other plugins keep beside files, inside their transaction |
| `HeldCounter` | `HeldBytes(ctx, s, scope)`: the bytes the plugin holds in a scope |
| `ScopePurger` | `PurgeScope(ctx, s, scope)`: deletes the plugin's rows of a scope once `Store.PurgeScope` has purged its files |
| `FileDiskMover` | `MoveFileDisk(ctx, s, f, to)`: moves the plugin's own objects for one file inside `Store.MoveFile`'s transaction |
| `DiskRenamer` | `RenameDisk(ctx, s, from, to)`: rewrites the plugin's own disk names inside `Store.RenameDisk`'s transaction |

Event kinds (`EventKindValues`): `file.created`, `file.trashed`, `file.restored`, `file.replaced`, `file.visibility_changed`, `file.status_changed`, `file.released`, `file.moved`, `file.purged`, `attachment.created`, `attachment.deleted`, `attachments.reordered`. Plugins add their own through `Emit`: [folders](folders/README.md), [tags](tags/README.md) and [share](share/README.md) report their changes, so [audit](audit/README.md) and webhooks see them.

Every event has an `ID` (`evt_` and random, or one the emitting plugin chose, at most `MaxEventIDBytes`), kept by every redelivery, so at-least-once consumers (webhooks, billing) can drop repeats.

Reads are reported with `Store.Served(ctx, Served{Kind, Scope, FileID, Subject, Bytes})`, kinds `ServeKindValues`: `access.link`, `access.share`, `access.variant`, `access.stream`, or a custom `access.<name>.<action>` (`ServeKind.Valid`) for reads the built-in kinds do not name, such as a zip download. `Store.URL` and `Store.DownloadURL` call it for every link they issue (`records.ServedAs(kind, subject)` names a share link or variant) and return no link when it fails, so recordshttp, share links, variants and application code are all metered, and a read of a file quarantined now (read from the database) is refused with `ErrQuarantined` before any hook; for public files that counts links issued, not fetches. `IsLockConflict(err)` recognises a transaction the database aborted over a lock conflict on every engine. `Store.Now()` is the Store's clock, for plugins.

Bundled plugins: [acl](acl/README.md), [audit](audit/README.md), [egress](egress/README.md), [folders](folders/README.md), [locks](locks/README.md), [quota](quota/README.md), [share](share/README.md), [tags](tags/README.md), [uploadsessions](uploadsessions/README.md), [versions](versions/README.md), [webhooks](webhooks/README.md), and [recordshttp](recordshttp/README.md) for the HTTP routes. In the imaging module: [variants](../contrib/imaging/variants/README.md), [dimensions](../contrib/imaging/dimensions/README.md).

## Tables

| Table | Holds |
|---|---|
| `fs_files` | One row per file. Typed columns `FileID`, `FileScope`, `FileDisk`, `FileKey`, `FileFilename`, `FileContentType`, `FileSize`, `FileSHA256`, `FileVisibility`, `FileMetadata`, `FileCreatedAt`, `FileDeletedAt`, `FileUploadedBy`, `FileChecksums` (JSON, NULL for none, core migration 0007) over `TableFiles`. |
| `fs_attachments` | One row per link. `AttachmentID`, `AttachmentScope`, `AttachmentFileID`, `AttachmentOwnerType`, `AttachmentOwnerID`, `AttachmentRole`, `AttachmentPosition`, `AttachmentCreatedAt` over `TableAttachments`. |
| `fs_migrations` | The ledger: `owner`, `name`, `checksum`, `applied_at`. |

Every statement names the tables unqualified, so they live wherever the Store's connection points: a Postgres schema of your own (`search_path=storage` alone in the pgx DSN), one MySQL database per connection, one SQLite file per Store (the Postgres and MySQL test suites run every test this way, in a schema or database of its own). To keep two Stores in one schema, or to follow a house naming rule, rename them with `WithTablePrefix` or `WithTableNames`: every name starting with `fs_` (`ObjectPrefix`) in what the Store runs, tables and indexes, is mapped, plugins' included, and nothing else is.

## Errors

Every error the Store creates is a `*filesystem.Error`; read its code with `filesystem.CodeOf(err)`: `invalid_argument` (bad input, unregistered owner type, stale reorder, edited migration), `not_found` (unknown or trashed file where a live one is needed, other scope), `conflict` (`ErrChanged`; `ErrQuarantined` for a quarantined file asked to be served or released by `SetStatus`; `ErrNotQuarantined` from `FailScan`), `exists` (duplicate attachment, occupied single role), `unsupported` (engine, or a disk that cannot sign or change visibility), `unavailable` (MySQL migration lock). Storage and plugin errors pass through unchanged. [Errors reference](../docs/reference/errors.md) maps codes to HTTP statuses.
