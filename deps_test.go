package filesystem_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// modulePath is this module's import path; its own packages may import each
// other.
const modulePath = "github.com/ubgo/filesystem"

// requireLine matches a require directive or a line inside a require block.
var requireLine = regexp.MustCompile(`^\s*(require\s+)?[a-zA-Z0-9.\-_]+\.[a-zA-Z0-9.\-_/]+\s+v\S+`)

// TestGuard_CoreModuleRequiresNothing keeps the core module free of every
// third-party dependency: an application that imports only the core, local
// and memory packages must not pull in any module. A package that needs one
// belongs in a module of its own (adapters/*, records, contrib/*).
func TestGuard_CoreModuleRequiresNothing(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if bad := requires(string(b)); len(bad) > 0 {
		t.Errorf("the core module requires %q; move the package that needs it into its own module", bad)
	}
}

func requires(gomod string) []string {
	var out []string
	for _, line := range strings.Split(gomod, "\n") {
		if requireLine.MatchString(line) {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// TestGuard_CorePackagesImportOnlyTheStandardLibrary parses every Go file of
// the core module (skipping the directories that are separate modules) and
// fails on any import that is neither the standard library nor this module.
// It reads imports with go/parser, so multi-line import blocks and aliases
// cannot slip past it the way they slip past a line-based scan.
func TestGuard_CorePackagesImportOnlyTheStandardLibrary(t *testing.T) {
	bad, scanned, err := foreignImports(".")
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files; the guard would pass vacuously", scanned)
	}
	for _, b := range bad {
		t.Errorf("core file imports a third-party package: %s", b)
	}
}

// TestGuard_DetectsAForeignImport proves the import guard can fail: it plants
// a file with a third-party import inside a multi-line import block and
// requires the guard to report it.
func TestGuard_DetectsAForeignImport(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n\nimport (\n\t\"fmt\"\n\n\tx \"example.com/vendor/sdk\"\n)\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bad, _, err := foreignImports(dir)
	if err != nil || len(bad) != 1 || !strings.Contains(bad[0], "example.com/vendor/sdk") {
		t.Errorf("planted foreign import not detected: %v %v", bad, err)
	}
	if got := requires("module m\n\nrequire (\n\tgithub.com/a/b v1.0.0\n)\nrequire c.io/d v0.1.0\n"); len(got) != 2 {
		t.Errorf("require detection = %q", got)
	}
}

func foreignImports(root string) ([]string, int, error) {
	var bad []string
	scanned := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // a separate module
				}
				if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		scanned++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && path != modulePath && !strings.HasPrefix(path, modulePath+"/") {
				bad = append(bad, p+": "+path)
			}
		}
		return nil
	})
	return bad, scanned, err
}
