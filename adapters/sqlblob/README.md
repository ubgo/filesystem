# adapters/sqlblob

`github.com/ubgo/filesystem/adapters/sqlblob` is the `filesystem.Driver` that stores objects as rows of one SQL table on SQLite, Postgres or MySQL, through `github.com/ubgo/sqlb` and the application's own `*sql.DB`. It is its own module and imports no SQL driver.

The guide is [docs/adapters/sqlblob.md](../../docs/adapters/sqlblob.md).

```sh
go get github.com/ubgo/filesystem/adapters/sqlblob
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `DB` | `*sql.DB` | required | The database holding the table; never closed by the driver |
| `Dialect` | `sqlb.Dialect` | required | `sqlb.SQLiteDialect{}`, `sqlb.PostgresDialect{}` or `sqlb.MySQLDialect{}` |
| `Table` | `string` | `DefaultTable` (`filesystem_blobs`) | A plain identifier |
| `Schema` | `string` | none (the connection's search path or database) | Qualifies `Table`, a plain identifier: a Postgres schema, a MySQL database, an attached SQLite database. `Migrate` creates the table there, not the schema |
| `Clock` | `func() time.Time` | `time.Now` | The time each written or copied row is stamped with (`Stat` and `List` `ModTime`) |
| `MaxObjectSize` | `int64` | `DefaultMaxObjectSize` (16 MiB) | The largest object a write stores; larger is `CodeTooLarge`. On MySQL keep it below `max_allowed_packet` (4 MB by default on 5.7) |

`New` returns `CodeInvalidArgument` without `DB` or `Dialect`, for a table or schema name that is not a plain identifier or a negative size, and `CodeUnsupported` for another dialect. It runs no SQL.

## Methods beyond the Driver

| Method | Does |
|---|---|
| `Migrate(ctx)` | Creates the table if it does not exist; safe on every start |
| `Schema()` | The `CREATE TABLE IF NOT EXISTS` statement, for an application's own migration tool |
| `Native()` | The `*sql.DB`, for `filesystem.As[*sql.DB]` |

## Capabilities

`Copy`, `Move`, `SetVisibility`, `DeletePrefix`, `DeleteKeys`, `Metadata`, `ContentType`, `Headers`, `ConditionalWrite`, `CompareAndSwap`. Never: URLs, multipart, versions, append.

## Behaviour

- One row per object: key (byte-ordered primary key), bytes, size, SHA-256 (the ETag, also `Entry.SHA256`), content type, visibility, metadata (JSON), the three stored headers, modification time in Unix nanoseconds.
- Writes read the body in full and store it in one upsert; a failing or oversized body stores nothing.
- Ranged reads select `substr(data, …)`, only the range.
- Recursive listings page in the database; non-recursive ones read every key's attributes under the directory.

## Tests

`go test ./...` runs the conformance suite on SQLite. With `FILESYSTEM_RECORDS_ENGINE=postgres` and `FILESYSTEM_RECORDS_PG_DSN`, or `mysql` and `FILESYSTEM_RECORDS_MYSQL_DSN`, it runs on that engine (`records/recordstest`, a test-only dependency). `deps_test.go` keeps production code to the standard library, the library and sqlb.
