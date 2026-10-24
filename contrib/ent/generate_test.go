package fsent_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// checkModule is the module path of testdata/entcheck, the application
// the test generates a client for.
const checkModule = "entcheck"

// entTool is ent's code generator, run with go tool: go.mod pins it with
// a tool directive (and a golang.org/x/tools new enough for this Go; the
// one ent v0.14.6 itself requires fails with "package without types"),
// and the temporary module copies that go.mod, so the generator is the
// same version on every machine and needs no install.
const entTool = "ent"

// networkErrors are fragments of go's errors when it cannot fetch a
// module: the only reason the test may skip.
var networkErrors = []string{"dial tcp", "no such host", "i/o timeout", "connection refused", "proxy.golang.org", "TLS handshake", "network is unreachable"}

// goCommand runs go in dir, outside any workspace, letting it add the
// requirements testdata's imports need from the copied go.sum.
func goCommand(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	return cmd.CombinedOutput()
}

// TestGenerateBuildAndRead is the build-is-the-test proof: it copies
// testdata/entcheck (two schemas mixing in fsent, and a main.go that reads
// with the generated client) to a temporary module, runs the pinned ent
// generator on the schemas, builds and runs the program against a SQLite
// database the Store wrote, and compares what it read with the Store. It
// also checks that the mixins' hook refuses a write. A mixin whose field,
// edge or annotation ent rejects fails here, not in an application. It
// skips only when go cannot download the generator (no network), and
// says so.
func TestGenerateBuildAndRead(t *testing.T) {
	if recordstest.Engine() != recordstest.SQLite {
		t.Skip("the generated client is run on SQLite; TestFieldsMatchTables checks the fields on every engine")
	}
	ctx := t.Context()
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", checkModule), dir)
	writeModule(t, dir)

	if out, err := goCommand(dir, "tool", entTool, "generate", "./ent/schema"); err != nil {
		for _, n := range networkErrors {
			if strings.Contains(string(out), n) {
				t.Skipf("no network to download %s, so the client was not generated (TestFieldsMatchTables still checked the fields): %s", entTool, out)
			}
		}
		t.Fatalf("ent rejected the fsent mixins: %v\n%s", err, out)
	}

	path := filepath.Join(dir, "records.db")
	s, err := newFileStore(t, path)
	if err != nil {
		t.Fatal(err)
	}
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}
	var ids []string
	for _, body := range []string{"one", "two", "three"} {
		f, err := s.Upload(ctx, "t", "media", strings.NewReader(body), body+".txt", policy)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.ID)
	}
	post := records.Owner{Type: "post", ID: "p1"}
	for _, id := range []string{ids[2], ids[0]} {
		if _, err := s.Attach(ctx, "t", id, post, "gallery"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Trash(ctx, "t", ids[1]); err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	page, err := s.Files(ctx, records.FileQuery{Scope: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range page.Files {
		fmt.Fprintf(&want, "file %s %s %d %s 1\n", f.ID, f.Filename, f.Size, f.SHA256)
	}
	attached, err := s.Attachments(ctx, "t", post, "gallery")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attached {
		fmt.Fprintf(&want, "attachment %s %d %s\n", a.Attachment.ID, a.Attachment.Position, a.File.ID)
	}
	want.WriteString("delete refused: true\n")

	out, err := goCommand(dir, "run", ".", path)
	if err != nil {
		t.Fatalf("the generated client did not build or run: %v\n%s", err, out)
	}
	if string(out) != want.String() {
		t.Fatalf("the generated client read:\n%s\nthe Store says:\n%s", out, want.String())
	}
	if n, err := s.Files(ctx, records.FileQuery{Scope: "t"}); err != nil || len(n.Files) != 2 {
		t.Fatalf("the refused delete changed the rows: %v, %v", n, err)
	}
}

// copyTree copies the files under src to dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dst, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// writeModule gives dir a go.mod requiring what this module requires and
// replacing fsent and the records modules with the working tree, plus
// this module's go.sum, so the build needs no network.
func writeModule(t *testing.T, dir string) {
	t.Helper()
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(here, "..", "..")
	mod, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	text := string(mod)
	text = strings.Replace(text, "module github.com/ubgo/filesystem/contrib/ent", "module "+checkModule, 1)
	oldReplace := "replace (\n\tgithub.com/ubgo/filesystem => ../../\n\tgithub.com/ubgo/filesystem/records => ../../records\n)\n"
	if !strings.Contains(text, oldReplace) {
		t.Fatalf("go.mod has no replace block to rewrite:\n%s", text)
	}
	text = strings.Replace(text, oldReplace, "require github.com/ubgo/filesystem/contrib/ent v0.0.0-00010101000000-000000000000\n\n"+
		"replace (\n\tgithub.com/ubgo/filesystem => "+root+"\n\tgithub.com/ubgo/filesystem/records => "+filepath.Join(root, "records")+
		"\n\tgithub.com/ubgo/filesystem/contrib/ent => "+here+"\n)\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile("go.sum")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o600); err != nil {
		t.Fatal(err)
	}
}
