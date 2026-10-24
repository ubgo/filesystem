# Extending: a records plugin for legal holds

A records plugin written from scratch: legal holds. A held file cannot be trashed or purged until the hold is released. The plugin owns a table and its migration, takes part in the file lifecycle through `FileHooks`, vetoes changes through `EventHooks`, joins the caller's transaction for its own writes, and sends a notification only after the transaction commits. Compliance holds, retention locks, per-file approvals and moderation flags are all this shape. It uses `github.com/ubgo/filesystem/records`, `upload`, the `memory` driver, `github.com/ubgo/sqlb` and SQLite (`modernc.org/sqlite`). The guide is [Write a records plugin](../../docs/extending/write-a-records-plugin.md).

## Run it

```sh
cd examples
go run ./extending-records-plugin
go test ./extending-records-plugin
```

It opens SQLite in memory (`:memory:`) and stores bytes on the memory driver, so nothing is left behind and no port is opened. A real plugin lives in a package of its own; it is in package `main` here so the example is one runnable file. The tests also check that a hold whose transaction rolls back notifies nobody, that a duplicate hold is refused, and that a trashed held file makes every sweep report it.

## What it does

1. Migrates the store: the core's migrations first, then the plugin's.
2. Uploads `minutes.txt` as `counsel@acme.test` and places it under hold. The notification runs after the hold commits.
3. Tries to trash and to purge the file: both are refused with the plugin's error and nothing changes.
4. Releases the hold, trashes the file and sweeps the trash, which now purges it.

```text
migrations: records/0001_files_and_attachments, records/0002_files_uploaded_by, records/0003_files_status, records/0004_files_object, records/0005_files_source, records/0006_files_expiry, records/0007_files_checksums, legalhold/0001_legal_holds
notified: counsel@acme.test held fil_jj6snbrfaeswdlyf56ceqz3en3 (litigation 2026-17)
trash: filesystem: permission: the file is under legal hold
purge: filesystem: permission: the file is under legal hold
after release: trashed and swept 1 file(s), <nil>
```

The file id changes on every run.

## The code

`Name` is the one required method of `records.Plugin`; it names the plugin in the migration ledger and in errors.

<!-- ds:block id=plugin-name-2xqe7aq8 -->
**Plugin.Name** · [`examples/extending-records-plugin/main.go:97`](main.go#L97-L97)

```go
func (p *Plugin) Name() string { return PluginName }
```
<!-- /ds:block hash=9fd92c -->

`Migrations` builds the plugin's table with `records.DDL` from the same typed columns its queries use, spelled for the engine by `records.TypesFor`, so one definition serves SQLite, Postgres and MySQL. A migration's name is permanent: a schema change is a new migration.

<!-- ds:block id=plugin-migrations-e4sm8uz8 -->
**Plugin.Migrations** · [`examples/extending-records-plugin/main.go:108-119`](main.go#L108-L119)

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

`isHeld` runs on the store's connection, so inside a hook it reads within the operation's transaction.

<!-- ds:block id=plugin-isheld-cews2uet -->
**Plugin.isHeld** · [`examples/extending-records-plugin/main.go:127-136`](main.go#L127-L136)

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

`Hold` writes the row inside `Store.Tx` with the Store's actor (`s.Actor`, which honours `WithActorFunc`), refuses a trashed file or a duplicate hold, and queues the notification with `AfterCommit` so a rolled-back hold tells nobody. `Release` deletes the row and answers `CodeNotFound` when there was none.

<!-- ds:block id=plugin-hold-mg4j96bw -->
**Plugin.Hold** · [`examples/extending-records-plugin/main.go:144-165`](main.go#L144-L165)

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

<!-- ds:block id=plugin-release-ce86gtzx -->
**Plugin.Release** · [`examples/extending-records-plugin/main.go:169-179`](main.go#L169-L179)

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

`BeforePurge` refuses to purge a held file, whether through `Purge`, `PurgeOwner` or `SweepTrash`; the error rolls the purge back before any row or object is deleted.

<!-- ds:block id=plugin-beforepurge-az88amcp -->
**Plugin.BeforePurge** · [`examples/extending-records-plugin/main.go:191-200`](main.go#L191-L200)

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

There is no file hook before a trash, but every change emits an event inside its own transaction and an error from `OnEvent` rolls it back. That is how the plugin vetoes trashing a held file.

<!-- ds:block id=plugin-onevent-7x79zumk -->
**Plugin.OnEvent** · [`examples/extending-records-plugin/main.go:206-218`](main.go#L206-L218)

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

`newStore` installs the plugin with `records.WithPlugins` and migrates; `Migrate` applies the core's migrations and then the plugin's.

<!-- ds:block id=newstore-ubxccq5d -->
**newStore** · [`examples/extending-records-plugin/main.go:233-253`](main.go#L233-L253)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The `legalhold` plugin (table, migration, `Hold`, `Release`, hooks), `newStore` and the demo `run` |
| `main_test.go` | The demo output, a rolled-back hold that notifies nobody, and a trashed held file that fails every sweep |

## Next

- [Write a records plugin](../../docs/extending/write-a-records-plugin.md): the guide for this program.
- [Events and hooks](../../docs/records/events-and-hooks.md) and [Migrations](../../docs/records/migrations.md): the exact moment each hook runs, and the migration ledger.
- Built-in plugins written the same way: [records-quota](../records-quota), [records-share](../records-share), [records-variants](../records-variants).
