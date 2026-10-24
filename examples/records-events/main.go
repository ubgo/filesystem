// Command records-events shows how an application takes part in what the
// records Store does, by installing its own plugins: one that is told of
// every change (EventHooks) and keeps a changelog table of its own
// (Migrator), with work that must wait for the commit (AfterCommit); one
// that stamps and guards files (FileHooks); and one that vets new
// content (ReplaceHooks). One run produces every event kind.
//
//	go run ./records-events
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
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the disk files are stored on.
	diskName = "media"
	// archiveDisk is a second disk files can move to (MoveFile).
	archiveDisk = "archive"
	// tenant is the scope of every file.
	tenant = "acme"
	// ownerPost is the application table files attach to.
	ownerPost = "post"
	// roleGallery is the role of a post's images.
	roleGallery = "gallery"
	// metaClass is the metadata key the guard plugin stamps.
	metaClass = "classification"
	// defaultClass is the classification of a file that has none.
	defaultClass = "internal"
	// metaHold marks a file under legal hold; such a file cannot be purged.
	metaHold = "legal_hold"
	// maxUpload caps one upload.
	maxUpload = 1 << 20
)

// textPolicy accepts plain text with a random key.
var textPolicy = upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}, Key: upload.RandomKey("posts")}

// errAbort fails the rollback demo's transaction on purpose.
var errAbort = errors.New("handler failed")

// TableNameChanges is the changelog plugin's own table.
const TableNameChanges = "app_file_changes"

// Typed columns of app_file_changes, built with sqlb like the bundled
// plugins' tables, so the same code runs on every engine.
var (
	changes      = sqlb.Table{Name: TableNameChanges, Alias: "ch"}
	changeScope  = sqlb.Col[string](changes, "scope")
	changeKind   = sqlb.Col[string](changes, "kind")
	changeFileID = sqlb.Col[string](changes, "file_id")
	changeAt     = sqlb.Col[time.Time](changes, "at")
)

// Changelog is an EventHooks plugin with a table of its own. It writes a
// row per event inside the change's transaction, and queues a
// notification for after the commit, which a rollback never sends.
// ds:def id=changelog-95pvcy28
type Changelog struct {
	// Sent collects notifications, standing in for webhook calls.
	Sent []string
}

// Name implements records.Plugin; it owns the plugin's migrations.
func (c *Changelog) Name() string { return "changelog" }

// Migrations implements records.Migrator: the table is built with
// records.DDL from the same typed columns OnEvent writes, spelled for the
// engine by records.TypesFor.
// ds:def id=changelog-migrations-b8j9my5y
func (c *Changelog) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: changes,
		Columns: []ddl.ColumnSpec{
			records.Column(changeScope, t.Key), records.Column(changeKind, t.Short),
			records.Column(changeFileID, t.Key), records.Column(changeAt, t.Timestamp),
		},
	}).Statements()
	return []records.Migration{{Owner: c.Name(), Name: "0001_changes", Statements: st}}, err
}

// OnEvent implements records.EventHooks. Its insert joins the change's
// transaction (Store.Exec runs on Store.Conn); an error would roll the
// change back. The notification waits for the commit.
// ds:def id=changelog-onevent-smrprhjr
func (c *Changelog) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	ins := sqlb.NewInsert(s.Dialect()).Into(changes).Row(
		sqlb.Set(changeScope, e.Scope), sqlb.Set(changeKind, string(e.Kind)), sqlb.Set(changeFileID, e.FileID), sqlb.Set(changeAt, e.At),
	)
	if _, err := s.Exec(ctx, ins); err != nil {
		return err
	}
	s.AfterCommit(ctx, func(context.Context) { c.Sent = append(c.Sent, string(e.Kind)) })
	return nil
}

// Kinds reads the changelog back. Without ORDER BY, SQLite returns rows
// in insertion order; a real table would carry an ID or sequence to sort by.
func (c *Changelog) Kinds(ctx context.Context, s *records.Store) ([]string, error) {
	var kind string
	var out []string
	q := sqlb.NewSelect(s.Dialect()).Columns(sqlb.Bind(changeKind, &kind)).From(changes).Where(sqlb.Eq(changeScope, tenant))
	err := s.Each(ctx, q, func() error { out = append(out, kind); return nil })
	return out, err
}

// Guard is a FileHooks plugin: it stamps a classification on every new
// file and refuses to purge a file under legal hold.
type Guard struct{}

// Name implements records.Plugin.
func (Guard) Name() string { return "guard" }

// BeforeCreate implements records.FileHooks; it may change the file's
// metadata and visibility, never its ID, Disk or Key.
// ds:def id=guard-beforecreate-abrznwmq
func (Guard) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	if f.Metadata[metaClass] == "" {
		meta := map[string]string{metaClass: defaultClass}
		for k, v := range f.Metadata {
			meta[k] = v
		}
		f.Metadata = meta
	}
	return nil
}

// AfterCreate implements records.FileHooks.
func (Guard) AfterCreate(context.Context, *records.Store, records.File) error { return nil }

// BeforePurge implements records.FileHooks; its error aborts the purge.
// ds:def id=guard-beforepurge-nq6eteax
func (Guard) BeforePurge(_ context.Context, _ *records.Store, f records.File) error {
	if f.Metadata[metaHold] == "true" {
		return filesystem.Errorf(filesystem.CodePermission, "file %s is under legal hold", f.ID)
	}
	return nil
}

// TypeLock is a ReplaceHooks plugin: new content must keep the file's
// content type, so a text document cannot turn into an image.
type TypeLock struct{}

// Name implements records.Plugin.
func (TypeLock) Name() string { return "typelock" }

// BeforeReplace implements records.ReplaceHooks.
// ds:def id=typelock-beforereplace-947bgqwy
func (TypeLock) BeforeReplace(_ context.Context, _ *records.Store, cur records.File, next *records.File) error {
	if cur.ContentType != next.ContentType {
		return filesystem.Errorf(filesystem.CodeRejected, "content type %s cannot become %s", cur.ContentType, next.ContentType)
	}
	return nil
}

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

// newStore installs the three plugins. Migrate creates the changelog
// table with the core's tables.
// ds:def id=newstore-9yrcnb6u
func newStore(ctx context.Context, db *sql.DB, log *Changelog) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	archive, err := filesystem.NewDisk(archiveDisk, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk, archive)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerPost),
		records.WithPlugins(log, Guard{}, TypeLock{}))
	if err != nil {
		return nil, err
	}
	_, err = s.Migrate(ctx)
	return s, err
}

// everyKind runs one change of each kind on a file.
// ds:def id=everykind-nd3v32m9
func everyKind(ctx context.Context, s *records.Store) (records.File, error) {
	post := records.Owner{Type: ownerPost, ID: "post_1"}
	f, err := s.Upload(ctx, tenant, diskName, strings.NewReader("hello"), "a.txt", textPolicy)
	if err != nil {
		return f, err
	}
	a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery)
	if err != nil {
		return f, err
	}
	steps := []func() error{
		func() error { return s.Reorder(ctx, tenant, post, roleGallery, []string{a.ID}) },
		func() error { return s.Trash(ctx, tenant, f.ID) },
		func() error { return s.Restore(ctx, tenant, f.ID) },
		func() error {
			_, err := s.Replace(ctx, tenant, f.ID, strings.NewReader("hello again"), "", textPolicy)
			return err
		},
		func() error {
			_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusQuarantined, "rescan")
			return err
		},
		// The scan passed: file.status_changed (ready), then file.released.
		func() error { _, err := s.PassScan(ctx, tenant, f.ID); return err },
		func() error { return s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic) },
		func() error { _, err := s.MoveFile(ctx, tenant, f.ID, archiveDisk); return err },
		func() error { return s.Detach(ctx, tenant, a.ID) },
		func() error { return s.Purge(ctx, tenant, f.ID) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return f, err
		}
	}
	return f, nil
}

// rolledBack uploads inside a transaction that fails: the changelog row
// rolls back with the file, and no notification is sent.
// ds:def id=rolledback-94kuxr9u
func rolledBack(ctx context.Context, s *records.Store) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.Upload(ctx, tenant, diskName, strings.NewReader("x"), "x.txt", textPolicy); err != nil {
			return err
		}
		return errAbort
	})
}

// held records a file under legal hold and tries to purge it.
func held(ctx context.Context, s *records.Store) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, "held.txt", bytes.NewReader([]byte("evidence")), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: "held.txt", Metadata: map[string]string{metaHold: "true"}})
	if err != nil {
		return f, err
	}
	return f, s.Purge(ctx, tenant, f.ID)
}

// run performs every demo and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	changelog := &Changelog{}
	s, err := newStore(ctx, db, changelog)
	if err != nil {
		return err
	}
	if _, err := everyKind(ctx, s); err != nil {
		return err
	}
	kinds, err := changelog.Kinds(ctx, s)
	fmt.Fprintf(w, "changelog: %s %v\nsent: %d\n", strings.Join(kinds, " "), err, len(changelog.Sent))
	err = rolledBack(ctx, s)
	kinds, _ = changelog.Kinds(ctx, s)
	fmt.Fprintf(w, "rolled back: %v; rows %d, sent %d\n", err, len(kinds), len(changelog.Sent))
	f, err := held(ctx, s)
	fmt.Fprintf(w, "purge under hold: %s; metadata %v\n", filesystem.CodeOf(err), f.Metadata)
	g, err := s.Upload(ctx, tenant, diskName, strings.NewReader("text"), "b.txt", textPolicy)
	if err != nil {
		return err
	}
	png := upload.Policy{MaxSize: maxUpload, Allowed: []string{"image/png"}}
	_, err = s.Replace(ctx, tenant, g.ID, bytes.NewReader([]byte("\x89PNG\r\n\x1a\n")), "", png)
	fmt.Fprintf(w, "replace text with PNG: %s\n", filesystem.CodeOf(err))
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "records-events")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
