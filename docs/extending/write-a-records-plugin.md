# Write a records plugin

You will have a plugin of your own for the records layer: legal holds, which stop a held file from being trashed or purged until the hold is released. On the way it uses every extension point a plugin has: its own table and migration on SQLite, Postgres and MySQL, statements that join the caller's transaction, a lifecycle hook that refuses a purge, an event hook that vetoes a change, and a notification that runs only after the commit.

Read this page when the built-in plugins ([audit](../plugins/audit.md), [folders](../plugins/folders.md), [quota](../plugins/quota.md), [share](../plugins/share.md), [tags](../plugins/tags.md), [versions](../plugins/versions.md)) do not cover a need that belongs next to the files table: retention rules, moderation state, per-file counters, a search index.

## Before you start

- [Records overview](../records/README.md) and [Events and hooks](../records/events-and-hooks.md): the Store, its tables, and when each hook runs.
- The program is [`examples/extending-records-plugin`](../../examples/extending-records-plugin/main.go). `go run ./extending-records-plugin` in `examples/` holds a file and shows trash and purge refused; `go test ./extending-records-plugin` checks rollback, duplicates and the sweep.
- A real plugin lives in its own package (`yourapp/storage/legalhold`); the example is in `package main` so it is one runnable file.

## What a plugin is

`records.Plugin` has one required method, `Name() string`. Everything else is an optional interface the Store detects:

| Interface | Methods | When the Store calls it |
|---|---|---|
| `records.Migrator` | `Migrations(engine string) ([]records.Migration, error)` | `Migrate`, `Migrations` and `MigrationSQL`, after the core's migrations, in installation order |
| `records.FileHooks` | `BeforeCreate(ctx, s, *File)`, `AfterCreate(ctx, s, File)`, `BeforePurge(ctx, s, File)` | inside the transaction of every file creation and every purge (`Purge`, `PurgeOwner`, `SweepTrash`, `SetAttachment` with `PreviousPurge`) |
| `records.EventHooks` | `OnEvent(ctx, s, records.Event) error` | inside the transaction of every change, after the change (every event kind: [operations](../reference/operations.md#event-kinds)) |
| `records.ReplaceHooks` | `BeforeReplace(ctx, s, current File, next *File)` | inside a `Replace`, before the row changes |
| `records.ReleaseHooks` | `BeforeRelease(ctx, s, f *File)` | inside `PassScan`, when a quarantined file's scan passes, before the row changes (metadata only) |
| `records.Versioner` | `KeepVersion(ctx, s, previous File) (bool, error)` | inside a `Replace`; returning true takes ownership of the old object |
| `records.ObjectOwner` | `OwnedObjects(ctx, s, disk string, keys []string) (map[string]bool, error)` | by `SweepOrphans`, before deleting objects no file row names; a plugin that stores objects of its own (kept versions, image variants) must report them, or the sweep deletes them |

Every hook runs inside the operation's transaction, and its error rolls the whole operation back, objects included. Install plugins with `records.WithPlugins(p1, p2)`; names must be unique and match `[a-z][a-z0-9_.]*`.

## Step 1 — Name and table

The name is the plugin's identity in the migration ledger (`fs_migrations.owner`) and in errors:

<!-- ds:block id=plugin-name-2xqe7aq8 -->
**Plugin.Name** · [`examples/extending-records-plugin/main.go:97`](../../examples/extending-records-plugin/main.go#L97-L97)

```go
func (p *Plugin) Name() string { return PluginName }
```
<!-- /ds:block hash=9fd92c -->

The table and its columns are declared once as constants and `sqlb` typed columns in the example's `Table`, `Scope`, `FileID`, `Reason`, `HeldBy` and `HeldAt`, so statements never spell a column name twice. Prefix your tables `fs_` to keep them next to the library's.

## Step 2 — The migration

Build the table with `records.DDL`, from the same typed columns the plugin's queries use, so a column cannot be created under one name or type and read under another. `records.TypesFor(engine)` gives each engine's spelling of the types the library's own tables use: indexed keys short enough for MySQL's index limit and microsecond timestamps; on MySQL `DDL` also makes the table InnoDB with the byte-exact collation that keeps `a.png` and `A.png` apart. One definition then serves all three engines, and every statement can run again after a crash ([Migrations](../records/migrations.md#build-them-from-the-typed-columns-recordsddl)):

<!-- ds:block id=plugin-migrations-e4sm8uz8 -->
**Plugin.Migrations** · [`examples/extending-records-plugin/main.go:108-119`](../../examples/extending-records-plugin/main.go#L108-L119)

```go
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(Reason, t.Text),
			records.Column(HeldBy, t.Key), records.Column(HeldAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{Scope, FileID},
	}).Statements()
	return []records.Migration{{Owner: PluginName, Name: "0001_legal_holds", Statements: st}}, err
}
```
<!-- /ds:block hash=35af86 -->

Rendered for each engine, the statement is:

```sql
-- sqlite
CREATE TABLE IF NOT EXISTS "fs_legal_holds" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "reason" TEXT NOT NULL, "held_by" TEXT NOT NULL, "held_at" TIMESTAMP NOT NULL, PRIMARY KEY ("scope", "file_id"))
-- postgres
CREATE TABLE IF NOT EXISTS "fs_legal_holds" ("scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "reason" TEXT NOT NULL, "held_by" TEXT NOT NULL, "held_at" TIMESTAMPTZ NOT NULL, PRIMARY KEY ("scope", "file_id"))
-- mysql
CREATE TABLE IF NOT EXISTS `fs_legal_holds` (`scope` VARCHAR(191) NOT NULL, `file_id` VARCHAR(191) NOT NULL, `reason` VARCHAR(255) NOT NULL, `held_by` VARCHAR(191) NOT NULL, `held_at` DATETIME(6) NOT NULL, PRIMARY KEY (`scope`, `file_id`)) ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin`
```

Every migration is `Owner` (your plugin's name; the Store refuses another), a permanent `Name`, and its `Statements`. The ledger stores a checksum of the statements: editing a migration after it ran makes the next `Migrate` fail with `invalid_argument` (`records: migration legalhold/0001_legal_holds changed after it was applied`) rather than silently skip or re-run it. A schema change is a new migration; `records.NewDDL(engine).AddColumn(table, column)` renders an idempotent `ADD COLUMN` for each engine from your typed columns. See [Migrations](../records/migrations.md).

Check: `go run ./extending-records-plugin` prints the migrations it applied, the core's first:

```
migrations: records/0001_files_and_attachments, records/0002_files_uploaded_by, records/0003_files_status, legalhold/0001_legal_holds
```

## Step 3 — Statements that join the transaction

`Store.Exec`, `Store.One` and `Store.Each` run a `sqlb` statement on `Store.Conn(ctx)`: the open transaction when the context carries one, the database otherwise. Inside a hook they read and write within the operation being hooked:

<!-- ds:block id=plugin-isheld-cews2uet -->
**Plugin.isHeld** · [`examples/extending-records-plugin/main.go:127-136`](../../examples/extending-records-plugin/main.go#L127-L136)

```go
func (p *Plugin) isHeld(ctx context.Context, s *records.Store, scope, fileID string) (bool, error) {
	var id string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(FileID, &id)).From(Table).
		Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
	err := s.One(ctx, q)
	if errors.Is(err, sqlb.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
```
<!-- /ds:block hash=a48a85 -->

A plugin's own operations open a transaction with `Store.Tx`. When the caller is already in one (an app that holds a file in the same transaction as its own rows), `Tx` joins it instead of starting another, so the hold commits or rolls back with the caller's work. `Store.AfterCommit` registers work that must only happen once the data is durable, such as telling someone; it runs at once when there is no transaction and never when it rolls back:

<!-- ds:block id=plugin-hold-mg4j96bw -->
**Plugin.Hold** · [`examples/extending-records-plugin/main.go:144-165`](../../examples/extending-records-plugin/main.go#L144-L165)

```go
func (p *Plugin) Hold(ctx context.Context, s *records.Store, scope, fileID, reason string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		f, err := s.File(ctx, scope, fileID)
		if err != nil {
			return err
		}
		if f.DeletedAt != nil {
			return filesystem.Errorf(filesystem.CodeInvalidArgument, "%s: restore file %s before holding it", PluginName, fileID)
		}
		h := Hold{Scope: scope, FileID: fileID, Reason: reason, HeldBy: s.Actor(ctx), HeldAt: time.Now().UTC().Truncate(time.Microsecond)}
		ins := sqlb.NewInsert(s.Dialect()).Into(Table).Row(sqlb.Set(Scope, h.Scope), sqlb.Set(FileID, h.FileID),
			sqlb.Set(Reason, h.Reason), sqlb.Set(HeldBy, h.HeldBy), sqlb.Set(HeldAt, h.HeldAt))
		if _, err := s.Exec(ctx, ins); err != nil {
			if records.IsUniqueViolation(err) {
				return filesystem.Errorf(filesystem.CodeExists, "%s: file %s is already held", PluginName, fileID)
			}
			return err
		}
		s.AfterCommit(ctx, func(ctx context.Context) { p.notify(ctx, h) })
		return nil
	})
}
```
<!-- /ds:block hash=fad90e -->

`records.IsUniqueViolation` recognises a duplicate key on all three engines, so a second hold of the same file becomes `exists` rather than a driver-specific error. `s.Actor(ctx)` is who the request acts for: the application's `records.WithActorFunc` lookup, else the actor set with `records.WithActor`, the same one the Store records as `UploadedBy`. Read it there rather than with `records.ActorFrom`, so an application with its own lookup rules your plugin too.

<!-- ds:block id=plugin-release-ce86gtzx -->
**Plugin.Release** · [`examples/extending-records-plugin/main.go:169-179`](../../examples/extending-records-plugin/main.go#L169-L179)

```go
func (p *Plugin) Release(ctx context.Context, s *records.Store, scope, fileID string) error {
	del := sqlb.NewDelete(s.Dialect()).From(Table).Where(sqlb.Eq(Scope, scope), sqlb.Eq(FileID, fileID))
	n, err := s.Exec(ctx, del)
	if err != nil {
		return err
	}
	if n == 0 {
		return filesystem.Errorf(filesystem.CodeNotFound, "%s: file %s is not held", PluginName, fileID)
	}
	return nil
}
```
<!-- /ds:block hash=198306 -->

## Step 4 — Refuse a purge

`BeforePurge` runs before a file's rows are deleted, for every path that purges. Its error rolls the purge back before any row or object is touched. The two other `FileHooks` methods must exist but may do nothing:

<!-- ds:block id=plugin-beforepurge-az88amcp -->
**Plugin.BeforePurge** · [`examples/extending-records-plugin/main.go:191-200`](../../examples/extending-records-plugin/main.go#L191-L200)

```go
func (p *Plugin) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	held, err := p.isHeld(ctx, s, f.Scope, f.ID)
	if err != nil {
		return err
	}
	if held {
		return filesystem.NewError(filesystem.CodePermission, errHeld)
	}
	return nil
}
```
<!-- /ds:block hash=40ff36 -->

## Step 5 — Veto a change through its event

`FileHooks` has no hook before a trash. Every change emits an event inside its own transaction, though, and an error from `OnEvent` rolls the change back, which makes events a veto point as well as a notification:

<!-- ds:block id=plugin-onevent-7x79zumk -->
**Plugin.OnEvent** · [`examples/extending-records-plugin/main.go:206-218`](../../examples/extending-records-plugin/main.go#L206-L218)

```go
func (p *Plugin) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind != records.EventFileTrashed {
		return nil
	}
	held, err := p.isHeld(ctx, s, e.Scope, e.FileID)
	if err != nil {
		return err
	}
	if held {
		return filesystem.NewError(filesystem.CodePermission, errHeld)
	}
	return nil
}
```
<!-- /ds:block hash=c1cd32 -->

Events are for in-transaction work: writes to your own tables (an audit row, a search-index outbox), checks like this one. Work that must not happen for a change that rolls back, such as calling a webhook or sending mail, goes in `Store.AfterCommit` from inside the hook.

## Step 6 — Install and run

<!-- ds:block id=newstore-ubxccq5d -->
**newStore** · [`examples/extending-records-plugin/main.go:233-253`](../../examples/extending-records-plugin/main.go#L233-L253)

```go
func newStore(ctx context.Context, p *Plugin) (*records.Store, []string, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	applied, err := store.Migrate(ctx)
	return store, applied, err
}
```
<!-- /ds:block hash=5c0301 -->

Check: `go run ./extending-records-plugin` holds a file, and the trash and the purge are both refused with the plugin's error; after the release the file trashes and the sweep purges it:

```
migrations: records/0001_files_and_attachments, records/0002_files_uploaded_by, records/0003_files_status, legalhold/0001_legal_holds
notified: counsel@acme.test held fil_fti67z2sr6y3g3zhvdzx5cgs73 (litigation 2026-17)
trash: filesystem: permission: the file is under legal hold
purge: filesystem: permission: the file is under legal hold
after release: trashed and swept 1 file(s), <nil>
```

`TestHoldRolledBackNotifiesNobody` holds a file inside a transaction that then fails: the hold row is gone and the notification never ran.

## Pitfalls

- **A refused purge fails `PurgeOwner` and is reported by every `SweepTrash`.** `PurgeOwner` fails as a whole when one of the owner's files is held, which for legal holds is the point: the account cannot be erased while its files are held. `SweepTrash` purges the other trashed files and returns an error naming the refused one, on every run until the hold is lifted, which turns a scheduled sweep's log red for as long as the file sits in the trash. That is why `Hold` refuses a file already in the trash, and `TestTrashedHoldFailsEverySweep` pins it. A plugin that refuses purges should keep the files it protects out of the trash, as this one does with `OnEvent`.
- **Keep hooks to reads and your own tables.** A hook runs inside the operation it hooks; reading with `Store.File` is fine, and writing your own tables with `Store.Exec` is what hooks are for. Starting another change to the same file from its own hook (trashing it from `OnEvent` of its creation) nests one change inside another and emits events for both.
- **Use `Store.Conn(ctx)`, never your own `*sql.DB`.** A statement on another connection is outside the transaction: it commits when the operation rolls back, and on SQLite with a single connection it waits for the transaction that is waiting for it.
- **Time columns in UTC with microseconds.** The library stores instants truncated to microseconds in UTC (`TypesFor` gives `DATETIME(6)` on MySQL); do the same, or equality checks differ between engines.
- **Test on all three engines.** The library's own plugins run their suites on SQLite, Postgres and MySQL (`task test:engines`); a migration that works on SQLite can fail on MySQL's index-length limit.

## Next

- [Events and hooks](../records/events-and-hooks.md): the exact moment each hook and event runs.
- [Migrations](../records/migrations.md): the ledger, locks, `MigrationSQL` and `records.DDL` (tables, indexes, columns added later).
- [Operations and events](../reference/operations.md): every event kind and the fields it sets.
