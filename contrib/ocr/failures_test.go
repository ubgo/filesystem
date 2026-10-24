package ocr_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// fakeMagic starts an image of a format a test registers with the image
// package, as any imported decoder package would.
const fakeMagic = "FAKEIMG!"

func init() {
	image.RegisterFormat("fakeimg", fakeMagic, func(io.Reader) (image.Image, error) {
		return image.NewGray(image.Rect(0, 0, 1, 1)), nil
	}, func(io.Reader) (image.Config, error) {
		return image.Config{Width: 1, Height: 1}, nil
	})
}

// TestRecognize_OtherRegisteredFormats pins that an image of a format
// some other imported package taught image.Decode is still refused: only
// PNG, JPEG and GIF reach tesseract.
func TestRecognize_OtherRegisteredFormats(t *testing.T) {
	tool := ocr.Tool{Tesseract: fakeTesseract(t, "echo should not run")}
	if _, err := tool.Recognize(t.Context(), strings.NewReader(fakeMagic+"rest")); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("got %v, want CodeRejected", err)
	}
}

// TestRecognize_FailingInput pins that an image whose reader fails is
// that read error.
func TestRecognize_FailingInput(t *testing.T) {
	broken := errors.New("upload connection reset")
	tool := ocr.Tool{Tesseract: fakeTesseract(t, "echo should not run")}
	if _, err := tool.Recognize(t.Context(), iotest.ErrReader(broken)); !errors.Is(err, broken) {
		t.Fatalf("got %v, want the reader's error", err)
	}
}

// failingStore returns a store (and its database) whose database steps
// faults can fail one at a time, over a disk whose writes of OCR text keys fail while
// failText is set. The plugin defers its work to Process and Resume.
func failingStore(t *testing.T, p *ocr.Plugin, failText *bool) (*records.Store, *filesystem.Disk, *recordstest.Faults, *sql.DB) {
	t.Helper()
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if failText != nil && *failText && op.Kind == filesystem.OpWrite && strings.HasSuffix(op.Key, ocr.TextSuffix) {
			return filesystem.Errorf(filesystem.CodeQuotaExceeded, "injected")
		}
		return call(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, faults := recordstest.OpenFaulty(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store, disk, faults, db
}

// TestProcess_EveryDatabaseStepFailsCleanly measures Process's database
// steps, then reruns it on a fresh database once per step with that step
// failing: every run returns an error, none reports the file read.
func TestProcess_EveryDatabaseStepFailsCleanly(t *testing.T) {
	prepared := func(t *testing.T) (*ocr.Plugin, *records.Store, *recordstest.Faults, string) {
		p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo receipt")}, ocr.WithDeferredProcessing())
		store, _, faults, _ := failingStore(t, p, nil)
		f, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
		if err != nil {
			t.Fatal(err)
		}
		return p, store, faults, f.ID
	}
	p, store, faults, id := prepared(t)
	faults.Arm(0)
	if err := p.Process(t.Context(), store, scope, id); err != nil {
		t.Fatalf("unfaulted run: %v", err)
	}
	steps := faults.Steps()
	for n := 1; n <= steps; n++ {
		p, store, faults, id := prepared(t)
		faults.Arm(n)
		if err := p.Process(t.Context(), store, scope, id); err == nil {
			t.Errorf("step %d of %d failing: Process succeeded", n, steps)
		}
	}
}

// TestResume_FailuresAreJoined pins that Resume reports a failing query,
// and runs every file even when one fails, returning the failures joined.
func TestResume_FailuresAreJoined(t *testing.T) {
	ctx := t.Context()
	p := ocr.NewPlugin(ocr.Tool{Tesseract: "/nonexistent/tesseract"}, ocr.WithDeferredProcessing())
	store, _, faults, _ := failingStore(t, p, nil)
	for range 2 {
		if _, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy); err != nil {
			t.Fatal(err)
		}
	}
	faults.Arm(1)
	if n, err := p.Resume(ctx, store, time.Now().Add(time.Minute)); n != 0 || !errors.Is(err, recordstest.ErrInjected) {
		t.Fatalf("failing query: %d, %v", n, err)
	}
	faults.Arm(0)
	n, err := p.Resume(ctx, store, time.Now().Add(time.Minute))
	if n != 2 || !errors.Is(err, ocr.ErrNoProgram) {
		t.Fatalf("Resume = %d, %v; want both run and the missing program reported", n, err)
	}
}

// TestProcess_Edges pins Process on a file that is not an image (settled
// without reading), on an image whose object is gone or whose disk the
// store no longer has (marked failed, error returned), on an image whose
// text cannot be stored, and on an image purged while it was read (the
// text deleted, no error: the file is gone).
func TestProcess_Edges(t *testing.T) {
	ctx := t.Context()
	failText := false
	p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo receipt")}, ocr.WithDeferredProcessing())
	store, disk, _, db := failingStore(t, p, &failText)
	upload := func(name string) records.File {
		t.Helper()
		f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), name, imagePolicy)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	txt, err := store.Upload(ctx, scope, "media", strings.NewReader("notes"), "n.txt", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, store, scope, txt.ID); err != nil {
		t.Fatalf("a text file: %v", err)
	}

	gone := upload("gone.png")
	if err := disk.Delete(ctx, gone.Key); err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, store, scope, gone.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing object: %v, want CodeNotFound", err)
	}
	if f := reread(t, store, gone); f.Status != records.StatusFailed {
		t.Fatalf("missing object left status %q", f.Status)
	}

	failText = true
	unstored := upload("unstored.png")
	if err := p.Process(ctx, store, scope, unstored.ID); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("unstorable text: %v, want CodeQuotaExceeded", err)
	}
	failText = false

	// A second store over the same rows whose manager lacks the disk.
	other, err := filesystem.NewDisk("elsewhere", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(other)
	if err != nil {
		t.Fatal(err)
	}
	blind, err := records.New(db, store.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Process(ctx, blind, scope, upload("nodisk.png").ID); err == nil {
		t.Fatal("an image on a disk the store does not have was processed")
	}
}

// TestProcess_PurgedWhileRead pins that an image purged while tesseract
// read it is not resurrected: Process returns nil and its text is deleted.
func TestProcess_PurgedWhileRead(t *testing.T) {
	ctx := t.Context()
	var store *records.Store
	var id string
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite && strings.HasSuffix(op.Key, ocr.TextSuffix) {
			if err := store.Purge(ctx, scope, id); err != nil {
				return err
			}
		}
		return call(ctx)
	}))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo receipt")}, ocr.WithDeferredProcessing())
	if store, err = records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := store.Upload(ctx, scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
	if err != nil {
		t.Fatal(err)
	}
	id = f.ID
	if err := p.Process(ctx, store, scope, id); err != nil {
		t.Fatalf("purged meanwhile: %v", err)
	}
	for e, err := range disk.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatalf("left behind: %s", e.Key)
	}
}

// TestUpload_EveryDatabaseStepFailsCleanly pins that marking an image
// processing is part of the upload's transaction: whichever database step
// fails, the upload fails rather than recording an image the plugin never
// marked.
func TestUpload_EveryDatabaseStepFailsCleanly(t *testing.T) {
	upload := func(t *testing.T, faults *recordstest.Faults, n int, store *records.Store) error {
		faults.Arm(n)
		_, err := store.Upload(t.Context(), scope, "media", bytes.NewReader(smallPNG(t, 8, 8)), "r.png", imagePolicy)
		return err
	}
	p := ocr.NewPlugin(ocr.Tool{Tesseract: fakeTesseract(t, "echo receipt")}, ocr.WithDeferredProcessing())
	store, _, faults, _ := failingStore(t, p, nil)
	if err := upload(t, faults, 0, store); err != nil {
		t.Fatalf("unfaulted upload: %v", err)
	}
	steps := faults.Steps()
	for n := 1; n <= steps; n++ {
		store, _, faults, _ := failingStore(t, p, nil)
		if err := upload(t, faults, n, store); err == nil {
			t.Errorf("step %d of %d failing: the upload succeeded", n, steps)
		}
	}
}
