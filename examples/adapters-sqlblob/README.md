# SQL table driver

The sqlblob driver stores objects as rows of the application's own database. This program opens a SQLite file in WAL mode with a busy timeout, creates an `avatars` table with `Migrate`, prints the statement `Schema()` returns for an application's own migration tool, stores an avatar with metadata and a `Cache-Control` header, reads a byte range (only the range leaves the database), swaps the object with a stale and then the current ETag, swaps a missing key, tries a create-only write, hits the size cap, copies and lists, counts the rows and bytes under a prefix through the `*sql.DB` escape hatch, moves a row, and finally serves a signed link with `signedurl` and `fshttp` (a table cannot sign one by itself). It uses the core `filesystem` package, `fshttp`, `signedurl`, `github.com/ubgo/filesystem/adapters/sqlblob` and the `modernc.org/sqlite` driver. The guide [SQL table (sqlblob)](../../docs/adapters/sqlblob.md) explains each step.

## Run it

```sh
cd examples
go run ./adapters-sqlblob
go test ./adapters-sqlblob
```

The SQLite file lives in a temporary directory (`adapters-sqlblob-*`) deleted when the program exits. On Postgres or MySQL only the `*sql.DB` and the dialect change.

## What it does

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

## The code

<!-- ds:block id=opendb-by52a2x6 -->
**openDB** · [`examples/adapters-sqlblob/main.go:61-73`](main.go#L61-L73)

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

<!-- ds:block id=opendisk-zrrtfejx -->
**openDisk** · [`examples/adapters-sqlblob/main.go:79-93`](main.go#L79-L93)

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

<!-- ds:block id=printschema-e57cnyep -->
**printSchema** · [`examples/adapters-sqlblob/main.go:100-113`](main.go#L100-L113)

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

<!-- ds:block id=storeavatar-fcvty6sj -->
**storeAvatar** · [`examples/adapters-sqlblob/main.go:118-140`](main.go#L118-L140)

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

<!-- ds:block id=conditionalwrites-7v2h6h4g -->
**conditionalWrites** · [`examples/adapters-sqlblob/main.go:146-163`](main.go#L146-L163)

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

<!-- ds:block id=copyandlist-3skkpass -->
**copyAndList** · [`examples/adapters-sqlblob/main.go:168-179`](main.go#L168-L179)

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

<!-- ds:block id=usage-hgxnn7q9 -->
**usage** · [`examples/adapters-sqlblob/main.go:186-204`](main.go#L186-L204)

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

<!-- ds:block id=movetoarchive-4z3vjndu -->
**moveToArchive** · [`examples/adapters-sqlblob/main.go:209-217`](main.go#L209-L217)

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

<!-- ds:block id=signedlinks-cqmrte9w -->
**signedLinks** · [`examples/adapters-sqlblob/main.go:223-248`](main.go#L223-L248)

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
