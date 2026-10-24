package fsent_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowed reports whether production code of this module may import p:
// the standard library, ent's own packages, the core and records. A SQL
// driver would be forced on every application; it brings its own.
func allowed(p string) bool {
	switch {
	case !strings.Contains(strings.SplitN(p, "/", 2)[0], "."):
		return true
	case p == "entgo.io/ent" || strings.HasPrefix(p, "entgo.io/ent/"):
		return true
	}
	return p == "github.com/ubgo/filesystem" || p == "github.com/ubgo/filesystem/records"
}

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
		if !allowed(p) {
			out = append(out, p)
		}
	}
	return out
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
			t.Errorf("%s imports %s; contrib/ent may import only ent and the records packages", f, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no file; the guard would pass vacuously")
	}
}

func TestGuard_DetectsADriverImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package fsent\n\nimport (\n\t\"entgo.io/ent\"\n\n\t_ \"modernc.org/sqlite\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := foreignImports(t, path); len(got) != 1 || got[0] != "modernc.org/sqlite" {
		t.Fatalf("planted import not detected: %v", got)
	}
}
