package local_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
)

func open(t *testing.T, dir string, opts ...local.Option) (*local.Driver, *filesystem.Disk) {
	t.Helper()
	drv, err := local.New(dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	d, err := filesystem.NewDisk("local", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return drv, d
}

func wantCode(t *testing.T, err error, code filesystem.Code) {
	t.Helper()
	if got := filesystem.CodeOf(err); got != code {
		t.Fatalf("code %q (err %v), want %q", got, err, code)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := local.New(t.TempDir(), local.WithPermissions(0o600, 0o600, 0o700)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("private public-mode accepted: %v", err)
	}
	if _, err := local.New(t.TempDir(), local.WithPermissions(0o644, 0o644, 0o755)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("world-readable private mode accepted: %v", err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := local.New(file); err == nil {
		t.Error("New on a regular file succeeded")
	}
	if _, err := local.New(filepath.Join(file, "sub")); err == nil {
		t.Error("New under a regular file succeeded")
	}
}

func TestPermissionsAndVisibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes do not carry visibility on Windows")
	}
	dir := t.TempDir()
	drv, d := open(t, dir, local.WithPermissions(0o640|0o004, 0o600, 0o750))
	if root, ok := filesystem.As[*os.Root](d); !ok || root != drv.Native() {
		t.Error("As did not return the os.Root")
	}
	ctx := t.Context()
	if _, err := d.WriteBytes(ctx, "pub/a.txt", []byte("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPublic}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "pub", "a.txt"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("public file mode = %v, %v", info.Mode().Perm(), err)
	}
	dirInfo, _ := os.Stat(filepath.Join(dir, "pub"))
	if dirInfo.Mode().Perm() != 0o750 {
		t.Errorf("dir mode = %v", dirInfo.Mode().Perm())
	}
	if err := d.SetVisibility(ctx, "pub/a.txt", filesystem.VisibilityPrivate); err != nil {
		t.Fatal(err)
	}
	if info, _ = os.Stat(filepath.Join(dir, "pub", "a.txt")); info.Mode().Perm() != 0o600 {
		t.Errorf("private file mode = %v", info.Mode().Perm())
	}
}

func TestReservedAndBlockedKeys(t *testing.T) {
	ctx := t.Context()
	_, d := open(t, t.TempDir())
	for _, k := range []string{".filesystem", ".filesystem/tmp/x"} {
		_, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{})
		wantCode(t, err, filesystem.CodeInvalidKey)
	}
	if _, err := d.WriteBytes(ctx, ".filesystem-not-reserved/x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("similar name rejected: %v", err)
	}
	// A key whose parent is a file cannot exist.
	if _, err := d.WriteBytes(ctx, "file.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := d.Stat(ctx, "file.txt/child")
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.Read(ctx, "file.txt/child")
	wantCode(t, err, filesystem.CodeNotFound)
	if err := d.Delete(ctx, "file.txt/child"); err != nil {
		t.Errorf("delete under a file: %v", err)
	}
	if err := d.DeletePrefix(ctx, "file.txt/child"); err != nil {
		t.Errorf("delete prefix under a file: %v", err)
	}
	if err := d.DeletePrefix(ctx, "file.txt"); err != nil {
		t.Errorf("delete prefix naming a file: %v", err)
	}
	if ok, _ := d.Exists(ctx, "file.txt"); !ok {
		t.Error("DeletePrefix on a file name deleted the file")
	}
	_, err = d.WriteBytes(ctx, "file.txt/child", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	if _, err := d.WriteBytes(ctx, "other.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = d.Move(ctx, "other.txt", "file.txt/x")
	wantCode(t, err, filesystem.CodeInvalidArgument)
	// A directory is not an object.
	if _, err := d.WriteBytes(ctx, "dir/inner.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = d.Stat(ctx, "dir")
	wantCode(t, err, filesystem.CodeNotFound)
	if err := d.Delete(ctx, "dir"); err != nil {
		t.Errorf("delete of a directory name: %v", err)
	}
	if ok, _ := d.Exists(ctx, "dir/inner.txt"); !ok {
		t.Error("Delete of a directory name removed its contents")
	}
	err = d.SetVisibility(ctx, "missing", filesystem.VisibilityPublic)
	wantCode(t, err, filesystem.CodeNotFound)
	_, err = d.Move(ctx, "missing", "x")
	wantCode(t, err, filesystem.CodeNotFound)
}

func TestSymlinksCannotEscapeTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	_, d := open(t, dir)
	ctx := t.Context()
	if b, err := d.ReadAll(ctx, "escape/secret.txt"); err == nil {
		t.Fatalf("read through a directory symlink escaped the root: %q", b)
	}
	if b, err := d.ReadAll(ctx, "link.txt"); err == nil {
		t.Fatalf("read through a file symlink escaped the root: %q", b)
	}
	if _, err := d.WriteBytes(ctx, "escape/planted.txt", []byte("x"), filesystem.WriteOptions{}); err == nil {
		t.Fatal("write through a symlink escaped the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a file was planted outside the root")
	}
	for e, err := range d.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(e.Key, "secret") || e.Key == "link.txt" {
			t.Errorf("listing followed a symlink: %q", e.Key)
		}
	}
}

func TestDeletePrunesEmptyDirectories(t *testing.T) {
	dir := t.TempDir()
	_, d := open(t, dir)
	ctx := t.Context()
	for _, k := range []string{"a/b/c/one.txt", "a/keep.txt"} {
		if _, err := d.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Delete(ctx, "a/b/c/one.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "b")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("empty directories left behind")
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "keep.txt")); err != nil {
		t.Error("pruning removed a non-empty directory")
	}
	if _, err := d.Move(ctx, "a/keep.txt", "z/moved.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("move left an empty source directory")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, local.StagingDir, "tmp"))
	if len(entries) != 0 {
		t.Errorf("staging files left behind: %d", len(entries))
	}
}

func TestCancelledContext(t *testing.T) {
	drv, _ := open(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := drv.Stat(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Stat: %v", err)
	}
	if _, err := drv.Read(ctx, "k", filesystem.ByteRange{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Read: %v", err)
	}
	if _, err := drv.Write(ctx, "k", strings.NewReader("x"), filesystem.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Write: %v", err)
	}
	if _, err := drv.List(ctx, "", filesystem.ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("List: %v", err)
	}
	for name, err := range map[string]error{
		"Delete":        drv.Delete(ctx, "k"),
		"DeletePrefix":  drv.DeletePrefix(ctx, "k"),
		"SetVisibility": drv.SetVisibility(ctx, "k", filesystem.VisibilityPublic),
		"Abort":         drv.AbortMultipart(ctx, "k", strings.Repeat("A", 26)),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := drv.Move(ctx, "a", "b"); !errors.Is(err, context.Canceled) {
		t.Errorf("Move: %v", err)
	}
	if _, err := drv.CreateMultipart(ctx, "k", filesystem.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateMultipart: %v", err)
	}
	if _, err := drv.ListParts(ctx, "k", "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("ListParts: %v", err)
	}
	// Cancellation mid-copy stops the write.
	live, stop := context.WithCancel(t.Context())
	_, err := drv.Write(live, "k", &cancelAfter{cancel: stop}, filesystem.WriteOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("mid-write cancel: %v", err)
	}
}

// cancelAfter cancels its context after the first read.
type cancelAfter struct {
	cancel context.CancelFunc
	done   bool
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	c.done = true
	c.cancel()
	return copy(p, "x"), nil
}

func TestMultipartEdges(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	drv, d := open(t, dir)
	id, err := d.CreateMultipart(ctx, "f.bin", filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"not-an-id", strings.Repeat("A", 26)} {
		_, err := d.UploadPart(ctx, "f.bin", bad, 1, strings.NewReader("x"), 1)
		wantCode(t, err, filesystem.CodeNotFound)
	}
	_, err = d.UploadPart(ctx, "other.bin", id, 1, strings.NewReader("x"), 1)
	wantCode(t, err, filesystem.CodeNotFound)
	p1, err := d.UploadPart(ctx, "f.bin", id, 1, strings.NewReader("one"), 3)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.CompleteMultipart(ctx, "f.bin", id, []filesystem.Part{{Number: 2, ETag: "x"}})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.CompleteMultipart(ctx, "f.bin", id, []filesystem.Part{{Number: 1, ETag: "wrong"}})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.CompleteMultipart(ctx, "f.bin", id, []filesystem.Part{p1, p1})
	wantCode(t, err, filesystem.CodeInvalidArgument)
	_, err = d.UploadPart(ctx, "f.bin", id, 2, &erroring{}, 1)
	if err == nil {
		t.Error("failing part body accepted")
	}
	if err := d.AbortMultipart(ctx, "f.bin", "not-an-id"); err != nil {
		t.Errorf("abort of a malformed ID: %v", err)
	}
	// Corrupt metadata surfaces as an error rather than a panic.
	metaPath := filepath.Join(dir, local.StagingDir, "multipart", id, "upload.json")
	if err := os.WriteFile(metaPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListParts(ctx, "f.bin", id); err == nil {
		t.Error("corrupt upload metadata accepted")
	}
	// Conditional completion.
	if _, err := d.WriteBytes(ctx, "taken", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	id2, _ := d.CreateMultipart(ctx, "taken", filesystem.WriteOptions{IfNotExists: true})
	p, _ := d.UploadPart(ctx, "taken", id2, 1, strings.NewReader("y"), 1)
	_, err = d.CompleteMultipart(ctx, "taken", id2, []filesystem.Part{p})
	wantCode(t, err, filesystem.CodeExists)
	_ = drv
}

type erroring struct{}

func (erroring) Read([]byte) (int, error) { return 0, errors.New("broken part") }

func TestModTimeAndETag(t *testing.T) {
	_, d := open(t, t.TempDir())
	ctx := t.Context()
	before := time.Now().Add(-time.Second)
	e, err := d.WriteBytes(ctx, "doc.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if e.ModTime.Before(before) || e.ETag == "" || e.ContentType != "application/pdf" {
		t.Errorf("entry = %+v", e)
	}
	st, _ := d.Stat(ctx, "doc.pdf")
	if st.ContentType != "application/pdf" {
		t.Errorf("stat content type from extension = %q", st.ContentType)
	}
}

// TestIfNotExistsETagIsCurrent: the ETag an IfNotExists write or multipart
// completion returns is the object's current one, so an IfMatch with it
// succeeds; dropping the staging link after reading the entry would change
// the change time and leave it stale.
func TestIfNotExistsETagIsCurrent(t *testing.T) {
	ctx := t.Context()
	drv, _ := open(t, t.TempDir())
	for i := range 20 {
		key := "new-" + strings.Repeat("x", i+1)
		e, err := drv.Write(ctx, key, strings.NewReader("v1"), filesystem.WriteOptions{IfNotExists: true})
		if err != nil {
			t.Fatal(err)
		}
		st, err := drv.Stat(ctx, key)
		if err != nil || st.ETag != e.ETag {
			t.Fatalf("write %d: returned ETag %s, Stat says %s (%v)", i, e.ETag, st.ETag, err)
		}
		if _, err := drv.Write(ctx, key, strings.NewReader("v2"), filesystem.WriteOptions{IfMatch: e.ETag}); err != nil {
			t.Fatalf("write %d: IfMatch with the returned ETag: %v", i, err)
		}
	}
	id, err := drv.CreateMultipart(ctx, "parts", filesystem.WriteOptions{IfNotExists: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := drv.UploadPart(ctx, "parts", id, 1, strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatal(err)
	}
	e, err := drv.CompleteMultipart(ctx, "parts", id, []filesystem.Part{p})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := drv.Stat(ctx, "parts"); err != nil || st.ETag != e.ETag {
		t.Fatalf("multipart: returned ETag %s, Stat says %s (%v)", e.ETag, st.ETag, err)
	}
}
