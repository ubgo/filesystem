package jobs_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// allowedImports are the only non-standard imports the jobs package may
// have: the core, the records module and sqlb, which records already
// requires (Scrub and MigrateDisk walk the files table with it). Runners
// (River, Asynq) live in their own modules under jobs/, so an application
// using the in-process scheduler never downloads them.
var allowedImports = []string{"github.com/ubgo/filesystem", "github.com/ubgo/sqlb"}

// deniedImports are module packages production code must not import even
// though the prefix allows them: the runner adapters, and the records test
// helper that pulls in SQL drivers. diskjobs, the records-free half (a
// module of its own), is the one jobs/ package jobs may import.
var deniedImports = []string{"github.com/ubgo/filesystem/jobs/river", "github.com/ubgo/filesystem/jobs/asynq", "github.com/ubgo/filesystem/records/recordstest"}

// runnerModules are the runner adapters' modules. They take diskjobs
// types only, so their go.mod files must not require records or sqlb: an
// application that schedules disk-only jobs on River or Asynq builds none
// of the SQL stack. (diskjobs' own guard lives in its module.)
var runnerModules = []string{"river/go.mod", "asynq/go.mod"}

// runnerDenied are the modules a runner's go.mod must not require.
var runnerDenied = []string{"github.com/ubgo/filesystem/records", "github.com/ubgo/sqlb", "github.com/ubgo/filesystem/jobs "}

func TestGuard_RunnersRequireNoRecords(t *testing.T) {
	for _, path := range runnerModules {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bad := deniedRequires(string(b)); len(bad) > 0 {
			t.Errorf("%s requires %v; a runner may require the core and diskjobs only", path, bad)
		}
	}
	// The scan sees a planted indirect records requirement.
	planted := "require (\n\tgithub.com/ubgo/filesystem/jobs/diskjobs v0.0.0\n\tgithub.com/ubgo/filesystem/records v0.0.0 // indirect\n)\n"
	if got := deniedRequires(planted); len(got) != 1 {
		t.Errorf("planted records requirement: got %v", got)
	}
}

// deniedRequires lists gomod's lines that require a runnerDenied module.
// Each denied entry is matched as a line prefix after the indentation, so
// "github.com/ubgo/filesystem/jobs " (with its space) names the jobs
// module and not jobs/diskjobs.
func deniedRequires(gomod string) []string {
	var bad []string
	for _, line := range strings.Split(gomod, "\n") {
		l := strings.TrimPrefix(strings.TrimSpace(line), "require ")
		for _, d := range runnerDenied {
			if strings.HasPrefix(l, d) && !strings.Contains(l, "=>") {
				bad = append(bad, l)
			}
		}
	}
	return bad
}

func TestGuard_JobsImportsOnlyTheCoreAndRecords(t *testing.T) {
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
			t.Errorf("%s imports %s; the jobs package may import only the standard library, the core and records", f, bad)
		}
	}
	if scanned < 2 {
		t.Fatalf("scanned %d files; the guard would pass vacuously", scanned)
	}
}

func TestGuard_DetectsARunnerImport(t *testing.T) {
	for _, imp := range []string{"github.com/riverqueue/river", "github.com/ubgo/filesystem/jobs/river", "github.com/ubgo/filesystem/records/recordstest"} {
		path := filepath.Join(t.TempDir(), "x.go")
		src := "package jobs\n\nimport (\n\t\"context\"\n\n\t\"github.com/ubgo/filesystem\"\n\t_ \"" + imp + "\"\n)\n"
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := foreignImports(t, path); len(got) != 1 || got[0] != imp {
			t.Errorf("planted import of %s: got %v", imp, got)
		}
	}
}

func foreignImports(t *testing.T, path string) []string {
	t.Helper()
	return importsOutside(t, path, allowedImports, deniedImports)
}

// importsOutside lists path's non-standard imports that no allowed prefix
// covers or that a denied prefix names.
func importsOutside(t *testing.T, path string, allowedImports, deniedImports []string) []string {
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
