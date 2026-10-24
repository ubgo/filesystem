package ocr_test

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_QuarantinedImage: a quarantined image is recorded without
// running the engine on its unscanned bytes, Process refuses it, and the
// scan passing reads its text; a replacement quarantined again is not
// read until its own release.
func TestPlugin_QuarantinedImage(t *testing.T) {
	ctx := t.Context()
	counter := t.TempDir() + "/n"
	prog := fakeTesseract(t, `n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`; echo "receipt $n"`)
	runs := func() string { b, _ := os.ReadFile(counter); return string(bytes.TrimSpace(b)) }
	store, disk := newStore(t, ocr.NewPlugin(ocr.Tool{Tesseract: prog}))
	f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy, records.Quarantine())
	if err != nil {
		t.Fatalf("a quarantined upload with OCR installed = %v", err)
	}
	if f = reread(t, store, f); !f.Quarantined() || runs() != "" {
		t.Fatalf("status %q, engine runs %q", f.Status, runs())
	}
	if err := ocr.NewPlugin(ocr.Tool{Tesseract: prog}).Process(ctx, store, scope, f.ID); !errors.Is(err, records.ErrQuarantined) || runs() != "" {
		t.Fatalf("Process of a quarantined image = %v, runs %q", err, runs())
	}
	if _, err := store.PassScan(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	f = reread(t, store, f)
	key, ok := ocr.TextKey(f)
	if f.Status != records.StatusReady || !ok || runs() != "1" {
		t.Fatalf("after release: %q %v, runs %q", f.Status, f.Metadata, runs())
	}
	if body, err := disk.ReadAll(ctx, key); err != nil || string(body) != "receipt 1" {
		t.Fatalf("text = %q, %v", body, err)
	}

	next, err := store.Replace(ctx, scope, f.ID, bytes.NewReader(smallPNG(t, 9, 9)), "", imagePolicy, records.QuarantineContent())
	if err != nil {
		t.Fatal(err)
	}
	if next = reread(t, store, next); !next.Quarantined() || runs() != "1" {
		t.Fatalf("quarantined replacement: %q, runs %q", next.Status, runs())
	}
	if _, err := store.PassScan(ctx, scope, f.ID); err != nil || runs() != "2" {
		t.Fatalf("second release: %v, runs %q", err, runs())
	}
}
