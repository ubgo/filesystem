package fsgorm_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedImports are the only non-standard imports production code of
// this module may have: GORM itself and the records packages. A SQL
// driver or a GORM dialector would be forced on every application; the
// application opens GORM with its own.
// gorm.io/gorm/schema is GORM itself (its naming-strategy interface, which
// Namer implements), not a driver.
var allowedImports = []string{"gorm.io/gorm", "gorm.io/gorm/clause", "gorm.io/gorm/schema", "github.com/ubgo/filesystem", "github.com/ubgo/filesystem/records"}

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
		if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") && !contains(allowedImports, p) {
			out = append(out, p)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestGuard_ProductionCodeImportsNoDriver(t *testing.T) {
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
			t.Errorf("%s imports %s; contrib/gorm may import only GORM and the records packages", f, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no file; the guard would pass vacuously")
	}
}

func TestGuard_DetectsADialectorImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package fsgorm\n\nimport (\n\t\"gorm.io/gorm\"\n\n\t\"gorm.io/driver/postgres\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := foreignImports(t, path); len(got) != 1 || got[0] != "gorm.io/driver/postgres" {
		t.Fatalf("planted import not detected: %v", got)
	}
}
