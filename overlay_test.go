package filesystem_test

import (
	"slices"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

func TestOverlayConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		o, err := filesystem.Overlay("overlay", mustDisk(t, "upper", memory.New()), mustDisk(t, "lower", memory.New()))
		if err != nil {
			t.Fatal(err)
		}
		return o
	})
}

func TestOverlayCopyOnWrite(t *testing.T) {
	ctx := t.Context()
	upper := mustDisk(t, "upper", memory.New())
	lower := mustDisk(t, "lower", memory.New())
	for _, k := range []string{"keep.txt", "edit.txt", "gone.txt", "d/one.txt", "d/two.txt", "e/x.txt"} {
		if _, err := lower.WriteBytes(ctx, k, []byte("lower"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	o, err := filesystem.Overlay("o", upper, lower)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := o.ReadAll(ctx, "keep.txt"); string(b) != "lower" {
		t.Errorf("read falls through to %q", b)
	}
	if _, err := o.WriteBytes(ctx, "edit.txt", []byte("upper"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := o.ReadAll(ctx, "edit.txt"); string(b) != "upper" {
		t.Errorf("upper does not win: %q", b)
	}
	for _, k := range []string{"gone.txt", "d/one.txt", "d/two.txt"} {
		if err := o.Delete(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := o.Exists(ctx, "gone.txt"); ok {
		t.Error("deleted lower key still visible")
	}
	if _, err := o.ReadAll(ctx, "gone.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("read of a whited-out key: %v", err)
	}
	// lower is never changed.
	if got := allKeys(t, lower, ""); len(got) != 6 {
		t.Errorf("lower changed: %v", got)
	}
	if b, _ := lower.ReadAll(ctx, "edit.txt"); string(b) != "lower" {
		t.Errorf("lower overwritten: %q", b)
	}
	// Whiteouts and fully whited-out directories are hidden.
	if got := keysOf(t, o, "", false); !slices.Equal(got, []string{"e/", "edit.txt", "keep.txt"}) {
		t.Errorf("listing %v", got)
	}
	if got := keysOf(t, o, "", true); !slices.Equal(got, []string{"e/x.txt", "edit.txt", "keep.txt"}) {
		t.Errorf("recursive listing %v", got)
	}
	// Writing a whited-out key brings it back.
	if _, err := o.WriteBytes(ctx, "gone.txt", []byte("again"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(ctx, "gone.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.WriteBytes(ctx, "gone.txt", []byte("back"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := o.ReadAll(ctx, "gone.txt"); string(b) != "back" {
		t.Errorf("rewritten key reads %q", b)
	}
	// A move of a lower key copies it up and whites it out.
	if _, err := o.Move(ctx, "keep.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := o.Exists(ctx, "keep.txt"); ok {
		t.Error("moved lower key still visible")
	}
	// The whiteout directory is reserved.
	if _, err := o.WriteBytes(ctx, filesystem.OverlayWhiteouts+"/x", nil, filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("write into the whiteouts: %v", err)
	}
	if _, err := o.ListPage(ctx, filesystem.OverlayWhiteouts, filesystem.ListOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("list of the whiteouts: %v", err)
	}
	// In any letter case: a case-folding upper disk would otherwise store
	// this as a whiteout hiding the lower "edit.txt".
	if _, err := o.WriteBytes(ctx, ".OVERLAY-WHITEOUTS/edit.txt", nil, filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("write into the whiteouts in upper case: %v", err)
	}
	if _, err := o.Stat(ctx, ".Overlay-Whiteouts/edit.txt"); filesystem.CodeOf(err) != filesystem.CodeInvalidKey {
		t.Errorf("stat into the whiteouts in mixed case: %v", err)
	}
	if _, err := filesystem.Overlay("o", upper, upper); err == nil {
		t.Error("an overlay of a disk over itself was accepted")
	}
}
