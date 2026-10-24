package sqlc_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedImport is the only non-standard import production code of this
// module may have: the package is data files plus a lookup, and an
// application importing it must not inherit a SQL driver or sqlc itself.
const allowedImport = "github.com/ubgo/filesystem"

// foreignImports returns the imports of file that are neither standard
// library nor allowedImport (exactly: records and its test helpers are
// denied, since they pull in SQL drivers).
func foreignImports(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, im := range f.Imports {
		p, err := strconv.Unquote(im.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		first := strings.SplitN(p, "/", 2)[0]
		if p != allowedImport && strings.Contains(first, ".") {
			out = append(out, p)
		}
	}
	return out
}

func TestGuard_ProductionCodeImportsOnlyTheCore(t *testing.T) {
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
			t.Errorf("%s imports %s; contrib/sqlc may import only the standard library and the core", f, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no file; the guard would pass vacuously")
	}
}

func TestGuard_DetectsARecordsImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package sqlc\n\nimport (\n\t\"embed\"\n\n\t\"github.com/ubgo/filesystem/records\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := foreignImports(t, path); len(got) != 1 || got[0] != "github.com/ubgo/filesystem/records" {
		t.Fatalf("planted import not detected: %v", got)
	}
}
