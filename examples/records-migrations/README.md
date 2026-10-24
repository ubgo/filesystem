# Creating and evolving the records schema

How the records tables are created and changed over time: `Migrate` applies what is new, once, under a lock; `MigrationSQL` renders the same changes as one script for a review or an external migration tool; a plugin ships its own migrations, built with `records.DDL` from typed columns, and adds a column later on any engine; `TypesFor` gives the per-engine type names; and a migration whose statements changed after it ran is refused. It uses `github.com/ubgo/filesystem/records` on SQLite, with `github.com/ubgo/sqlb` (and its `ddl` package) for the statements and the Postgres and MySQL output. The guide [Migrations](../../docs/records/migrations.md) explains each step.

## Run it

```sh
cd examples
go run ./records-migrations        # the demo
go run ./records-migrations sql    # the SQLite script only
go test ./records-migrations
```

It needs no network, database server or environment variables. The demo's SQLite database lives in a temporary directory that is deleted when the program exits; the `sql` mode uses an in-memory SQLite database and runs no statement.

## What it does

```sh
go run ./records-migrations
```

```text
first start applied: [records/0001_files_and_attachments records/0002_files_uploaded_by records/0003_files_status records/0004_files_object records/0005_files_source records/0006_files_expiry records/0007_files_checksums notes/0001_notes notes/0002_notes_author] <nil>
second start applied: [] <nil>
edited migration: invalid_argument: filesystem: invalid_argument: records: migration notes/0001_notes changed after it was applied
sqlite   Key=TEXT Short=TEXT Timestamp=TIMESTAMP Doc=TEXT
postgres Key=TEXT Short=TEXT Timestamp=TIMESTAMPTZ Doc=TEXT
mysql    Key=VARCHAR(191) Short=VARCHAR(64) Timestamp=DATETIME(6) Doc=MEDIUMTEXT
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

1. The first start applies the core's seven migrations, then the `notes` plugin's two, each recorded in the `fs_migrations` ledger as `owner/name`.
2. The second start applies nothing: run `Migrate` on every start.
3. A store whose `notes/0001_notes` statements were edited after they ran is refused with `invalid_argument` instead of skipping or re-running it. A change ships as a new migration, never as an edit.
4. `TypesFor` gives each engine's names for keys, short strings, timestamps and documents, so one plugin migration works on all three. The plugin builds its table and its added column with `records.DDL` from the same typed columns its queries use.
5. The second migration, `DDL.AddColumn`, renders an idempotent column addition per engine: plain on SQLite, `IF NOT EXISTS` on Postgres, an `information_schema` check on MySQL.

The `sql` mode prints the script and exits (50 lines; the first ones):

```sh
go run ./records-migrations sql
```

```text
-- fs_migrations (the ledger Migrate reads)
CREATE TABLE IF NOT EXISTS "fs_migrations" ("owner" TEXT NOT NULL, "name" TEXT NOT NULL, "checksum" TEXT NOT NULL, "applied_at" TIMESTAMP NOT NULL, PRIMARY KEY ("owner", "name"));

-- records 0001_files_and_attachments
CREATE TABLE IF NOT EXISTS "fs_files" ("id" TEXT NOT NULL, "scope" TEXT NOT NULL, "disk" TEXT NOT NULL, "storage_key" TEXT NOT NULL, "filename" TEXT NOT NULL, "content_type" TEXT NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "created_at" TIMESTAMP NOT NULL, "deleted_at" TIMESTAMP, PRIMARY KEY ("id"));
```

## The code

A plugin with a table of its own and a column added in a later release, written once with `TypesFor`:

<!-- ds:block id=notes-migrations-sk59n7yu -->
**Notes.Migrations** · [`examples/records-migrations/main.go:72-88`](main.go#L72-L88)

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

A Store with the plugin installed; building it does not migrate:

<!-- ds:block id=newstore-2dna975c -->
**newStore** · [`examples/records-migrations/main.go:111-121`](main.go#L111-L121)

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

What an app runs at every start, and the script for teams that apply schema changes with their own tool:

<!-- ds:block id=migrateonstart-cjhttfvq -->
**migrateOnStart** · [`examples/records-migrations/main.go:127-129`](main.go#L127-L129)

```go
func migrateOnStart(ctx context.Context, s *records.Store) ([]string, error) {
	return s.Migrate(ctx)
}
```
<!-- /ds:block hash=7281c6 -->

<!-- ds:block id=script-54jkpwk3 -->
**script** · [`examples/records-migrations/main.go:134-136`](main.go#L134-L136)

```go
func script(s *records.Store) (string, error) {
	return s.MigrationSQL()
}
```
<!-- /ds:block hash=9307cd -->

The per-engine type names:

<!-- ds:block id=keytypes-rwytdkrm -->
**keyTypes** · [`examples/records-migrations/main.go:140-145`](main.go#L140-L145)

```go
func keyTypes(w io.Writer) {
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		t := records.TypesFor(engine)
		fmt.Fprintf(w, "%-8s Key=%s Short=%s Timestamp=%s Doc=%s\n", engine, t.Key, t.Short, t.Timestamp, t.Doc)
	}
}
```
<!-- /ds:block hash=26764b -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the `Notes` plugin, migrate twice, the edited-migration refusal, type names, the `records.DDL` column addition, and the `sql` mode |
| `main_test.go` | Runs the same steps, renders the script for each engine, and checks the migration list order and a negative lock wait |

## Next

- The guide: [Migrations](../../docs/records/migrations.md).
- Related examples: [start-production](../start-production) for `migrate` and `migrate-sql` commands in a deployed app, [records-events](../records-events) for a plugin with a table of its own.
