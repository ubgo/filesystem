package ocr_test

import (
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// TestRenameDisk_RewritesTextDisk pins the DiskRenamer: after
// Store.RenameDisk, a text WithLocation put on the renamed disk is found
// under the new name (TextLocation, the sweep, the purge). "search2" is
// a second name for search's storage, as a rename requires.
func TestRenameDisk_RewritesTextDisk(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	media, _ := filesystem.NewDisk("media", memory.New())
	shared := memory.New()
	search, _ := filesystem.NewDisk("search", shared)
	search2, _ := filesystem.NewDisk("search2", shared)
	mgr, err := filesystem.NewManager(media, search, search2)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	loc := func(f records.File, _ string) (string, string, error) { return "search", "texts/" + f.ID + "-", nil }
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(ocr.NewPlugin(ocr.Tool{Tesseract: prog}, ocr.WithLocation(loc))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := uploadImage(t, store)
	_, key, _ := ocr.TextLocation(f)
	if _, err := store.RenameDisk(t.Context(), "search", "search2"); err != nil {
		t.Fatal(err)
	}
	got := reread(t, store, f)
	if disk, k, ok := ocr.TextLocation(got); !ok || disk != "search2" || k != key || !exists(t, search2, k) {
		t.Fatalf("text after the rename at %s:%s (%v)", disk, k, got.Metadata)
	}
	if n := sweepAll(t, store, "search2"); n != 0 {
		t.Fatalf("the sweep of the renamed disk deleted %d objects", n)
	}
	if err := store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if exists(t, search2, key) {
		t.Fatal("the text outlived its purged image")
	}
}
