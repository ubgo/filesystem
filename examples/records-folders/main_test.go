package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/folders"
)

// testStore returns a migrated Store with fp installed over a fresh SQLite
// file, and the database for direct checks.
func testStore(t *testing.T, fp *folders.Plugin) (*records.Store, *sql.DB) {
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
	s, err := newStore(t.Context(), db, disks, fp)
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
		"breadcrumbs: My Drive / Projects / Website / Assets", "Assets holds [icons.txt hero.txt logo.txt] (2 pages)",
		"move into own subfolder: invalid_argument; move beside a same-named folder: exists",
		"logo.txt in the root: true", `"Reports 2026"=ok "a/b"=invalid_argument`,
		"shortcuts after linking: Q4 [report.txt], Board pack [report.txt (shortcut)]",
		"shortcuts of report.txt: 1, in Board pack: true", "shortcut into its home: invalid_argument",
		"shortcuts in the trash: Q4 [empty], Board pack [empty]",
		"shortcuts restored: Q4 [report.txt], Board pack [report.txt (shortcut)]",
		"shortcuts after moving its home into Board pack: Q4 [empty], Board pack [report.txt]",
		"shortcut without WithShortcuts: unsupported",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestMaxDepth(t *testing.T) {
	fp := folders.New()
	s, _ := testStore(t, fp)
	parent := folders.RootID
	for range folders.MaxDepth {
		f, err := fp.Create(t.Context(), s, tenant, parent, "level")
		if err != nil {
			t.Fatal(err)
		}
		parent = f.ID
	}
	_, err := fp.Create(t.Context(), s, tenant, parent, "too deep")
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("folder %d deep = %v, want invalid_argument", folders.MaxDepth+1, err)
	}
}

func TestPurgeRemovesThePlace(t *testing.T) {
	fp := folders.New()
	s, db := testStore(t, fp)
	tr, err := makeTree(t.Context(), s, fp)
	if err != nil {
		t.Fatal(err)
	}
	f, err := uploadInto(t.Context(), s, fp, tr.assets.ID, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Purge(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+folders.TableNamePlaces).Scan(&n); err != nil || n != 0 {
		t.Fatalf("places after purge = %d, %v", n, err)
	}
}

func TestRenameAndDeleteErrors(t *testing.T) {
	fp := folders.New()
	s, _ := testStore(t, fp)
	tr, err := makeTree(t.Context(), s, fp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fp.Create(t.Context(), s, tenant, tr.website.ID, "Docs"); err != nil {
		t.Fatal(err)
	}
	if err := fp.Rename(t.Context(), s, tenant, tr.assets.ID, "Docs"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Errorf("rename onto a sibling = %v, want exists", err)
	}
	if err := fp.Rename(t.Context(), s, tenant, tr.assets.ID, "Assets"); err != nil {
		t.Errorf("rename to the current name = %v", err)
	}
	if err := fp.Delete(t.Context(), s, tenant, "fld_missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("delete of an unknown folder = %v, want not_found", err)
	}
	if err := fp.Delete(t.Context(), s, tenant, tr.projects.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("delete of a folder with subfolders = %v, want invalid_argument", err)
	}
}

// TestCaseInsensitiveNames pins both models: byte-for-byte names let
// "docs" sit beside "Docs"; WithNameKey(FoldCase) refuses it with exists
// and still allows a rename to a case variant of the folder's own name.
func TestCaseInsensitiveNames(t *testing.T) {
	var out strings.Builder
	if err := caseNames(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"byte for byte: create docs beside Docs: ok; rename Reports to DOCS: ok; rename Docs to DOCS: exists",
		"FoldCase: create docs beside Docs: exists; rename Reports to DOCS: exists; rename Docs to DOCS: ok",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
