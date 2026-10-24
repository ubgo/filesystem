package sftp_test

import (
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// inbox is the drop folder of these tests: a directory the server lets
// the account write files into but refuses every mkdir inside, as bank,
// EDI and partner inboxes do.
const inbox = "/inbox"

// dropServer connects to an in-memory server holding inbox, which then
// refuses every Mkdir inside inbox (and, with noMkdir, every Mkdir at
// all). posix is whether it offers posix-rename and hard links; without
// it the server offers no extension, as many appliances do.
func dropServer(t *testing.T, posix, noMkdir bool) (*sftp.Client, *cmdLog) {
	t.Helper()
	exts := []string{}
	if posix {
		exts = allExtensions
	}
	var ready atomic.Bool
	c, log := memClientIf(t, exts, posix, "", func(r *sftp.Request) bool {
		return ready.Load() && r.Method == "Mkdir" && (noMkdir || strings.HasPrefix(r.Filepath, inbox+"/"))
	})
	if err := c.Mkdir(inbox); err != nil {
		t.Fatal(err)
	}
	ready.Store(true)
	log.mu.Lock()
	log.seen = nil // the setup's own Mkdir is not the driver's
	log.mu.Unlock()
	return c, log
}

// names lists the entries of dir on the server.
func names(t *testing.T, c *sftp.Client, dir string) []string {
	t.Helper()
	infos, err := c.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range infos {
		out = append(out, i.Name())
	}
	slices.Sort(out)
	return out
}

// TestStagingPathDropFolder proves a staging directory outside Root opens
// a drop folder that refuses mkdir inside it (where the default fails at
// New), keeps writes atomic (staged there, renamed in), reserves no key,
// sweeps only there, and that a misplaced StagingPath is refused.
func TestStagingPathDropFolder(t *testing.T) {
	ctx := t.Context()
	c, _ := dropServer(t, true, false)
	if _, err := fssftp.New(fssftp.Config{Client: c, Root: inbox}); err == nil {
		t.Fatal("the default staging directory opened in a drop folder that refuses mkdir")
	}
	c, log := dropServer(t, true, false)
	drv, err := fssftp.New(fssftp.Config{Client: c, Root: inbox, StagingPath: "/staging"})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "two" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if !slices.Contains(log.methods(), "PosixRename") {
		t.Errorf("writes were not renamed into place: %v", log.methods())
	}
	if _, err := d.WriteBytes(ctx, fssftp.StagingDir, []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Errorf("the default staging name is reserved without a staging dir under Root: %v", err)
	}
	if page, err := d.ListPage(ctx, "", filesystem.ListOptions{}); err != nil || len(page.Entries) != 2 {
		t.Errorf("listing = %+v, %v; want both keys", page.Entries, err)
	}
	if got := names(t, c, inbox); !slices.Equal(got, []string{fssftp.StagingDir, "a.txt"}) {
		t.Errorf("inbox holds %v, want only the keys", got)
	}
	if got := names(t, c, "/staging"); len(got) != 0 {
		t.Errorf("staging kept %v", got)
	}
	// An abandoned staged file is found and swept in StagingPath.
	f, err := c.Create("/staging/" + strings.Repeat("a", 26))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	later := time.Now().Add(time.Hour)
	if found, err := d.FindAbandoned(ctx, later); err != nil || len(found) != 1 {
		t.Errorf("FindAbandoned = %v, %v", found, err)
	}
	if n, err := d.SweepAbandoned(ctx, later); err != nil || n != 1 {
		t.Errorf("SweepAbandoned = %d, %v", n, err)
	}
	for _, cfg := range []fssftp.Config{
		{StagingPath: "staging"},
		{StagingPath: inbox},
		{StagingPath: inbox + "/tmp"},
		{StagingPath: "/"},
		{StagingPath: "/staging", StagingDir: "_s"},
	} {
		cfg.Client, cfg.Root = c, inbox
		if _, err := fssftp.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(StagingPath %q, StagingDir %q) = %v, want CodeInvalidArgument", cfg.StagingPath, cfg.StagingDir, err)
		}
	}
}

// TestWriteInPlaceDropFolder proves WriteInPlace opens a drop folder where
// no directory may be created anywhere, writes and overwrites keys
// directly (no mkdir, no rename, nothing else left in Root), keeps
// IfNotExists exclusive, removes a failed write, declares nothing to
// sweep, and needs AllowNonAtomicWrites.
func TestWriteInPlaceDropFolder(t *testing.T) {
	ctx := t.Context()
	c, log := dropServer(t, false, true)
	for _, cfg := range []fssftp.Config{
		{WriteInPlace: true},
		{WriteInPlace: true, AllowNonAtomicWrites: true, StagingDir: "_s"},
		{WriteInPlace: true, AllowNonAtomicWrites: true, StagingPath: "/staging"},
	} {
		cfg.Client, cfg.Root = c, inbox
		if _, err := fssftp.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%+v) = %v, want CodeInvalidArgument", cfg, err)
		}
	}
	drv, err := fssftp.New(fssftp.Config{Client: c, Root: inbox, AllowNonAtomicWrites: true, WriteInPlace: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := log.methods(); len(got) != 0 {
		t.Errorf("New sent %v under WriteInPlace, want nothing", got)
	}
	d, _ := filesystem.NewDisk("sftp", drv)
	for _, body := range []string{"one", "two"} {
		if _, err := d.WriteBytes(ctx, "a.txt", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := d.ReadAll(ctx, "a.txt"); err != nil || string(b) != "two" {
		t.Fatalf("read = %q, %v", b, err)
	}
	_, err = d.WriteBytes(ctx, "a.txt", []byte("three"), filesystem.WriteOptions{IfNotExists: true})
	wantCode(t, err, filesystem.CodeExists)
	if b, _ := d.ReadAll(ctx, "a.txt"); string(b) != "two" {
		t.Errorf("a refused IfNotExists changed the file: %q", b)
	}
	if _, err := d.WriteBytes(ctx, "b.txt", []byte("b"), filesystem.WriteOptions{IfNotExists: true}); err != nil {
		t.Errorf("IfNotExists on a new key = %v", err)
	}
	if _, err := d.Write(ctx, "c.txt", failingBody{}, filesystem.WriteOptions{}); err == nil {
		t.Error("a failing body was stored")
	}
	if got := names(t, c, inbox); !slices.Equal(got, []string{"a.txt", "b.txt"}) {
		t.Errorf("inbox holds %v, want only the written keys", got)
	}
	for _, m := range log.methods() {
		if m == "Mkdir" || m == "Rename" || m == "PosixRename" || m == "Link" {
			t.Errorf("in-place writes sent %s: %v", m, log.methods())
			break
		}
	}
	caps := drv.Capabilities()
	if !caps.ConditionalWrite || caps.SweepAbandoned || caps.FindAbandoned || caps.CaseInsensitive {
		t.Errorf("capabilities = %+v", caps)
	}
	if _, err := drv.SweepAbandoned(ctx, time.Now()); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("SweepAbandoned = %v, want CodeUnsupported", err)
	}
}
