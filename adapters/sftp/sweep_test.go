package sftp_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// staged writes a file into the driver's staging directory, as a Write
// that died before its rename leaves one, last modified at mtime.
func staged(t *testing.T, root, name string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(root, fssftp.StagingDir, "tmp", name)
	if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSweepAbandoned proves the sweep removes staged files older than the
// cutoff, and keeps newer ones, files Write did not name, directories and
// stored files.
func TestSweepAbandoned(t *testing.T) {
	ctx := t.Context()
	_, d, root := driver(t)
	if !d.Capabilities().SweepAbandoned {
		t.Fatal("SweepAbandoned not declared")
	}
	if _, err := d.WriteBytes(ctx, "kept.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	abandoned := staged(t, root, "abcdefghijklmnopqrstuvwxyz", old)
	recent := staged(t, root, "zyxwvutsrqponmlkjihgfedcba", time.Now())
	foreign := staged(t, root, "notes.txt", old)
	dir := filepath.Join(root, fssftp.StagingDir, "tmp", "aaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(dir, old, old)

	cutoff := time.Now().Add(-24 * time.Hour)
	n, err := d.SweepAbandoned(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Error("abandoned staged file kept")
	}
	for _, p := range []string{recent, foreign, dir, filepath.Join(root, "kept.txt")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != 0 {
		t.Errorf("second sweep = %d, %v", n, err)
	}
}

// TestSweepAbandonedFailures covers a missing staging directory, a
// cancelled context, an unlistable staging directory and a file the
// server refuses to remove.
func TestSweepAbandonedFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	future := time.Now().Add(time.Hour)
	drv, _, root := driver(t)
	tmp := filepath.Join(root, fssftp.StagingDir, "tmp")

	staged(t, root, "abcdefghijklmnopqrstuvwxyz", time.Now().Add(-time.Hour))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := drv.SweepAbandoned(cancelled, future); err == nil {
		t.Error("sweep with a cancelled context succeeded")
	}

	_ = os.Chmod(tmp, 0o500)
	n, err := drv.SweepAbandoned(t.Context(), future)
	_ = os.Chmod(tmp, 0o700)
	if err == nil || n != 0 {
		t.Errorf("sweep in a read-only staging directory = %d, %v", n, err)
	}

	_ = os.Chmod(tmp, 0o000)
	_, err = drv.SweepAbandoned(t.Context(), future)
	_ = os.Chmod(tmp, 0o700)
	if err == nil {
		t.Error("sweep of an unlistable staging directory succeeded")
	}

	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if n, err := drv.SweepAbandoned(t.Context(), future); err != nil || n != 0 {
		t.Errorf("sweep without a staging directory = %d, %v", n, err)
	}
}

// laterCancel reports no error on its first Err call and Canceled after,
// so a sweep passes its start check and stops inside its loop.
type laterCancel struct {
	context.Context
	calls atomic.Int32
}

func (c *laterCancel) Err() error {
	if c.calls.Add(1) > 1 {
		return context.Canceled
	}
	return nil
}

// TestSweepAbandonedStopsOnCancel proves the sweep checks the context
// between files, not only before it starts.
func TestSweepAbandonedStopsOnCancel(t *testing.T) {
	drv, _, root := driver(t)
	p := staged(t, root, "abcdefghijklmnopqrstuvwxyz", time.Now().Add(-time.Hour))
	n, err := drv.SweepAbandoned(&laterCancel{Context: t.Context()}, time.Now())
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("sweep cancelled mid-way = %d, %v", n, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("a cancelled sweep removed a file")
	}
}

// phantomInfo is a staged file the listing reports but the server no
// longer has: what a sweep sees when the file's Write renames it between
// the listing and the removal.
type phantomInfo struct{}

func (phantomInfo) Name() string       { return "abcdefghijklmnopqrstuvwxyz" }
func (phantomInfo) Size() int64        { return 1 }
func (phantomInfo) Mode() os.FileMode  { return 0o600 }
func (phantomInfo) ModTime() time.Time { return time.Now().Add(-48 * time.Hour) }
func (phantomInfo) IsDir() bool        { return false }
func (phantomInfo) Sys() any           { return nil }

type phantomLister []os.FileInfo

func (l phantomLister) ListAt(out []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[off:])
	if int(off)+n == len(l) {
		return n, io.EOF
	}
	return n, nil
}

// phantomTmp lists a phantom staged file in the staging directory and
// delegates everything else to the in-memory handlers.
type phantomTmp struct{ next sftp.FileLister }

func (p phantomTmp) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if r.Method == "List" && strings.HasSuffix(r.Filepath, "/"+fssftp.StagingDir+"/tmp") {
		return phantomLister{phantomInfo{}}, nil
	}
	return p.next.Filelist(r)
}

// TestSweepAbandonedVanishedFile proves a staged file that disappears
// between the listing and the removal is skipped, not counted and not an
// error.
func TestSweepAbandonedVanishedFile(t *testing.T) {
	h := sftp.InMemHandler()
	h.FileList = phantomTmp{next: h.FileList}
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
	drv, err := fssftp.New(fssftp.Config{Client: client, Root: "/disk"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := drv.SweepAbandoned(t.Context(), time.Now()); err != nil || n != 0 {
		t.Fatalf("sweep of a vanished file = %d, %v", n, err)
	}
}
