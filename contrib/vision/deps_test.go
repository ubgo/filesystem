package vision_test

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
// module may have: the library's own packages. Poppler is reached through
// os/exec, never through a Go binding or cgo.
var allowedImports = []string{"github.com/ubgo/filesystem"}

// deniedImports are library packages production code must not import even
// though allowedImports' prefix covers them: they register SQL drivers.
var deniedImports = []string{"github.com/ubgo/filesystem/records/recordstest"}

func TestGuard_ProductionCodeImportsOnlyTheLibrary(t *testing.T) {
	files, err := globAll("*.go", "google/*.go")
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
			t.Errorf("%s imports %s; this module may import only the standard library and github.com/ubgo/filesystem", f, bad)
		}
	}
	if scanned < 3 {
		t.Fatalf("scanned %d files; the guard would pass vacuously", scanned)
	}
}

func TestGuard_DetectsAForeignImport(t *testing.T) {
	for _, imp := range []string{"cloud.google.com/go/vision/v2/apiv1", deniedImports[0]} {
		path := filepath.Join(t.TempDir(), "x.go")
		src := "package vision\n\nimport (\n\t\"os/exec\"\n\n\t\"github.com/ubgo/filesystem\"\n\t_ \"" + imp + "\"\n)\n"
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

// globAll is the files matching any of patterns: the module's packages.
func globAll(patterns ...string) ([]string, error) {
	var out []string
	for _, p := range patterns {
		m, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		out = append(out, m...)
	}
	return out, nil
}
