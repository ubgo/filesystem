package pdf_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// fakeProgram writes an executable shell script standing in for a Poppler
// program, so a test controls exactly what it prints and how it exits.
func fakeProgram(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbe_ParsesWhatPdfinfoPrints(t *testing.T) {
	for name, tc := range map[string]struct {
		report string
		want   pdf.Info
		code   filesystem.Code
	}{
		"no page count":      {"Title: x\nnot a field\n", pdf.Info{}, filesystem.CodeRejected},
		"zero pages":         {"Pages: 0\n", pdf.Info{}, filesystem.CodeRejected},
		"size not WxH":       {"Pages: 2\nPage size: letter\n", pdf.Info{Pages: 2}, ""},
		"size not numbers":   {"Pages: 2\nPage size: a x b pts\n", pdf.Info{Pages: 2}, ""},
		"size without x":     {"Pages: 2\nPage size: 1 by 2 pts\n", pdf.Info{Pages: 2}, ""},
		"first size wins":    {"Pages: 2\nPage    1 size: 10 x 20 pts\nPage    2 size: 30 x 40 pts\n", pdf.Info{Pages: 2, Width: 10, Height: 20}, ""},
		"line without colon": {"Pages: 1\nweird line\nPage size: 1 x 2 pts\n", pdf.Info{Pages: 1, Width: 1, Height: 2}, ""},
	} {
		tool := pdf.Tool{PDFInfo: fakeProgram(t, "pdfinfo", "printf '"+strings.ReplaceAll(tc.report, "\n", `\n`)+"'")}
		info, err := tool.Probe(t.Context(), strings.NewReader("%PDF-1.4"))
		if filesystem.CodeOf(err) != tc.code || info != tc.want {
			t.Errorf("%s: %+v, %v; want %+v, %q", name, info, err, tc.want, tc.code)
		}
	}
}

func TestPreview_EmptyOutputIsRejected(t *testing.T) {
	tool := pdf.Tool{PDFToPPM: fakeProgram(t, "pdftoppm", "exit 0")}
	if _, err := tool.Preview(t.Context(), strings.NewReader("%PDF-1.4")); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("err = %v", err)
	}
}

// TestRun_KeepsOnlyTheStartOfALongErrorOutput pins that a program
// printing megabytes of errors cannot grow ProgramError.Detail past a few
// KiB.
func TestRun_KeepsOnlyTheStartOfALongErrorOutput(t *testing.T) {
	tool := pdf.Tool{PDFInfo: fakeProgram(t, "pdfinfo", `i=0; while [ $i -lt 2000 ]; do echo "Syntax Error: line $i of noise" >&2; i=$((i+1)); done; exit 1`)}
	_, err := tool.Probe(t.Context(), strings.NewReader("x"))
	var pe *pdf.ProgramError
	if !errors.As(err, &pe) || len(pe.Detail) == 0 || len(pe.Detail) > 4<<10 {
		t.Fatalf("detail is %d bytes, err %v", len(pe.Detail), err)
	}
	if pe.Program != "pdfinfo" || strings.Contains(fshttp.ClientMessage(err), string(filepath.Separator)) {
		t.Fatalf("the client message names the configured path: %q", fshttp.ClientMessage(err))
	}
}

func TestTool_WorkFileFailures(t *testing.T) {
	if _, err := (pdf.Tool{TempDir: filepath.Join(t.TempDir(), "missing")}).Probe(t.Context(), strings.NewReader("x")); err == nil {
		t.Fatal("a missing TempDir was not reported")
	}
	errRead := errors.New("client went away")
	if _, err := (pdf.Tool{}).Probe(t.Context(), iotest.ErrReader(errRead)); !errors.Is(err, errRead) {
		t.Fatalf("a failing reader: %v", err)
	}
}

func TestInspect_Failures(t *testing.T) {
	requirePrograms(t)
	d := newDisk(t)
	inspect := pdf.Tool{}.Inspect(pdf.Limits{})
	if err := inspect(t.Context(), d, upload.Result{Key: "missing.pdf", ContentType: pdf.ContentType}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("missing object: %v", err)
	}
	if _, err := d.WriteBytes(t.Context(), "bad.pdf", []byte("not a pdf"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := inspect(t.Context(), d, upload.Result{Key: "bad.pdf", ContentType: pdf.ContentType + "; charset=binary"}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("unreadable object: %v", err)
	}
}

func TestPlugin_Failures(t *testing.T) {
	requirePrograms(t)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}))
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview())
	ctx := t.Context()
	if err := p.BeforeCreate(ctx, e.store, &records.File{Disk: "nope", Key: "a.pdf", ContentType: pdf.ContentType}); filesystem.CodeOf(err) == "" {
		t.Fatal("an unknown disk was not reported")
	}
	if err := p.BeforeCreate(ctx, e.store, &records.File{Disk: "media", Key: "missing.pdf", ContentType: pdf.ContentType}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a missing object: %v", err)
	}
	slow := pdf.NewPlugin(pdf.Tool{Timeout: 1})
	if _, err := e.disk.WriteBytes(ctx, "r.pdf", document(1, a4W, a4H), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := slow.BeforeCreate(ctx, e.store, &records.File{Disk: "media", Key: "r.pdf", ContentType: pdf.ContentType}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("a timed-out run must fail the upload, not record it without metadata: %v", err)
	}

	// Without WithPreview, only the metadata.
	f, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(document(2, a4W, a4H)), "r.pdf", docPolicy)
	if _, ok := pdf.PreviewKey(f); err != nil || ok {
		t.Fatalf("a preview without WithPreview: %v %v", f.Metadata, err)
	}
	if m, ok := pdf.Of(f); !ok || m.Pages != 2 {
		t.Fatalf("meta = %+v", m)
	}

	// A strict plugin refuses a replacement it cannot read, and the file
	// keeps its content.
	strict := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithStrict()))
	g, err := strict.store.Upload(ctx, scope, "media", bytes.NewReader(document(1, a4W, a4H)), "r.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.store.Replace(ctx, scope, g.ID, bytes.NewReader(document(1, a4W, a4H)[:200]), "", docPolicy); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("strict replace: %v", err)
	}
}

// TestPlugin_PreviewWriteFails covers a disk that refuses the preview: the
// file stays recorded, marked failed with the reason.
func TestPlugin_PreviewWriteFails(t *testing.T) {
	requirePrograms(t)
	drv := memory.New()
	writable, err := filesystem.NewDisk("media", drv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writable.WriteBytes(t.Context(), "r.pdf", document(1, a4W, a4H), filesystem.WriteOptions{}); err != nil {
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
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview())
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(p))
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
	if _, ok := pdf.PreviewKey(f); ok {
		t.Fatal("a preview key was recorded")
	}
}

// TestRun_PassesOnlyAnAllowlistedEnvironment pins that a program sees
// PATH and the variables of Tool.Env, never the application's secrets.
func TestRun_PassesOnlyAnAllowlistedEnvironment(t *testing.T) {
	t.Setenv("FS_TEST_SECRET", "planted-secret")
	tool := pdf.Tool{
		PDFInfo:   fakeProgram(t, "pdfinfo", "echo 'Pages: 1'"),
		PDFToText: fakeProgram(t, "pdftotext", "env"),
		Env:       []string{"FS_TEST_EXTRA=passed"},
	}
	got, err := tool.Text(t.Context(), strings.NewReader("%PDF"), nil)
	if err != nil || strings.Contains(got.Text, "planted-secret") || !strings.Contains(got.Text, "PATH=") || !strings.Contains(got.Text, "FS_TEST_EXTRA=passed") {
		t.Fatalf("environment:\n%s\n%v", got.Text, err)
	}
}
