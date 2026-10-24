# github.com/ubgo/filesystem/contrib/sqlc

The records tables (`fs_files`, `fs_attachments`) and their read queries for [sqlc](https://sqlc.dev), so an application that generates its data layer with sqlc reads files with typed, generated Go. The guide is [docs/integrations/sqlc.md](../../docs/integrations/sqlc.md).

## Install

```sh
go get github.com/ubgo/filesystem/contrib/sqlc
```

It imports only the standard library and the core module (`deps_test.go` enforces it), or copy the `.sql` and `.yaml` files: they are the whole product.

## Files

| File | What it holds |
|---|---|
| `queries.sql` | Read queries shared by every engine: `GetFile`, `ListFilesBySHA256`, `ScopeTotals`, `ListOwnerAttachments`, `ListFileAttachments` |
| `<engine>/pages.sql` | The paginated listings `ListLiveFiles` and `ListLiveFilesAfter`, per engine because sqlc's MySQL parser takes only a bare `LIMIT ?` |
| `postgres/schema.sql`, `mysql/schema.sql`, `sqlite/schema.sql` | The tables exactly as `records.Store.Migrate` leaves them on that engine, for sqlc's type checking |
| `<engine>/sqlc.yaml` | A sqlc configuration that generates package `fsdb` from that schema, `../queries.sql` and `pages.sql` |

## Read only

Writes go through `records.Store`, which runs plugin hooks, emits events and keeps stored objects consistent with their rows. Never create the tables from `schema.sql` (use `Store.Migrate`), and never write `fs_` rows with generated queries.

## Go API

| Symbol | What it does |
|---|---|
| `FS() fs.FS` | Every file, in the layout `sqlc.yaml` expects: write it out and run `sqlc generate` |
| `Schema(engine) (string, error)` | One engine's `schema.sql`; an unknown engine is `CodeInvalidArgument` |
| `Queries(engine) (string, error)` | Every query of an engine: `queries.sql` then its `pages.sql`; an unknown engine is `CodeInvalidArgument` |
| `FSWith(names)`, `SchemaWith(engine, names)`, `QueriesWith(engine, names)`, `MapNames(sql, names)` | The same files with every records table and index name mapped by a Store's `records.TableNamer`, for a Store built with `records.WithTablePrefix` or `WithTableNames`; nil names returns them unchanged |
| `EnginePostgres`, `EngineMySQL`, `EngineSQLite` | The engine directory names, equal to `records.EnginePostgres` and friends |
| `QueriesFile`, `SchemaFile`, `PagesFile`, `ConfigFile` | File names inside `FS` |

## Tests

`TestSchemaMatchesStore` compares each `schema.sql` with the tables the Store creates (columns, types, nullability, defaults, indexes) on the engine under test; `TestQueries` runs every query over rows the Store wrote and checks the answers against the Store's; `TestSchemaWithMatchesPrefixedStore` and `TestQueriesWithPrefixedStore` repeat both against a prefixed Store; `TestSqlcGenerate` runs the pinned sqlc (`go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`) for every engine, from `FS()` and from `FSWith` with a prefix, and builds the generated Go; it skips only when go cannot download sqlc, and says so. `task test:engines` runs the first two on Postgres and MySQL as well as SQLite.
