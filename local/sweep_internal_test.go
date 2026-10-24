package local

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
)

// sweepRoot injects the races and failures SweepAbandoned must handle:
// entries listed under infoDir whose Info fails with infoErr (a file that
// vanished, or a stat that failed, between the directory read and the
// stat), a Stat of statPrefix failing with statErr, and Remove failing with
// removeErr.
type sweepRoot struct {
	fileRoot
	infoDir    string
	infoErr    error
	statPrefix string
	statErr    error
	removeErr  error
}

func (r sweepRoot) FS() fs.FS { return sweepFS{FS: r.fileRoot.FS(), dir: r.infoDir, err: r.infoErr} }

func (r sweepRoot) Stat(name string) (fs.FileInfo, error) {
	if r.statErr != nil && strings.HasPrefix(name, r.statPrefix) {
		return nil, r.statErr
	}
	return r.fileRoot.Stat(name)
}

func (r sweepRoot) Remove(name string) error {
	if r.removeErr != nil {
		return r.removeErr
	}
	return r.fileRoot.Remove(name)
}

type sweepFS struct {
	fs.FS
	dir string
	err error
}

func (f sweepFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if f.err != nil && strings.HasPrefix(name, f.dir) {
		for i, e := range entries {
			entries[i] = errInfoEntry{DirEntry: e, err: f.err}
		}
	}
	return entries, err
}

type errInfoEntry struct {
	fs.DirEntry
	err error
}

func (e errInfoEntry) Info() (fs.FileInfo, error) { return nil, e.err }

// endsAfter is a context whose Err reports cancellation from its n+1th
// call on, so a sweep is cancelled between two entries.
type endsAfter struct {
	context.Context
	n     int
	calls int
}

func (c *endsAfter) Err() error {
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

// seedStaging returns a driver over a new directory holding one multipart
// upload (when upload) and one staging file (when staged).
func seedStaging(t *testing.T, upload, staged bool) *Driver {
	t.Helper()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if upload {
		if _, err := d.CreateMultipart(t.Context(), "a.bin", filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if staged {
		if err := d.root.WriteFile(tmpDir+"/"+randomName(), []byte("x"), DefaultFilePrivate); err != nil {
			t.Fatal(err)
		}
		// A directory in tmp is not a staging file the driver wrote.
		if err := d.root.MkdirAll(tmpDir+"/SUBDIR", DefaultDirMode); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestSweepAbandonedRacesAndFailures(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := []struct {
		name          string
		upload, stage bool
		wrap          func(fileRoot) fileRoot
		ctx           func(context.Context) context.Context
		wantN         int
		wantErr       error
	}{
		{name: "upload vanishes before its stat", upload: true,
			wrap: func(r fileRoot) fileRoot {
				return sweepRoot{fileRoot: r, statPrefix: multipartDir + "/", statErr: fs.ErrNotExist}
			},
			wantN: 0},
		{name: "part vanishes before its stat", upload: true,
			wrap: func(r fileRoot) fileRoot {
				return sweepRoot{fileRoot: r, infoDir: multipartDir + "/", infoErr: fs.ErrNotExist}
			},
			wantN: 1},
		{name: "part stat fails", upload: true,
			wrap: func(r fileRoot) fileRoot {
				return sweepRoot{fileRoot: r, infoDir: multipartDir + "/", infoErr: errInjected}
			},
			wantErr: errInjected},
		{name: "staging file vanishes before its stat", stage: true,
			wrap:  func(r fileRoot) fileRoot { return sweepRoot{fileRoot: r, infoDir: tmpDir, infoErr: fs.ErrNotExist} },
			wantN: 0},
		{name: "staging file stat fails", stage: true,
			wrap:    func(r fileRoot) fileRoot { return sweepRoot{fileRoot: r, infoDir: tmpDir, infoErr: errInjected} },
			wantErr: errInjected},
		{name: "staging file removed by its write meanwhile", stage: true,
			wrap:  func(r fileRoot) fileRoot { return sweepRoot{fileRoot: r, removeErr: fs.ErrNotExist} },
			wantN: 1},
		{name: "staging file remove fails", stage: true,
			wrap:    func(r fileRoot) fileRoot { return sweepRoot{fileRoot: r, removeErr: errInjected} },
			wantErr: errInjected},
		{name: "cancelled between uploads", upload: true,
			ctx:     func(ctx context.Context) context.Context { return &endsAfter{Context: ctx, n: 1} },
			wantErr: context.Canceled},
		{name: "cancelled between staging files", stage: true,
			ctx:     func(ctx context.Context) context.Context { return &endsAfter{Context: ctx, n: 1} },
			wantErr: context.Canceled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := seedStaging(t, c.upload, c.stage)
			if c.wrap != nil {
				d.root = c.wrap(d.root)
			}
			ctx := t.Context()
			if c.ctx != nil {
				ctx = c.ctx(ctx)
			}
			n, err := d.SweepAbandoned(ctx, future)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil || n != c.wantN {
				t.Fatalf("sweep = %d, %v; want %d", n, err, c.wantN)
			}
		})
	}
}
