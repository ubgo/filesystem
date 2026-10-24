package records_test

import (
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

// updateGolden rewrites testdata/migrations_<engine>.sql from the code:
// go test -run TestMigrationSQLGolden -update-migrations.
var updateGolden = flag.Bool("update-migrations", false, "rewrite the migration golden files")

// TestMigrationSQLGolden renders every migration of the core and of every
// plugin in this module, on every engine, and compares the text with the
// golden files (with the checksums the ledger records), so any change to the DDL the module ships (a type, an
// index, a quoting) is seen in review and chosen, never incidental.
func TestMigrationSQLGolden(t *testing.T) {
	for _, d := range []sqlb.Dialect{sqlb.SQLiteDialect{}, sqlb.PostgresDialect{}, sqlb.MySQLDialect{}} {
		engine := sqlb.DialectName(d)
		t.Run(engine, func(t *testing.T) {
			compareGolden(t, filepath.Join("testdata", "migrations_"+engine+".sql"), renderMigrations(t, d))
		})
	}
}

// compareGolden compares a rendered migration script with its golden
// file, or rewrites the file under -update-migrations.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s changed; review the diff and rerun with -update-migrations if it is intended:\n%s", path, got)
	}
}

// renderMigrations is MigrationSQL with every plugin installed: the ledger
// table, every migration's statements and its ledger insert, whose
// checksum changes with any change to the statements.
func renderMigrations(t *testing.T, d sqlb.Dialect, opts ...records.Option) string {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:") // never queried: Migrations renders only
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mgr, _ := filesystem.NewManager()
	s, err := records.New(db, d, mgr, append([]records.Option{records.WithPlugins(allPlugins(t)...)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	script, err := s.MigrationSQL()
	if err != nil {
		t.Fatal(err)
	}
	return script
}
