# github.com/ubgo/filesystem/contrib/gorm

GORM models of the records tables (`fs_files`, `fs_attachments`) and scopes that make GORM queries behave like the Store's listings, so an application that reads its data with GORM can list, preload and join files. The package is `fsgorm`. The guide is [docs/integrations/gorm.md](../../docs/integrations/gorm.md).

> [!WARNING]
> Never call `AutoMigrate` on these models, and never `Create`, `Save`, `Update` or `Delete` them. The tables belong to `Store.Migrate`, and AutoMigrate alters their columns: on SQLite it recreates the whole table (found while writing this module; it even panics in dry-run mode). Writes go through `records.Store`, which runs plugin hooks, emits events and keeps stored objects consistent with their rows.

## Install

```sh
go get github.com/ubgo/filesystem/contrib/gorm
```

It imports GORM, the core and records, and no dialector or SQL driver (`deps_test.go` enforces it): open GORM with your own, on the `*sql.DB` the Store uses.

## Read only

Writes go through `records.Store`, which runs plugin hooks, emits events and keeps stored objects consistent with their rows. Never `Create`, `Save`, `Update` or `Delete` these models, and never `AutoMigrate` them: the tables belong to `Store.Migrate`, and AutoMigrate alters their columns (on SQLite by recreating the table).

## API

| Symbol | What it is |
|---|---|
| `File` | A row of `fs_files`; `Attachments` preloads its attachments. `DeletedAt` is `*time.Time`, never a GORM soft delete |
| `(File).Record() (records.File, error)` | The row as a `records.File` with metadata decoded, for `Store.URL`; malformed metadata is `CodeInvalidArgument` |
| `Attachment` | A row of `fs_attachments`; `File` preloads its file |
| `InScope(scope)` | Keeps one scope's rows; start every query with it |
| `Live` | Keeps files not in the trash |
| `NewestFirst` | Orders files as `Store.Files` does |
| `WithLiveFile` | Keeps attachments whose file is not in the trash |
| `ByPosition` | Orders attachments as `Store.Attachments` does |
| `NewNamer(base, names)`, `Namer` | A GORM naming strategy that names the records tables as a Store built with `records.WithTableNames` or `WithTablePrefix` does (`names`, nil: the defaults), and every other table as `base` does (nil: GORM's default); set it as `gorm.Config.NamingStrategy` |

Every scope qualifies its column with the query's own table, so it works beside joins.

## Tests

`TestModelsMatchTables` compares the model fields with the columns `Store.Migrate` creates; `TestNamerModelsMatchTables` and `TestNamerReadsWhatTheStoreWrote` repeat the checks over a prefixed Store with `NewNamer`; `TestReadsWhatTheStoreWrote` compares GORM's answers with the Store's over rows the Store wrote; `Example` is the gallery query of the guide. `task test:engines` runs them on Postgres and MySQL as well as SQLite.
