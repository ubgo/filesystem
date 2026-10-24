package sftp_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/pkg/sftp"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// cmdLog wraps the in-memory server's command handler and records the
// method of every command, so a test can see which rename the driver
// used. It offers PosixRename only when posix is set; without it the
// request server would answer posix-rename as a plain rename.
type cmdLog struct {
	next sftp.FileCmder
	mu   sync.Mutex
	seen []string
	// refuse names a method answered with errRefused instead.
	refuse string
	// refuseIf, when set, answers the commands it matches with errRefused
	// too, for a server that refuses by path (a drop folder).
	refuseIf func(r *sftp.Request) bool
}

var errRefused = errors.New("refused by the test server")

func (c *cmdLog) Filecmd(r *sftp.Request) error {
	c.mu.Lock()
	c.seen = append(c.seen, r.Method)
	c.mu.Unlock()
	if r.Method == c.refuse || (c.refuseIf != nil && c.refuseIf(r)) {
		return errRefused
	}
	return c.next.Filecmd(r)
}

func (c *cmdLog) methods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

// posixCmdLog is a cmdLog whose server offers posix-rename.
type posixCmdLog struct{ *cmdLog }

func (p posixCmdLog) PosixRename(r *sftp.Request) error {
	p.mu.Lock()
	p.seen = append(p.seen, "PosixRename")
	p.mu.Unlock()
	return p.next.(sftp.PosixRenameFileCmder).PosixRename(r)
}

// memClient connects a client to an in-memory request server whose plain
// Rename refuses an existing target, as SFTP v3 servers do. exts is the
// extension list the server advertises; posix wires the PosixRename
// handler in.
func memClient(t *testing.T, exts []string, posix bool, refuse string) (*sftp.Client, *cmdLog) {
	t.Helper()
	return memClientIf(t, exts, posix, refuse, nil)
}

// memClientIf is memClient whose server also refuses the commands
// refuseIf matches.
func memClientIf(t *testing.T, exts []string, posix bool, refuse string, refuseIf func(*sftp.Request) bool) (*sftp.Client, *cmdLog) {
	t.Helper()
	t.Cleanup(func() { _ = sftp.SetSFTPExtensions(allExtensions...) })
	if err := sftp.SetSFTPExtensions(exts...); err != nil {
		t.Fatal(err)
	}
	h := sftp.InMemHandler()
	log := &cmdLog{next: h.FileCmd, refuse: refuse, refuseIf: refuseIf}
	if posix {
		h.FileCmd = posixCmdLog{log}
	} else {
		h.FileCmd = log
	}
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{c2sR, s2cW}, h)
	go func() { _ = srv.Serve() }()
	client, err := sftp.NewClientPipe(s2cR, c2sW)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close(); _ = c2sR.Close(); _ = client.Close() })
	return client, log
}

// TestNonAtomicWritesAreOptIn proves a server without posix-rename is
// refused by default, and that AllowNonAtomicWrites replaces existing
// files on it (Write and Move) through remove-then-rename.
func TestNonAtomicWritesAreOptIn(t *testing.T) {
	ctx := t.Context()
	noPosix := []string{"hardlink@openssh.com"}
	c, _ := memClient(t, noPosix, false, "")
	if _, err := fssftp.New(fssftp.Config{Client: c, Root: "/disk"}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("New without posix-rename = %v, want unsupported", err)
	}
	c, log := memClient(t, noPosix, false, "")
	drv, err := fssftp.New(fssftp.Config{Client: c, Root: "/disk", AllowNonAtomicWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "k", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatalf("write %q = %v", body, err)
		}
	}
	if b, err := d.ReadAll(ctx, "k"); err != nil || string(b) != "two" {
		t.Fatalf("after overwrite = %q, %v", b, err)
	}
	if _, err := d.WriteBytes(ctx, "dst", []byte("old"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Move(ctx, "k", "dst"); err != nil {
		t.Fatalf("move onto an existing key = %v", err)
	}
	if b, err := d.ReadAll(ctx, "dst"); err != nil || string(b) != "two" {
		t.Fatalf("after move = %q, %v", b, err)
	}
	if slices.Contains(log.methods(), "PosixRename") {
		t.Errorf("posix-rename used on a server that does not offer it: %v", log.methods())
	}
}

// TestNonAtomicKeepsPosixRename proves AllowNonAtomicWrites changes
// nothing on a server that offers posix-rename: every replace is still one
// atomic posix-rename and never a remove.
func TestNonAtomicKeepsPosixRename(t *testing.T) {
	ctx := t.Context()
	c, log := memClient(t, allExtensions, true, "")
	drv, err := fssftp.New(fssftp.Config{Client: c, Root: "/disk", AllowNonAtomicWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	for range 2 {
		if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	got := log.methods()
	if !slices.Contains(got, "PosixRename") || slices.Contains(got, "Rename") {
		t.Fatalf("methods = %v, want posix-rename and no plain rename", got)
	}
}

// TestStagingDir proves a custom staging directory is used, reserved and
// hidden from listings, frees the default name, and that invalid names
// are refused.
func TestStagingDir(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t), Root: root, StagingDir: "_staging"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	if _, err := os.Stat(filepath.Join(root, "_staging", "tmp")); err != nil {
		t.Fatalf("custom staging dir not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, fssftp.StagingDir)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("default staging dir created anyway: %v", err)
	}
	_, err = d.WriteBytes(ctx, "_staging/x", []byte("x"), filesystem.WriteOptions{})
	wantCode(t, err, filesystem.CodeInvalidKey)
	if _, err := d.WriteBytes(ctx, fssftp.StagingDir+"/x", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("the default name is an ordinary key now: %v", err)
	}
	page, err := d.ListPage(ctx, "", filesystem.ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Entries {
		if e.Key != fssftp.StagingDir+"/x" {
			t.Errorf("listing shows %q", e.Key)
		}
	}
	for _, bad := range []string{"a/b", ".", "..", `a\b`} {
		_, err := fssftp.New(fssftp.Config{Client: pipeClient(t), Root: t.TempDir(), StagingDir: bad})
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
}

// TestFileModes proves custom modes reach the server and visibility is
// read back from the bits public has and private lacks, that an
// indistinguishable pair is refused, and that the default still reads a
// group-readable file as private.
func TestFileModes(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	drv, err := fssftp.New(fssftp.Config{Client: pipeClient(t), Root: root, FilePublic: 0o660, FilePrivate: 0o600})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	for v, want := range map[filesystem.Visibility]fs.FileMode{filesystem.VisibilityPublic: 0o660, filesystem.VisibilityPrivate: 0o600} {
		e, err := d.WriteBytes(ctx, string(v), []byte("x"), filesystem.WriteOptions{Visibility: v})
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, string(v)))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode = %v (%v), want %v", v, info.Mode().Perm(), err, want)
		}
		if e.Visibility != v {
			t.Errorf("%s read back as %s", v, e.Visibility)
		}
	}
	_, err = fssftp.New(fssftp.Config{Client: pipeClient(t), Root: t.TempDir(), FilePublic: 0o600, FilePrivate: 0o640})
	wantCode(t, err, filesystem.CodeInvalidArgument)

	_, dd, droot := driver(t)
	if err := os.WriteFile(filepath.Join(droot, "g"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(droot, "g"), 0o640); err != nil {
		t.Fatal(err)
	}
	if e, err := dd.Stat(ctx, "g"); err != nil || e.Visibility != filesystem.VisibilityPrivate {
		t.Errorf("default reads a 0640 file as %s (%v), want private", e.Visibility, err)
	}
}

// TestSkipChmod proves a server refusing SETSTAT fails every write by
// default, and that SkipChmod writes without setting a mode and stops
// declaring SetVisibility.
func TestSkipChmod(t *testing.T) {
	ctx := t.Context()
	c, _ := memClient(t, allExtensions, true, "Setstat")
	drv, err := fssftp.New(fssftp.Config{Client: c, Root: "/disk"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err == nil {
		t.Fatal("write succeeded on a server refusing SETSTAT without SkipChmod")
	}
	c, log := memClient(t, allExtensions, true, "Setstat")
	drv, err = fssftp.New(fssftp.Config{Client: c, Root: "/disk", SkipChmod: true})
	if err != nil {
		t.Fatal(err)
	}
	if drv.Capabilities().SetVisibility {
		t.Error("SkipChmod still declares SetVisibility")
	}
	d, _ = filesystem.NewDisk("sftp", drv)
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("write with SkipChmod = %v", err)
	}
	if slices.Contains(log.methods(), "Setstat") {
		t.Errorf("SkipChmod still sent SETSTAT: %v", log.methods())
	}
	wantCode(t, drv.SetVisibility(ctx, "k", filesystem.VisibilityPublic), filesystem.CodeUnsupported)
}

// TestNonAtomicReplaceFailures pins the two-step replace's failures: a
// rename refused with nothing at the target is returned as is, and a
// refused remove of the old file fails the write with the old file kept.
func TestNonAtomicReplaceFailures(t *testing.T) {
	ctx := t.Context()
	noPosix := []string{"hardlink@openssh.com"}
	open := func(refuse string) *filesystem.Disk {
		c, _ := memClient(t, noPosix, false, refuse)
		drv, err := fssftp.New(fssftp.Config{Client: c, Root: "/disk", AllowNonAtomicWrites: true})
		if err != nil {
			t.Fatal(err)
		}
		d, _ := filesystem.NewDisk("sftp", drv)
		return d
	}
	if _, err := open("Rename").WriteBytes(ctx, "new", []byte("x"), filesystem.WriteOptions{}); err == nil {
		t.Fatal("a refused rename with nothing in the way succeeded")
	}
	d := open("Remove")
	if _, err := d.WriteBytes(ctx, "k", []byte("old"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "k", []byte("new"), filesystem.WriteOptions{}); err == nil {
		t.Fatal("a refused remove of the old file succeeded")
	}
	if b, err := d.ReadAll(ctx, "k"); err != nil || string(b) != "old" {
		t.Fatalf("after a refused replace = %q, %v", b, err)
	}
}
