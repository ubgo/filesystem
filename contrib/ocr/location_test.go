package ocr_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// twoDiskStore is newStore with a second disk, "search", in the Manager.
func twoDiskStore(t *testing.T, p *ocr.Plugin) (*records.Store, *filesystem.Disk, *filesystem.Disk) {
	t.Helper()
	media, _ := filesystem.NewDisk("media", memory.New())
	search, _ := filesystem.NewDisk("search", memory.New())
	mgr, err := filesystem.NewManager(media, search)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store, media, search
}

// keysOn lists every key on d.
func keysOn(t *testing.T, d *filesystem.Disk) []string {
	t.Helper()
	var keys []string
	for e, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, e.Key)
	}
	return keys
}

// TestLocation_CustomDiskAndPrefix pins B10: WithLocation puts the text on
// the disk and under the prefix it returns, the disk is recorded, and a
// replacement and the purge delete it there.
func TestLocation_CustomDiskAndPrefix(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	loc := func(f records.File, name string) (string, string, error) {
		return "search", f.Scope + "/" + name + "/" + f.ID + "-", nil
	}
	store, media, search := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}, ocr.WithLocation(loc)))
	ctx := t.Context()
	f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	disk, key, ok := ocr.TextLocation(f)
	if !ok || disk != "search" || f.Metadata[ocr.MetaTextDisk] != "search" || !strings.HasPrefix(key, scope+"/text/"+f.ID+"-") || !strings.HasSuffix(key, ocr.TextSuffix) {
		t.Fatalf("text at %s:%s (%v)", disk, key, f.Metadata)
	}
	if body, err := search.ReadAll(ctx, key); err != nil || string(body) != "receipt" {
		t.Fatalf("text = %q, %v", body, err)
	}
	if keys := keysOn(t, media); len(keys) != 1 || keys[0] != f.Key {
		t.Fatalf("media disk holds %v, want only the image", keys)
	}
	next, err := store.Replace(ctx, scope, f.ID, bytes.NewReader(smallPNG(t, 9, 9)), "", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	next = reread(t, store, next)
	if _, nextKey, _ := ocr.TextLocation(next); nextKey == key || exists(t, search, key) || !exists(t, search, nextKey) {
		t.Fatalf("after replace: new %q, old exists %v", nextKey, exists(t, search, key))
	}
	if err := store.Purge(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if keys := keysOn(t, search); len(keys) != 0 {
		t.Fatalf("texts outlived their purged image: %v", keys)
	}
}

// TestLocation_ErrorFailsClosed pins B10: a Location error writes nothing,
// records no text, and marks the image failed with the error's code.
func TestLocation_ErrorFailsClosed(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	loc := func(records.File, string) (string, string, error) {
		return "", "", filesystem.Errorf(filesystem.CodeUnavailable, "the search bucket is down")
	}
	store, media, search := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}, ocr.WithLocation(loc)))
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	if f.Status != records.StatusFailed || f.StatusDetail != ocr.StatusFailedPrefix+string(filesystem.CodeUnavailable) {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	if _, ok := ocr.TextKey(f); ok {
		t.Fatalf("a text was recorded: %v", f.Metadata)
	}
	if keys := append(keysOn(t, media), keysOn(t, search)...); len(keys) != 1 || keys[0] != f.Key {
		t.Fatalf("disks hold %v, want only the image", keys)
	}
}

// TestLocation_Default: without WithLocation (and with a nil one) the
// text stays beside its image on its disk, no disk recorded.
func TestLocation_Default(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	store, _, _ := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}, ocr.WithLocation(nil)))
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	disk, key, ok := ocr.TextLocation(f)
	if _, recorded := f.Metadata[ocr.MetaTextDisk]; !ok || recorded || disk != "media" || !strings.HasPrefix(key, f.Key+ocr.TextInfix) {
		t.Fatalf("text at %s:%s (%v)", disk, key, f.Metadata)
	}
}

// TestLocation_UnknownDisk: a Location naming a disk the Store's manager
// does not have marks the image failed and records no text.
func TestLocation_UnknownDisk(t *testing.T) {
	prog := fakeTesseract(t, `echo "receipt"`)
	loc := func(records.File, string) (string, string, error) { return "nowhere", "texts/", nil }
	store, media, search := twoDiskStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}, ocr.WithLocation(loc)))
	f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	if f.Status != records.StatusFailed || f.StatusDetail != ocr.StatusFailedPrefix+string(filesystem.CodeNotFound) {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	if _, ok := ocr.TextKey(f); ok {
		t.Fatalf("a text was recorded: %v", f.Metadata)
	}
	if keys := append(keysOn(t, media), keysOn(t, search)...); len(keys) != 1 || keys[0] != f.Key {
		t.Fatalf("disks hold %v, want only the image", keys)
	}
}
