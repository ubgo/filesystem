package webdav_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/adapters/webdav"
)

// TestStagingDir proves a custom staging collection is used, reserved and
// hidden from listings, frees the default name, and that invalid names
// are refused before any request.
func TestStagingDir(t *testing.T) {
	ctx := t.Context()
	s := newServer(t)
	if err := os.MkdirAll(filepath.Join(s.dir, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := s.config()
	cfg.StagingDir = "_staging"
	drv, err := webdav.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("webdav", drv)
	if _, err := d.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "app", "_staging", "tmp")); err != nil {
		t.Fatalf("custom staging collection not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "app", webdav.StagingDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default staging collection created anyway: %v", err)
	}
	if _, err := d.WriteBytes(ctx, "_staging/x", []byte("x"), filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Fatalf("write under the staging collection: %v, want CodeInvalidKey", err)
	}
	page, err := d.ListPage(ctx, "", filesystem.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Key != "a.txt" {
		t.Errorf("listing = %+v", page.Entries)
	}
	before := s.counts["PROPFIND"].Load()
	for _, bad := range []string{"a/b", ".", "..", `a\b`} {
		cfg := s.config()
		cfg.StagingDir = bad
		if _, err := webdav.New(context.Background(), cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("StagingDir %q: %v, want CodeInvalidArgument", bad, err)
		}
	}
	if s.counts["PROPFIND"].Load() != before {
		t.Error("an invalid StagingDir reached the server")
	}
}
