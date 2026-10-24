package ocr_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/records"
)

// sweepAll runs SweepOrphans over the whole of disk, every object old
// enough, and returns how many it deleted.
func sweepAll(t *testing.T, s *records.Store, disk string) int {
	t.Helper()
	n, err := s.SweepOrphans(t.Context(), disk, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// uploadImage records an image and returns it as Process left it.
func uploadImage(t *testing.T, s *records.Store) records.File {
	t.Helper()
	f, err := s.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	return reread(t, s, f)
}

// TestOwnedObjects_SweepKeepsTexts pins the ObjectOwner: SweepOrphans
// never deletes a text whose file row (live or trashed) names it, on the
// image's disk or on a WithLocation disk, and still deletes one whose
// image is gone.
func TestOwnedObjects_SweepKeepsTexts(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	onSearch := func(f records.File, _ string) (string, string, error) { return "search", "texts/" + f.ID + "-", nil }
	for name, opts := range map[string][]ocr.PluginOption{"default": nil, "location": {ocr.WithLocation(onSearch)}} {
		t.Run(name, func(t *testing.T) {
			store, media, search := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}, opts...))
			on := map[string]*filesystem.Disk{"media": media, "search": search}
			f := uploadImage(t, store)
			disk, key, ok := ocr.TextLocation(f)
			if !ok {
				t.Fatalf("no text: %v", f.Metadata)
			}
			if n := sweepAll(t, store, disk); n != 0 {
				t.Fatalf("the sweep deleted %d objects of a live image", n)
			}
			if err := store.Trash(t.Context(), scope, f.ID); err != nil {
				t.Fatal(err)
			}
			if n := sweepAll(t, store, disk); n != 0 || !exists(t, on[disk], key) {
				t.Fatalf("the sweep deleted %d objects of a trashed image", n)
			}
			if err := store.Purge(t.Context(), scope, f.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := on[disk].WriteBytes(t.Context(), key, []byte("left behind"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			if n := sweepAll(t, store, disk); n != 1 || exists(t, on[disk], key) {
				t.Fatalf("the orphaned text survived the sweep (%d deleted)", n)
			}
		})
	}
}

// TestMoveFileDisk_MovesTheText pins the FileDiskMover: MoveFile takes a
// text on the image's disk along, and leaves one WithLocation put on a
// disk of its own.
func TestMoveFileDisk_MovesTheText(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	store, media, search := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}))
	f := uploadImage(t, store)
	_, key, _ := ocr.TextLocation(f)
	moved, err := store.MoveFile(t.Context(), scope, f.ID, "search")
	if err != nil {
		t.Fatal(err)
	}
	if disk, k, _ := ocr.TextLocation(moved); disk != "search" || k != key || !exists(t, search, key) || exists(t, media, key) {
		t.Fatalf("text after the move at %s:%s (left on media %v)", disk, k, exists(t, media, key))
	}
	elsewhere := func(f records.File, _ string) (string, string, error) { return "search", "texts/" + f.ID + "-", nil }
	store2, _, search2 := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}, ocr.WithLocation(elsewhere)))
	g := uploadImage(t, store2)
	_, gKey, _ := ocr.TextLocation(g)
	movedG, err := store2.MoveFile(t.Context(), scope, g.ID, "search")
	if err != nil {
		t.Fatal(err)
	}
	if disk, k, _ := ocr.TextLocation(movedG); disk != "search" || k != gKey || !exists(t, search2, gKey) {
		t.Fatalf("a located text moved: %s:%s", disk, k)
	}
}
