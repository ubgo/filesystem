package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/sqlb"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"first start applied: [records/0001_files_and_attachments records/0002_files_uploaded_by records/0003_files_status records/0004_files_object records/0005_files_source records/0006_files_expiry records/0007_files_checksums notes/0001_notes notes/0002_notes_author] <nil>",
		"second start applied: [] <nil>",
		"edited migration: invalid_argument",
		"mysql    Key=VARCHAR(191)",
		`ADD COLUMN IF NOT EXISTS "author"`,
		"information_schema.columns",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestScriptPerEngine(t *testing.T) {
	var out strings.Builder
	if err := printScript(&out); err != nil {
		t.Fatal(err)
	}
	// The script carries the ledger, so a later Migrate sees its migrations as applied.
	if !strings.Contains(out.String(), "-- notes 0002_notes_author") || !strings.Contains(out.String(), "INSERT INTO fs_migrations") {
		t.Fatalf("script:\n%s", out.String())
	}
	db, err := openSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, d := range []sqlb.Dialect{sqlb.PostgresDialect{}, sqlb.MySQLDialect{}} {
		s, err := newStore(db, d, Notes{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := script(s); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMigrationsList lists the core migrations first, then the
// plugin's, and refuses a negative lock wait.
func TestMigrationsList(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	s, err := newStore(db, sqlb.SQLiteDialect{}, Notes{})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := s.Migrations()
	if err != nil || len(ms) != 9 || ms[0].Owner != records.CoreOwner {
		t.Fatalf("Migrations = %v, %v", ms, err)
	}
	if _, err := records.New(db, sqlb.SQLiteDialect{}, s.Disks(), records.WithMigrationLockWait(-1)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("negative lock wait = %v", err)
	}
}
