package pickers_test

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
// module may have. Anything else (a SQL driver, an SDK) would land in every
// application importing the adapter.
var allowedImports = []string{"github.com/ubgo/filesystem"}

// deniedImports are covered by an allowed prefix but must stay out of
// production code: the records module is a test-only dependency.
var deniedImports = []string{"github.com/ubgo/filesystem/records"}

func TestGuard_ProductionCodeImportsOnlyAllowed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		for _, bad := range foreignImports(t, f) {
			t.Errorf("%s imports %s; allowed outside the standard library: %v", f, bad, allowedImports)
		}
	}
	if scanned == 0 {
		t.Fatalf("scanned %d files; the guard would pass vacuously", scanned)
	}
}

func TestGuard_DetectsAForeignImport(t *testing.T) {
	for _, imp := range []string{deniedImports[0] + "/recordstest"} {
		path := filepath.Join(t.TempDir(), "x.go")
		src := "package pickers\n\nimport (\n\t\"io\"\n\n\t\"github.com/ubgo/filesystem\"\n\t_ \"" + imp + "\"\n)\n"
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := foreignImports(t, path); len(got) != 1 || got[0] != imp {
			t.Errorf("planted import of %s: got %v", imp, got)
		}
	}
}

// foreignImports lists the imports of path outside the standard library
// that allowedImports does not cover or deniedImports names.
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
