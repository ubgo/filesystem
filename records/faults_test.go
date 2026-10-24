package records_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// failOn installs a SQLite trigger that makes every op on table fail, the
// way a lost connection or a constraint added by an operator would.
func failOn(t *testing.T, db *sql.DB, op, table string) {
	t.Helper()
	exec(t, db, "CREATE TRIGGER fail_"+op+"_"+table+" BEFORE "+op+" ON "+table+" BEGIN SELECT RAISE(FAIL, 'injected'); END")
}

func injected(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "injected") && !strings.Contains(err.Error(), "no such table") {
		t.Fatalf("expected an injected database failure, got %v", err)
	}
}

func TestDatabaseFailuresSurface(t *testing.T) {
	onlySQLite(t)
	ctx := t.Context()
	post := records.Owner{Type: "post", ID: "p"}

	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	failOn(t, e.db, "INSERT", records.TableNameFiles)
	_, err := e.store.CreateFile(ctx, records.NewFile{Disk: "media", Key: "a.png"})
	injected(t, err)

	e = newEnv(t, nil)
	f = e.put(t, "t", "a.png")
	att, _ := e.store.Attach(ctx, "t", f.ID, post, "g")
	failOn(t, e.db, "UPDATE", records.TableNameAttachments)
	injected(t, e.store.Reorder(ctx, "t", post, "g", []string{att.ID}))
	failOn(t, e.db, "DELETE", records.TableNameAttachments)
	injected(t, e.store.Purge(ctx, "t", f.ID))

	e = newEnv(t, nil)
	f = e.put(t, "t", "a.png")
	failOn(t, e.db, "DELETE", records.TableNameFiles)
	injected(t, e.store.Purge(ctx, "t", f.ID))

	e = newEnv(t, nil)
	f = e.put(t, "t", "a.png")
	exec(t, e.db, "DROP TABLE "+records.TableNameAttachments)
	_, err = e.store.Attach(ctx, "t", f.ID, post, "g")
	injected(t, err)
	_, err = e.store.Attachments(ctx, "t", post, "g")
	injected(t, err)
	injected(t, e.store.Reorder(ctx, "t", post, "g", nil))

	exec(t, e.db, "DROP TABLE "+records.TableNameFiles)
	_, err = e.store.Attach(ctx, "t", f.ID, post, "g")
	injected(t, err)
	_, err = e.store.Files(ctx, records.FileQuery{Scope: "t"})
	injected(t, err)
	_, err = e.store.SweepTrash(ctx, time.Now())
	injected(t, err)
}

func TestLedgerFailures(t *testing.T) {
	onlySQLite(t)
	ctx := t.Context()
	mgr, _ := filesystem.NewManager()
	newStore := func(db *sql.DB) *records.Store {
		s, err := records.New(db, testDialect(), mgr)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The ledger name is taken by an index: it cannot be created.
	db := openDB(t)
	exec(t, db, "CREATE TABLE other (x INTEGER)")
	exec(t, db, "CREATE INDEX "+records.TableNameMigrations+" ON other (x)")
	if _, err := newStore(db).Migrate(ctx); err == nil || !strings.Contains(err.Error(), "create ledger") {
		t.Errorf("ledger blocked by an index = %v", err)
	}
	// A ledger of the wrong shape cannot be read.
	db = openDB(t)
	exec(t, db, "CREATE TABLE "+records.TableNameMigrations+" (unrelated INTEGER)")
	if _, err := newStore(db).Migrate(ctx); err == nil || !strings.Contains(err.Error(), "read ledger") {
		t.Errorf("malformed ledger = %v", err)
	}
	// A ledger row that does not scan.
	db = openDB(t)
	exec(t, db, "CREATE TABLE "+records.TableNameMigrations+" (owner TEXT, name TEXT, checksum TEXT, applied_at TIMESTAMP)")
	exec(t, db, "INSERT INTO "+records.TableNameMigrations+" VALUES (NULL, 'x', 'y', CURRENT_TIMESTAMP)")
	if _, err := newStore(db).Migrate(ctx); err == nil {
		t.Error("NULL ledger row accepted")
	}
	// Recording a migration fails after its statements ran: rolled back.
	db = openDB(t)
	s := newStore(db)
	if _, err := s.Migrate(ctx); err != nil { // creates the ledger
		t.Fatal(err)
	}
	exec(t, db, "DELETE FROM "+records.TableNameMigrations)
	failOn(t, db, "INSERT", records.TableNameMigrations)
	if _, err := s.Migrate(ctx); err == nil {
		t.Error("ledger insert failure ignored")
	}
}

// commitFails makes the next commit fail with a deferred foreign key.
func TestCommitFailureRunsRollbackWork(t *testing.T) {
	onlySQLite(t)
	ctx := t.Context()
	e := newEnv(t, nil)
	exec(t, e.db, "PRAGMA foreign_keys = ON")
	exec(t, e.db, "CREATE TABLE parent (id TEXT PRIMARY KEY)")
	exec(t, e.db, "CREATE TABLE child (parent_id TEXT REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)")
	before := countObjects(t, e.disk)
	err := e.store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := e.store.Upload(ctx, "t", "media", reader(pngData), "x.png", images); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO child VALUES ('missing')")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("Tx = %v, want a commit failure", err)
	}
	if countObjects(t, e.disk) != before {
		t.Error("a failed commit left the uploaded object")
	}
}

// badStatement fails to build.
type badStatement struct{}

func (badStatement) Build() (sqlb.Query, error) { return sqlb.Query{}, filesystem.ErrInvalidArgument }

func TestStatementBuildErrors(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	if _, err := e.store.Exec(ctx, badStatement{}); err == nil || !strings.Contains(err.Error(), "records: build") {
		t.Errorf("Exec = %v", err)
	}
	if err := e.store.One(ctx, badStatement{}); err == nil {
		t.Error("One accepted a bad statement")
	}
	if err := e.store.Each(ctx, badStatement{}, func() error { return nil }); err == nil {
		t.Error("Each accepted a bad statement")
	}
}

// noVisibility reports no visibility, so the disk default applies.
func TestDefaultVisibilityFromDisk(t *testing.T) {
	ctx := t.Context()
	d, _ := filesystem.NewDisk("plain", plainDriver{}, filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	_, _ = d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{})
	mgr, _ := filesystem.NewManager(d)
	s, _ := records.New(openDB(t), testDialect(), mgr)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Disk: "plain", Key: "k"})
	if err != nil || f.Visibility != filesystem.VisibilityPublic {
		t.Errorf("visibility = %q, %v", f.Visibility, err)
	}
}

func TestAttachInsertFailure(t *testing.T) {
	onlySQLite(t)
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	failOn(t, e.db, "INSERT", records.TableNameAttachments)
	_, err := e.store.Attach(t.Context(), "t", f.ID, records.Owner{Type: "post", ID: "p"}, "g")
	injected(t, err)
}
