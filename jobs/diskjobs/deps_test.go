package diskjobs_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// corePath is the only non-standard module diskjobs may import or require:
// diskjobs is its own module so that an application without records (and
// the River and Asynq runners) builds none of records, sqlb or the SQL
// drivers.
const corePath = "github.com/ubgo/filesystem"

// requireLine matches a require directive or a line inside a require block.
var requireLine = regexp.MustCompile(`^\s*(require\s+)?([a-zA-Z0-9.\-_]+\.[a-zA-Z0-9.\-_/]+)\s+v\S+`)

func TestGuard_ModuleRequiresOnlyTheCore(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	got := requiredModules(string(b))
	if len(got) != 1 || got[0] != corePath {
		t.Errorf("diskjobs requires %v; it may require the core module only", got)
	}
	// The scan sees a planted records requirement inside a require block.
	planted := "module x\n\nrequire (\n\tgithub.com/ubgo/filesystem v0.0.0\n\tgithub.com/ubgo/filesystem/records v0.0.0 // indirect\n)\n"
	if got := requiredModules(planted); len(got) != 2 {
		t.Errorf("planted records requirement: got %v", got)
	}
}

func TestGuard_ImportsOnlyTheCore(t *testing.T) {
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
		for _, bad := range importsOutsideCore(t, f) {
			t.Errorf("%s imports %s; diskjobs may import only the standard library and the core module", f, bad)
		}
	}
	if scanned < 2 {
		t.Fatalf("scanned %d files; the guard would pass vacuously", scanned)
	}
	// The guard sees a planted records import, multi-line block and all.
	path := filepath.Join(t.TempDir(), "x.go")
	src := "package diskjobs\n\nimport (\n\t\"context\"\n\n\t\"github.com/ubgo/filesystem\"\n\t_ \"github.com/ubgo/filesystem/records\"\n)\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := importsOutsideCore(t, path); len(got) != 1 {
		t.Errorf("planted records import: got %v", got)
	}
}

func requiredModules(gomod string) []string {
	var out []string
	for _, line := range strings.Split(gomod, "\n") {
		if m := requireLine.FindStringSubmatch(line); m != nil {
			out = append(out, m[2])
		}
	}
	return out
}

// notCore lists the paths under the core module's import path that are
// modules of their own and so do not count as "the core".
var notCore = []string{corePath + "/records", corePath + "/jobs", corePath + "/adapters", corePath + "/contrib"}

// importsOutsideCore lists path's non-standard imports outside the core
// module.
func importsOutsideCore(t *testing.T, path string) []string {
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
		ok := p == corePath || strings.HasPrefix(p, corePath+"/")
		for _, d := range notCore {
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
