package webp_test

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
// module may have: the imaging package it plugs into and the one codec it
// wraps. An application adding WebP output must not inherit records, a
// SQL driver or a second codec.
var allowedImports = []string{"github.com/ubgo/filesystem/contrib/imaging", "github.com/gen2brain/webp"}

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

func TestGuard_ProductionCodeImportsOnlyImagingAndTheCodec(t *testing.T) {
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
			t.Errorf("%s imports %s; contrib/imaging/webp may import only the standard library, contrib/imaging and its codec", f, bad)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no file; the guard would pass vacuously")
	}
}

func TestGuard_DetectsARecordsImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package webp\n\nimport (\n\t\"io\"\n\n\t\"github.com/ubgo/filesystem/records\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := foreignImports(t, path); len(got) != 1 || got[0] != "github.com/ubgo/filesystem/records" {
		t.Fatalf("planted import not detected: %v", got)
	}
}
