package video_test

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

// allowedImports are the only non-standard imports production code of this
// module may have: the library's own packages, and sqlb, which records
// already requires, for the query that tells SweepOrphans which posters
// are owned (owned.go). FFmpeg is reached through os/exec, never through a
// Go binding or cgo.
var allowedImports = []string{"github.com/ubgo/filesystem", "github.com/ubgo/sqlb"}

// deniedImports are library packages production code must not import even
// though allowedImports' prefix covers them: they register SQL drivers.
var deniedImports = []string{"github.com/ubgo/filesystem/records/recordstest"}

func TestGuard_ProductionCodeImportsOnlyTheLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := filepath.Glob(filepath.Join("ffmpeg", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, runner...)
	if !slices.Contains(files, filepath.Join("ffmpeg", "ffmpeg.go")) {
		t.Fatal("the guard does not scan the ffmpeg package")
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		scanned++
		for _, bad := range foreignImports(t, f) {
			t.Errorf("%s imports %s; this module may import only the standard library, github.com/ubgo/filesystem and github.com/ubgo/sqlb", f, bad)
		}
	}
	if scanned < 3 {
		t.Fatalf("scanned %d files; the guard would pass vacuously", scanned)
	}
}

func TestGuard_DetectsAForeignImport(t *testing.T) {
	for _, imp := range []string{"github.com/u2takey/ffmpeg-go", deniedImports[0]} {
		path := filepath.Join(t.TempDir(), "x.go")
		src := "package video\n\nimport (\n\t\"os/exec\"\n\n\t\"github.com/ubgo/filesystem\"\n\t_ \"" + imp + "\"\n)\n"
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
