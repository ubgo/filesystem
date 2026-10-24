package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

var errInjected = errors.New("injected OS failure")

// faultRoot fails the n-th call to any fileRoot or file method and records
// which method it failed.
type faultRoot struct {
	inner fileRoot
	n     int
	calls int
	fired string
}

func (f *faultRoot) hit(method string) bool {
	f.calls++
	if f.calls == f.n {
		f.fired = method
		return true
	}
	return false
}

func (f *faultRoot) Stat(name string) (fs.FileInfo, error) {
	if f.hit("Stat") {
		return nil, errInjected
	}
	return f.inner.Stat(name)
}
func (f *faultRoot) Lstat(name string) (fs.FileInfo, error) {
	if f.hit("Lstat") {
		return nil, errInjected
	}
	return f.inner.Lstat(name)
}
func (f *faultRoot) Open(name string) (file, error) {
	if f.hit("Open") {
		return nil, errInjected
	}
	h, err := f.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultFile{file: h, root: f}, nil
}
func (f *faultRoot) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	if f.hit("OpenFile") {
		return nil, errInjected
	}
	h, err := f.inner.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultFile{file: h, root: f}, nil
}
func (f *faultRoot) MkdirAll(name string, perm fs.FileMode) error {
	if f.hit("MkdirAll") {
		return errInjected
	}
	return f.inner.MkdirAll(name, perm)
}
func (f *faultRoot) Link(o, n string) error {
	if f.hit("Link") {
		return errInjected
	}
	return f.inner.Link(o, n)
}
func (f *faultRoot) Rename(o, n string) error {
	if f.hit("Rename") {
		return errInjected
	}
	return f.inner.Rename(o, n)
}
func (f *faultRoot) Remove(name string) error {
	if f.hit("Remove") {
		return errInjected
	}
	return f.inner.Remove(name)
}
func (f *faultRoot) RemoveAll(name string) error {
	if f.hit("RemoveAll") {
		return errInjected
	}
	return f.inner.RemoveAll(name)
}
func (f *faultRoot) Chmod(name string, mode fs.FileMode) error {
	if f.hit("Chmod") {
		return errInjected
	}
	return f.inner.Chmod(name, mode)
}
func (f *faultRoot) ReadFile(name string) ([]byte, error) {
	if f.hit("ReadFile") {
		return nil, errInjected
	}
	return f.inner.ReadFile(name)
}
func (f *faultRoot) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if f.hit("WriteFile") {
		return errInjected
	}
	return f.inner.WriteFile(name, data, perm)
}
func (f *faultRoot) Chtimes(name string, a, m time.Time) error {
	if f.hit("Chtimes") {
		return errInjected
	}
	return f.inner.Chtimes(name, a, m)
}
func (f *faultRoot) FS() fs.FS {
	if f.hit("FS") {
		return failingFS{}
	}
	return f.inner.FS()
}
func (f *faultRoot) Close() error { return f.inner.Close() }

// failingFS is what FS returns when that call is the faulted one.
type failingFS struct{}

func (failingFS) Open(string) (fs.File, error) { return nil, errInjected }

type faultFile struct {
	file
	root *faultRoot
}

func (f *faultFile) Read(p []byte) (int, error) {
	if f.root.hit("Read") {
		return 0, errInjected
	}
	return f.file.Read(p)
}
func (f *faultFile) Write(p []byte) (int, error) {
	if f.root.hit("Write") {
		return 0, errInjected
	}
	return f.file.Write(p)
}
func (f *faultFile) Seek(o int64, w int) (int64, error) {
	if f.root.hit("Seek") {
		return 0, errInjected
	}
	return f.file.Seek(o, w)
}
func (f *faultFile) Sync() error {
	if f.root.hit("Sync") {
		return errInjected
	}
	return f.file.Sync()
}
func (f *faultFile) Close() error {
	cerr := f.file.Close()
	if f.root.hit("Close") {
		return errInjected
	}
	return cerr
}

// bestEffort are the calls whose failure an operation deliberately ignores:
// cleanup of staging files and empty directories, and closing read handles
// it is done with. A fault there may leave the operation successful.
var bestEffort = map[string]bool{"Remove": true, "RemoveAll": true, "Close": true}

// op is one driver operation run under fault injection, after seed has
// prepared the directory with a fault-free driver.
type op struct {
	name string
	seed func(ctx context.Context, d *Driver) error
	run  func(ctx context.Context, d *Driver) error
}

func write(ctx context.Context, d *Driver, key, data string, o filesystem.WriteOptions) error {
	_, err := d.Write(ctx, key, strings.NewReader(data), o)
	return err
}

func readAll(ctx context.Context, d *Driver, key string, rng filesystem.ByteRange) error {
	rc, err := d.Read(ctx, key, rng)
	if err != nil {
		return err
	}
	if _, err := io.ReadAll(rc); err != nil {
		_ = rc.Close()
		return err
	}
	return rc.Close()
}

func seedFile(key string) func(context.Context, *Driver) error {
	return func(ctx context.Context, d *Driver) error {
		return write(ctx, d, key, "seed", filesystem.WriteOptions{})
	}
}

func noSeed(context.Context, *Driver) error { return nil }

// multipartSeed starts an upload of "mp.bin" with one part and returns its
// ID through the pointer, for the operations that continue it.
func multipartSeed(id *string, part *filesystem.Part) func(context.Context, *Driver) error {
	return func(ctx context.Context, d *Driver) error {
		var err error
		if *id, err = d.CreateMultipart(ctx, "mp.bin", filesystem.WriteOptions{}); err != nil {
			return err
		}
		*part, err = d.UploadPart(ctx, "mp.bin", *id, 1, strings.NewReader("part one"), 8)
		return err
	}
}

func ops() []op {
	var id string
	var part filesystem.Part
	return []op{
		{"write nested", noSeed, func(ctx context.Context, d *Driver) error {
			return write(ctx, d, "a/b/c.txt", "data", filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic})
		}},
		{"write conditional", noSeed, func(ctx context.Context, d *Driver) error {
			return write(ctx, d, "once.txt", "data", filesystem.WriteOptions{IfNotExists: true})
		}},
		{"overwrite", seedFile("x.txt"), func(ctx context.Context, d *Driver) error {
			return write(ctx, d, "x.txt", "new", filesystem.WriteOptions{})
		}},
		{"read range", seedFile("r.txt"), func(ctx context.Context, d *Driver) error {
			return readAll(ctx, d, "r.txt", filesystem.ByteRange{Offset: 1, Length: 2})
		}},
		{"stat", seedFile("s.txt"), func(ctx context.Context, d *Driver) error {
			_, err := d.Stat(ctx, "s.txt")
			return err
		}},
		{"delete", seedFile("d/e/f.txt"), func(ctx context.Context, d *Driver) error {
			return d.Delete(ctx, "d/e/f.txt")
		}},
		{"list", seedFile("l/m.txt"), func(ctx context.Context, d *Driver) error {
			_, err := d.List(ctx, "l", filesystem.ListOptions{Recursive: true})
			return err
		}},
		{"move", seedFile("from/a.txt"), func(ctx context.Context, d *Driver) error {
			_, err := d.Move(ctx, "from/a.txt", "to/b.txt")
			return err
		}},
		{"set visibility", seedFile("v.txt"), func(ctx context.Context, d *Driver) error {
			return d.SetVisibility(ctx, "v.txt", filesystem.VisibilityPublic)
		}},
		{"delete prefix", seedFile("p/q/r.txt"), func(ctx context.Context, d *Driver) error {
			return d.DeletePrefix(ctx, "p/q")
		}},
		{"create multipart", noSeed, func(ctx context.Context, d *Driver) error {
			_, err := d.CreateMultipart(ctx, "mp.bin", filesystem.WriteOptions{})
			return err
		}},
		{"upload part", multipartSeed(&id, &part), func(ctx context.Context, d *Driver) error {
			_, err := d.UploadPart(ctx, "mp.bin", id, 2, strings.NewReader("two"), 3)
			return err
		}},
		{"list parts", multipartSeed(&id, &part), func(ctx context.Context, d *Driver) error {
			_, err := d.ListParts(ctx, "mp.bin", id)
			return err
		}},
		{"complete multipart", multipartSeed(&id, &part), func(ctx context.Context, d *Driver) error {
			_, err := d.CompleteMultipart(ctx, "mp.bin", id, []filesystem.Part{part})
			return err
		}},
		{"abort multipart", multipartSeed(&id, &part), func(ctx context.Context, d *Driver) error {
			return d.AbortMultipart(ctx, "mp.bin", id)
		}},
		{"append to a new file", noSeed, func(ctx context.Context, d *Driver) error {
			_, err := d.Append(ctx, "log/a.txt", strings.NewReader("line"), filesystem.AppendOptions{})
			return err
		}},
		{"append to a file", seedFile("log/b.txt"), func(ctx context.Context, d *Driver) error {
			_, err := d.Append(ctx, "log/b.txt", strings.NewReader("line"), filesystem.AppendOptions{})
			return err
		}},
		{"append with a size cap", seedFile("log/c.txt"), func(ctx context.Context, d *Driver) error {
			_, err := d.Append(ctx, "log/c.txt", strings.NewReader("line"), filesystem.AppendOptions{MaxSize: 100})
			return err
		}},
		{"set mod time", seedFile("t.txt"), func(ctx context.Context, d *Driver) error {
			return d.SetModTime(ctx, "t.txt", time.Unix(1, 0))
		}},
		{"compare and swap", seedFile("cas.txt"), func(ctx context.Context, d *Driver) error {
			e, err := d.Stat(ctx, "cas.txt")
			if err != nil {
				return err
			}
			return write(ctx, d, "cas.txt", "new", filesystem.WriteOptions{IfMatch: e.ETag})
		}},
		{"find abandoned", multipartSeed(&id, &part), func(ctx context.Context, d *Driver) error {
			found, err := d.FindAbandoned(ctx, time.Now().Add(time.Hour))
			if err == nil && (len(found) != 1 || found[0].Key != "mp.bin") {
				return fmt.Errorf("found %+v, want the upload of mp.bin", found)
			}
			return err
		}},
		{"make dir", seedFile("md/x.txt"), func(ctx context.Context, d *Driver) error {
			return d.MakeDir(ctx, "md/new/deeper")
		}},
		{"make existing dir", seedFile("me/x.txt"), func(ctx context.Context, d *Driver) error {
			return d.MakeDir(ctx, "me")
		}},
		{"dir exists", seedFile("de/x.txt"), func(ctx context.Context, d *Driver) error {
			ok, err := d.DirExists(ctx, "de")
			if err == nil && !ok {
				return errors.New("the directory is not reported")
			}
			return err
		}},
		{"sweep abandoned", multipartSeed(&id, &part), func(ctx context.Context, d *Driver) error {
			n, err := d.SweepAbandoned(ctx, time.Now().Add(time.Hour))
			if err == nil && n != 1 {
				return fmt.Errorf("swept %d uploads, want 1", n)
			}
			return err
		}},
	}
}

// TestEveryOSFailureIsReported injects a failure at every OS call of every
// operation in turn. A failure must surface as an error unless it hit a
// best-effort cleanup call, and whenever cleanup ran normally no staging
// file may be left behind.
func TestEveryOSFailureIsReported(t *testing.T) {
	for _, o := range ops() {
		t.Run(o.name, func(t *testing.T) {
			for n := 1; ; n++ {
				dir := t.TempDir()
				clean, err := New(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := o.seed(t.Context(), clean); err != nil {
					t.Fatalf("seed: %v", err)
				}
				_ = clean.Close()

				native, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				fr := &faultRoot{inner: osRoot{native}, n: n}
				d := &Driver{filePublic: DefaultFilePublic, filePrivate: DefaultFilePrivate, dirMode: DefaultDirMode, native: native}
				if _, err := d.init(fr); err != nil {
					if fr.fired == "" {
						t.Fatalf("init failed without a fault: %v", err)
					}
					continue
				}
				err = o.run(t.Context(), d)
				_ = d.Close()
				if fr.fired == "" {
					if err != nil {
						t.Fatalf("%s failed without a fault: %v", o.name, err)
					}
					return
				}
				if err == nil && !bestEffort[fr.fired] {
					t.Fatalf("%s succeeded although OS call %d (%s) failed", o.name, n, fr.fired)
				}
				if !bestEffort[fr.fired] {
					if left, _ := os.ReadDir(filepath.Join(dir, tmpDir)); len(left) != 0 {
						t.Fatalf("%s left %d staging files after %s failed", o.name, len(left), fr.fired)
					}
				}
			}
		})
	}
}

func TestInitFailureClosesRoot(t *testing.T) {
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fr := &faultRoot{inner: osRoot{native}, n: 1}
	d := &Driver{dirMode: DefaultDirMode}
	if _, err := d.init(fr); !errors.Is(err, errInjected) {
		t.Fatalf("init = %v", err)
	}
	if _, err := native.Stat("."); err == nil {
		t.Error("root left open after init failed")
	}
}

func TestOSRootAdapterErrors(t *testing.T) {
	native, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = native.Close() }()
	r := osRoot{native}
	if _, err := r.Open("missing"); err == nil {
		t.Error("Open of a missing file succeeded")
	}
	if _, err := r.OpenFile("missing/deeper", os.O_RDONLY, 0); err == nil {
		t.Error("OpenFile of a missing file succeeded")
	}
}

// infoFailFS lists real directory entries whose Info fails, as when a file
// is deleted between the directory read and the stat.
type infoFailFS struct{ fs.FS }

func (f infoFailFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	for i, e := range entries {
		entries[i] = infoFailEntry{e}
	}
	return entries, err
}

type infoFailEntry struct{ fs.DirEntry }

func (infoFailEntry) Info() (fs.FileInfo, error) { return nil, errInjected }

type infoFailRoot struct{ fileRoot }

func (r infoFailRoot) FS() fs.FS { return infoFailFS{r.fileRoot.FS()} }

func TestListReportsVanishingFiles(t *testing.T) {
	dir := t.TempDir()
	clean, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := write(t.Context(), clean, "a.txt", "x", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	d := &Driver{dirMode: DefaultDirMode, root: infoFailRoot{clean.root}}
	if _, err := d.List(t.Context(), "", filesystem.ListOptions{}); !errors.Is(err, errInjected) {
		t.Errorf("List = %v", err)
	}
	_ = clean.Close()
}

func TestNewOnUnopenableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any directory")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := New(dir); err == nil {
		t.Error("New opened a directory without permissions")
	}
}
