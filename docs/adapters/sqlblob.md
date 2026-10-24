# SQL table (sqlblob)

The sqlblob driver stores each object as a row of one table in the database the application already runs: SQLite, Postgres or MySQL. It suits small files that belong with the data they describe (avatars, signatures, generated PDFs, configuration exports) in a deployment that would rather back up one database than run an object store. It is not a home for video or large media: every write holds its body in memory, and the size cap is low by default.

## When to choose it

| You have | Choose | Why |
|---|---|---|
| Small files (under a few MiB), one database already backed up, no wish to run or pay for an object store | **sqlblob** | One backup holds the rows and the files; compare-and-swap, create-only writes and atomic moves come from the database |
| One server, files of any size, a disk you can back up | [local](local.md) | Streams bodies without holding them in memory; `rsync` and snapshots work; signs links through `signedurl` the same way |
| Several servers, large files, browsers uploading straight to storage, a CDN | [S3](s3.md), [GCS](gcs.md) or [Azure](azure.md) | Multipart and resumable uploads, presigned URLs signed by the backend, storage that grows without growing the database |
| Tests | [memory](memory.md), or sqlblob on a SQLite file in `t.TempDir()` | No server either way; sqlblob also exercises the real SQL |

Pick sqlblob when the files are small and few enough that the database stays comfortable to back up and restore. When they are not, [move to a bucket](../recipes/move-to-s3.md): the code above the Disk does not change.

## Before you start

- The package is `github.com/ubgo/filesystem/adapters/sqlblob`, a module of its own that requires only the library and `github.com/ubgo/sqlb`. It imports no SQL driver; the application opens its own `*sql.DB` with the driver it already uses (`modernc.org/sqlite`, `github.com/jackc/pgx/v5/stdlib`, `github.com/go-sql-driver/mysql`). Its reference is [adapters/sqlblob/README.md](../../adapters/sqlblob/README.md).
- The runnable program is [`examples/adapters-sqlblob`](../../examples/adapters-sqlblob/). It uses a SQLite file in a temporary directory, so it needs no server. Every code block below is copied from it, and the output under [Try it](#try-it) is from running it.

## Step 1 — Install

```sh
go get github.com/ubgo/filesystem github.com/ubgo/filesystem/adapters/sqlblob
```

## Step 2 — Open the database

The driver uses whatever pool it is given. On SQLite, open the file in WAL mode with a busy timeout: readers then stop waiting for the writer, and a second writer waits for the lock instead of failing at once with `database is locked`. With `modernc.org/sqlite` both are `_pragma` parameters of the DSN (`file:app.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`); the program reads the journal mode back to show they applied.

<!-- ds:block id=opendb-by52a2x6 -->
**openDB** · [`examples/adapters-sqlblob/main.go:61-73`](../../examples/adapters-sqlblob/main.go#L61-L73)

```go
func openDB(ctx context.Context, out io.Writer, dir string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf(sqliteDSN, filepath.Join(dir, "app.db")))
	if err != nil {
		return nil, err
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		_ = db.Close()
		return nil, err
	}
	fmt.Fprintf(out, "journal mode: %s\n", mode)
	return db, nil
}
```
<!-- /ds:block hash=e4afc4 -->

On Postgres and MySQL open the pool as usual; nothing in the driver changes but the dialect in Step 3.

## Step 3 — Open a disk

Give the driver the pool, the pool's sqlb dialect, a table name and the size cap, create the table once with `Migrate`, and wrap the driver in a Disk. `New` runs no SQL; `Migrate` runs `CREATE TABLE IF NOT EXISTS`, so it is safe on every start (the program opens a second disk over the same table in Step 9 and runs it again).

<!-- ds:block id=opendisk-zrrtfejx -->
**openDisk** · [`examples/adapters-sqlblob/main.go:79-93`](../../examples/adapters-sqlblob/main.go#L79-L93)

```go
func openDisk(ctx context.Context, db *sql.DB, opts ...filesystem.Option) (*filesystem.Disk, error) {
	drv, err := sqlblob.New(sqlblob.Config{
		DB:            db,
		Dialect:       sqlb.SQLiteDialect{},
		Table:         "avatars",
		MaxObjectSize: maxAvatar,
	})
	if err != nil {
		return nil, err
	}
	if err := drv.Migrate(ctx); err != nil {
		return nil, err
	}
	return filesystem.NewDisk("avatars", drv, opts...)
}
```
<!-- /ds:block hash=93d861 -->

The dialect is `sqlb.SQLiteDialect{}`, `sqlb.PostgresDialect{}` or `sqlb.MySQLDialect{}`; any other is refused with `CodeUnsupported`. A missing `DB` or `Dialect`, a negative `MaxObjectSize` or a table name that is not a plain identifier (letters, digits and underscores, at most 63, not starting with a digit) is `CodeInvalidArgument`, so a name read from configuration can never carry SQL. An empty `Table` means `DefaultTable`, `filesystem_blobs`. Two drivers over two tables of one database are two independent disks: one table for avatars with a 1 MiB cap and another for exports with a larger one is a normal setup.

Two more fields fit the table into a database you already run. `Schema` puts it in a schema other than the connection's default (a Postgres schema, a MySQL database, an attached SQLite database), checked like `Table`; `Migrate` creates the table there but not the schema, which your migrations own. `Clock` stamps each written or copied row instead of the wall clock, for deterministic tests or several application servers whose clocks disagree:

```go
drv, err := sqlblob.New(sqlblob.Config{
	DB: db, Dialect: sqlb.PostgresDialect{},
	Schema: "storage", Table: "avatars", // "storage"."avatars"
	Clock:  dbClock,                     // func() time.Time, for example the database's now()
})
```

The column names are fixed: the driver relies on every column's type and on the key's byte-wise collation for `List` order and directory ranges, so it does not map onto an existing table of a different shape.

## Step 4 — Or run the schema yourself

An application that applies its own migrations (goose, atlas, a SQL file in the repository) runs the statement `Schema()` returns instead of calling `Migrate`. Building a driver to read it is free, since `New` touches nothing.

<!-- ds:block id=printschema-e57cnyep -->
**printSchema** · [`examples/adapters-sqlblob/main.go:100-113`](../../examples/adapters-sqlblob/main.go#L100-L113)

```go
func printSchema(out io.Writer, db *sql.DB) error {
	drv, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: sqlb.SQLiteDialect{}, Table: "avatars"})
	if err != nil {
		return err
	}
	stmts, err := drv.Schema()
	if err != nil {
		return err
	}
	for _, s := range stmts {
		fmt.Fprintf(out, "schema: %s\n", strings.Join(strings.Fields(s), " "))
	}
	return nil
}
```
<!-- /ds:block hash=f2b753 -->

What each column holds:

| Column | Holds |
|---|---|
| `key` | The object's key, the primary key. Compared byte by byte on every engine (`TEXT COLLATE "C"` on Postgres, `VARBINARY(1024)` on MySQL, `TEXT` on SQLite), so listings come back in the order the library promises |
| `data` | The bytes (`BYTEA`, `LONGBLOB`, `BLOB`); an empty object is an empty value, never `NULL` |
| `size`, `sha256` | The length and the SHA-256, which is also the ETag |
| `content_type`, `visibility`, `metadata` | As written; metadata is a JSON object |
| `cache_control`, `content_disposition`, `content_encoding` | The stored HTTP headers |
| `modified_ns` | The modification time in Unix nanoseconds, identical on every engine |

The primary key is the only index. A key is at most 1024 bytes (`filesystem.MaxKeyLength`, checked by the Disk), which is exactly what the MySQL column holds.

## Step 5 — Write and read

A write reads the body in full, at most `MaxObjectSize` bytes (`DefaultMaxObjectSize`, 16 MiB, when zero), and stores it in one statement: each engine's own upsert. Readers see the old object or the new one. The Disk sniffs the content type from the bytes (`image/png` below), and metadata and the three stored headers live in the same row. A ranged read selects `substr(data, offset+1, length)`, so only the range leaves the database; a range past the end reads nothing.

<!-- ds:block id=storeavatar-fcvty6sj -->
**storeAvatar** · [`examples/adapters-sqlblob/main.go:118-140`](../../examples/adapters-sqlblob/main.go#L118-L140)

```go
func storeAvatar(ctx context.Context, out io.Writer, disk *filesystem.Disk) (filesystem.Entry, error) {
	e, err := disk.Write(ctx, "users/42.png", strings.NewReader("\x89PNG\r\n\x1a\nfake-image-bytes"), filesystem.WriteOptions{
		Metadata: map[string]string{"uploaded-by": "42"},
		Headers:  filesystem.HTTPHeaders{CacheControl: "public, max-age=86400"},
	})
	if err != nil {
		return filesystem.Entry{}, err
	}
	fmt.Fprintf(out, "stored: %s %d bytes %s metadata=%v cache=%q\n", e.Key, e.Size, e.ContentType, e.Metadata, e.Headers.CacheControl)
	fmt.Fprintf(out, "etag is the sha256: %t\n", e.ETag == e.SHA256)

	rc, err := disk.ReadRange(ctx, "users/42.png", filesystem.ByteRange{Offset: 8, Length: 4})
	if err != nil {
		return filesystem.Entry{}, err
	}
	part, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return filesystem.Entry{}, err
	}
	fmt.Fprintf(out, "range 8+4: %q\n", part)
	return e, nil
}
```
<!-- /ds:block hash=b5f704 -->

The ETag is the SHA-256 of the bytes, so it is also `Entry.SHA256`, and a client can check a download against it.

## Step 6 — Conditional writes and the size cap

`IfMatch` is a compare-and-swap, one `UPDATE ... WHERE key = ? AND sha256 = ?`: a stale ETag changes nothing and is `CodeConflict`, and so is an ETag for a key that does not exist. `IfNotExists` is a plain `INSERT`, and a key that is taken is `CodeExists` (the driver asks again after a failed insert rather than parsing each engine's duplicate-key error, so it imports no SQL driver). A body over the cap is `CodeTooLarge`, refused before any statement runs, and nothing is stored.

<!-- ds:block id=conditionalwrites-7v2h6h4g -->
**conditionalWrites** · [`examples/adapters-sqlblob/main.go:146-163`](../../examples/adapters-sqlblob/main.go#L146-L163)

```go
func conditionalWrites(ctx context.Context, out io.Writer, disk *filesystem.Disk, e filesystem.Entry) error {
	_, err := disk.Write(ctx, "users/42.png", strings.NewReader("new"), filesystem.WriteOptions{IfMatch: "stale-etag"})
	fmt.Fprintf(out, "swap with a stale etag: %s\n", filesystem.CodeOf(err))
	if _, err := disk.Write(ctx, "users/42.png", strings.NewReader("new"), filesystem.WriteOptions{IfMatch: e.ETag}); err != nil {
		return err
	}
	fmt.Fprintln(out, "swap with the current etag: ok")

	_, err = disk.Write(ctx, "users/gone.png", strings.NewReader("x"), filesystem.WriteOptions{IfMatch: e.ETag})
	fmt.Fprintf(out, "swap on a missing key: %s\n", filesystem.CodeOf(err))

	_, err = disk.Write(ctx, "users/42.png", strings.NewReader("x"), filesystem.WriteOptions{IfNotExists: true})
	fmt.Fprintf(out, "create-only over an existing key: %s\n", filesystem.CodeOf(err))

	_, err = disk.Write(ctx, "users/huge.png", strings.NewReader(strings.Repeat("x", maxAvatar+1)), filesystem.WriteOptions{})
	fmt.Fprintf(out, "over MaxObjectSize: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=8630df -->

## Step 7 — Copy, list and query the table yourself

`Copy` reads the source row and writes it under the new key in one transaction, with a new modification time. A recursive listing is paged by the database (`key > cursor ORDER BY key LIMIT n`), one indexed query per page, and comes back in byte order. A non-recursive listing reads the attributes (never the bytes) of every key under the directory and folds sub-directories, O(n) in the keys under it.

<!-- ds:block id=copyandlist-3skkpass -->
**copyAndList** · [`examples/adapters-sqlblob/main.go:168-179`](../../examples/adapters-sqlblob/main.go#L168-L179)

```go
func copyAndList(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	if _, err := disk.Copy(ctx, "users/42.png", "users/42-backup.png"); err != nil {
		return err
	}
	for entry, err := range disk.List(ctx, "users", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "listed: %s\n", entry.Key)
	}
	return nil
}
```
<!-- /ds:block hash=721ae0 -->

`filesystem.As[*sql.DB](disk)` returns the pool, for a question the driver does not answer, such as how many bytes a prefix holds. The range `[users/, users0)` is the one the driver itself uses for a directory (`0` is the byte after `/`), so it is served by the primary key's index and needs no `LIKE` escaping.

<!-- ds:block id=usage-hgxnn7q9 -->
**usage** · [`examples/adapters-sqlblob/main.go:186-204`](../../examples/adapters-sqlblob/main.go#L186-L204)

```go
func usage(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	db, ok := filesystem.As[*sql.DB](disk)
	if !ok {
		return fmt.Errorf("disk %s has no *sql.DB", disk.Name())
	}
	var rows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM avatars").Scan(&rows); err != nil {
		return err
	}
	fmt.Fprintf(out, "rows in the avatars table: %d\n", rows)

	var n, total int64
	q := `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM avatars WHERE key >= ? AND key < ?`
	if err := db.QueryRowContext(ctx, q, "users/", "users0").Scan(&n, &total); err != nil {
		return err
	}
	fmt.Fprintf(out, "under users/: %d objects, %d bytes\n", n, total)
	return nil
}
```
<!-- /ds:block hash=8c652c -->

## Step 8 — Move

`Move` deletes any row at the destination and renames the source row in one transaction, so no reader sees the object under both keys or under neither. Moving a key onto itself keeps the object.

<!-- ds:block id=movetoarchive-4z3vjndu -->
**moveToArchive** · [`examples/adapters-sqlblob/main.go:209-217`](../../examples/adapters-sqlblob/main.go#L209-L217)

```go
func moveToArchive(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	moved, err := disk.Move(ctx, "users/42-backup.png", "archive/42.png")
	if err != nil {
		return err
	}
	_, err = disk.Stat(ctx, "users/42-backup.png")
	fmt.Fprintf(out, "moved to %s, old key: %s\n", moved.Key, filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=0ce264 -->

`SetVisibility` is one `UPDATE`, `DeletePrefix` one `DELETE` over the key range (the whole table for the root), and `DeleteKeys` deletes 500 keys per statement, well under SQLite's bound-parameter limit.

## Step 9 — Signed links

A table cannot sign a URL, so `SignedURL` on a plain sqlblob disk is `CodeUnsupported`, and the driver has no public URLs either. Give the disk a `URLSigner` (`signedurl.New`) and serve the links from the application with `fshttp.Signed`: the same pattern as local disk, described in [Serving files](../concepts/serving-files.md). The handler reads through the Disk, so a download with a `Range` header reads only that range from the table.

<!-- ds:block id=signedlinks-cqmrte9w -->
**signedLinks** · [`examples/adapters-sqlblob/main.go:223-248`](../../examples/adapters-sqlblob/main.go#L223-L248)

```go
func signedLinks(ctx context.Context, out io.Writer, db *sql.DB, plain *filesystem.Disk) error {
	_, err := plain.SignedURL(ctx, "users/42.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	fmt.Fprintf(out, "signed URL without a signer: %s\n", filesystem.CodeOf(err))

	srv := httptest.NewServer(nil)
	defer srv.Close()
	signer, err := signedurl.New(srv.URL+"/files", []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}})
	if err != nil {
		return err
	}
	disk, err := openDisk(ctx, db, filesystem.WithURLSigner(signer))
	if err != nil {
		return err
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	srv.Config.Handler = http.StripPrefix("/files", fshttp.Signed(mgr, signer))

	link, err := disk.SignedURL(ctx, "users/42.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		return err
	}
	return fetch(out, link.URL)
}
```
<!-- /ds:block hash=78af4f -->

For public files, set `filesystem.WithPublicURL(base)` and mount `fshttp.Public(disk)` there.

## Capabilities

Every operation is one or two statements, so the driver declares `Copy`, `Move`, `SetVisibility`, `DeletePrefix`, `DeleteKeys`, `Metadata`, `ContentType`, `Headers`, `ConditionalWrite` and `CompareAndSwap`. It has no signed or public URLs of its own (Step 9), no multipart uploads, no appends and no versions. See [Capabilities](../concepts/capabilities.md) for what the Disk does when a capability is missing.

## Try it

```sh
cd examples
go run ./adapters-sqlblob
```

```text
journal mode: wal
capabilities: [copy,move,set-visibility,delete-prefix,delete-keys,metadata,content-type,headers,conditional-write,compare-and-swap]
schema: CREATE TABLE IF NOT EXISTS "avatars" ("key" TEXT NOT NULL, "data" BLOB NOT NULL, "size" INTEGER NOT NULL, "sha256" TEXT NOT NULL, "content_type" TEXT NOT NULL, "visibility" TEXT NOT NULL, "metadata" TEXT NOT NULL, "cache_control" TEXT NOT NULL, "content_disposition" TEXT NOT NULL, "content_encoding" TEXT NOT NULL, "modified_ns" INTEGER NOT NULL, PRIMARY KEY ("key"))
stored: users/42.png 24 bytes image/png metadata=map[uploaded-by:42] cache="public, max-age=86400"
etag is the sha256: true
range 8+4: "fake"
swap with a stale etag: conflict
swap with the current etag: ok
swap on a missing key: conflict
create-only over an existing key: exists
over MaxObjectSize: too_large
listed: users/42-backup.png
listed: users/42.png
rows in the avatars table: 2
under users/: 2 objects, 6 bytes
moved to archive/42.png, old key: not_found
signed URL without a signer: unsupported
signed download: status 200, "new"
```

`go test ./adapters-sqlblob` runs the same program and checks every line above. The 6 bytes under `users/` are the two copies of `new`, the body the compare-and-swap wrote.

## Pitfalls

- **The database grows with every file, and so do its backups.** A 10 GB table makes every dump, restore and replica 10 GB larger, and slows the backups of rows that have nothing to do with files. Deleted rows do not shrink the file by themselves: SQLite keeps the free pages until `VACUUM`, and Postgres reuses the space only after (auto)vacuum. Watch the table's size with a query like Step 7's, and move to a bucket before restores get slow.
- **Row size limits differ per engine.** The driver's cap is `MaxObjectSize`, but the engine has its own: MySQL refuses a statement larger than `max_allowed_packet`, whose default is 4 MB on 5.7 (64 MB on 8.0), so the default 16 MiB cap fails on a stock 5.7 server; raise one or lower the other. Postgres caps a `bytea` value at 1 GB and SQLite a blob at 1,000,000,000 bytes by default, both far above any sensible cap here. Set `MaxObjectSize` to what the application really stores, as the example does with 1 MiB.
- **Memory per operation.** A write holds the whole body in memory before the statement runs. A full read selects the whole value and holds it too (only a ranged read holds just the range), and `Copy` passes the bytes through the application's process. Twenty concurrent 16 MiB uploads are 320 MiB of heap; keep the cap low, refuse oversized requests early with the [upload handler's size limits](../concepts/uploads.md), and bound how many uploads your server accepts at once.
- **The driver takes a pool, not your transaction.** Each write is its own statement and `Copy` and `Move` open their own transactions, so writing an object cannot join the transaction that inserts your business row. Use the [records](../records/README.md) module for files as rows, which orders the two writes so a failure never leaves a row pointing at a missing object (see [Consistency](../concepts/consistency.md)).
- **SQLite has one writer.** Every write, `Copy` and `Move` takes the database's write lock; without a busy timeout a concurrent writer fails at once with `database is locked`. Open the file as in Step 2, and keep the pool small (`db.SetMaxOpenConns`) if writes queue for a long time.
- **`Migrate` only creates.** It runs `CREATE TABLE IF NOT EXISTS` and never alters an existing table, so a table of the same name created earlier with other columns is left as it is, and the first write fails. Give the table its own name, and if migrations are yours, run `Schema()`'s statement as one of them (Step 4) rather than also calling `Migrate`.
- **Compare-and-swap compares content, not versions.** The ETag is the SHA-256 of the bytes: if another writer stores the same bytes with new metadata or headers between your read and your swap, the ETag is unchanged and your swap still succeeds, overwriting their metadata. Guard metadata-only changes some other way, such as a version column in a table of your own. A swap on a missing key is `CodeConflict`, not `CodeNotFound`. On MySQL, which does not count an update that changed nothing, the driver re-reads the row and treats an unchanged one as a success.
- **No signed or public URLs without a signer.** `SignedURL` is `CodeUnsupported` until the disk has `WithURLSigner`, and the links then point at your own server, which must mount `fshttp.Signed` (Step 9). Browsers cannot upload straight to a table the way they upload to a bucket; a signed PUT link goes through your server too.
- **Hand-edited rows are reported, not repaired.** A `metadata` value that is not a JSON object makes `Stat` and `List` fail for that key instead of serving it without its metadata. Edit rows through the Disk, or keep the column valid JSON.

## Testing on every engine

The module's tests run the conformance suite on SQLite by default, and on Postgres or MySQL with the records module's environment (`FILESYSTEM_RECORDS_ENGINE=postgres` with `FILESYSTEM_RECORDS_PG_DSN`, or `mysql` with `FILESYSTEM_RECORDS_MYSQL_DSN`), through `records/recordstest`. A fault-injection test fails every database step of every operation in turn and checks that each one returns the error rather than reporting success.

## Related

- [adapters/sqlblob/README.md](../../adapters/sqlblob/README.md): the configuration and method reference.
- [Comparison of every backend](README.md).
- [Serving files](../concepts/serving-files.md): `signedurl`, `fshttp.Signed` and `fshttp.Public`.
- [Capabilities](../concepts/capabilities.md) and [Errors](../concepts/errors.md): what `unsupported`, `conflict`, `exists` and `too_large` mean everywhere.
- [Records](../records/README.md) and [Consistency](../concepts/consistency.md): files as rows of your own schema.
- [Avatars](../recipes/avatars.md): a full avatar flow on any disk.
- [Move a local store to S3](../recipes/move-to-s3.md): when the table outgrows the database.
