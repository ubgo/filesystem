package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/tags"
)

// testStore returns a migrated Store with tp installed over a fresh SQLite
// file, and the database for direct checks.
func testStore(t *testing.T, tp *tags.Plugin) (*records.Store, *sql.DB) {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, err := newDisks()
	if err != nil {
		t.Fatal(err)
	}
	s, err := newStore(t.Context(), db, disks, tp)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"jan.txt: [2026 invoice]", "feb.txt: [invoice paid]", "invoice+paid: [feb.txt]; invoices under 20 bytes: [feb.txt]",
		"cloud with shop.txt trashed: invoice(2) 2026(1) paid(1)", "deleting 2026: invoice(2)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSlugNormalize(t *testing.T) {
	tp := newSlugPlugin()
	s, _ := testStore(t, tp)
	f, err := uploadText(t.Context(), s, "a.txt", "a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tp.Set(t.Context(), s, tenant, f.ID, "  Annual  Report ")
	if err != nil || !slices.Equal(got, []string{"annual-report"}) {
		t.Fatalf("Set = %v, %v", got, err)
	}
}

func TestLimits(t *testing.T) {
	tp := tags.New()
	s, _ := testStore(t, tp)
	f, err := uploadText(t.Context(), s, "a.txt", "a")
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, tags.MaxPerFile+1)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	if _, err := tp.Set(t.Context(), s, tenant, f.ID, many...); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("%d tags = %v, want invalid_argument", len(many), err)
	}
	long := strings.Repeat("x", tags.MaxTagChars+1)
	if _, err := tp.Add(t.Context(), s, tenant, f.ID, long); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("a %d-character tag = %v, want invalid_argument", len(long), err)
	}
	if _, err := tp.Filter(s, tenant); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("Filter without tags = %v, want invalid_argument", err)
	}
	if _, err := tp.Tags(t.Context(), s, tenant, "fil_missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("Tags of an unknown file = %v, want not_found", err)
	}
}

func TestPurgeRemovesTags(t *testing.T) {
	tp := tags.New()
	s, db := testStore(t, tp)
	f, err := uploadText(t.Context(), s, "a.txt", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.Set(t.Context(), s, tenant, f.ID, "x", "y"); err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+tags.TableNameTags).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tag rows after purge = %d, %v", n, err)
	}
}
