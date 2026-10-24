package filesystem_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
	"github.com/ubgo/filesystem/memory"
)

// fsTree is the content the io/fs tests put on a disk.
var fsTree = map[string]string{
	"readme.txt":           "hello",
	"docs/a.txt":           "alpha",
	"docs/b.txt":           "bravo",
	"docs/deep/nested.txt": "deep",
	"empty.txt":            "",
}

func fsDisk(t *testing.T, drv filesystem.Driver) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("media", drv)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range fsTree {
		if _, err := d.WriteBytes(t.Context(), k, []byte(v), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// TestFSPassesFSTest runs the standard library's conformance check on the
// view over every driver kind: memory, local and the Disk's fallbacks
// (basicDriver), and on a sub-directory view.
func TestFSPassesFSTest(t *testing.T) {
	drivers := map[string]func(t *testing.T) filesystem.Driver{
		"memory": func(*testing.T) filesystem.Driver { return memory.New() },
		"basic":  func(*testing.T) filesystem.Driver { return newBasic() },
		"local": func(t *testing.T) filesystem.Driver {
			drv, err := local.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return drv
		},
	}
	for name, mk := range drivers {
		t.Run(name, func(t *testing.T) {
			fsys := fsDisk(t, mk(t)).FS(t.Context())
			if err := fstest.TestFS(fsys, "readme.txt", "docs/a.txt", "docs/b.txt", "docs/deep/nested.txt", "empty.txt"); err != nil {
				t.Fatal(err)
			}
			sub, err := fs.Sub(fsys, "docs")
			if err != nil {
				t.Fatal(err)
			}
			if err := fstest.TestFS(sub, "a.txt", "b.txt", "deep/nested.txt"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFSContentAndInfo(t *testing.T) {
	fsys := fsDisk(t, memory.New()).FS(t.Context())
	b, err := fs.ReadFile(fsys, "docs/deep/nested.txt")
	if err != nil || string(b) != "deep" {
		t.Fatalf("ReadFile = %q, %v", b, err)
	}
	info, err := fs.Stat(fsys, "docs/a.txt")
	if err != nil || info.Name() != "a.txt" || info.Size() != 5 || info.Mode() != 0o444 || info.IsDir() {
		t.Fatalf("Stat = %v, %v", info, err)
	}
	if e, ok := info.Sys().(filesystem.Entry); !ok || e.Key != "docs/a.txt" {
		t.Errorf("Sys = %#v", info.Sys())
	}
	dir, err := fs.Stat(fsys, "docs")
	if err != nil || !dir.IsDir() || dir.Mode() != fs.ModeDir|0o555 || dir.Name() != "docs" {
		t.Errorf("Stat(docs) = %v, %v", dir, err)
	}
	root, err := fs.Stat(fsys, ".")
	if err != nil || !root.IsDir() || root.Name() != "." {
		t.Errorf("Stat(.) = %v, %v", root, err)
	}
	entries, err := fs.ReadDir(fsys, "docs")
	if err != nil || len(entries) != 3 || entries[0].Name() != "a.txt" || entries[2].Name() != "deep" || !entries[2].IsDir() {
		t.Errorf("ReadDir(docs) = %v, %v", entries, err)
	}
	var walked []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			walked = append(walked, p)
		}
		return err
	})
	if len(walked) != len(fsTree) {
		t.Errorf("WalkDir found %v", walked)
	}
}

func TestFSEmptyDiskHasARoot(t *testing.T) {
	d, _ := filesystem.NewDisk("media", memory.New())
	fsys := d.FS(t.Context())
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil || len(entries) != 0 {
		t.Fatalf("ReadDir(.) on an empty disk = %v, %v", entries, err)
	}
	if err := fstest.TestFS(fsys); err != nil {
		t.Fatal(err)
	}
}

// TestFSNamesAreExactlyKeys pins that a name the Disk would clean into
// another key (a backslash, a doubled slash, a traversal) never reaches an
// object, so two names cannot alias one object.
func TestFSNamesAreExactlyKeys(t *testing.T) {
	fsys := fsDisk(t, memory.New()).FS(t.Context())
	for _, name := range []string{`docs\a.txt`, "docs/\x01a.txt"} {
		if _, err := fs.Stat(fsys, name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%q) = %v, want not exist", name, err)
		}
		if _, err := fsys.Open(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(%q) = %v, want not exist", name, err)
		}
	}
	for _, name := range []string{"../etc/passwd", "/abs", "docs//a.txt", "docs/./a.txt", "", "\xffbad"} {
		if _, err := fsys.Open(name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Open(%q) = %v, want invalid", name, err)
		}
	}
}

func TestFSErrors(t *testing.T) {
	fsys := fsDisk(t, memory.New()).FS(t.Context())
	_, err := fs.Stat(fsys, "missing.txt")
	if !errors.Is(err, fs.ErrNotExist) || filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("missing = %v (%s)", err, filesystem.CodeOf(err))
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Op != "stat" || pe.Path != "missing.txt" {
		t.Errorf("not a PathError: %#v", err)
	}
	if _, err := fs.ReadDir(fsys, "readme.txt"); err == nil {
		t.Error("ReadDir of a file succeeded")
	}
	if _, err := fs.ReadDir(fsys, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDir(nope) = %v", err)
	}
	if _, err := fs.ReadFile(fsys, "docs"); err == nil {
		t.Error("ReadFile of a directory succeeded")
	}
	if _, err := fs.ReadFile(fsys, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile(nope) = %v", err)
	}
	dir, _ := fsys.Open("docs")
	if _, err := dir.Read(make([]byte, 1)); err == nil {
		t.Error("Read of a directory succeeded")
	}
	_ = dir.Close()
	if err := dir.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close = %v", err)
	}
	if _, err := dir.(fs.ReadDirFile).ReadDir(-1); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("ReadDir after Close = %v", err)
	}
	f, _ := fsys.Open("readme.txt")
	_ = f.Close() // never read: nothing was opened on the disk
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Read after Close = %v", err)
	}
	if err := f.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close = %v", err)
	}
	// An object deleted between Open and Read.
	d := fsDisk(t, memory.New())
	fsys = d.FS(t.Context())
	f, _ = fsys.Open("readme.txt")
	_ = d.Delete(t.Context(), "readme.txt")
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read of a deleted object = %v", err)
	}
}

// failing is a disk whose operations fail with code after the setup.
func failing(t *testing.T, code filesystem.Code, kinds ...filesystem.OpKind) (*filesystem.Disk, *bool) {
	t.Helper()
	on := new(bool)
	fail := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if *on {
			for _, k := range kinds {
				if op.Kind == k {
					return filesystem.Errorf(code, "injected")
				}
			}
		}
		return call(ctx)
	}
	d, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(fail))
	for k, v := range fsTree {
		_, _ = d.WriteBytes(t.Context(), k, []byte(v), filesystem.WriteOptions{})
	}
	return d, on
}

func TestFSBackendFailures(t *testing.T) {
	d, on := failing(t, filesystem.CodePermission, filesystem.OpStat)
	*on = true
	if _, err := fs.Stat(d.FS(t.Context()), "readme.txt"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("permission = %v", err)
	}
	d, on = failing(t, filesystem.CodeUnavailable, filesystem.OpStat)
	*on = true
	_, err := fs.Stat(d.FS(t.Context()), "readme.txt")
	if err == nil || errors.Is(err, fs.ErrNotExist) || filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("unavailable = %v", err)
	}
	d, on = failing(t, filesystem.CodeUnavailable, filesystem.OpList)
	*on = true
	fsys := d.FS(t.Context())
	if _, err := fs.Stat(fsys, "docs"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("directory probe failure = %v", err)
	}
	*on = false
	dir, err := fsys.Open("docs")
	if err != nil {
		t.Fatal(err)
	}
	*on = true
	if _, err := dir.(fs.ReadDirFile).ReadDir(1); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("listing failure = %v", err)
	}
	if _, err := fs.ReadDir(fsys, "."); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("root listing failure = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fs.ReadFile(fsDisk(t, memory.New()).FS(ctx), "readme.txt"); err == nil {
		t.Error("a cancelled context did not stop the view")
	}
}

// TestFSUsesTheDisksKeyNormalizer pins that names reach the Disk's own key
// rules: a disk whose normaliser folds a name to another key exposes the
// object only under the key it stores, so the view never opens a name the
// disk would store elsewhere.
func TestFSUsesTheDisksKeyNormalizer(t *testing.T) {
	fold := filesystem.WithKeyNormalizer(func(k string) (string, error) { return strings.ToLower(k), nil })
	d, err := filesystem.NewDisk("media", memory.New(), fold)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(t.Context(), "Report.TXT", []byte("q3"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	fsys := d.FS(t.Context())
	if b, err := fs.ReadFile(fsys, "report.txt"); err != nil || string(b) != "q3" {
		t.Fatalf("stored key = %q, %v", b, err)
	}
	if _, err := fs.Stat(fsys, "Report.TXT"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a name the disk folds to another key = %v, want fs.ErrNotExist", err)
	}
}
