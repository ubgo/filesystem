package local_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
)

func TestWindowsReservedNames(t *testing.T) {
	ctx := t.Context()
	_, d := open(t, t.TempDir())
	cases := map[string]bool{
		"CON":             true,
		"con.txt":         true,
		"docs/Nul.tar.gz": true,
		"com1":            true,
		"LPT9.log":        true,
		"aux /x":          true,
		"report.":         true,
		"dir./a.txt":      true,
		"name ":           true,
		"console.txt":     false,
		"com10.txt":       false,
		"lpt.txt":         false,
		"a.b.c":           false,
		"CONTRACT.pdf":    false,
	}
	for key, reserved := range cases {
		_, err := d.WriteBytes(ctx, key, []byte("x"), filesystem.WriteOptions{})
		if got := filesystem.CodeOf(err) == filesystem.CodeInvalidKey; got != reserved {
			t.Errorf("write %q: %v, want reserved=%v", key, err, reserved)
		}
	}
	// Every operation that could create one refuses it too.
	_, appendErr := d.Append(ctx, "nul.log", strings.NewReader("x"), filesystem.AppendOptions{})
	_, _ = d.WriteBytes(ctx, "ok.txt", []byte("x"), filesystem.WriteOptions{})
	_, moveErr := d.Move(ctx, "ok.txt", "aux.txt")
	_, mpErr := d.CreateMultipart(ctx, "com1.bin", filesystem.WriteOptions{})
	for _, err := range []error{appendErr, moveErr, mpErr} {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("creating a reserved key: %v", err)
		}
	}
	// A file that already has such a name (copied in from elsewhere) can
	// be found, read, renamed away and deleted.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "con.txt"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, legacy := open(t, dir)
	if got, err := legacy.ReadAll(ctx, "con.txt"); err != nil || string(got) != "legacy" {
		t.Errorf("read existing reserved name: %q, %v", got, err)
	}
	if _, err := legacy.Move(ctx, "con.txt", "console.txt"); err != nil {
		t.Errorf("rename away: %v", err)
	}
	if err := legacy.SetModTime(ctx, "console.txt", time.Now()); err != nil {
		t.Error(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "report."), []byte("x"), 0o600)
	if err := legacy.Delete(ctx, "report."); err != nil {
		t.Errorf("delete existing reserved name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.")); err == nil {
		t.Error("existing reserved name not deleted")
	}
	var err error
	// Allowed on request, except on Windows, where they cannot be created.
	_, allowed := open(t, t.TempDir(), local.AllowReservedNames())
	_, err = allowed.WriteBytes(ctx, "con.txt", []byte("x"), filesystem.WriteOptions{})
	if runtime.GOOS != "windows" && err != nil {
		t.Errorf("AllowReservedNames: %v", err)
	}
}

func TestCaseProbe(t *testing.T) {
	ctx := t.Context()
	_, d := open(t, t.TempDir())
	_, _ = d.WriteBytes(ctx, "Probe.txt", []byte("upper"), filesystem.WriteOptions{})
	_, err := d.Stat(ctx, "probe.txt")
	folds := err == nil
	if d.Capabilities().CaseInsensitive != folds {
		t.Errorf("CaseInsensitive = %v, but the filesystem folds case: %v", d.Capabilities().CaseInsensitive, folds)
	}
}
