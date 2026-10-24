package ftp_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/afero"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/ftp"
)

// inbox is the drop folder of these tests: a directory the server lets
// the account store files into but refuses every MKD inside, as bank, EDI
// and partner inboxes do.
const inbox = "/inbox"

// dropFs refuses MKD inside inbox (everywhere with noMkdir), refuses to
// rename onto an existing file with refuseExisting (IIS), refuses a rename
// out of inbox with confined (an account that may write nowhere else),
// refuses every delete with noRemove, and counts renames.
type dropFs struct {
	afero.Fs
	noMkdir, refuseExisting, confined, noRemove bool
	renames                                     atomic.Int32
}

func (f *dropFs) Remove(name string) error {
	if f.noRemove {
		return errors.New("permission denied")
	}
	return f.Fs.Remove(name)
}

func (f *dropFs) refused(name string) bool {
	return f.noMkdir || strings.HasPrefix(filepath.ToSlash(name), inbox+"/")
}

func (f *dropFs) Mkdir(name string, perm os.FileMode) error {
	if f.refused(name) {
		return errors.New("permission denied")
	}
	return f.Fs.Mkdir(name, perm)
}

func (f *dropFs) MkdirAll(name string, perm os.FileMode) error {
	if f.refused(name) {
		return errors.New("permission denied")
	}
	return f.Fs.MkdirAll(name, perm)
}

func (f *dropFs) Rename(oldname, newname string) error {
	f.renames.Add(1)
	if f.confined && !strings.HasPrefix(filepath.ToSlash(newname), inbox+"/") {
		return errors.New("permission denied")
	}
	if _, err := f.Stat(newname); err == nil && f.refuseExisting {
		return errors.New("Cannot create a file when that file already exists.")
	}
	return f.Fs.Rename(oldname, newname)
}

// slowFail fails after a pause, long enough for the server to have
// stored what came before it.
type slowFail struct{}

func (slowFail) Read([]byte) (int, error) {
	time.Sleep(200 * time.Millisecond)
	return 0, errors.New("client went away")
}

// dropServer starts a server holding inbox whose filesystem is fs.
func dropServer(t *testing.T, fs *dropFs) (*mainDriver, ftp.Config) {
	t.Helper()
	m, addr := startServer(t, nil)
	if err := os.Mkdir(filepath.Join(m.dir, "inbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	m.wrapFs = func(inner afero.Fs) afero.Fs { fs.Fs = inner; return fs }
	cfg := config(addr)
	cfg.Root = inbox
	return m, cfg
}

// dirNames lists dir under the server's directory.
func dirNames(t *testing.T, m *mainDriver, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(m.dir, dir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// TestStagingPathDropFolder proves a staging directory outside Root opens
// a drop folder that refuses MKD inside it (where the default fails at
// New), still stages and renames writes, reserves no key, sweeps only
// there, and that a misplaced StagingPath is refused before any dial.
func TestStagingPathDropFolder(t *testing.T) {
	ctx := t.Context()
	fs := &dropFs{}
	m, cfg := dropServer(t, fs)
	if _, err := ftp.New(ctx, cfg); err == nil {
		t.Fatal("the default staging directory opened in a drop folder that refuses MKD")
	}
	cfg.StagingPath = "/staging"
	drv := newDriver(t, cfg)
	d, _ := filesystem.NewDisk("ftp", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "two" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if fs.renames.Load() == 0 {
		t.Error("writes were not renamed into place")
	}
	if _, err := d.WriteBytes(ctx, ftp.StagingDir, []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("the default staging name is reserved without a staging dir under Root: %v", err)
	}
	if page, err := d.ListPage(ctx, "", filesystem.ListOptions{}); err != nil || len(page.Entries) != 2 {
		t.Errorf("listing = %+v, %v; want both keys", page.Entries, err)
	}
	if got := dirNames(t, m, "inbox"); !slices.Equal(got, []string{ftp.StagingDir, "a.txt"}) {
		t.Errorf("inbox holds %v, want only the keys", got)
	}
	if got := dirNames(t, m, "staging"); len(got) != 0 {
		t.Errorf("staging kept %v", got)
	}
	if err := os.WriteFile(filepath.Join(m.dir, "staging", strings.Repeat("a", 26)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if found, err := d.FindAbandoned(ctx, later); err != nil || len(found) != 1 {
		t.Errorf("FindAbandoned = %v, %v", found, err)
	}
	if n, err := d.SweepAbandoned(ctx, later); err != nil || n != 1 {
		t.Errorf("SweepAbandoned = %d, %v", n, err)
	}
	for _, bad := range []ftp.Config{
		{StagingPath: "staging"},
		{StagingPath: inbox},
		{StagingPath: inbox + "/tmp"},
		{StagingPath: "/"},
		{StagingPath: "/staging", StagingDir: "_s"},
	} {
		bad.Addr, bad.Root = "127.0.0.1:1", inbox
		if _, err := ftp.New(context.Background(), bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(StagingPath %q, StagingDir %q) = %v, want CodeInvalidArgument", bad.StagingPath, bad.StagingDir, err)
		}
	}
}

// TestWriteInPlaceDropFolder proves WriteInPlace opens a drop folder where
// no directory may be created anywhere, stores and overwrites keys
// directly (no MKD, no rename, nothing else left in Root), deletes a
// failed write, moves onto an existing key on a server that refuses to
// rename over a file, declares nothing to sweep, and needs
// AllowNonAtomicWrites.
func TestWriteInPlaceDropFolder(t *testing.T) {
	ctx := t.Context()
	fs := &dropFs{noMkdir: true, refuseExisting: true, confined: true}
	m, cfg := dropServer(t, fs)
	for _, bad := range []ftp.Config{
		{WriteInPlace: true},
		{WriteInPlace: true, AllowNonAtomicWrites: true, StagingDir: "_s"},
		{WriteInPlace: true, AllowNonAtomicWrites: true, StagingPath: "/staging"},
	} {
		bad.Addr, bad.Root = "127.0.0.1:1", inbox
		if _, err := ftp.New(context.Background(), bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%v) = %v, want CodeInvalidArgument", bad, err)
		}
	}
	cfg.WriteInPlace, cfg.AllowNonAtomicWrites = true, true
	drv := newDriver(t, cfg)
	d, _ := filesystem.NewDisk("ftp", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "two" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if n := fs.renames.Load(); n != 0 {
		t.Errorf("in-place writes renamed %d times", n)
	}
	// The body fails only after the server has the file open and some of
	// its bytes, so a partial file exists until the driver deletes it.
	part := io.MultiReader(strings.NewReader(strings.Repeat("p", 64<<10)), slowFail{})
	if _, err := d.Write(ctx, "c.txt", part, filesystem.WriteOptions{}); err == nil {
		t.Error("a failing body was stored")
	}
	if got := dirNames(t, m, "inbox"); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("inbox holds %v, want only the written key", got)
	}
	if _, err := d.WriteBytes(ctx, "b.txt", []byte("b"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(ctx, "b.txt", "a.txt"); err != nil {
		t.Fatalf("move onto an existing key = %v", err)
	}
	if b, _ := d.ReadAll(ctx, "a.txt"); string(b) != "b" {
		t.Errorf("after move a.txt = %q", b)
	}
	if got := dirNames(t, m, "inbox"); !slices.Equal(got, []string{"a.txt"}) {
		t.Errorf("after move inbox holds %v", got)
	}
	if got := dirNames(t, m, ""); !slices.Equal(got, []string{"inbox"}) {
		t.Errorf("the server's root holds %v, want only the inbox", got)
	}
	caps := drv.Capabilities()
	if caps.SweepAbandoned || caps.FindAbandoned || caps.CaseInsensitive {
		t.Errorf("capabilities = %+v", caps)
	}
	if _, err := drv.SweepAbandoned(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("SweepAbandoned = %v, want CodeUnsupported", err)
	}
}

// TestWriteInPlaceRefusals proves an in-place write of a nested key whose
// parent the server refuses to make fails with nothing stored, and that a
// Move onto an existing key on a server refusing both the rename over it
// and its delete fails, leaving both files as they were.
func TestWriteInPlaceRefusals(t *testing.T) {
	ctx := t.Context()
	fs := &dropFs{noMkdir: true, refuseExisting: true}
	m, cfg := dropServer(t, fs)
	cfg.WriteInPlace, cfg.AllowNonAtomicWrites = true, true
	d, _ := filesystem.NewDisk("ftp", newDriver(t, cfg))
	if _, err := d.WriteBytes(ctx, "sub/a.txt", []byte("x"), filesystem.WriteOptions{}); err == nil {
		t.Error("a nested key was stored where no directory may be made")
	}
	if got := dirNames(t, m, "inbox"); len(got) != 0 {
		t.Errorf("inbox holds %v after a refused nested write", got)
	}
	for key, body := range map[string]string{"a.txt": "a", "b.txt": "b"} {
		if _, err := d.WriteBytes(ctx, key, []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	fs.noRemove = true
	if _, err := d.Move(ctx, "b.txt", "a.txt"); err == nil {
		t.Error("a move whose target cannot be deleted succeeded")
	}
	for key, want := range map[string]string{"a.txt": "a", "b.txt": "b"} {
		if b, err := d.ReadAll(ctx, key); err != nil || string(b) != want {
			t.Errorf("after the refused move %s = %q, %v", key, b, err)
		}
	}
}
