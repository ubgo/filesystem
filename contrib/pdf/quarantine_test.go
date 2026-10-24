package pdf_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/records"
)

// TestPlugin_QuarantinedPDF: a quarantined PDF is recorded without running
// a program on its unscanned bytes (it succeeds even with no pdfinfo at
// all), Process refuses it, and the scan passing describes it and makes
// its preview; a replacement quarantined again keeps nothing of the old
// content and is processed on its own release.
func TestPlugin_QuarantinedPDF(t *testing.T) {
	requirePrograms(t)
	ctx := t.Context()
	missing := newEnv(t, pdf.NewPlugin(pdf.Tool{PDFInfo: "/nonexistent/pdfinfo"}, pdf.WithPreview()))
	if _, err := missing.store.Upload(ctx, scope, "media", bytes.NewReader(document(2, a4W, a4H)), "q.pdf", docPolicy, records.Quarantine()); err != nil {
		t.Fatalf("a quarantined upload ran a program: %v", err)
	}

	p := pdf.NewPlugin(pdf.Tool{MaxSide: 300}, pdf.WithPreview())
	e := newEnv(t, p)
	f, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(document(2, a4W, a4H)), "q.pdf", docPolicy, records.Quarantine())
	if err != nil {
		t.Fatal(err)
	}
	if f = e.reread(t, f); !f.Quarantined() {
		t.Fatalf("status = %q", f.Status)
	}
	if _, ok := pdf.Of(f); ok {
		t.Fatalf("a quarantined PDF was described: %v", f.Metadata)
	}
	if _, ok := pdf.PreviewKey(f); ok {
		t.Fatal("a quarantined PDF got a preview")
	}
	if err := p.Process(ctx, e.store, scope, f.ID); !errors.Is(err, records.ErrQuarantined) {
		t.Fatalf("Process of a quarantined PDF = %v", err)
	}
	if _, err := e.store.PassScan(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	preview, ok := pdf.PreviewKey(f)
	if m, _ := pdf.Of(f); f.Status != records.StatusReady || m.Pages != 2 || !ok || !e.exists(t, preview) {
		t.Fatalf("after release: %q %v", f.Status, f.Metadata)
	}

	next, err := e.store.Replace(ctx, scope, f.ID, bytes.NewReader(document(1, letterW, letterH)), "", docPolicy, records.QuarantineContent())
	if err != nil {
		t.Fatal(err)
	}
	next = e.reread(t, next)
	if _, ok := pdf.Of(next); ok || !next.Quarantined() {
		t.Fatalf("quarantined replacement: %q %v", next.Status, next.Metadata)
	}
	if _, err := e.store.PassScan(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := pdf.Of(e.reread(t, f)); m.Pages != 1 {
		t.Fatalf("after the second release: %+v", m)
	}
}

// TestPlugin_QuarantinedConvertedFile: a quarantined file of a type the
// converter takes is recorded without being converted, and converted
// (with its information, preview and text) when its scan passes.
func TestPlugin_QuarantinedConvertedFile(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	ctx := t.Context()
	conv := testConverter{pdf: textDocument(reportInfo, "Slide one", "Slide two")}
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{MaxSide: 100}, pdf.WithConverter(conv), pdf.WithPreview(), pdf.WithText()))
	in := records.NewFile{Scope: scope, Disk: "media", Key: "deck.x", Filename: "deck.x", ContentType: convertedType, Status: records.StatusQuarantined}
	if _, err := e.disk.WriteBytes(ctx, in.Key, []byte("a deck"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ctx, in)
	if err != nil {
		t.Fatalf("a quarantined file the converter takes = %v", err)
	}
	if f = e.reread(t, f); !f.Quarantined() {
		t.Fatalf("status = %q", f.Status)
	}
	if _, ok := pdf.Of(f); ok {
		t.Fatalf("a quarantined file was converted: %v", f.Metadata)
	}
	if _, err := e.store.PassScan(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	_, hasText := pdf.TextKey(f)
	if m, ok := pdf.Of(f); !ok || m.Pages != 2 || !hasText || f.Status != records.StatusReady {
		t.Fatalf("after release: %+v %q (%v)", m, f.Status, f.Metadata)
	}
}
