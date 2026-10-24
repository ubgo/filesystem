// Command records-lifecycle shows the end of a file's life: the trash
// (Trash, Restore, listing trashed files), deleting for good (Purge, with
// its object deleted only after the commit), a rollback that keeps both
// row and object, deleting everything an owner leaves behind
// (PurgeOwner) in the transaction that deletes the owner's own row, a
// trash retention sweep (SweepTrash), and where object deletion failures
// go (WithObjectErrorHandler).
//
//	go run ./records-lifecycle
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the disk files are stored on.
	diskName = "media"
	// archiveName is a read-only view of the same objects: deleting from
	// it always fails, which is how this demo shows the error handler.
	archiveName = "archive"
	// tenant is the scope of every file.
	tenant = "acme"
	// ownerUser is the application table files attach to.
	ownerUser = "user"
	// roleUpload is the role of a user's uploaded files.
	roleUpload = "upload"
	// trashRetention is how long a trashed file stays restorable.
	trashRetention = 30 * 24 * time.Hour
)

// users is the application's own table, created beside the fs_ tables
// with the same builder the library's tables use (records.DDL).
var (
	users     = sqlb.Table{Name: "users"}
	userID    = sqlb.Col[string](users, "id")
	userEmail = sqlb.Col[string](users, "email")
)

// usersDDL is the statements creating the users table on engine.
func usersDDL(engine string) ([]string, error) {
	t := records.TypesFor(engine)
	return records.NewDDL(engine).CreateTable(records.Table{
		Table:      users,
		Columns:    []ddl.ColumnSpec{records.Column(userID, t.Key), records.Column(userEmail, t.Text)},
		PrimaryKey: []ddl.ColumnRef{userID},
	}).Statements()
}

// errAbort is the error the rollback demo returns from its transaction.
var errAbort = errors.New("request failed after the purge")

// openSQLite opens a SQLite database file with one connection (SQLite has
// one writer) and a busy timeout.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newDisks builds two disks over one memory driver: media, which can
// delete, and archive, which refuses every write (filesystem.ReadOnly).
// ds:def id=newdisks-hvtukymk
func newDisks() (*filesystem.Manager, error) {
	drv := memory.New()
	media, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, err
	}
	archive, err := filesystem.NewDisk(archiveName, drv, filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(media, archive)
}

// Clock is a settable clock, so the demo can age the trash.
type Clock struct{ now time.Time }

// Now returns the clock's time.
func (c *Clock) Now() time.Time { return c.now }

// newStore wires a Store with a test clock and an object error handler.
// The handler receives object deletions that failed after a commit, when
// no caller is left to return them to: log them or queue a retry.
// ds:def id=newstore-8hms7sx8
func newStore(ctx context.Context, db *sql.DB, clock *Clock, failed *[]string) (*records.Store, error) {
	disks, err := newDisks()
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser),
		records.WithClock(clock.Now),
		records.WithObjectErrorHandler(func(_ context.Context, f records.File, err error) {
			*failed = append(*failed, f.Disk+"/"+f.Key+": "+string(filesystem.CodeOf(err)))
		}),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	st, err := usersDDL(records.EngineSQLite)
	for _, q := range st {
		if err == nil {
			_, err = db.ExecContext(ctx, q)
		}
	}
	return s, err
}

// store writes an object and records it on the named disk.
func store(ctx context.Context, s *records.Store, disk, key string) (records.File, error) {
	media, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := media.Write(ctx, key, bytes.NewReader([]byte(key)), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: disk, Key: key})
}

// trashCounts returns how many live and trashed files the tenant has.
// ds:def id=trashcounts-q9ep7g6t
func trashCounts(ctx context.Context, s *records.Store) (live, trashed int, err error) {
	page, err := s.Files(ctx, records.FileQuery{Scope: tenant})
	if err != nil {
		return 0, 0, err
	}
	bin, err := s.Files(ctx, records.FileQuery{Scope: tenant, Trashed: true})
	if err != nil {
		return 0, 0, err
	}
	return len(page.Files), len(bin.Files), nil
}

// objectExists reports whether a file's object is still on its disk.
func objectExists(ctx context.Context, s *records.Store, f records.File) (bool, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return false, err
	}
	return disk.Exists(ctx, f.Key)
}

// purgeThenFail purges a file inside a transaction that then fails. The
// rollback restores the row, and the object, which Purge deletes only
// after a commit, was never touched.
// ds:def id=purgethenfail-b6b94u7z
func purgeThenFail(ctx context.Context, s *records.Store, f records.File) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := s.Purge(ctx, tenant, f.ID); err != nil {
			return err
		}
		return errAbort
	})
}

// deleteUser deletes the application's user row and everything only that
// user owned, in one transaction: both happen or neither. tx is the same
// transaction the Store's calls join.
// ds:def id=deleteuser-mk6djv64
func deleteUser(ctx context.Context, s *records.Store, userID string) (int, error) {
	purged := 0
	err := s.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID); err != nil {
			return err
		}
		n, err := s.PurgeOwner(ctx, tenant, records.Owner{Type: ownerUser, ID: userID})
		purged = n
		return err
	})
	return purged, err
}

// sweep purges files trashed longer ago than trashRetention. Schedule it
// (cron, a ticker): the library starts no background work.
// ds:def id=sweep-q4hu6fkd
func sweep(ctx context.Context, s *records.Store, now time.Time) (int, error) {
	return s.SweepTrash(ctx, now.Add(-trashRetention))
}

func main() {
	dir, err := os.MkdirTemp("", "records-lifecycle")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}

// run wires the Store and runs every demo.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	clock := &Clock{now: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	var failed []string
	s, err := newStore(ctx, db, clock, &failed)
	if err != nil {
		return err
	}
	for _, demo := range []func(context.Context, io.Writer, *records.Store, *Clock) error{trashDemo, purgeDemo, ownerDemo, sweepDemo} {
		if err := demo(ctx, w, s, clock); err != nil {
			return err
		}
	}
	if err := objectErrorDemo(ctx, w, s, &failed); err != nil {
		return err
	}
	if err := retentionDemo(ctx, w, db, clock); err != nil {
		return err
	}
	return planDemo(ctx, w, dir)
}

// trashDemo trashes and restores a file.
// ds:def id=trashdemo-jwggkmyz
func trashDemo(ctx context.Context, w io.Writer, s *records.Store, _ *Clock) error {
	f, err := store(ctx, s, diskName, "trash/a.txt")
	if err != nil {
		return err
	}
	if err := s.Trash(ctx, tenant, f.ID); err != nil {
		return err
	}
	live, trashed, err := trashCounts(ctx, s)
	fmt.Fprintf(w, "after Trash: live=%d trashed=%d %v\n", live, trashed, err)
	if err := s.Restore(ctx, tenant, f.ID); err != nil {
		return err
	}
	live, trashed, err = trashCounts(ctx, s)
	fmt.Fprintf(w, "after Restore: live=%d trashed=%d %v\n", live, trashed, err)
	return s.Purge(ctx, tenant, f.ID)
}

// purgeDemo purges inside a failing transaction, then for real.
func purgeDemo(ctx context.Context, w io.Writer, s *records.Store, _ *Clock) error {
	f, err := store(ctx, s, diskName, "purge/b.txt")
	if err != nil {
		return err
	}
	err = purgeThenFail(ctx, s, f)
	_, rowErr := s.File(ctx, tenant, f.ID)
	kept, _ := objectExists(ctx, s, f)
	fmt.Fprintf(w, "rolled-back purge: err=%v row kept=%t object kept=%t\n", err, rowErr == nil, kept)
	if err := s.Purge(ctx, tenant, f.ID); err != nil {
		return err
	}
	_, rowErr = s.File(ctx, tenant, f.ID)
	kept, _ = objectExists(ctx, s, f)
	fmt.Fprintf(w, "purged: row=%s object kept=%t\n", filesystem.CodeOf(rowErr), kept)
	return nil
}

// ownerDemo deletes a user who shares one file with another user.
func ownerDemo(ctx context.Context, w io.Writer, s *records.Store, _ *Clock) error {
	if _, err := s.Conn(ctx).ExecContext(ctx, "INSERT INTO users (id, email) VALUES ('usr_1', 'a@example.com')"); err != nil {
		return err
	}
	alice, bob := records.Owner{Type: ownerUser, ID: "usr_1"}, records.Owner{Type: ownerUser, ID: "usr_2"}
	own, err := store(ctx, s, diskName, "owner/own.txt")
	if err != nil {
		return err
	}
	shared, err := store(ctx, s, diskName, "owner/shared.txt")
	if err != nil {
		return err
	}
	for _, at := range []struct {
		f records.File
		o records.Owner
	}{{own, alice}, {shared, alice}, {shared, bob}} {
		if _, err := s.Attach(ctx, tenant, at.f.ID, at.o, roleUpload); err != nil {
			return err
		}
	}
	n, err := deleteUser(ctx, s, alice.ID)
	left, _ := s.Attachments(ctx, tenant, bob, roleUpload)
	fmt.Fprintf(w, "PurgeOwner: purged=%d err=%v bob still has %d file(s)\n", n, err, len(left))
	return nil
}

// sweepDemo trashes two files 40 and 10 days before the sweep.
func sweepDemo(ctx context.Context, w io.Writer, s *records.Store, clock *Clock) error {
	for _, key := range []string{"sweep/old.txt", "sweep/new.txt"} {
		f, err := store(ctx, s, diskName, key)
		if err != nil {
			return err
		}
		if err := s.Trash(ctx, tenant, f.ID); err != nil {
			return err
		}
		clock.now = clock.now.Add(30 * 24 * time.Hour)
	}
	clock.now = clock.now.Add(-20 * 24 * time.Hour)
	n, err := sweep(ctx, s, clock.now)
	_, trashed, _ := trashCounts(ctx, s)
	fmt.Fprintf(w, "SweepTrash: purged=%d err=%v still in trash=%d\n", n, err, trashed)
	return nil
}

// objectErrorDemo purges a file whose disk refuses deletes, outside and
// inside a transaction.
// ds:def id=objecterrordemo-z4zrdxnd
func objectErrorDemo(ctx context.Context, w io.Writer, s *records.Store, failed *[]string) error {
	a, err := store(ctx, s, archiveName, "archive/a.txt")
	if err != nil {
		return err
	}
	err = s.Purge(ctx, tenant, a.ID)
	fmt.Fprintf(w, "Purge outside a Tx returns the object error: %s\n", filesystem.CodeOf(err))
	b, err := store(ctx, s, archiveName, "archive/b.txt")
	if err != nil {
		return err
	}
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return s.Purge(ctx, tenant, b.ID) })
	fmt.Fprintf(w, "Purge inside a Tx: err=%v handler got %q\n", err, *failed)
	return nil
}
