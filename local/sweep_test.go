package local_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/local"
)

// age sets the modification time of every file and directory under p,
// p included, to t.
func age(t *testing.T, p string, at time.Time) {
	t.Helper()
	err := filepath.WalkDir(p, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, at, at)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSweepAbandonedStaging(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	_, d := open(t, dir)
	staging := filepath.Join(dir, local.StagingDir)
	old := time.Now().Add(-48 * time.Hour)
	cutoff := time.Now().Add(-24 * time.Hour)

	// An upload idle for two days, one started two days ago that received a
	// part just now, and a fresh one.
	idle, err := d.CreateMultipart(ctx, "a.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := d.CreateMultipart(ctx, "b.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := d.CreateMultipart(ctx, "c.bin", filesystem.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UploadPart(ctx, "a.bin", idle, 1, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	age(t, filepath.Join(staging, "multipart", idle), old)
	age(t, filepath.Join(staging, "multipart", busy), old)
	if _, err := d.UploadPart(ctx, "b.bin", busy, 1, strings.NewReader("y"), 1); err != nil {
		t.Fatal(err)
	}

	// Staging files: one a crash left two days ago, one being written now.
	tmp := filepath.Join(staging, "tmp")
	// Staging names have the driver's random shape (26 base32 characters).
	crashed, writing := filepath.Join(tmp, "CRASHEDAAAAAAAAAAAAAAAAAAA"), filepath.Join(tmp, "WRITINGAAAAAAAAAAAAAAAAAAA")
	for _, p := range []string{crashed, writing} {
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	age(t, crashed, old)
	// Entries the driver did not create are left alone, however old.
	foreign := filepath.Join(staging, "multipart", "not-an-upload-id")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	age(t, foreign, old)
	foreignTmp := filepath.Join(tmp, "notes.txt")
	if err := os.WriteFile(foreignTmp, []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	age(t, foreignTmp, old)
	// A stored object of any age is never touched.
	if _, err := d.WriteBytes(ctx, "kept.txt", []byte("k"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	age(t, filepath.Join(dir, "kept.txt"), old)

	n, err := d.SweepAbandoned(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d, want the idle upload and the crashed staging file", n)
	}
	_, err = d.ListParts(ctx, "a.bin", idle)
	wantCode(t, err, filesystem.CodeNotFound)
	for _, id := range []string{busy, fresh} {
		if _, err := os.Stat(filepath.Join(staging, "multipart", id)); err != nil {
			t.Errorf("active upload %s removed: %v", id, err)
		}
	}
	for p, want := range map[string]bool{crashed: false, writing: true, foreign: true, foreignTmp: true, filepath.Join(dir, "kept.txt"): true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", p, err == nil, want)
		}
	}

	// Running it again finds nothing more.
	if n, err := d.SweepAbandoned(ctx, cutoff); n != 0 || err != nil {
		t.Errorf("second sweep = %d, %v", n, err)
	}
}

func TestSweepAbandonedWithoutStaging(t *testing.T) {
	dir := t.TempDir()
	_, d := open(t, dir)
	// The multipart directory is created only by the first upload, and tmp
	// may be removed by hand; neither is an error.
	if err := os.RemoveAll(filepath.Join(dir, local.StagingDir)); err != nil {
		t.Fatal(err)
	}
	if n, err := d.SweepAbandoned(t.Context(), time.Now()); n != 0 || err != nil {
		t.Errorf("sweep = %d, %v", n, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := d.SweepAbandoned(cancelled, time.Now())
	wantCode(t, err, filesystem.CodeUnavailable)
}
