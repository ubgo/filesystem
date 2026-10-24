// Package recordstest is a test helper: it opens a fresh, empty database
// for one test on the engine the environment selects, so the records
// module's tests, its plugins' tests and any other module's tests that run
// SQL against the records tables (contrib/imaging, an application's own
// plugin) run unchanged on SQLite, Postgres and MySQL.
//
// It reads three environment variables (task test:engines sets them):
//
//   - FILESYSTEM_RECORDS_ENGINE: "postgres" or "mysql"; unset means SQLite,
//     so a plain go test runs offline.
//   - FILESYSTEM_RECORDS_PG_DSN: a pgx DSN of a database the tests may
//     create schemas in.
//   - FILESYSTEM_RECORDS_MYSQL_DSN: a DSN without a database name
//     (user:pass@tcp(host:port)) of a server the tests may create
//     databases on.
//
// It registers the three SQL drivers, so only test code should import it:
// the records library itself imports no driver (records/deps_test.go
// enforces that), and an application brings its own.
package recordstest

import (
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	// The three engines the records module supports.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/ubgo/sqlb"
)

// Environment of the engine tests (task test:engines). Without them every
// test runs on SQLite.
const (
	// EnvEngine selects "postgres" or "mysql".
	EnvEngine = "FILESYSTEM_RECORDS_ENGINE"
	// EnvPostgresDSN is a pgx DSN of a database the tests may create
	// schemas in.
	EnvPostgresDSN = "FILESYSTEM_RECORDS_PG_DSN"
	// EnvMySQLDSN is a DSN (without a database name) of a server the tests
	// may create databases on.
	EnvMySQLDSN = "FILESYSTEM_RECORDS_MYSQL_DSN"
)

// Engine names, as sqlb reports them.
const (
	Postgres = sqlb.DialectPostgres
	MySQL    = sqlb.DialectMySQL
	SQLite   = sqlb.DialectSQLite
)

// Engine returns the engine under test.
func Engine() string {
	if e := os.Getenv(EnvEngine); e != "" {
		return e
	}
	return SQLite
}

// Dialect returns the sqlb dialect of the engine under test.
func Dialect() sqlb.Dialect {
	switch Engine() {
	case Postgres:
		return sqlb.PostgresDialect{}
	case MySQL:
		return sqlb.MySQLDialect{}
	}
	return sqlb.SQLiteDialect{}
}

// Open returns a fresh, empty database for one test and its dialect: a
// temporary SQLite file, a new Postgres schema, or a new MySQL database,
// removed when the test ends.
func Open(t testing.TB) (*sql.DB, sqlb.Dialect) {
	t.Helper()
	driverName, dsn := create(t)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return keep(t, db), Dialect()
}

// OpenFaulty is Open with a Faults switch that makes one chosen database
// step fail, to prove an operation fails cleanly at every step.
func OpenFaulty(t testing.TB) (*sql.DB, *Faults) {
	t.Helper()
	driverName, dsn := create(t)
	probe, err := sql.Open(driverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	inner := probe.Driver()
	_ = probe.Close()
	f := &Faults{}
	return keep(t, sql.OpenDB(connector{dsn: dsn, inner: inner, faults: f})), f
}

// keep applies the engine's pool settings and closes db after the test.
func keep(t testing.TB, db *sql.DB) *sql.DB {
	if Engine() == SQLite {
		db.SetMaxOpenConns(1) // one writer; a second connection only waits on the lock
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// create makes the test's empty database and returns how to connect to it.
func create(t testing.TB) (driverName, dsn string) {
	t.Helper()
	name := "fstest_" + strings.ToLower(rand.Text())
	switch Engine() {
	case Postgres:
		admin := mustOpen(t, "pgx", os.Getenv(EnvPostgresDSN))
		Exec(t, admin, "CREATE SCHEMA "+name)
		t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + name + " CASCADE"); _ = admin.Close() })
		sep := "?"
		if strings.Contains(os.Getenv(EnvPostgresDSN), "?") {
			sep = "&"
		}
		return "pgx", os.Getenv(EnvPostgresDSN) + sep + "search_path=" + name
	case MySQL:
		admin := mustOpen(t, "mysql", os.Getenv(EnvMySQLDSN)+"/")
		Exec(t, admin, "CREATE DATABASE "+name)
		t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE " + name); _ = admin.Close() })
		return "mysql", os.Getenv(EnvMySQLDSN) + "/" + name + "?parseTime=true&loc=UTC"
	}
	return "sqlite", "file:" + filepath.Join(t.TempDir(), "records.db") + "?_pragma=busy_timeout(5000)"
}

func mustOpen(t testing.TB, driver, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// Exec runs a statement or fails the test.
func Exec(t testing.TB, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// OnlySQLite skips tests that break the database with SQLite-specific
// tricks (triggers raising errors).
func OnlySQLite(t testing.TB) {
	t.Helper()
	if Engine() != SQLite {
		t.Skip("uses SQLite-specific fault injection")
	}
}
