package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/dimensions"
	"github.com/ubgo/filesystem/records"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"photo.png: 640x480, metadata map[height:480 width:640]", "notes.txt: no dimensions",
		"broken.png: no dimensions", "broken.png with WithStrict: rejected", "photo.png after replace: 1920x1080",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestStrictLeavesNothing checks a refused image leaves neither a row nor
// an object: Upload deletes the object when the row is refused.
func TestStrictLeavesNothing(t *testing.T) {
	ctx := t.Context()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	disks, _ := newDisks()
	s, err := newStore(ctx, db, disks, dimensions.WithStrict())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = uploadAndMeasure(ctx, s, "broken.png", []byte(pngSignature+"garbage"))
	if filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("strict upload = %v", err)
	}
	page, err := s.Files(ctx, records.FileQuery{Scope: tenant})
	if err != nil || len(page.Files) != 0 {
		t.Fatalf("files after a refused upload = %d, %v", len(page.Files), err)
	}
	disk, _ := disks.Disk(diskName)
	for e, err := range disk.List(ctx, "files", filesystem.ListOptions{Recursive: true}) {
		t.Fatalf("object left behind: %s %v", e.Key, err)
	}
}

func TestOfWithoutMetadata(t *testing.T) {
	if _, _, ok := dimensions.Of(records.File{}); ok {
		t.Fatal("Of reported dimensions for a file without metadata")
	}
}
