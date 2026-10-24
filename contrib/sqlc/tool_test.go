package sqlc_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/sqlc"
)

// sqlcTool is sqlc, pinned: the test runs it with go run, so it needs no
// install and every machine checks with the same version. Bump it
// deliberately; a newer sqlc may read the schema differently.
const sqlcTool = "github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1"

// networkErrors are fragments of go's errors when it cannot fetch a
// module: the only reason the test may skip.
var networkErrors = []string{"dial tcp", "no such host", "i/o timeout", "connection refused", "proxy.golang.org", "TLS handshake", "network is unreachable"}

// TestSqlcGenerate writes FS out, runs the pinned sqlc's generate for
// every engine and builds the generated Go: the proof that sqlc accepts
// the schema and every query. For code generation the build is the test,
// so it never skips because sqlc is not installed; it skips only when go
// cannot download sqlc (no network), and says so. The first run builds
// sqlc (about a minute); later runs use the build cache.
//
// It does the same for FSWith(prefixed) under prefixed/, the project for
// a Store built with WithTablePrefix.
func TestSqlcGenerate(t *testing.T) {
	dir := t.TempDir()
	mapped, err := sqlc.FSWith(prefixed)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]fs.FS{".": sqlc.FS(), "prefixed": mapped}
	for root, fsys := range roots {
		if err := writeFS(fsys, filepath.Join(dir, root)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fsdbcheck\n\ngo 1.26.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := goCommand(dir, "run", sqlcTool, "version"); err != nil {
		for _, n := range networkErrors {
			if strings.Contains(string(out), n) {
				t.Skipf("no network to download %s, so sqlc generate was not run (the schema and queries were still checked against the Store): %s", sqlcTool, out)
			}
		}
		t.Fatalf("go run %s: %v\n%s", sqlcTool, err, out)
	}
	for root := range roots {
		for _, engine := range []string{sqlc.EnginePostgres, sqlc.EngineMySQL, sqlc.EngineSQLite} {
			if out, err := goCommand(filepath.Join(dir, root, engine), "run", sqlcTool, "generate"); err != nil {
				t.Fatalf("sqlc generate for %s/%s: %v\n%s", root, engine, err, out)
			}
			gen, err := os.ReadFile(filepath.Join(dir, root, engine, "fsdb", "queries.sql.go"))
			if err != nil {
				t.Fatalf("sqlc generated no queries for %s/%s: %v", root, engine, err)
			}
			if root == "prefixed" && !strings.Contains(string(gen), "FROM app_fs_files") {
				t.Fatalf("%s/%s: the generated queries do not name the prefixed tables", root, engine)
			}
		}
	}
	if out, err := goCommand(dir, "build", "./..."); err != nil {
		t.Fatalf("the generated code does not build: %v\n%s", err, out)
	}
}

// writeFS writes every file of fsys under dir.
func writeFS(fsys fs.FS, dir string) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, p), b, 0o600)
	})
}

// goCommand runs go in dir, outside any workspace.
func goCommand(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	return cmd.CombinedOutput()
}
