package local

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// syncRoot counts the fsyncs the driver makes: of files it writes
// (OpenFile) and of directories it opens to sync (Open).
type syncRoot struct {
	fileRoot
	fileSyncs, dirSyncs *[]string
	lstatErr            error
}

func (r syncRoot) Open(name string) (file, error) {
	f, err := r.fileRoot.Open(name)
	if err != nil {
		return nil, err
	}
	return countedFile{file: f, name: name, syncs: r.dirSyncs}, nil
}

func (r syncRoot) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	f, err := r.fileRoot.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return countedFile{file: f, name: name, syncs: r.fileSyncs}, nil
}

func (r syncRoot) Lstat(name string) (fs.FileInfo, error) {
	if r.lstatErr != nil {
		return nil, r.lstatErr
	}
	return r.fileRoot.Lstat(name)
}

type countedFile struct {
	file
	name  string
	syncs *[]string
}

func (f countedFile) Sync() error {
	*f.syncs = append(*f.syncs, f.name)
	return f.file.Sync()
}

// syncDriver opens dir with durability v on system goos, counting syncs.
func syncDriver(t *testing.T, v Durability, goos string) (*Driver, *[]string, *[]string) {
	t.Helper()
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, dirs := &[]string{}, &[]string{}
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native, durability: v, goos: goos}
	if _, err := d.init(syncRoot{fileRoot: osRoot{native}, fileSyncs: files, dirSyncs: dirs}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, files, dirs
}

func TestDurabilityLevels(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		v         Durability
		goos      string
		fileSyncs int
		dirs      []string // synced by the first write of a/b/c.txt
		again     []string // synced by its overwrite
	}{
		{DurabilityFull, "linux", 1, []string{"a/b", "a", "."}, []string{"a/b"}},
		{DurabilityFull, goosWindows, 1, nil, nil},
		{DurabilityFile, "linux", 1, nil, nil},
		{DurabilityNone, "linux", 0, nil, nil},
	}
	for _, c := range cases {
		t.Run(string(c.v)+"/"+c.goos, func(t *testing.T) {
			d, files, dirs := syncDriver(t, c.v, c.goos)
			*files, *dirs = nil, nil
			if err := write(ctx, d, "a/b/c.txt", "one", filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if len(*files) != c.fileSyncs || strings.Join(*dirs, ",") != strings.Join(c.dirs, ",") {
				t.Errorf("first write synced files %v and dirs %v, want %d files and %v", *files, *dirs, c.fileSyncs, c.dirs)
			}
			*files, *dirs = nil, nil
			if err := write(ctx, d, "a/b/c.txt", "two", filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(*dirs, ",") != strings.Join(c.again, ",") {
				t.Errorf("overwrite synced dirs %v, want %v", *dirs, c.again)
			}
			// Appending to an existing file syncs it (unless none) and no
			// directory: its entry does not change.
			*files, *dirs = nil, nil
			if _, err := d.Append(ctx, "a/b/c.txt", strings.NewReader("+"), filesystem.AppendOptions{}); err != nil {
				t.Fatal(err)
			}
			if len(*dirs) != 0 || (len(*files) == 0) != (c.v == DurabilityNone) {
				t.Errorf("append synced files %v and dirs %v", *files, *dirs)
			}
			if err := expectContent(ctx, d, "a/b/c.txt", "two+"); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestDurabilityOfMoveAndMakeDir(t *testing.T) {
	ctx := t.Context()
	d, _, dirs := syncDriver(t, DurabilityFull, "linux")
	if err := write(ctx, d, "src/a.txt", "x", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	*dirs = nil
	if _, err := d.Move(ctx, "src/a.txt", "dst/new/b.txt"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*dirs, ","); got != "dst/new,dst,." {
		t.Errorf("move synced %s", got)
	}
	*dirs = nil
	if err := d.MakeDir(ctx, "m/n"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*dirs, ","); got != "m,." {
		t.Errorf("MakeDir synced %s", got)
	}
}

func expectContent(ctx context.Context, d *Driver, key, want string) error {
	rc, err := d.Read(ctx, key, filesystem.ByteRange{})
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if string(b) != want {
		return errors.New("content " + string(b) + ", want " + want)
	}
	return nil
}

func TestUnknownDurabilityIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never")
	_, err := New(dir, WithDurability("fsync-sometimes"))
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("New: %v", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a refused New created its directory (%v)", statErr)
	}
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, durability: "bogus"}
	if _, err := d.init(osRoot{native}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("init: %v", err)
	}
	if _, err := native.Stat("."); err == nil {
		t.Error("a refused init left its root open")
	}
	for _, v := range DurabilityValues {
		d, err := New(t.TempDir(), WithDurability(v))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		_ = d.Close()
	}
}

func TestMakeDirAndDirExists(t *testing.T) {
	ctx := t.Context()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := write(ctx, d, "f.txt", "x", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.MakeDir(ctx, "f.txt"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Errorf("MakeDir over a file: %v", err)
	}
	if err := d.MakeDir(ctx, "f.txt/sub"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("MakeDir under a file: %v", err)
	}
	for _, reserved := range []string{StagingDir, StagingDir + "/x"} {
		if err := d.MakeDir(ctx, reserved); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("MakeDir(%q): %v", reserved, err)
		}
		if _, err := d.DirExists(ctx, reserved); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
			t.Errorf("DirExists(%q): %v", reserved, err)
		}
	}
	if err := d.MakeDir(ctx, "CON"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("MakeDir of a reserved Windows name: %v", err)
	}
	for dir, want := range map[string]bool{"f.txt": false, "f.txt/sub": false, "none": false} {
		if ok, err := d.DirExists(ctx, dir); ok != want || err != nil {
			t.Errorf("DirExists(%q) = %v, %v", dir, ok, err)
		}
	}
	// An empty directory goes with the last key deleted under it.
	if err := d.MakeDir(ctx, "e"); err != nil {
		t.Fatal(err)
	}
	if err := write(ctx, d, "e/k.txt", "x", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "e/k.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.DirExists(ctx, "e"); ok {
		t.Error("the directory outlived its last key")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.MakeDir(cancelled, "c"); !errors.Is(err, context.Canceled) {
		t.Errorf("MakeDir on a cancelled context: %v", err)
	}
	if _, err := d.DirExists(cancelled, "c"); !errors.Is(err, context.Canceled) {
		t.Errorf("DirExists on a cancelled context: %v", err)
	}
}

func TestDirLstatFailures(t *testing.T) {
	ctx := t.Context()
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native}
	if _, err := d.init(osRoot{native}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	d.root = syncRoot{fileRoot: d.root, fileSyncs: &[]string{}, dirSyncs: &[]string{}, lstatErr: errInjected}
	if err := d.MakeDir(ctx, "x"); !errors.Is(err, errInjected) {
		t.Errorf("MakeDir: %v", err)
	}
	if _, err := d.DirExists(ctx, "x"); !errors.Is(err, errInjected) {
		t.Errorf("DirExists: %v", err)
	}
	if _, err := d.syncStop("a/b"); !errors.Is(err, errInjected) {
		t.Errorf("syncStop: %v", err)
	}
}

func TestFindAbandonedReadsUploadKeys(t *testing.T) {
	ctx := t.Context()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	gone, err := d.CreateMultipart(ctx, "gone.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	damaged, err := d.CreateMultipart(ctx, "damaged.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.root.Remove(uploadDir(gone) + "/" + metaFile); err != nil {
		t.Fatal(err)
	}
	if err := d.root.WriteFile(uploadDir(damaged)+"/"+metaFile, []byte("{"), DefaultFilePrivate); err != nil {
		t.Fatal(err)
	}
	if k, err := d.uploadKey(gone); k != "" || err != nil {
		t.Errorf("key of an upload without metadata = %q, %v", k, err)
	}
	if k, err := d.uploadKey(damaged); k != "" || err != nil {
		t.Errorf("key of an upload with damaged metadata = %q, %v", k, err)
	}
}
