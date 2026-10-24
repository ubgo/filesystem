package records_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedImports are the only non-standard imports production code of this
// module may have. The SQL drivers in go.mod are for the tests: the
// application brings its own driver, so the library never forces one.
var allowedImports = []string{"github.com/ubgo/sqlb", "github.com/ubgo/filesystem"}

// testOnlyPackages may import the SQL drivers: recordstest is the
// engine-aware test helper, imported only by tests (this module's and other
// modules' that run SQL on the records tables), and internal/testdb is this
// module's thin wrapper over it; no library package imports either.
var testOnlyPackages = map[string]bool{"recordstest": true, filepath.Join("internal", "testdb"): true}

// deniedImports are module packages production code must not import even
// though allowedImports' prefix covers them: they pull in the SQL drivers.
var deniedImports = []string{"github.com/ubgo/filesystem/records/recordstest", "github.com/ubgo/filesystem/records/internal/testdb"}

func TestGuard_ProductionCodeImportsOnlySqlbAndTheCore(t *testing.T) {
	var files []string
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && testOnlyPackages[p] {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		scanned++
		for _, bad := range foreignImports(t, f) {
			t.Errorf("%s imports %s; the records module may import only sqlb and the core", f, bad)
		}
	}
	if scanned < 5 {
		t.Fatalf("scanned %d files; the guard would pass vacuously", scanned)
	}
}

func TestGuard_DetectsADriverImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package records\n\nimport (\n\t\"fmt\"\n\n\t_ \"github.com/jackc/pgx/v5/stdlib\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := foreignImports(t, path); len(got) != 1 {
		t.Errorf("planted driver import not detected: %v", got)
	}
}

func TestGuard_DetectsATestHelperImport(t *testing.T) {
	for _, helper := range deniedImports {
		path := filepath.Join(t.TempDir(), "x.go")
		src := "package records\n\nimport (\n\t\"github.com/ubgo/filesystem\"\n\t_ \"" + helper + "\"\n)\n"
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := foreignImports(t, path); len(got) != 1 || got[0] != helper {
			t.Errorf("planted import of %s: got %v", helper, got)
		}
	}
}

func foreignImports(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		first, _, _ := strings.Cut(p, "/")
		if !strings.Contains(first, ".") {
			continue
		}
		ok := false
		for _, a := range allowedImports {
			if p == a || strings.HasPrefix(p, a+"/") {
				ok = true
			}
		}
		for _, d := range deniedImports {
			if p == d || strings.HasPrefix(p, d+"/") {
				ok = false
			}
		}
		if !ok {
			bad = append(bad, p)
		}
	}
	return bad
}
