# sqlc

You will read the records tables (`fs_files`, `fs_attachments`) with Go code that [sqlc](https://sqlc.dev) generates from SQL: typed rows, compile-time checked queries, and joins with your own tables in one statement. The module is `github.com/ubgo/filesystem/contrib/sqlc`. It holds the tables as sqlc needs them, one `schema.sql` per engine, a shared `queries.sql` of read paths, a `pages.sql` per engine with the paginated listings, and a `sqlc.yaml` per engine.

Read this page if your app already generates its data layer with sqlc and you want a dashboard, a report or an admin listing to read files without calling the Store row by row. Writes still go through the Store.

## Before you start

- A Store, as in [Files](../records/files.md), migrated with `Store.Migrate`. The tables come from there, never from `schema.sql`.
- sqlc for `sqlc generate`, installed or run as `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`, the version the module's tests generate with. The module itself needs nothing but the standard library and the core.
- Install the module: `go get github.com/ubgo/filesystem/contrib/sqlc`, or copy its files: they are plain SQL and YAML.

## Read only, on purpose

Every write to `fs_files` and `fs_attachments` goes through `records.Store`. The Store runs plugin hooks (quota charges, versions, folders), emits the events the audit log and your webhooks hear, and keeps a stored object and its row consistent: object first, row second on create; row first, object after the commit on purge. A row inserted or deleted by a generated query skips all of that, so the files here hold reads only, and an app should keep its sqlc writes away from `fs_` tables.

## Step 1 — Lay out the files

Each engine has a directory with `schema.sql`, `pages.sql` and `sqlc.yaml`; `queries.sql` sits beside them and is shared:

```text
contrib/sqlc/
  queries.sql
  postgres/schema.sql   postgres/pages.sql   postgres/sqlc.yaml
  mysql/schema.sql      mysql/pages.sql      mysql/sqlc.yaml
  sqlite/schema.sql     sqlite/pages.sql     sqlite/sqlc.yaml
```

The paginated listings live per engine because sqlc spells `LIMIT` differently: its MySQL parser accepts only a bare `?` there (the generated parameter is named `Limit`), while Postgres and SQLite take `sqlc.arg(page_size)` (named `PageSize`). Everything else is shared.

Copy the directory of your engine and `queries.sql` into your project, or write them out from Go: `sqlc.FS()` returns them in that layout, `sqlc.Schema(engine)` one schema, `sqlc.Queries(engine)` every query of an engine.

sqlc bakes table names in when it generates, so a Store built with `records.WithTablePrefix` or `records.WithTableNames` needs files that name its tables. `sqlc.FSWith(names)` returns the same layout with every records table and index name in the `.sql` files mapped by the Store's mapping (`sqlc.SchemaWith` and `sqlc.QueriesWith` do it for one file, `sqlc.MapNames` for any SQL); write it out and generate as below. sqlc names the models after the tables (`app_fs_files` gives `AppFsFile`); a `rename` in your `sqlc.yaml` keeps `FsFile` if you prefer. Regenerate whenever the mapping changes, as you would after a records migration.

```go
names := func(name string) string { return "app_" + name } // what records.WithTablePrefix("app_") does
project, err := sqlc.FSWith(names)
// write project out (os.CopyFS(dir, project)), then run sqlc generate in dir/<engine>
``` Add your own queries next to `queries.sql` (sqlc accepts a list) so one generated package covers your tables and the files.

The `sqlc.yaml` of Postgres:

```yaml
version: "2"
sql:
  - engine: "postgresql"
    schema: "schema.sql"
    queries: ["../queries.sql", "pages.sql"]
    gen:
      go:
        package: "fsdb"
        out: "fsdb"
```

## Step 2 — Generate

```sh
cd postgres
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

`fsdb` then holds a `Queries` type with one method per query, taking the arguments by name (`GetFileParams{Scope, ID}`) and returning typed rows.

| Query | Returns | Matches |
|---|---|---|
| `GetFile` | one file of a scope, live or trashed | `Store.File` |
| `ListLiveFiles`, `ListLiveFilesAfter` | a page of live files, newest first, then the page after a given `(created_at, id)` | `Store.Files` |
| `ListFilesBySHA256` | the live files of a scope with one digest (lowercase hex), newest first | `Store.FileBySHA256`, which returns the first row |
| `ScopeTotals` | file count and bytes of a scope, trashed files included | what the quota plugin counts for files |
| `ListOwnerAttachments` | the live files attached to an owner under a role, in position order | `Store.Attachments` |
| `ListFileAttachments` | every owner and role a file is attached to | |

Every query filters by `scope` first, as the Store does: never drop it, or a listing crosses tenants.

## Step 3 — Query with the generated package

The program [`examples/integrations-orm`](../../examples/integrations-orm/README.md) carries the package sqlc v1.31.1 generated from the `sqlite` directory (`fsdb/`, committed as generated) and reads a SQLite database that a Store wrote. The Store writes every row; sqlc only reads:

<!-- ds:block id=seed-8tv7pj5w -->
**seed** · [`examples/integrations-orm/main.go:117-138`](../../examples/integrations-orm/main.go#L117-L138)

```go
func seed(ctx context.Context, store *records.Store) error {
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}
	post := records.Owner{Type: ownerPost, ID: postID}
	for _, name := range galleryFiles {
		f, err := store.Upload(ctx, tenant, diskName, strings.NewReader(name), name, policy)
		if err != nil {
			return err
		}
		if _, err := store.Attach(ctx, tenant, f.ID, post, roleGallery); err != nil {
			return err
		}
		if name == trashedName {
			if err := store.Trash(ctx, tenant, f.ID); err != nil {
				return err
			}
		}
		// SQLite stores timestamps to the microsecond; a pause keeps the
		// newest-first order of the listings deterministic.
		time.Sleep(time.Millisecond)
	}
	return nil
}
```
<!-- /ds:block hash=0534f3 -->

A post's gallery is one call. `ListOwnerAttachments` joins each attachment to its file and leaves trashed files out, as `Store.Attachments` does:

<!-- ds:block id=sqlcgallery-udcqpkzw -->
**sqlcGallery** · [`examples/integrations-orm/main.go:169-176`](../../examples/integrations-orm/main.go#L169-L176)

```go
func sqlcGallery(ctx context.Context, q *fsdb.Queries, scope, owner string) ([]fsdb.ListOwnerAttachmentsRow, error) {
	return q.ListOwnerAttachments(ctx, fsdb.ListOwnerAttachmentsParams{
		Scope:     scope,
		OwnerType: ownerPost,
		OwnerID:   owner,
		Role:      roleGallery,
	})
}
```
<!-- /ds:block hash=d082fe -->

The paginated listing is keyset pagination: `ListLiveFiles` for the first page, then `ListLiveFilesAfter` from the last row's `created_at` and `id` until a page comes back short. It is the order of `Store.Files`, newest first:

<!-- ds:block id=sqlcalllivefiles-tgujm898 -->
**sqlcAllLiveFiles** · [`examples/integrations-orm/main.go:183-199`](../../examples/integrations-orm/main.go#L183-L199)

```go
func sqlcAllLiveFiles(ctx context.Context, q *fsdb.Queries, scope string) ([]string, error) {
	page, err := q.ListLiveFiles(ctx, fsdb.ListLiveFilesParams{Scope: scope, PageSize: pageSize})
	var names []string
	for err == nil {
		for _, f := range page {
			names = append(names, f.Filename)
		}
		if len(page) < pageSize {
			return names, nil
		}
		last := page[len(page)-1]
		page, err = q.ListLiveFilesAfter(ctx, fsdb.ListLiveFilesAfterParams{
			Scope: scope, AfterCreatedAt: last.CreatedAt, AfterID: last.ID, PageSize: pageSize,
		})
	}
	return nil, err
}
```
<!-- /ds:block hash=d7ff3d -->

Run it from `examples/` (`go run ./integrations-orm`; `go test ./integrations-orm` pins the output):

```text
sqlc gallery: 1 cover.txt 9 bytes
sqlc gallery: 3 map.txt 7 bytes
sqlc live files, 1 per page: map.txt, cover.txt
sqlc totals: 3 files, 25 bytes (trash included)
```

`draft.txt`, attached at position 2 and then trashed, is in neither the gallery nor the listing, but its 9 bytes are in `ScopeTotals`, which counts trashed files as the quota plugin does. The program's test also fails when `contrib/sqlc` ships a query the committed `fsdb` was not generated with, so a stale copy is caught rather than silently missing a method.

## How it is kept true

The files describe tables the Store owns, so the module's tests check them against it on every engine (`task test:engines` runs them on Postgres and MySQL too):

- `TestSchemaMatchesStore` creates the tables once with `Store.Migrate` and once from `schema.sql`, and compares every column's type, nullability and default and every index, as the engine's catalogue reports them. When a records migration adds a column, this test fails until `schema.sql` has it.
- `TestQueries` writes files and attachments through the Store, runs every query of `queries.sql` and compares each answer with the Store's own (`Store.Files`, `Store.Attachments`, …).
- `TestSchemaWithMatchesPrefixedStore` and `TestQueriesWithPrefixedStore` do the same for `SchemaWith` and `QueriesWith` against a Store built with `WithTableNames`, and `TestMapNames` pins that only records names change.
- `TestSqlcGenerate` runs the pinned sqlc (`go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`, so nothing needs installing) for the three engines, from `FS()` and from `FSWith` with a prefix, and builds the generated Go. For generated code the build is the test, so it skips only when go cannot download sqlc, and says so. It is what found that MySQL needs the bare `LIMIT ?`. The first run builds sqlc, about a minute; later runs take a second or two.

## What can go wrong

- `no such table: fs_files` (or `relation "fs_files" does not exist`) at run time — the Store was built with a table prefix or mapping and the code was generated from `FS()`; generate from `FSWith` with the Store's mapping.
- `sqlc generate` reports a missing column — your copy of `schema.sql` is older than the Store's tables; copy it again from the module version you run.
- A query sees a file the Store hides — `ListLiveFiles` and `ListOwnerAttachments` leave trashed files out; `ListFilesBySHA256` leaves them out too; `GetFile` and `ScopeTotals` include them, as their Store counterparts do. Filter on `deleted_at IS NULL` in queries of your own.
- **Untyped columns on SQLite.** sqlc types `deleted_at` and `expires_at` (nullable `TIMESTAMP`) and `ScopeTotals.Bytes` (a `COALESCE`) as `interface{}` on SQLite; the program prints `Bytes` with `%v`. Convert them yourself, or override the column types in your `sqlc.yaml` (`overrides`), rather than editing the generated code.
- **Never edit `fsdb/` by hand.** It is regenerated whole; put your own queries in your own `.sql` file listed in `sqlc.yaml` beside `queries.sql`.
- **Keep the page size and the cursor together.** `ListLiveFilesAfter` needs both the `created_at` and the `id` of the last row: two files created in the same instant are ordered by `id`, and a cursor with only the time would skip or repeat one of them.
- Rows of plugin tables (`fs_audit`, `fs_file_versions`, `fs_quota_usage`, …) are not described; read them through their plugins' Go API.

## Next

- [`examples/integrations-orm`](../../examples/integrations-orm/README.md): the runnable program behind Step 3.
- [GORM](gorm.md) and [ent](ent.md) read the same tables through those ORMs.
- [Files](../records/files.md) and [Attachments](../records/attachments.md): the Store calls these queries mirror.
- The module's [README](../../contrib/sqlc/README.md) lists the files and the Go API.
