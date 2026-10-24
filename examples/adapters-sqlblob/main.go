// Command adapters-sqlblob walks through the SQL table driver: objects
// stored as rows of the application's own database, with metadata and
// headers kept beside the bytes, a ranged read that selects only the
// range, a compare-and-swap, a conditional write, the size cap, copy and
// move, a query of its own through the escape hatch, the schema for a
// migration tool, and signed links served by the application.
//
// It uses a SQLite file in a temporary directory, so it needs no server;
// the same code runs on Postgres and MySQL with their dialect.
//
//	go run ./adapters-sqlblob            (from the examples directory)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the application's own SQL driver

	"github.com/ubgo/sqlb"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/sqlblob"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/signedurl"
)

// maxAvatar is this application's own cap for one object: avatars only.
const maxAvatar = 1 << 20

// sqliteDSN opens the file in WAL mode with a busy timeout: readers no
// longer wait for the writer, and a second writer waits up to five seconds
// for the lock instead of failing at once with "database is locked".
const sqliteDSN = "file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

func main() {
	dir, err := os.MkdirTemp("", "adapters-sqlblob-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// openDB opens the application's SQLite file and reports the journal mode
// it ended up in, so the pragmas in the DSN are seen to apply.
// ds:def id=opendb-by52a2x6
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

// openDisk opens the table driver over the application's database, creates
// the table, and wraps it in a Disk. Migrate is safe on every start, so
// opening a second disk over the same table runs it again harmlessly.
// ds:def id=opendisk-zrrtfejx
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

// printSchema prints the statement Migrate runs, for an application that
// applies its own migrations instead (goose, atlas, a SQL file in the
// repo). New runs no SQL, so building a driver only to read its schema is
// free.
// ds:def id=printschema-e57cnyep
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

// storeAvatar writes one object with metadata and a stored header, then
// reads four bytes of it: only those four bytes leave the database.
// ds:def id=storeavatar-fcvty6sj
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

// conditionalWrites shows the guarded writes: a compare-and-swap on the
// ETag (an UPDATE ... WHERE sha256 = ?), a create-only write (a plain
// INSERT) and the size cap, which refuses the body before any SQL runs.
// ds:def id=conditionalwrites-7v2h6h4g
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

// copyAndList copies a row under a new key inside one transaction and lists
// the directory, paged by the database in key order.
// ds:def id=copyandlist-3skkpass
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

// usage asks the database a question the driver does not offer, through
// the escape hatch: how many rows and bytes are stored under users/. The
// range [users/, users0) is the one the driver itself uses for a
// directory, so it is served by the primary key's index.
// ds:def id=usage-hgxnn7q9
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

// moveToArchive renames a row in one transaction: no reader ever sees the
// object under both keys or under neither.
// ds:def id=movetoarchive-4z3vjndu
func moveToArchive(ctx context.Context, out io.Writer, disk *filesystem.Disk) error {
	moved, err := disk.Move(ctx, "users/42-backup.png", "archive/42.png")
	if err != nil {
		return err
	}
	_, err = disk.Stat(ctx, "users/42-backup.png")
	fmt.Fprintf(out, "moved to %s, old key: %s\n", moved.Key, filesystem.CodeOf(err))
	return nil
}

// signedLinks shows that a table cannot sign a URL by itself, then gives a
// disk over the same table a signer and serves the link with fshttp from
// the application's own HTTP server (here an httptest server).
// ds:def id=signedlinks-cqmrte9w
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

// fetch downloads url and prints what came back.
func fetch(out io.Writer, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "signed download: status %d, %q\n", resp.StatusCode, body)
	return nil
}

// run is main without the process: dir holds the SQLite file, and every
// step prints what it saw to out.
func run(ctx context.Context, out io.Writer, dir string) error {
	db, err := openDB(ctx, out, dir)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	disk, err := openDisk(ctx, db)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "capabilities: %s\n", disk.Capabilities())
	if err := printSchema(out, db); err != nil {
		return err
	}
	e, err := storeAvatar(ctx, out, disk)
	if err != nil {
		return err
	}
	steps := []func() error{
		func() error { return conditionalWrites(ctx, out, disk, e) },
		func() error { return copyAndList(ctx, out, disk) },
		func() error { return usage(ctx, out, disk) },
		func() error { return moveToArchive(ctx, out, disk) },
		func() error { return signedLinks(ctx, out, db, disk) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}
