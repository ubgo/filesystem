package ocr_test

import (
	"context"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/records"
)

// cloudOCR stands in for a cloud OCR service: it returns text, or err.
type cloudOCR struct {
	text string
	err  error
	seen int
}

func (c *cloudOCR) Recognize(_ context.Context, image io.Reader) (string, error) {
	c.seen++
	if _, err := io.Copy(io.Discard, image); err != nil {
		return "", err
	}
	return c.text, c.err
}

// TestPlugin_CustomRecognizer pins the Recognizer seam: another engine
// reads the types WithContentTypes names (TIFF here, which Tool does not
// read), the default PNG type is then left alone, and an engine's error
// fails closed: the file is marked failed with the error's code and gets
// no text.
func TestPlugin_CustomRecognizer(t *testing.T) {
	ctx := t.Context()
	create := func(store *records.Store, disk *filesystem.Disk, key, ct string) records.File {
		t.Helper()
		if _, err := disk.WriteBytes(ctx, key, []byte("scan"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: key, Filename: key, ContentType: ct})
		if err != nil {
			t.Fatal(err)
		}
		return reread(t, store, f)
	}

	cloud := &cloudOCR{text: "invoice 7"}
	store, disk := newStore(t, ocr.NewPlugin(cloud, ocr.WithContentTypes("Image/TIFF")))
	f := create(store, disk, "a.tiff", "image/tiff; x=1")
	key, ok := ocr.TextKey(f)
	if !ok || f.Status != records.StatusReady {
		t.Fatalf("tiff: %q %v", f.Status, f.Metadata)
	}
	if body, err := disk.ReadAll(ctx, key); err != nil || string(body) != "invoice 7" {
		t.Fatalf("text = %q, %v", body, err)
	}
	if g := create(store, disk, "b.png", "image/png"); g.Metadata != nil || cloud.seen != 1 {
		t.Fatalf("a type outside WithContentTypes was read: %v, %d calls", g.Metadata, cloud.seen)
	}

	failing := &cloudOCR{err: filesystem.Errorf(filesystem.CodeUnavailable, "cloud down")}
	store, disk = newStore(t, ocr.NewPlugin(failing, ocr.WithContentTypes("image/tiff")))
	f = create(store, disk, "c.tiff", "image/tiff")
	if f.Status != records.StatusFailed || f.StatusDetail != ocr.StatusFailedPrefix+string(filesystem.CodeUnavailable) || f.Metadata[ocr.MetaTextKey] != "" {
		t.Fatalf("a failing engine: %q %q %v", f.Status, f.StatusDetail, f.Metadata)
	}

	// The default types are unchanged: Tool's three.
	if got := ocr.ContentTypes(); len(got) != 3 {
		t.Fatalf("ContentTypes = %v", got)
	}
	// An empty WithContentTypes keeps them: a PNG is read, a TIFF is not.
	kept := &cloudOCR{text: "receipt"}
	store, disk = newStore(t, ocr.NewPlugin(kept, ocr.WithContentTypes()))
	if f := create(store, disk, "d.png", "image/png"); f.Status != records.StatusReady || kept.seen != 1 {
		t.Fatalf("default type with an empty list: %q, %d calls", f.Status, kept.seen)
	}
	if g := create(store, disk, "e.tiff", "image/tiff"); g.Metadata != nil || kept.seen != 1 {
		t.Fatalf("a TIFF was read with the default types: %v, %d calls", g.Metadata, kept.seen)
	}
}
