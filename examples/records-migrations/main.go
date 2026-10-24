// Command records-migrations shows how the records schema is created and
// evolved: Migrate (applies what is new, once, under a lock), MigrationSQL
// (the same changes as one script, for review or an external tool), a
// plugin shipping its own migrations built with records.DDL from typed
// columns (a table, then a column added later), the per-engine type names
// (TypesFor), and the refusal of a migration whose statements changed
// after it ran.
//
//	go run ./records-migrations        prints the demo
//	go run ./records-migrations sql    prints the SQLite script only
package main

import (
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
	"github.com/ubgo/sqlb"
	"github.com/ubgo/sqlb/ddl"
)

// Decisions of this demo app, not of the library.
const (
	// diskName is the one disk of the demo.
	diskName = "media"
	// lockWait bounds how long Migrate waits for another instance's lock
	// on MySQL; Postgres waits without a bound, SQLite needs no lock.
	lockWait = 2 * time.Minute
	// TableNameNotes is the demo plugin's own table.
	TableNameNotes = "app_file_notes"
	// ColNoteAuthor is the column the plugin's second migration adds.
	ColNoteAuthor = "author"
)

// The plugin's table and typed columns: the same values its queries use
// build its migrations, so a column cannot be created under one name and
// read under another.
var (
	notes      = sqlb.Table{Name: TableNameNotes}
	noteFileID = sqlb.Col[string](notes, "file_id")
	noteBody   = sqlb.Col[string](notes, "body")
	noteAuthor = sqlb.Col[string](notes, ColNoteAuthor)
	// noteText is what an edit of the first migration renames body to.
	noteText = sqlb.Col[string](notes, "text")
)

// Notes is a plugin with a table of its own and two migrations: the
// table, then a column added in a later release. Migration names are
// permanent; a change ships as a new migration, never as an edit.
type Notes struct {
	// Edited simulates the mistake of editing a migration that already ran.
	Edited bool
}

// Name implements records.Plugin. It is the owner of the migrations.
func (n Notes) Name() string { return "notes" }

// Migrations implements records.Migrator for the engine Migrate runs on.
// ds:def id=notes-migrations-sk59n7yu
func (n Notes) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	body := records.Column(noteBody, t.Doc)
	if n.Edited {
		body = records.Column(noteText, t.Doc)
	}
	table, err1 := records.NewDDL(engine).CreateTable(records.Table{
		Table:      notes,
		Columns:    []ddl.ColumnSpec{records.Column(noteFileID, t.Key), body},
		PrimaryKey: []ddl.ColumnRef{noteFileID},
	}).Statements()
	author, err2 := addAuthor(engine)
	return []records.Migration{
		{Owner: n.Name(), Name: "0001_notes", Statements: table},
		{Owner: n.Name(), Name: "0002_notes_author", Statements: author},
	}, errors.Join(err1, err2)
}

// addAuthor is the second migration: a column added in a later release,
// NOT NULL with a default for the rows already there, re-runnable on
// every engine.
// ds:def id=addauthor-xz8sjzh6
func addAuthor(engine string) ([]string, error) {
	return records.NewDDL(engine).AddColumn(notes, records.Column(noteAuthor, records.TypesFor(engine).Key).Default("")).Statements()
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

// newStore builds a Store with the Notes plugin; it does not migrate.
// ds:def id=newstore-2dna975c
func newStore(db *sql.DB, dialect sqlb.Dialect, notes Notes) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	return records.New(db, dialect, disks, records.WithPlugins(notes), records.WithMigrationLockWait(lockWait))
}

// migrateOnStart is what an app runs at every start: it applies the
// migrations not yet in the fs_migrations ledger and returns them as
// "owner/name"; an up-to-date database returns none.
// ds:def id=migrateonstart-cjhttfvq
func migrateOnStart(ctx context.Context, s *records.Store) ([]string, error) {
	return s.Migrate(ctx)
}

// script renders every migration of s as one SQL script, for a review or
// for an external migration tool. It runs no statement.
// ds:def id=script-54jkpwk3
func script(s *records.Store) (string, error) {
	return s.MigrationSQL()
}

// keyTypes prints the type names a plugin's migration gets per engine.
// ds:def id=keytypes-rwytdkrm
func keyTypes(w io.Writer) {
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		t := records.TypesFor(engine)
		fmt.Fprintf(w, "%-8s Key=%s Short=%s Timestamp=%s Doc=%s\n", engine, t.Key, t.Short, t.Timestamp, t.Doc)
	}
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := openSQLite(filepath.Join(dir, "records.db"))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	s, err := newStore(db, sqlb.SQLiteDialect{}, Notes{})
	if err != nil {
		return err
	}
	applied, err := migrateOnStart(ctx, s)
	fmt.Fprintf(w, "first start applied: %v %v\n", applied, err)
	applied, err = migrateOnStart(ctx, s)
	fmt.Fprintf(w, "second start applied: %v %v\n", applied, err)
	edited, err := newStore(db, sqlb.SQLiteDialect{}, Notes{Edited: true})
	if err != nil {
		return err
	}
	_, err = migrateOnStart(ctx, edited)
	fmt.Fprintf(w, "edited migration: %s: %v\n", filesystem.CodeOf(err), err)
	keyTypes(w)
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		st, err := addAuthor(engine)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "AddColumn %s:\n  %s\n", engine, strings.Join(st, "\n  "))
	}
	return nil
}

// printScript writes the SQLite script; sql.Open connects lazily, so no
// database is touched.
func printScript(w io.Writer) error {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	s, err := newStore(db, sqlb.SQLiteDialect{}, Notes{})
	if err != nil {
		return err
	}
	out, err := script(s)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, out)
	return err
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "sql" {
		if err := printScript(os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	dir, err := os.MkdirTemp("", "records-migrations")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
