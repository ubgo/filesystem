# GORM

You will read the records tables (`fs_files`, `fs_attachments`) with GORM models: list a scope's files, preload a file's attachments or an attachment's file, and join them with your own models in one query. The module is `github.com/ubgo/filesystem/contrib/gorm` (package `fsgorm`). It holds two models, `File` and `Attachment`, and the scopes that make a GORM query behave like the Store's listings.

> [!WARNING]
> Never call `AutoMigrate` on these models, and never `Create`, `Save`, `Update` or `Delete` them. The tables belong to `Store.Migrate`, and AutoMigrate alters their columns: on SQLite it recreates the whole table (found while writing this module; it even panics in dry-run mode). Writes go through `records.Store`, which runs plugin hooks, emits events and keeps stored objects consistent with their rows.

Read this page if your app reads its data with GORM and you want a gallery, an admin table or a report over files without a second data layer. Writes still go through the Store.

## Before you start

- A Store, as in [Files](../records/files.md), migrated with `Store.Migrate`.
- GORM opened on the same database, with your own dialector. The module imports GORM but no dialector, so it never forces a driver on you (`deps_test.go` enforces it). The tests use `gorm.io/driver/postgres`, `gorm.io/driver/mysql` and the pure-Go `github.com/ncruces/go-sqlite3/gormlite`, each over the `*sql.DB` the Store uses.
- Install: `go get github.com/ubgo/filesystem/contrib/gorm`.

## Read only, on purpose

Every write goes through `records.Store`: it runs plugin hooks (quota, versions, folders), emits the events the audit log and your webhooks hear, and keeps a stored object consistent with its row. Never `Create`, `Save`, `Update` or `Delete` these models, and never `AutoMigrate` them. The tables belong to `Store.Migrate` and its ledger, and AutoMigrate wants to alter their columns: on SQLite it recreates the table to do so. `DeletedAt` is a plain `*time.Time`, not `gorm.DeletedAt`, so a stray `Delete` can never turn into a soft delete the Store does not know about.

## Step 1 — Open GORM on the Store's database

Pass the same `*sql.DB` to the Store and to your dialector, so both see the same rows and transactions are not split across pools: `postgres.New(postgres.Config{Conn: db})`, `mysql.New(mysql.Config{Conn: db})`, or `gormlite.OpenDB(db)` for SQLite. The program [`examples/integrations-orm`](../../examples/integrations-orm/README.md) does it on SQLite:

<!-- ds:block id=opengorm-me6qbh5f -->
**openGorm** · [`examples/integrations-orm/gorm.go:21-23`](../../examples/integrations-orm/gorm.go#L21-L23)

```go
func openGorm(db *sql.DB) (*gorm.DB, error) {
	return gorm.Open(gormlite.OpenDB(db), &gorm.Config{Logger: logger.Discard})
}
```
<!-- /ds:block hash=847b46 -->

`logger.Discard` keeps GORM's SQL log quiet; keep your usual logger in an app.

A Store built with `records.WithTablePrefix` or `records.WithTableNames` (see [Migrations: Step 6 — Other table names](../records/migrations.md#step-6--other-table-names)) keeps its tables under other names. Open GORM with `fsgorm.NewNamer` as its naming strategy, giving it the Store's mapping, and the models, both preloads and `WithLiveFile` name those tables. Your own models keep the base strategy (`nil` is GORM's default), and the records tables never take its `TablePrefix`, as before:

```go
names := func(name string) string { return "app_" + name } // what records.WithTablePrefix("app_") does
store, err := records.New(db, dialect, disks, records.WithTableNames(names))
// ...
g, err := gorm.Open(gormlite.OpenDB(db), &gorm.Config{NamingStrategy: fsgorm.NewNamer(nil, names)})
```

GORM caches a model's schema per `*gorm.DB`, so two databases opened with different namers keep their own names.

## Step 2 — Query with the scopes

The scopes reproduce the Store's rules, and qualify their columns with the query's own table so they stay unambiguous next to joins:

| Scope | Does | Like |
|---|---|---|
| `InScope(scope)` | keeps one scope's rows; start every query with it, or a listing crosses tenants | every Store call |
| `Live` | keeps files not in the trash | `Store.Files` |
| `NewestFirst` | orders files newest first, ID breaking ties | `Store.Files` |
| `WithLiveFile` | keeps attachments whose file is not in the trash | `Store.Attachments` |
| `ByPosition` | orders attachments by position, then ID | `Store.Attachments` |

A post's gallery, the way `Store.Attachments` returns it, from the package's runnable example ([`contrib/gorm/example_test.go`](../../contrib/gorm/example_test.go), output checked by `go test`):

<!-- ds:block id=galleryof-8cd932hh -->
**galleryOf** · [`contrib/gorm/example_test.go:27-33`](../../contrib/gorm/example_test.go#L27-L33)

```go
func galleryOf(db *gorm.DB, scope, postID string) ([]fsgorm.Attachment, error) {
	var out []fsgorm.Attachment
	err := db.Scopes(fsgorm.InScope(scope), fsgorm.WithLiveFile, fsgorm.ByPosition).
		Where(&fsgorm.Attachment{OwnerType: "post", OwnerID: postID, Role: "gallery"}).
		Preload("File").Find(&out).Error
	return out, err
}
```
<!-- /ds:block hash=87dc1c -->

The example uploads three files to a post's gallery through the Store, trashes the middle one, and lists the gallery with GORM:

```text
1 cover.txt 9 bytes
3 map.txt 7 bytes
```

The trashed `draft.txt` (position 2) is left out, as the Store leaves it out. A file's attachments load with `Preload("Attachments")`, an attachment's file with `Preload("File")`.

## Step 3 — Hand a row back to the Store

`File.Record()` converts a row to a `records.File`, with its metadata decoded from JSON, so the Store can build a link to it (`Store.URL`, `Store.DownloadURL`). A row whose metadata is not a JSON object of strings is refused with `invalid_argument` rather than read as empty.

## Step 4 — Put it together

In [`examples/integrations-orm`](../../examples/integrations-orm/README.md) a Store uploads `cover.txt`, `draft.txt` and `map.txt` to post `p1`'s gallery and trashes `draft.txt`; GORM then lists the gallery, lists the live files newest first with their attachments preloaded, and converts each row with `Record`:

<!-- ds:block id=gormgallery-27wg6mmp -->
**gormGallery** · [`examples/integrations-orm/gorm.go:30-36`](../../examples/integrations-orm/gorm.go#L30-L36)

```go
func gormGallery(db *gorm.DB, scope, owner string) ([]fsgorm.Attachment, error) {
	var out []fsgorm.Attachment
	err := db.Scopes(fsgorm.InScope(scope), fsgorm.WithLiveFile, fsgorm.ByPosition).
		Where(&fsgorm.Attachment{OwnerType: ownerPost, OwnerID: owner, Role: roleGallery}).
		Preload("File").Find(&out).Error
	return out, err
}
```
<!-- /ds:block hash=045bec -->

<!-- ds:block id=readwithgorm-fgmhqteb -->
**readWithGorm** · [`examples/integrations-orm/gorm.go:50-74`](../../examples/integrations-orm/gorm.go#L50-L74)

```go
func readWithGorm(out io.Writer, db *sql.DB) error {
	g, err := openGorm(db)
	if err != nil {
		return err
	}
	gallery, err := gormGallery(g, tenant, postID)
	if err != nil {
		return err
	}
	for _, a := range gallery {
		fmt.Fprintf(out, "gorm gallery: %d %s %d bytes\n", a.Position, a.File.Filename, a.File.Size)
	}
	files, err := gormLiveFiles(g, tenant)
	if err != nil {
		return err
	}
	for _, f := range files {
		rec, err := f.Record()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "gorm live file: %s, %d attachment(s), records.File status %s\n", rec.Filename, len(f.Attachments), rec.Status)
	}
	return nil
}
```
<!-- /ds:block hash=97654f -->

From `go run ./integrations-orm` in `examples/` (the same program reads the tables with sqlc and ent too; these are its GORM lines, which `go test ./integrations-orm` pins):

```text
gorm gallery: 1 cover.txt 9 bytes
gorm gallery: 3 map.txt 7 bytes
gorm live file: map.txt, 1 attachment(s), records.File status ready
gorm live file: cover.txt, 1 attachment(s), records.File status ready
```

The trashed file is in neither list: `WithLiveFile` drops its attachment and `Live` drops the file, which is what `Store.Attachments` and `Store.Files` answer.

## How it is kept true

- `TestModelsMatchTables` compares every model field with the columns the Store's migrations create, as GORM's migrator reads them from the catalogue: a column the Store gains (the processing status, say) fails it until the model has the field.
- `TestReadsWhatTheStoreWrote` writes files and attachments through the Store, lists and preloads them with GORM, and compares each row with `Store.Files` and `Store.Attachments`.
- `TestNamerModelsMatchTables` and `TestNamerReadsWhatTheStoreWrote` do the same over a Store built with `WithTableNames` and GORM opened with `fsgorm.NewNamer`; `TestNamerDefaults` pins that any other naming strategy keeps the default names.
- They run on SQLite by default and on Postgres and MySQL with `task test:engines`.

## What can go wrong

- `ambiguous column name: scope` — a raw `Where("scope = ?")` in a query that joins both tables; use `InScope`, which qualifies the column.
- An attachment of a trashed file appears — the query lacks `WithLiveFile`.
- `no such table: fs_files` (or `relation "fs_files" does not exist`) — the Store was built with a table prefix or mapping and GORM was not; open it with `NamingStrategy: fsgorm.NewNamer(nil, names)`.
- Columns changed after `AutoMigrate` — restore them with the records migrations' definitions; the module's models are not a schema source.
- **Preloading `File` brings trashed files back.** `Preload("File")` loads the attachment's file whatever its trash state; keep `WithLiveFile` on the attachment query (Step 4), or preload with a condition, `Preload("File", fsgorm.Live)`.
- **`Count` and `Find` without `InScope`** cross tenants silently. There is no default scope on the models, on purpose; wrap your own reads in a helper that always applies it.
- Plugin tables (`fs_audit`, `fs_file_versions`, `fs_quota_usage`, …) have no model; read them through their plugins' Go API.

## Next

- [`examples/integrations-orm`](../../examples/integrations-orm/README.md): the runnable program behind Step 4.
- [sqlc](sqlc.md) and [ent](ent.md) read the same tables through those tools.
- [Attachments](../records/attachments.md): the Store side of what these models read.
- The module's [README](../../contrib/gorm/README.md) lists the models and scopes.
