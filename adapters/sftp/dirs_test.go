package sftp_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	fssftp "github.com/ubgo/filesystem/adapters/sftp"
)

// TestFindAbandoned proves the preview reports exactly the staged files a
// sweep removes, removes nothing, and agrees with the sweep after it.
func TestFindAbandoned(t *testing.T) {
	ctx := t.Context()
	drv, d, root := driver(t)
	if !d.Capabilities().FindAbandoned {
		t.Fatal("FindAbandoned not declared")
	}
	old := time.Now().Add(-48 * time.Hour)
	abandoned := staged(t, root, "abcdefghijklmnopqrstuvwxyz", old)
	staged(t, root, "zyxwvutsrqponmlkjihgfedcba", time.Now())
	staged(t, root, "notes.txt", old)
	cutoff := time.Now().Add(-24 * time.Hour)
	found, err := d.FindAbandoned(ctx, cutoff)
	if err != nil || len(found) != 1 {
		t.Fatalf("preview = %+v, %v; want one staged file", found, err)
	}
	if a := found[0]; a.Kind != filesystem.AbandonedStaging || a.ID != "abcdefghijklmnopqrstuvwxyz" || a.Key != "" || !a.LastActivity.Before(cutoff) {
		t.Errorf("reported %+v", a)
	}
	if _, err := os.Stat(abandoned); err != nil {
		t.Fatalf("the preview removed the file: %v", err)
	}
	if n, err := d.SweepAbandoned(ctx, cutoff); err != nil || n != len(found) {
		t.Fatalf("sweep after the preview = %d, %v", n, err)
	}
	if found, err := d.FindAbandoned(ctx, cutoff); err != nil || len(found) != 0 {
		t.Errorf("preview after the sweep = %+v, %v", found, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := drv.FindAbandoned(cancelled, cutoff); err == nil {
		t.Error("preview with a cancelled context succeeded")
	}
	if os.Geteuid() != 0 {
		tmp := filepath.Join(root, fssftp.StagingDir, "tmp")
		_ = os.Chmod(tmp, 0o000)
		_, err := drv.FindAbandoned(ctx, cutoff)
		_ = os.Chmod(tmp, 0o700)
		if err == nil {
			t.Error("preview of an unlistable staging directory succeeded")
		}
	}
}

// TestMakeDirAndDirExists covers the cases the conformance suite cannot
// reach: a file on the path, a reserved key, an unreadable directory and
// a cancelled context.
func TestMakeDirAndDirExists(t *testing.T) {
	ctx := t.Context()
	drv, d, root := driver(t)
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
	if err := d.MakeDir(ctx, fssftp.StagingDir+"/x"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("MakeDir in the staging directory: %v", err)
	}
	if _, err := d.DirExists(ctx, fssftp.StagingDir); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("DirExists of the staging directory: %v", err)
	}
	// An empty directory goes with the last file deleted under it.
	if err := d.MakeDir(ctx, "e"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.WriteBytes(ctx, "e/k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(ctx, "e/k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.DirExists(ctx, "e"); ok {
		t.Error("the directory outlived its last file")
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(root, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		if _, err := d.DirExists(ctx, "locked/inner"); err == nil {
			t.Error("DirExists inside an unreadable directory answered without an error")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := drv.MakeDir(cancelled, "c"); err == nil {
		t.Error("MakeDir with a cancelled context succeeded")
	}
	if _, err := drv.DirExists(cancelled, "c"); err == nil {
		t.Error("DirExists with a cancelled context succeeded")
	}
}
