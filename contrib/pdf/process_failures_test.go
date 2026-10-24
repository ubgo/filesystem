package pdf_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// bareEnv is a store without the plugin installed, on a database whose
// steps can be made to fail, standing in for the process that runs
// Process and Resume from a job. store knows the disks media and other;
// runner, on the same database, knows only media, as a job deployed
// without one of the application's disks would.
type bareEnv struct {
	disk, other   *filesystem.Disk
	store, runner *records.Store
	faults        *recordstest.Faults
}

func newBareEnv(t *testing.T) *bareEnv {
	t.Helper()
	disk := newDisk(t)
	other, err := filesystem.NewDisk("other", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	both, err := filesystem.NewManager(disk, other)
	if err != nil {
		t.Fatal(err)
	}
	mediaOnly, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, faults := recordstest.OpenFaulty(t)
	store, err := records.New(db, recordstest.Dialect(), both)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner, err := records.New(db, recordstest.Dialect(), mediaOnly)
	if err != nil {
		t.Fatal(err)
	}
	return &bareEnv{disk: disk, other: other, store: store, runner: runner, faults: faults}
}

// pending stores body at key on the named disk and records it as a
// described PDF marked processing by the plugin, as an upload would leave
// it.
func (e *bareEnv) pending(t *testing.T, diskName, key string, body []byte) records.File {
	t.Helper()
	d := e.disk
	if diskName == e.other.Name() {
		d = e.other
	}
	if _, err := d.WriteBytes(t.Context(), key, body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{
		Scope: scope, Disk: diskName, Key: key, Filename: key, ContentType: pdf.ContentType,
		Metadata: map[string]string{pdf.MetaPages: "1"}, Status: records.StatusProcessing, StatusDetail: pdf.StatusDetail,
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// keys lists every object on the disk.
func (e *bareEnv) keys(t *testing.T) []string {
	t.Helper()
	var out []string
	for entry, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry.Key)
	}
	return out
}

// TestProcess_FaultSweep fails each database step of a Process run in
// turn: every run must return the injected fault, leave the file
// processing, and delete the text it made, so a retry starts clean. The
// run after the last step succeeds and records the text.
func TestProcess_FaultSweep(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	e := newBareEnv(t)
	f := e.pending(t, "media", "r.pdf", textDocument("", "Revenue grew"))
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithText())
	ctx := t.Context()
	for n := 1; ; n++ {
		if n > 50 {
			t.Fatal("Process still failing after 50 steps")
		}
		e.faults.Arm(n)
		err := p.Process(ctx, e.store, scope, f.ID)
		e.faults.Arm(0)
		if err == nil {
			break
		}
		if !errors.Is(err, recordstest.ErrInjected) {
			t.Fatalf("step %d: %v, want the injected fault", n, err)
		}
		got, gerr := e.store.File(ctx, scope, f.ID)
		if gerr != nil || got.Status != records.StatusProcessing {
			t.Fatalf("step %d: file left %q, %v", n, got.Status, gerr)
		}
		if keys := e.keys(t); !slices.Equal(keys, []string{"r.pdf"}) {
			t.Fatalf("step %d: objects %v, want only the source", n, keys)
		}
	}
	got, err := e.store.File(ctx, scope, f.ID)
	if _, ok := pdf.TextKey(got); err != nil || !ok || got.Status != records.StatusReady {
		t.Fatalf("after the sweep: %q %v, %v", got.Status, got.Metadata, err)
	}
}

// TestOnEvent_StatusFailurePassesThrough pins that a failure to mark the
// file processing is returned, so the change that recorded it rolls back
// instead of committing a file nobody will process.
func TestOnEvent_StatusFailurePassesThrough(t *testing.T) {
	e := newBareEnv(t)
	f := e.pending(t, "media", "r.pdf", []byte("x"))
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithText())
	e.faults.Arm(1)
	err := p.OnEvent(t.Context(), e.store, records.Event{Kind: records.EventFileCreated, Scope: scope, FileID: f.ID, File: &f})
	e.faults.Arm(0)
	if !errors.Is(err, recordstest.ErrInjected) {
		t.Fatalf("OnEvent = %v, want the injected fault", err)
	}
}

// TestResume_Failures pins that Resume runs every pending file even when
// some fail (a disk the job does not have, a missing object, bytes no PDF reader
// takes), returns how many it ran with their failures joined, and marks
// each failed file; and that a failed query is returned with nothing run.
func TestResume_Failures(t *testing.T) {
	requirePrograms(t)
	e := newBareEnv(t)
	ctx := t.Context()
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithText())
	files := []records.File{
		e.pending(t, "other", "a.pdf", []byte("x")),
		e.pending(t, "media", "missing.pdf", []byte("x")),
		e.pending(t, "media", "garbage.pdf", []byte("not a pdf at all")),
	}
	if err := e.disk.Delete(ctx, "missing.pdf"); err != nil {
		t.Fatal(err)
	}
	e.faults.Arm(1)
	n, err := p.Resume(ctx, e.runner, time.Now().Add(time.Minute))
	e.faults.Arm(0)
	if n != 0 || !errors.Is(err, recordstest.ErrInjected) {
		t.Fatalf("Resume with a failing query = %d, %v", n, err)
	}
	n, err = p.Resume(ctx, e.runner, time.Now().Add(time.Minute))
	if n != len(files) || err == nil {
		t.Fatalf("Resume = %d, %v; want %d runs and their failures", n, err, len(files))
	}
	if filesystem.CodeOf(err) == "" {
		t.Errorf("joined failures carry no code: %v", err)
	}
	for _, f := range files {
		got, gerr := e.store.File(ctx, scope, f.ID)
		if gerr != nil || got.Status != records.StatusFailed || len(got.StatusDetail) <= len(pdf.StatusFailedPrefix) {
			t.Errorf("%s: %q %q, %v", f.Key, got.Status, got.StatusDetail, gerr)
		}
	}
}

// TestProcess_NothingToDo pins that Process on a file it has no work for
// (no preview or text wanted, or a trashed file) makes nothing and leaves
// a status it did not set alone.
func TestProcess_NothingToDo(t *testing.T) {
	requirePrograms(t)
	e := newBareEnv(t)
	ctx := t.Context()
	f := e.pending(t, "media", "r.pdf", textDocument("", "Revenue grew"))
	if _, err := e.store.SetStatus(ctx, scope, f.ID, records.StatusReady, ""); err != nil {
		t.Fatal(err)
	}
	if err := pdf.NewPlugin(pdf.Tool{}).Process(ctx, e.store, scope, f.ID); err != nil {
		t.Fatalf("Process with no work: %v", err)
	}
	got, err := e.store.File(ctx, scope, f.ID)
	if err != nil || got.Status != records.StatusReady {
		t.Fatalf("status = %q, %v", got.Status, err)
	}
	if err := e.store.Trash(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := pdf.NewPlugin(pdf.Tool{}, pdf.WithText()).Process(ctx, e.store, scope, f.ID); err != nil {
		t.Fatalf("Process of a trashed file: %v", err)
	}
	if keys := e.keys(t); !slices.Equal(keys, []string{"r.pdf"}) {
		t.Errorf("objects %v, want no text for a trashed file", keys)
	}
	if err := pdf.NewPlugin(pdf.Tool{}).Process(ctx, e.store, scope, "no-such-file"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("Process of an unknown file = %v, want CodeNotFound", err)
	}
}

// failingProgram is a Poppler stand-in that always fails.
func failingProgram(t *testing.T, name string) string {
	t.Helper()
	return fakeProgram(t, name, "echo 'renderer crashed' >&2\nexit 3")
}

// TestProcess_RenderFailures pins that a renderer failing for the preview,
// or for OCR (at the default OCR size), marks the upload failed with no
// preview or text recorded and no object left behind, and that the
// recognizer never runs on a page that was not rendered.
func TestProcess_RenderFailures(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	rec := &pageReader{}
	for name, p := range map[string]*pdf.Plugin{
		"preview": pdf.NewPlugin(pdf.Tool{PDFToPPM: failingProgram(t, "pdftoppm")}, pdf.WithPreview()),
		"ocr":     pdf.NewPlugin(pdf.Tool{PDFToPPM: failingProgram(t, "pdftoppm"), MaxOCRPages: 1}, pdf.WithOCR(rec)),
	} {
		e := newEnv(t, p)
		f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "")), "scan.pdf", docPolicy)
		if err != nil {
			t.Fatalf("%s: a failing renderer failed the upload: %v", name, err)
		}
		f = e.reread(t, f)
		_, hasPreview := pdf.PreviewKey(f)
		_, hasText := pdf.TextKey(f)
		if f.Status != records.StatusFailed || hasPreview || hasText {
			t.Errorf("%s: %q %q %v", name, f.Status, f.StatusDetail, f.Metadata)
		}
	}
	if rec.calls != 0 {
		t.Errorf("the recognizer ran %d times without a rendered page", rec.calls)
	}
}

// TestProcess_TextWriteFails covers a disk that refuses the text: the
// file stays recorded, marked failed with the reason, and no text key is
// recorded.
func TestProcess_TextWriteFails(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	drv := memory.New()
	writable, err := filesystem.NewDisk("media", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writable.WriteBytes(t.Context(), "r.pdf", textDocument("", "Revenue grew"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	readOnly, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(readOnly)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(pdf.NewPlugin(pdf.Tool{}, pdf.WithText())))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f, err := store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "r.pdf", Filename: "r.pdf", ContentType: pdf.ContentType})
	if err != nil {
		t.Fatal(err)
	}
	if f, err = store.File(t.Context(), scope, f.ID); err != nil || f.Status != records.StatusFailed || f.StatusDetail != pdf.StatusFailedPrefix+string(filesystem.CodeReadOnly) {
		t.Fatalf("file = %q %q, %v", f.Status, f.StatusDetail, err)
	}
	if _, ok := pdf.TextKey(f); ok {
		t.Fatal("a text key was recorded")
	}
}

// purgingConverter purges the file being converted, as a concurrent
// request would, then converts it.
type purgingConverter struct {
	store *records.Store
	pdf   []byte
}

func (c *purgingConverter) Converts(contentType string) bool { return contentType == convertedType }

func (c *purgingConverter) ConvertToPDF(context.Context, io.Reader, string) (io.ReadCloser, error) {
	ctx := context.Background()
	page, err := c.store.Files(ctx, records.FileQuery{Scope: scope})
	if err != nil || len(page.Files) != 1 {
		return nil, errors.Join(err, errors.New("want exactly one file"))
	}
	if err := c.store.Purge(ctx, scope, page.Files[0].ID); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(c.pdf)), nil
}

// TestProcess_FilePurgedMeanwhile pins that a run whose file was purged
// while it worked records nothing, returns no error, and deletes the text
// it made, so nothing is left on the disk.
func TestProcess_FilePurgedMeanwhile(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	conv := &purgingConverter{pdf: textDocument("", "Slide one")}
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithConverter(conv), pdf.WithText()))
	conv.store = e.store
	in := records.NewFile{Scope: scope, Disk: "media", Key: "deck.x", Filename: "deck.x", ContentType: convertedType}
	if _, err := e.disk.WriteBytes(t.Context(), in.Key, []byte("a deck"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.File(t.Context(), scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("file after the purge: %v", err)
	}
	for entry, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Errorf("object %s left after the purge", entry.Key)
	}
}
