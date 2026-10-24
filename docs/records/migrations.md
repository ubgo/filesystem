# Migrations

The records module creates and evolves its own tables, and every plugin with a table ships its migrations alongside. This page covers running them at start-up (`Migrate`), rendering them as a script for review or an external tool (`MigrationSQL`), the cross-process lock that lets several instances start together, the ledger that refuses an edited migration, and how a plugin of yours writes portable migrations with `TypesFor` and `records.DDL`. Read it before your first production deploy, and before you write a plugin with a table.

## Before you start

- A Store, as built in [Files](files.md).
- The example is [`examples/records-migrations`](../../examples/records-migrations): a Store with a small `notes` plugin that has two migrations. `go run ./records-migrations` in `examples/` prints the demo; `go run ./records-migrations sql` prints the SQLite script; `go test ./records-migrations` checks both.

## Step 1 — Migrate on start

`Migrate(ctx)` applies every migration that is not yet in the `fs_migrations` ledger, in order (the core's, then each plugin's in installation order), and returns the ones it applied as `owner/name`.

<!-- ds:block id=newstore-2dna975c -->
**newStore** · [`examples/records-migrations/main.go:111-121`](../../examples/records-migrations/main.go#L111-L121)

```go
func newStore(db *sql.DB, dialect sqlb.Dialect, notes Notes) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	return records.New(db, dialect, disks, records.WithPlugins(notes), records.WithMigrationLockWait(lockWait))
}
```
<!-- /ds:block hash=b0aa98 -->

<!-- ds:block id=migrateonstart-cjhttfvq -->
**migrateOnStart** · [`examples/records-migrations/main.go:127-129`](../../examples/records-migrations/main.go#L127-L129)

```go
func migrateOnStart(ctx context.Context, s *records.Store) ([]string, error) {
	return s.Migrate(ctx)
}
```
<!-- /ds:block hash=7281c6 -->

Check: the first start prints `first start applied: [records/0001_files_and_attachments records/0002_files_uploaded_by records/0003_files_status records/0004_files_object records/0005_files_source records/0006_files_expiry records/0007_files_checksums notes/0001_notes notes/0002_notes_author]`, the second `second start applied: []`. Running it on every start is the intended use: it costs one ledger read when there is nothing to do.

What `Migrate` does, step by step:

1. Takes a dedicated connection and a cross-process lock on it, so instances starting together apply each migration once (see step 3).
2. Creates the ledger `fs_migrations (owner, name, checksum, applied_at)` if needed and reads it.
3. For each migration already in the ledger, compares the SHA-256 of its statements with the stored checksum. A difference stops `Migrate` with `invalid_argument` (step 4).
4. Applies each new migration and records it. On SQLite and Postgres, a migration's statements and its ledger row commit in one transaction, so a crash never leaves it half applied. MySQL commits DDL implicitly, so there each statement runs on its own and the ledger row is written last; the statements are written to be re-runnable for that reason (`records.DDL`: `CREATE … IF NOT EXISTS`, inline indexes, and a catalogue check before an added MySQL column or index).

## The tables the core creates

| Table | Columns | Indexes |
|---|---|---|
| `fs_files` | `id`, `scope`, `disk`, `storage_key`, `filename`, `content_type`, `size`, `sha256`, `visibility`, `metadata` (JSON text), `created_at`, `deleted_at` (null while live), `uploaded_by`, `status` (`ready`, `processing`, `failed`; migration `0003_files_status`), `status_detail` | `(scope, created_at, id)` for listings, `(scope, sha256)` for duplicate lookups, `(scope, status)` for listing one processing status (migration `0003_files_status`) |
| `fs_attachments` | `id`, `scope`, `file_id`, `owner_type`, `owner_id`, `role`, `position`, `created_at` | unique `(scope, owner_type, owner_id, role, file_id)`, `(file_id)` |
| `fs_migrations` | `owner`, `name`, `checksum`, `applied_at` | primary key `(owner, name)` |

Every statement names the tables unqualified, so they live wherever the Store's connection points: put them in a Postgres schema of your own with `search_path=storage` (that schema alone) in the pgx DSN, a MySQL database per connection, or a SQLite file per Store. That is also how two Stores share one server. The Postgres and MySQL test suites run every test this way, each in a schema or database of its own.

There are no foreign keys between them or to your tables: the Store keeps them consistent, and your owner rows live in tables it never reads. The SQLite script, from `go run ./records-migrations sql`:

```sql
-- fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMP NOT NULL, PRIMARY KEY ("owner", "name"));

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS "fs_files" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "disk" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "deleted_at" TIMESTAMP, PRIMARY KEY ("id"));
CREATE INDEX IF NOT EXISTS "fs_files_scope_created" ON "fs_files" ("scope", "created_at", "id");
CREATE INDEX IF NOT EXISTS "fs_files_scope_sha256" ON "fs_files" ("scope", "sha256");
CREATE TABLE IF NOT EXISTS "fs_attachments" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "file_id" TEXT NOT NULL, "owner_type" TEXT NOT NULL, "owner_id" TEXT NOT NULL, "role" TEXT NOT NULL, "position" INTEGER NOT NULL, "created_at" TIMESTAMP NOT NULL, PRIMARY KEY ("id"));
CREATE UNIQUE INDEX IF NOT EXISTS "fs_attachments_owner" ON "fs_attachments" ("scope", "owner_type", "owner_id", "role", "file_id");
CREATE INDEX IF NOT EXISTS "fs_attachments_file" ON "fs_attachments" ("file_id");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0001_files_and_attachments', '9b05c159cf93d091de55e546575858878c817cb01e18fbac4f39b186b34c5aa9', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0002_files_uploaded_by
ALTER TABLE "fs_files" ADD COLUMN "uploaded_by" TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0002_files_uploaded_by', 'd8b47bc5d946ab9ec47b516a984a9744ba7d0db5a5c9b10db0de300c1101037b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- records 0003_files_status
ALTER TABLE "fs_files" ADD COLUMN "status" TEXT NOT NULL DEFAULT 'ready';
ALTER TABLE "fs_files" ADD COLUMN "status_detail" TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS "fs_files_scope_status" ON "fs_files" ("scope", "status");
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('records', '0003_files_status', '5f66e178b335a533d7fd81c9cf0e251aba8b3e61d130b507c65b8baa91205363', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- notes 0001_notes
CREATE TABLE IF NOT EXISTS app_file_notes (file_id TEXT NOT NULL PRIMARY KEY, body TEXT NOT NULL);
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('notes', '0001_notes', 'f41633b8d417c0055516fa5d1ebeaa8f2b9ffc876cb6f06fd2c49658af06091b', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;

-- notes 0002_notes_author
ALTER TABLE app_file_notes ADD COLUMN author TEXT NOT NULL DEFAULT '';
INSERT INTO fs_migrations (owner, name, checksum, applied_at) VALUES ('notes', '0002_notes_author', '1612536f407195d96056cad347af80baa0e534e9edf97c8b2600db8fe3707f06', CURRENT_TIMESTAMP) ON CONFLICT (owner, name) DO NOTHING;
```

On MySQL the indexes are declared inside `CREATE TABLE` (MySQL has no `CREATE INDEX IF NOT EXISTS`), an index added later runs only when `information_schema` lacks it, and every table ends with ``ENGINE = InnoDB DEFAULT CHARSET = `utf8mb4` COLLATE = `utf8mb4_bin` ``, so keys and names compare byte for byte: with MySQL's default collation `a.png` would equal `A.png`. Identifiers are quoted on every engine, since the statements are built by `sqlb/ddl`.

## Step 2 — Render the script instead

`MigrationSQL()` returns every migration as one script, each preceded by `-- owner name` and followed by the insert that records it in the `fs_migrations` ledger, which the script creates first, without touching the database. Use it to review a release's schema change, or to hand the change to an external migration tool when your deploy process requires one.

<!-- ds:block id=script-54jkpwk3 -->
**script** · [`examples/records-migrations/main.go:134-136`](../../examples/records-migrations/main.go#L134-L136)

```go
func script(s *records.Store) (string, error) {
	return s.MigrationSQL()
}
```
<!-- /ds:block hash=9307cd -->

The script records each migration in the ledger exactly as `Migrate` does (owner, name and the checksum of its statements), so after applying it with another tool a later `Migrate` sees those migrations as applied and does nothing for them; you can switch between the two from one release to the next. The ledger inserts skip a migration already recorded (`ON CONFLICT DO NOTHING`, `INSERT IGNORE` on MySQL), but not every statement is re-runnable (SQLite has no `ADD COLUMN IF NOT EXISTS`), so apply a script once, to a database `Migrate` has not set up. On MySQL, run it on one connection: the column additions use a session variable and a prepared statement.

## Step 3 — Several instances at once

`Migrate` holds a lock for its whole run:

| Engine | Lock | Waiting |
|---|---|---|
| Postgres | `pg_advisory_lock` on a fixed key, per database | waits without a bound until the other instance finishes |
| MySQL | `GET_LOCK` on a name that includes a hash of the database name, so two apps on one server do not block each other | at most `WithMigrationLockWait` (whole seconds; default `records.DefaultMigrationLockWait`, 60s; 0 fails at once). A lock still held after that is `unavailable: records: another process holds the migration lock` |
| SQLite | none needed: one writer | none |

A stuck migration on MySQL therefore fails the next deploy instead of hanging it. A negative wait makes `records.New` fail with `invalid_argument`.

## Step 4 — Never edit a migration that ran

A migration's `Name` identifies it for ever; its checksum is the SHA-256 of its statements. Changing the statements of a migration that already ran makes the next `Migrate` stop with `invalid_argument` rather than skip it (the database would not match the code) or re-run it (it might not be re-runnable). Ship a change as a new migration instead.

Check: the demo builds a second Store whose `notes/0001_notes` statement was edited and prints `edited migration: invalid_argument: … records: migration notes/0001_notes changed after it was applied`.

## Step 5 — Migrations for your own plugin

A plugin with a table implements `records.Migrator`. Each `records.Migration` has `Owner` (the plugin's `Name()`; anything else is `invalid_argument`), a permanent `Name`, and the `Statements` for the engine it is asked for. Build them with `records.DDL` from the plugin's typed columns (see below): the demo's first migration creates its table, the second adds a column; an edit of the first is simulated by building it with a different column:

<!-- ds:block id=notes-migrations-sk59n7yu -->
**Notes.Migrations** · [`examples/records-migrations/main.go:72-88`](../../examples/records-migrations/main.go#L72-L88)

```go
func (n Notes) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	body := records.Column(noteBody, t.Doc)
	if n.Edited {
		body = records.Column(noteText, t.Doc)
	}
	table, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table:      notes,
		Columns:    []ddl.ColumnSpec{records.Column(noteFileID, t.Key), body},
		PrimaryKey: []ddl.ColumnRef{noteFileID},
	}).Statements()
	author, err2 := addAuthor(engine)
	return []records.Migration{
		{Owner: n.Name(), Name: "0001_notes", Statements: table},
		{Owner: n.Name(), Name: "0002_notes_author", Statements: author},
	}, errors.Join(err1, err2)
}
```
<!-- /ds:block hash=566bd4 -->

`records.TypesFor(engine)` returns the type names the `fs_` tables use, so a plugin's columns match them on every engine:

<!-- ds:block id=keytypes-rwytdkrm -->
**keyTypes** · [`examples/records-migrations/main.go:140-145`](../../examples/records-migrations/main.go#L140-L145)

```go
func keyTypes(w io.Writer) {
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		t := records.TypesFor(engine)
		fmt.Fprintf(w, "%-8s Key=%s Short=%s Timestamp=%s Doc=%s\n", engine, t.Key, t.Short, t.Timestamp, t.Doc)
	}
}
```
<!-- /ds:block hash=26764b -->

```text
sqlite   Key=TEXT Short=TEXT Timestamp=TIMESTAMP Doc=TEXT
postgres Key=TEXT Short=TEXT Timestamp=TIMESTAMPTZ Doc=TEXT
mysql    Key=VARCHAR(191) Short=VARCHAR(64) Timestamp=DATETIME(6) Doc=MEDIUMTEXT
```

| Field | For |
|---|---|
| `Key` | IDs and scopes: indexed, up to 191 characters (MySQL's indexed utf8mb4 width) |
| `Short` | Owner types, roles, kinds: indexed, up to 64 characters, so composite indexes stay under MySQL's 3072-byte limit |
| `Text` | Display strings up to 255 characters |
| `Long` | Up to 1024 characters (a storage key), not indexed |
| `Int` | 64-bit integers |
| `Timestamp` | UTC instants with microsecond precision |
| `Doc` | Large text such as JSON, not indexed |

### Build them from the typed columns: records.DDL

The core and every bundled plugin build their migrations with `records.DDL`, on top of [`github.com/ubgo/sqlb/ddl`](https://github.com/ubgo/sqlb/blob/main/docs/ddl/README.md), from the same `sqlb.Col` values their queries use, so a table cannot be created under one name or type and read under another. `records.NewDDL(engine)` collects the statements of one migration; each step returns the builder, and `Statements()` returns them or the first error, with nothing half built:

- `CreateTable(records.Table{Table, Columns, PrimaryKey, Indexes})`: `CREATE TABLE IF NOT EXISTS`. A column is `records.Column(col, spelling)`, the typed column spelled with `TypesFor`; nullability follows the Go type (a pointer column is `NULL`), and `.Default(v)` gives a default. On MySQL the indexes are declared inside the table, and the table is InnoDB with `utf8mb4` and `utf8mb4_bin`; on Postgres and SQLite each index is a `CREATE INDEX IF NOT EXISTS` after it.
- `CreateIndex(table, records.Index{Name, Unique, Columns})`: an index on an existing table. Columns are `records.Key(col)`.
- `AddColumn(table, column)`: `ADD COLUMN IF NOT EXISTS` on Postgres; on SQLite a plain `ADD COLUMN`, since its DDL is transactional and a migration never half applies; on MySQL, which has no `IF NOT EXISTS` for it, an `ADD COLUMN` that runs only when `information_schema` lacks the column. A `NOT NULL` column needs a default, for the rows already there.
- `ModifyColumn(table, column)`: MySQL's `MODIFY COLUMN`, for a step only MySQL needs; it writes nothing on the other engines.
- `Raw(statements...)`: for what `sqlb/ddl` does not build, such as a data backfill between two schema steps. Keep them re-runnable.

Every statement can run again after a crash between it and the ledger write, which matters on MySQL, whose DDL commits at once. `sqlb/ddl` records MySQL's repeatable `CREATE INDEX` as a `SkipIf` query for a runner to honour; the Store's migrations are plain statements, so `DDL` folds the check into the statement with `PREPARE` (`SET @fs_ddl = IF(EXISTS(<check>), 'DO 0', '<statement>')`). The share plugin's migrations, as an example:

<!-- ds:block id=plugin-migrations-8x5tnuwt -->
**Plugin.Migrations** · [`records/share/share.go:291-326`](../../records/share/share.go#L291-L326)

```go
func (p *Plugin) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: Table,
		Columns: []ddl.ColumnSpec{
			records.Column(ID, t.Key), records.Column(Scope, t.Key), records.Column(FileID, t.Key), records.Column(TokenHash, t.Short),
			records.Column(ExpiresAt, t.Timestamp), records.Column(MaxDownloads, t.Int), records.Column(Downloads, t.Int),
			records.Column(RevokedAt, t.Timestamp), records.Column(CreatedAt, t.Timestamp),
		},
		PrimaryKey: []ddl.ColumnRef{ID},
		Indexes: []records.Index{
			{Name: "fs_shares_token", Unique: true, Columns: []records.IndexColumn{records.Key(TokenHash)}},
			{Name: "fs_shares_file", Columns: []records.IndexColumn{records.Key(FileID)}},
		},
	}).Statements()
	more, err2 := records.NewDDL(engine).
		AddColumn(Table, records.Column(PasswordHash, t.Text).Default("")).
		AddColumn(Table, records.Column(TargetKind, t.Short).Default(KindFile)).
		AddColumn(Table, records.Column(TargetRef, t.Key).Default("")).
		AddColumn(Table, records.Column(Mode, t.Short).Default(ModeDownload)).
		CreateTable(records.Table{
			Table: EventsTable,
			Columns: []ddl.ColumnSpec{
				records.Column(EventID, t.Key), records.Column(EventScope, t.Key), records.Column(EventShareID, t.Key),
				records.Column(EventAt, t.Timestamp), records.Column(EventViewer, t.Key),
			},
			PrimaryKey: []ddl.ColumnRef{EventID},
			Indexes: []records.Index{
				{Name: "fs_share_events_share", Columns: []records.IndexColumn{records.Key(EventShareID), records.Key(EventAt), records.Key(EventID)}},
			},
		}).Statements()
	return []records.Migration{
		{Owner: PluginName, Name: "0001_shares", Statements: st},
		{Owner: PluginName, Name: "0002_shares_access", Statements: more},
	}, errors.Join(p.err, err, err2)
}
```
<!-- /ds:block hash=620956 -->

The demo plugin's second migration adds a column with `DDL.AddColumn`, and the program prints what it renders on each engine:

<!-- ds:block id=addauthor-xz8sjzh6 -->
**addAuthor** · [`examples/records-migrations/main.go:94-96`](../../examples/records-migrations/main.go#L94-L96)

```go
func addAuthor(engine string) ([]string, error) {
	return records.NewDDL(engine).AddColumn(notes, records.Column(noteAuthor, records.TypesFor(engine).Key).Default("")).Statements()
}
```
<!-- /ds:block hash=745378 -->

```text
AddColumn sqlite:
  ALTER TABLE "app_file_notes" ADD COLUMN "author" TEXT NOT NULL DEFAULT ''
AddColumn postgres:
  ALTER TABLE "app_file_notes" ADD COLUMN IF NOT EXISTS "author" TEXT NOT NULL DEFAULT ''
AddColumn mysql:
  SET @fs_ddl = IF(EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'app_file_notes' AND column_name = 'author'), 'DO 0', 'ALTER TABLE `app_file_notes` ADD COLUMN `author` VARCHAR(191) NOT NULL DEFAULT ''''')
  PREPARE fs_ddl FROM @fs_ddl
  EXECUTE fs_ddl
  DEALLOCATE PREPARE fs_ddl
```

Table, index and column names are pasted into the SQL as identifiers, and the MySQL check quotes them as string literals: pass your own constants, never user input.

The module's tests render every migration of the core and of every bundled plugin on all three engines and compare the text with checked-in files (`records/testdata/migrations_<engine>.sql`), so a change to the shipped DDL is a reviewed choice, never a side effect.

`Store.Migrations()` returns the full ordered list, which a test can use to check that a plugin's migrations are present.

Name your plugin's tables and indexes with the `fs_` prefix (`records.ObjectPrefix`), as the bundled plugins do: that is how they follow a Store's table names (next step). A table named otherwise keeps its name under any mapping.

## Step 6 — Other table names

When the namespace is not enough (two Stores in one Postgres schema, a house rule such as `storage_files`, a database whose `fs_` names are taken), rename every table and index with one option on the Store:

```go
store, err := records.New(db, sqlb.PostgresDialect{}, disks,
	records.WithTablePrefix("app_"), // fs_files becomes app_fs_files, fs_acl_grants app_fs_acl_grants
	records.WithPlugins(acl.New(), folders.New()),
)
```

Or map each name with a function of your own:

```go
records.WithTableNames(func(name string) string {
	return "storage_" + strings.TrimPrefix(name, records.ObjectPrefix) // fs_files -> storage_files
})
```

What it covers and what it does not:

| | |
|---|---|
| Queries | Every statement the Store and its plugins run (`Exec`, `One`, `Each`, `Build`) names the mapped tables. |
| Migrations | `Migrate`, `Migrations` and `MigrationSQL` create the mapped tables and indexes, and the ledger is the mapped `fs_migrations`. Their checksums are those of the mapped statements, so the default Store and a prefixed one keep separate ledgers in one database. |
| Your SQL | Name the tables with `store.TableName(records.TableNameFiles)`; the typed columns (`records.FileID`) carry the default names, so a query you render yourself with them names `fs_files`. Run it through `store.Exec`, `One` or `Each`, which map it. |
| Checks | `Migrate` refuses (`invalid_argument`) a mapping that gives two names one table, or a name that is not an identifier of letters, digits and `_` of at most 63 bytes (Postgres cuts longer names silently). `WithTablePrefix` takes `[a-z_][a-z0-9_]*`, at most 32 bytes. |
| ORMs | `contrib/ent`: the mixins take the same mapping in `Names` (`fsent.FileMixin{Names: names}`; [ent](../integrations/ent.md#step-1--declare-the-schemas)). `contrib/gorm`: open GORM with `NamingStrategy: fsgorm.NewNamer(nil, names)` ([GORM](../integrations/gorm.md#step-1--open-gorm-on-the-stores-database)). `contrib/sqlc`: generate from `sqlc.FSWith(names)` ([sqlc](../integrations/sqlc.md#step-1--lay-out-the-files)). |
| Existing data | Nothing is renamed: pick the names before the first `Migrate`. To move a running database, rename the tables yourself, then the ledger. |

The module's tests render every migration under `WithTablePrefix("app_")` too (`records/testdata/migrations_<engine>_prefixed.sql`), and run every plugin against a prefixed Store beside a default one in the same database.

Try it: [`examples/integrations-orm`](../../examples/integrations-orm) runs this section in `prefix.go` ([`openPrefixedStore`](ds:block?id=openprefixedstore-g4t8w2nc), [`prefixedReaders`](ds:block?id=prefixedreaders-u6m3x9kd)): `go run ./integrations-orm` from `examples/`, then look for `prefix: the Store writes app_fs_files`.

## What can go wrong

- `invalid_argument: records: migration owner/name changed after it was applied`: someone edited a shipped migration. Restore its statements and add a new migration.
- `unavailable: records: another process holds the migration lock` (MySQL): another instance is migrating, or crashed while holding the lock on a connection that is still open. Raise `WithMigrationLockWait` or find the stuck process.
- A deploy hangs at start-up on Postgres: another instance holds the advisory lock. Look for it in `pg_locks`.
- `records: migration owner/name: …` wrapping a database error: a statement failed. On MySQL earlier statements of the same migration may already be applied; fix the cause and run `Migrate` again, since the statements are re-runnable.
- `duplicate column name` on SQLite: the script was applied outside `Migrate`, then `Migrate` ran.
- `invalid_argument: records: table names "fs_a" and "fs_b" both map to "x"` or `… not an identifier`: the `WithTableNames` function is not injective or returns a name the engines cannot take. Nothing was created.

## Next

- [Events and hooks](events-and-hooks.md): the plugin interfaces besides `Migrator`.
- [Production checklist](../start/production.md): where migrations fit in a deploy.
- [Write a records plugin](../extending/write-a-records-plugin.md).
