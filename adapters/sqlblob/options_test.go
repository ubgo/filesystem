package sqlblob_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/sqlblob"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/sqlb"
)

// TestSchemaQualifier proves Config.Schema qualifies the table in the DDL
// on every engine, that a schema name carrying SQL is refused, and, on
// SQLite, that a disk stores its rows in the attached database it names.
func TestSchemaQualifier(t *testing.T) {
	db, dialect := recordstest.Open(t)
	for d, want := range map[sqlb.Dialect]string{
		sqlb.PostgresDialect{}: `"store"."filesystem_blobs"`,
		sqlb.MySQLDialect{}:    "`store`.`filesystem_blobs`",
		sqlb.SQLiteDialect{}:   `"store"."filesystem_blobs"`,
	} {
		drv, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: d, Schema: "store"})
		if err != nil {
			t.Fatal(err)
		}
		stmts, err := drv.Schema()
		if err != nil || len(stmts) != 1 || !strings.Contains(stmts[0], want) {
			t.Errorf("%s: %v (%v), want %s", sqlb.DialectName(d), stmts, err, want)
		}
	}
	if _, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: dialect, Schema: "x; DROP TABLE y"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("SQL in the schema name: %v, want CodeInvalidArgument", err)
	}
	if sqlb.DialectName(dialect) != sqlb.DialectSQLite {
		return // the round trip below attaches a SQLite database
	}
	// ATTACH is per connection, so the pool keeps one.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), "ATTACH DATABASE ? AS store", filepath.Join(t.TempDir(), "store.db")); err != nil {
		t.Fatal(err)
	}
	drv, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: dialect, Schema: "store"})
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	disk, _ := filesystem.NewDisk("sqlblob", drv)
	if _, err := disk.WriteBytes(t.Context(), "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM store.filesystem_blobs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows in store.filesystem_blobs = %d, %v; want 1", n, err)
	}
}

// TestClock proves writes and copies stamp rows with Config.Clock, and
// that without one the stamp is the wall clock.
func TestClock(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2030, 1, 2, 3, 4, 5, 6, time.UTC)
	drv, _ := newDriver(t, sqlblob.Config{Clock: func() time.Time { return at }})
	d, _ := filesystem.NewDisk("sqlblob", drv)
	for _, step := range []func() (filesystem.Entry, error){
		func() (filesystem.Entry, error) {
			return d.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{})
		},
		func() (filesystem.Entry, error) { return d.Copy(ctx, "a", "b") },
	} {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"a", "b"} {
		if e, err := d.Stat(ctx, k); err != nil || !e.ModTime.Equal(at) {
			t.Errorf("%s ModTime = %v (%v), want %v", k, e.ModTime, err, at)
		}
	}

	before := time.Now()
	plain, _ := newDriver(t, sqlblob.Config{})
	pd, _ := filesystem.NewDisk("sqlblob", plain)
	e, err := pd.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{})
	if err != nil || e.ModTime.Before(before) || e.ModTime.After(time.Now()) {
		t.Errorf("default ModTime = %v (%v), want the wall clock", e.ModTime, err)
	}
}
