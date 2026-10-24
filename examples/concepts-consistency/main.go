// Command concepts-consistency shows how bytes and rows stay consistent
// when an object store and a database cannot share a transaction: object
// first and row second on the way in, row first and object last on the
// way out, rolled-back uploads deleting their objects, finding orphan
// objects safely, conditional writes, and a mirrored disk converging
// after a partial failure.
//
//	go run ./concepts-consistency          (from the examples directory)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Names of this demo.
const (
	// diskName is the disk the Store records files on.
	diskName = "media"
	// scope is the tenant every file belongs to.
	scope = "acme"
	// uploadDir is where the policy stores uploads.
	uploadDir = "uploads"
	// memoryDB is a private in-memory SQLite database; one connection keeps
	// every statement on the same database.
	memoryDB = "file::memory:?_pragma=foreign_keys(ON)"
	// pdf is a small file whose first bytes sniff recognises.
	pdf = "%PDF-1.7 consistency demo"
)

// policy accepts PDFs under uploadDir with random names.
var policy = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"application/pdf"}, Key: upload.RandomKey(uploadDir)}

// errAppFailed stands for the application's own statement failing after
// the upload, inside the same transaction.
var errAppFailed = errors.New("insert invoice row: constraint failed")

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// demo is the wiring every step uses.
type demo struct {
	db    *sql.DB
	disk  *filesystem.Disk
	store *records.Store
	// age shifts the memory driver's clock back, so a step can write an
	// object that looks as old as one a crash left days ago.
	age time.Duration
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	d, err := newDemo(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = d.db.Close() }()
	for _, step := range []func(context.Context, *demo, io.Writer) error{
		rolledBackUpload, objectFirst, purgeAfterCommit, findOrphans,
	} {
		if err := step(ctx, d, out); err != nil {
			return err
		}
	}
	if err := conditionalWrite(ctx, out); err != nil {
		return err
	}
	return mirrorConverges(ctx, out)
}

// newDemo opens SQLite, a memory disk and a Store, and migrates.
func newDemo(ctx context.Context) (*demo, error) {
	db, err := sql.Open("sqlite", memoryDB)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	d := &demo{db: db}
	clock := func() time.Time { return time.Now().Add(-d.age) }
	disk, err := filesystem.NewDisk(diskName, memory.New(memory.WithClock(clock)))
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	store, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if _, err := store.Migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	d.disk, d.store = disk, store
	return d, nil
}

// state reports how many objects the disk holds and how many rows the
// files table holds.
func (d *demo) state(ctx context.Context) (objects, rows int, err error) {
	for _, err := range d.disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return 0, 0, err
		}
		objects++
	}
	err = d.db.QueryRowContext(ctx, "SELECT count(*) FROM "+records.TableNameFiles).Scan(&rows)
	return objects, rows, err
}
