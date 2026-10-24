package icc_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is empty: production code of this module imports only
// the standard library, so an application adding colour management
// inherits nothing; imaging is a test-only requirement.
var allowedImports = []string{}

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
		if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") && !slices.Contains(allowedImports, p) {
			out = append(out, p)
		}
	}
	return out
}

func TestGuard_ProductionCodeImportsOnlyTheStandardLibrary(t *testing.T) {
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
			t.Errorf("%s imports %s; contrib/imaging/icc may import only the standard library", f, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no file; the guard would pass vacuously")
	}
}

func TestGuard_DetectsAnImagingImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package icc\n\nimport (\n\t\"io\"\n\n\t\"github.com/ubgo/filesystem/contrib/imaging\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := foreignImports(t, path); len(got) != 1 || got[0] != "github.com/ubgo/filesystem/contrib/imaging" {
		t.Fatalf("planted import not detected: %v", got)
	}
}
