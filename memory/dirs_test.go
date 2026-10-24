package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

func TestDirsAndPreviewHonourContext(t *testing.T) {
	d := memory.New()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.MakeDir(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("MakeDir: %v", err)
	}
	if _, err := d.DirExists(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("DirExists: %v", err)
	}
	if _, err := d.FindAbandoned(ctx, time.Now()); !errors.Is(err, context.Canceled) {
		t.Errorf("FindAbandoned: %v", err)
	}
}

func TestMadeDirectoriesLastUntilDeletePrefix(t *testing.T) {
	ctx := t.Context()
	d := memory.New()
	if err := d.MakeDir(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	disk, err := filesystem.NewDisk("mem", d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(ctx, "a/b/k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := disk.Delete(ctx, "a/b/k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := disk.DirExists(ctx, "a/b"); !ok {
		t.Error("a made directory went with its last key")
	}
	if err := disk.DeletePrefix(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := disk.DirExists(ctx, "a/b"); ok {
		t.Error("a made directory outlived DeletePrefix of its parent")
	}
}
