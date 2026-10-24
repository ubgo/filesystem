package sqlblob_test

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/sqlblob"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/sqlb"
)

// newDriver opens a fresh database on the engine the environment selects
// (SQLite by default; see recordstest) and migrates the table.
func newDriver(t *testing.T, cfg sqlblob.Config) (*sqlblob.Driver, *sql.DB) {
	t.Helper()
	db, dialect := recordstest.Open(t)
	cfg.DB, cfg.Dialect = db, dialect
	drv, err := sqlblob.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return drv, db
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	drv, _ := newDriver(t, sqlblob.Config{})
	d, err := filesystem.NewDisk("sqlblob", drv)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestConformance(t *testing.T) {
	filesystemtest.Run(t, newDisk)
}

// TestRangedReadSelectsOnlyTheRange pins that Read asks the database for the
// range alone: the bytes returned are the range, including a range that
// starts past the end (empty) and one with no length (to the end).
func TestRangedReadSelectsOnlyTheRange(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	body := []byte("0123456789")
	if _, err := drv.Write(ctx, "r.bin", bytes.NewReader(body), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rng  filesystem.ByteRange
		want string
	}{
		{filesystem.ByteRange{}, "0123456789"},
		{filesystem.ByteRange{Offset: 3, Length: 4}, "3456"},
		{filesystem.ByteRange{Offset: 7}, "789"},
		{filesystem.ByteRange{Offset: 8, Length: 100}, "89"},
		{filesystem.ByteRange{Offset: 50}, ""},
	} {
		rc, err := drv.Read(ctx, "r.bin", tc.rng)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(got) != tc.want {
			t.Errorf("Read(%+v) = %q, want %q", tc.rng, got, tc.want)
		}
	}
}

// TestMaxObjectSize pins the cap: one byte over is CodeTooLarge and stores
// nothing; exactly the cap is stored.
func TestMaxObjectSize(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{MaxObjectSize: 8})
	if _, err := drv.Write(ctx, "big", strings.NewReader("123456789"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("over the cap: %v, want CodeTooLarge", err)
	}
	if _, err := drv.Stat(ctx, "big"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a refused write left a row: %v", err)
	}
	if _, err := drv.Write(ctx, "fits", strings.NewReader("12345678"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("at the cap: %v", err)
	}
}

// TestTablesAreIndependent pins Config.Table: two drivers over two tables
// of one database never see each other's keys.
func TestTablesAreIndependent(t *testing.T) {
	ctx := context.Background()
	a, db := newDriver(t, sqlblob.Config{Table: "blobs_a"})
	b, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: recordstest.Dialect(), Table: "blobs_b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write(ctx, "k", strings.NewReader("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("table b sees table a's key: %v", err)
	}
	if err := a.Migrate(ctx); err != nil {
		t.Fatalf("Migrate is not re-runnable: %v", err)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	db, dialect := recordstest.Open(t)
	for name, cfg := range map[string]sqlblob.Config{
		"no db":         {Dialect: dialect},
		"no dialect":    {DB: db},
		"sql in name":   {DB: db, Dialect: dialect, Table: "x; DROP TABLE y"},
		"negative size": {DB: db, Dialect: dialect, MaxObjectSize: -1},
	} {
		if _, err := sqlblob.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v, want CodeInvalidArgument", name, err)
		}
	}
	if _, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: sqlb.ClickHouseDialect{}}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("clickhouse: %v, want CodeUnsupported", err)
	}
}

// TestSchemaIsByteOrdered pins the key column's spelling per engine: the
// byte order List and the directory range rely on.
func TestSchemaIsByteOrdered(t *testing.T) {
	db, _ := recordstest.Open(t)
	for dialect, want := range map[sqlb.Dialect]string{
		sqlb.PostgresDialect{}: `COLLATE "C"`,
		sqlb.MySQLDialect{}:    "VARBINARY(1024)",
		sqlb.SQLiteDialect{}:   `"key" TEXT`,
	} {
		drv, err := sqlblob.New(sqlblob.Config{DB: db, Dialect: dialect})
		if err != nil {
			t.Fatal(err)
		}
		stmts, err := drv.Schema()
		if err != nil {
			t.Fatal(err)
		}
		if len(stmts) != 1 || !strings.Contains(stmts[0], want) || !strings.Contains(stmts[0], "IF NOT EXISTS") {
			t.Errorf("%s: %v, want one CREATE TABLE IF NOT EXISTS with %s", sqlb.DialectName(dialect), stmts, want)
		}
	}
}

// TestListOrderIsBytewise writes keys whose byte order differs from a
// linguistic collation's and checks a recursive listing pages them by
// bytes, with the directory range excluding the sibling "a0".
func TestListOrderIsBytewise(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	keys := []string{"a/B", "a/a", "a/_", "a/Z-", "a/b/c", "a0"}
	for _, k := range keys {
		if _, err := drv.Write(ctx, k, strings.NewReader(k), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	cursor := ""
	for {
		p, err := drv.List(ctx, "a", filesystem.ListOptions{Recursive: true, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range p.Entries {
			got = append(got, e.Key)
		}
		if cursor = p.NextCursor; cursor == "" {
			break
		}
	}
	want := []string{"a/B", "a/Z-", "a/_", "a/a", "a/b/c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("listing %v, want %v", got, want)
	}
}

// TestEmptyObjectIsNotNull pins that an empty body is stored as an empty
// value (the data column is NOT NULL) and reads back empty.
func TestEmptyObjectIsNotNull(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	if _, err := drv.Write(ctx, "empty", strings.NewReader(""), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	rc, err := drv.Read(ctx, "empty", filesystem.ByteRange{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); len(b) != 0 {
		t.Fatalf("read %q", b)
	}
}

// TestAsReachesTheDB pins the escape hatch: filesystem.As[*sql.DB] returns
// the application's own pool.
func TestAsReachesTheDB(t *testing.T) {
	drv, db := newDriver(t, sqlblob.Config{})
	d, err := filesystem.NewDisk("sqlblob", drv)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := filesystem.As[*sql.DB](d)
	if !ok || got != db {
		t.Fatal("As did not reach the driver's *sql.DB")
	}
}

// TestMoveOntoItself pins that moving a key onto itself keeps the object
// (deleting dst first would otherwise delete src).
func TestMoveOntoItself(t *testing.T) {
	ctx := context.Background()
	drv, _ := newDriver(t, sqlblob.Config{})
	if _, err := drv.Write(ctx, "same", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Move(ctx, "same", "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.Stat(ctx, "same"); err != nil {
		t.Fatalf("object lost by a move onto itself: %v", err)
	}
}
