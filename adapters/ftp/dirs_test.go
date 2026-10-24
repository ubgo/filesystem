package ftp_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/afero"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/ftp"
)

// TestFindAbandoned proves the preview reports exactly the staged files a
// sweep removes, removes nothing, and agrees with the sweep after it.
func TestFindAbandoned(t *testing.T) {
	ctx := t.Context()
	m, addr := startServer(t, nil)
	d, err := filesystem.NewDisk("ftp", newDriver(t, config(addr)))
	if err != nil {
		t.Fatal(err)
	}
	if !d.Capabilities().FindAbandoned {
		t.Fatal("FindAbandoned not declared")
	}
	// Writing once creates the staging directory.
	if _, err := d.WriteBytes(ctx, "kept.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(m.dir, "app", ftp.StagingDir, "tmp")
	old, fresh, other := filepath.Join(tmp, strings.Repeat("a", 26)), filepath.Join(tmp, strings.Repeat("b", 26)), filepath.Join(tmp, "keep.me")
	for _, f := range []string{old, fresh, other} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, f := range []string{old, other} {
		if err := os.Chtimes(f, past, past); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-time.Hour)
	found, err := d.FindAbandoned(ctx, cutoff)
	if err != nil || len(found) != 1 {
		t.Fatalf("preview = %+v, %v; want one staged file", found, err)
	}
	if a := found[0]; a.Kind != filesystem.AbandonedStaging || a.ID != strings.Repeat("a", 26) || a.Key != "" || !a.LastActivity.Before(cutoff) {
		t.Errorf("reported %+v", a)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the preview removed the file: %v", err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("sweep after the preview = %d, %v", n, err)
	}
	if found, err := d.FindAbandoned(ctx, cutoff); err != nil || len(found) != 0 {
		t.Errorf("preview after the sweep = %+v, %v", found, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := d.FindAbandoned(cancelled, cutoff); err == nil {
		t.Error("preview with a cancelled context succeeded")
	}
}

// TestFindAbandonedWithoutStaging: a server without the staging directory
// has nothing abandoned.
func TestFindAbandonedWithoutStaging(t *testing.T) {
	_, addr := startServer(t, nil)
	found, err := newDriver(t, config(addr)).FindAbandoned(t.Context(), time.Now())
	if err != nil || len(found) != 0 {
		t.Fatalf("preview = %+v, %v", found, err)
	}
}

// TestMakeDirAndDirExists covers what the conformance suite cannot: a
// file on the path, a reserved key, a directory going with its last file.
func TestMakeDirAndDirExists(t *testing.T) {
	ctx := t.Context()
	d := newDisk(t)
	if _, err := d.WriteBytes(ctx, "f.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.MakeDir(ctx, "f.txt"); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Errorf("MakeDir over a file: %v", err)
	}
	if err := d.MakeDir(ctx, "f.txt/sub"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("MakeDir under a file: %v", err)
	}
	for dir, want := range map[string]bool{"f.txt": false, "f.txt/sub": false, "none": false} {
		if ok, err := d.DirExists(ctx, dir); ok != want || err != nil {
			t.Errorf("DirExists(%q) = %v, %v", dir, ok, err)
		}
	}
	if err := d.MakeDir(ctx, ftp.StagingDir+"/x"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("MakeDir in the staging directory: %v", err)
	}
	if _, err := d.DirExists(ctx, ftp.StagingDir); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("DirExists of the staging directory: %v", err)
	}
	if err := d.MakeDir(ctx, "e/f"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "e/f/k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "e/f/k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.DirExists(ctx, "e"); ok {
		t.Error("the directories outlived their last file")
	}
}

// stallStat stalls every Stat and directory listing of a name holding
// marker, the way a hung server stops answering a command.
type stallStat struct {
	afero.Fs
	marker string
}

func (f stallStat) stall(name string) {
	if strings.Contains(name, f.marker) {
		time.Sleep(time.Second)
	}
}

func (f stallStat) Stat(name string) (os.FileInfo, error) {
	f.stall(name)
	return f.Fs.Stat(name)
}

func (f stallStat) Open(name string) (afero.File, error) {
	f.stall(name)
	return f.Fs.Open(name)
}

// TestDirsAndPreviewOnABrokenConnection: a command cut off by the
// caller's deadline (the pool closes the connection) is an error from
// MakeDir, DirExists and FindAbandoned, never a false or an empty preview.
func TestDirsAndPreviewOnABrokenConnection(t *testing.T) {
	for _, c := range []struct {
		name   string
		marker string
		call   func(ctx context.Context, d *ftp.Driver) error
	}{
		{"MakeDir", "slow", func(ctx context.Context, d *ftp.Driver) error { return d.MakeDir(ctx, "slow") }},
		{"DirExists", "slow", func(ctx context.Context, d *ftp.Driver) error {
			_, err := d.DirExists(ctx, "slow")
			return err
		}},
		{"FindAbandoned", "tmp", func(ctx context.Context, d *ftp.Driver) error {
			_, err := d.FindAbandoned(ctx, time.Now())
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, addr := startServer(t, nil)
			m.wrapFs = func(inner afero.Fs) afero.Fs { return stallStat{Fs: inner, marker: c.marker} }
			drv := newDriver(t, config(addr))
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			if err := c.call(ctx, drv); err == nil {
				t.Fatal("a call cut off by its deadline succeeded")
			}
		})
	}
}

// sweepFs makes the server misbehave inside the staging directory: a
// listing raises listed (or fails, when unlistable: the server's reply,
// which the sweep takes as no staging directory), Remove of a name holding refuse fails (a reply),
// and Remove of one holding stall hangs.
type sweepFs struct {
	afero.Fs
	listed        *atomic.Bool
	refuse, stall string
	unlistable    bool
}

func (f sweepFs) Open(name string) (afero.File, error) {
	if strings.HasSuffix(name, "/tmp") {
		if f.unlistable {
			return nil, errors.New("no listing")
		}
		f.listed.Store(true)
	}
	return f.Fs.Open(name)
}

func (f sweepFs) Remove(name string) error {
	switch {
	case f.refuse != "" && strings.Contains(name, f.refuse):
		return errors.New("refused")
	case f.stall != "" && strings.Contains(name, f.stall):
		time.Sleep(time.Second)
	}
	return f.Fs.Remove(name)
}

// listedCtx reports Canceled from Err once the server has listed the
// staging directory, so a scan gets its listing and stops in its loop;
// Done stays the parent's, so the command is not cut off.
type listedCtx struct {
	context.Context
	listed *atomic.Bool
}

func (c listedCtx) Err() error {
	if c.listed.Load() {
		return context.Canceled
	}
	return nil
}

// TestSweepServerFailures: a file the server refuses to remove is skipped
// (gone meanwhile, as far as a reply can tell), a removal cut off by the
// deadline is an error, and a scan stops at a cancelled context.
func TestSweepServerFailures(t *testing.T) {
	for _, c := range []struct {
		name          string
		refuse, stall string
		deadline      bool
		unlistable    bool
		wantErr       bool
	}{
		{name: "unlistable staging directory", refuse: "-", unlistable: true},
		{name: "refused", refuse: strings.Repeat("a", 26)},
		{name: "cut off", stall: strings.Repeat("a", 26), deadline: true, wantErr: true},
		{name: "cancelled during the scan", wantErr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, addr := startServer(t, nil)
			drv := newDriver(t, config(addr))
			if _, err := drv.Write(t.Context(), "k", strings.NewReader("x"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(m.dir, "app", ftp.StagingDir, "tmp", strings.Repeat("a", 26))
			if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-time.Hour)
			_ = os.Chtimes(old, past, past)
			listed := new(atomic.Bool)
			m.wrapFs = func(inner afero.Fs) afero.Fs {
				return sweepFs{Fs: inner, listed: listed, refuse: c.refuse, stall: c.stall, unlistable: c.unlistable}
			}
			var ctx context.Context = t.Context()
			switch {
			case c.deadline:
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			case c.refuse == "":
				ctx = listedCtx{Context: ctx, listed: listed}
			}
			// A second driver: the first one's pooled connection was
			// opened before wrapFs was set, so it would not misbehave.
			n, err := newDriver(t, config(addr)).SweepAbandoned(ctx, time.Now())
			if (err != nil) != c.wantErr || n != 0 {
				t.Errorf("sweep = %d, %v; want 0, error %t", n, err, c.wantErr)
			}
		})
	}
}
