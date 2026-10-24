package pdf_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
)

// textDocument builds a letter-size PDF with one page per entry of pages,
// each showing its text in Helvetica ("" leaves the page blank, like a
// scan without a text layer), and info as the document information
// dictionary's entries ("/Title (Q3)"), none when empty.
func textDocument(info string, pages ...string) []byte {
	n := len(pages)
	kids := make([]string, n)
	for i := range n {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	for i, text := range pages {
		content := "0.9 g 36 36 540 720 re f"
		if text != "" {
			content = fmt.Sprintf("BT /F1 18 Tf 72 700 Td (%s) Tj ET", text)
		}
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", letterW, letterH, 5+2*i))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	trailerInfo := ""
	if info != "" {
		objs = append(objs, "<< "+info+" >>")
		trailerInfo = fmt.Sprintf(" /Info %d 0 R", len(objs))
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R%s >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, trailerInfo, xref)
	return b.Bytes()
}

const reportInfo = "/Title (Q3 Report) /Author (Jane Doe) /CreationDate (D:20260102030405Z)"

func TestProbe_ReadsDocumentInformation(t *testing.T) {
	requirePrograms(t)
	info, err := pdf.Tool{}.Probe(t.Context(), bytes.NewReader(textDocument(reportInfo, "Revenue grew")))
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err != nil || info.Title != "Q3 Report" || info.Author != "Jane Doe" || !info.Created.Equal(want) || info.Encrypted {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if info, err = (pdf.Tool{}).Probe(t.Context(), bytes.NewReader(textDocument("", "x"))); err != nil || info.Title != "" || !info.Created.IsZero() {
		t.Fatalf("without an information dictionary: %+v, %v", info, err)
	}
}

// TestProbe_DocumentFieldsAreCleaned pins how untrusted document fields
// are read: control characters gone, cut to MaxInfoChars, the first title
// wins over an imitation printed later, every date form pdfinfo prints.
func TestProbe_DocumentFieldsAreCleaned(t *testing.T) {
	long := strings.Repeat("é", pdf.MaxInfoChars+10)
	for name, tc := range map[string]struct {
		report string
		check  func(pdf.Info) bool
	}{
		"control characters": {"Pages: 1\nTitle: a\\tb\\rc\n", func(i pdf.Info) bool { return i.Title == "a b c" }},
		"long author": {"Pages: 1\nAuthor: " + long + "\n", func(i pdf.Info) bool {
			return utf8.RuneCountInString(i.Author) == pdf.MaxInfoChars && utf8.ValidString(i.Author)
		}},
		"first title wins": {"Title: real\nPages: 1\nTitle: fake\n", func(i pdf.Info) bool { return i.Title == "real" }},
		"zone with minutes": {"Pages: 1\nCreationDate: 2026-01-02T03:04:05+05:30\n", func(i pdf.Info) bool {
			return i.Created.Equal(time.Date(2026, 1, 1, 21, 34, 5, 0, time.UTC))
		}},
		"zone in hours": {"Pages: 1\nCreationDate: 2026-01-02T03:04:05-02\n", func(i pdf.Info) bool {
			return i.Created.Equal(time.Date(2026, 1, 2, 5, 4, 5, 0, time.UTC))
		}},
		"no zone":       {"Pages: 1\nCreationDate: 2026-01-02T03:04:05\n", func(i pdf.Info) bool { return i.Created.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) }},
		"bad date":      {"Pages: 1\nCreationDate: Tue Jan 2\n", func(i pdf.Info) bool { return i.Created.IsZero() }},
		"encrypted":     {"Pages: 1\nEncrypted: yes (print:no copy:no)\n", func(i pdf.Info) bool { return i.Encrypted }},
		"not encrypted": {"Pages: 1\nEncrypted: no\n", func(i pdf.Info) bool { return !i.Encrypted }},
	} {
		tool := pdf.Tool{PDFInfo: fakeProgram(t, "pdfinfo", "printf '"+strings.ReplaceAll(tc.report, "\n", `\n`)+"'")}
		info, err := tool.Probe(t.Context(), strings.NewReader("%PDF-1.4"))
		if err != nil || !tc.check(info) {
			t.Errorf("%s: %+v, %v", name, info, err)
		}
	}
}

func TestText(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	doc := textDocument("", "Revenue grew", "Costs fell")
	got, err := pdf.Tool{}.Text(t.Context(), bytes.NewReader(doc), nil)
	if err != nil || !strings.Contains(got.Text, "Revenue grew") || !strings.Contains(got.Text, "Costs fell") || got.Truncated || got.OCR {
		t.Fatalf("text = %+v, %v", got, err)
	}
	// MaxTextPages reads the first pages only, and says so.
	got, err = pdf.Tool{MaxTextPages: 1}.Text(t.Context(), bytes.NewReader(doc), nil)
	if err != nil || strings.Contains(got.Text, "Costs") || !got.Truncated {
		t.Fatalf("one page = %+v, %v", got, err)
	}
	// MaxText cuts the text, and says so.
	got, err = pdf.Tool{MaxText: 7}.Text(t.Context(), bytes.NewReader(doc), nil)
	if err != nil || got.Text != "Revenue" || !got.Truncated {
		t.Fatalf("cut = %+v, %v", got, err)
	}
	if _, err := (pdf.Tool{}).Text(t.Context(), strings.NewReader("not a pdf"), nil); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("not a pdf: %v", err)
	}
}

// TestText_EndlessOutputIsCut pins that a program printing without end
// is stopped at MaxText and its text kept as truncated, not an error.
func TestText_EndlessOutputIsCut(t *testing.T) {
	tool := pdf.Tool{
		PDFInfo:   fakeProgram(t, "pdfinfo", "echo 'Pages: 1'"),
		PDFToText: fakeProgram(t, "pdftotext", "yes 'é text'"),
		MaxText:   1001,
		Timeout:   10 * time.Second,
	}
	got, err := tool.Text(t.Context(), strings.NewReader("%PDF"), nil)
	if err != nil || !got.Truncated || len(got.Text) > 1001 || len(got.Text) < 990 || !utf8.ValidString(got.Text) {
		t.Fatalf("%d bytes, truncated %v, %v", len(got.Text), got.Truncated, err)
	}
	tool.PDFToText = fakeProgram(t, "pdftotext", "echo broken >&2; exit 1")
	if _, err := tool.Text(t.Context(), strings.NewReader("%PDF"), nil); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a failing pdftotext: %v", err)
	}
}

// pageReader is a Recognizer that names each page it is given, after
// checking it is a PNG, and counts its calls.
type pageReader struct{ calls int }

func (r *pageReader) Recognize(_ context.Context, img io.Reader) (string, error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(img, head); err != nil || !bytes.Equal(head, []byte("\x89PNG\r\n\x1a\n")) {
		return "", fmt.Errorf("not a PNG: %q %v", head, err)
	}
	r.calls++
	return fmt.Sprintf("scanned page %d", r.calls), nil
}

func TestText_OCRReadsAScan(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	scan := textDocument("", "", "", "")
	rec := &pageReader{}
	got, err := pdf.Tool{MaxOCRPages: 2, OCRSide: 200}.Text(t.Context(), bytes.NewReader(scan), rec)
	if err != nil || !got.OCR || !got.Truncated || rec.calls != 2 || got.Text != "scanned page 1\fscanned page 2\f" {
		t.Fatalf("scan = %+v (%d calls), %v", got, rec.calls, err)
	}
	// Without a recognizer, a scan has no text.
	if got, err = (pdf.Tool{}).Text(t.Context(), bytes.NewReader(scan), nil); err != nil || strings.TrimSpace(got.Text) != "" || got.OCR {
		t.Fatalf("scan without OCR = %+v, %v", got, err)
	}
	// A PDF with a text layer is never sent to the recognizer.
	rec = &pageReader{}
	if got, err = (pdf.Tool{}).Text(t.Context(), bytes.NewReader(textDocument("", "Revenue")), rec); err != nil || got.OCR || rec.calls != 0 {
		t.Fatalf("text layer = %+v (%d calls), %v", got, rec.calls, err)
	}
	// Recognized text is bounded by MaxText too.
	rec = &pageReader{}
	if got, err = (pdf.Tool{MaxText: 20, OCRSide: 200}).Text(t.Context(), bytes.NewReader(scan), rec); err != nil || len(got.Text) != 20 || !got.Truncated || rec.calls != 2 {
		t.Fatalf("cut OCR = %+v (%d calls), %v", got, rec.calls, err)
	}
	if _, err := (pdf.Tool{OCRSide: 200}).Text(t.Context(), bytes.NewReader(scan), failingReader{}); err == nil {
		t.Fatal("a failing recognizer was not reported")
	}
}

type failingReader struct{}

func (failingReader) Recognize(context.Context, io.Reader) (string, error) {
	return "", filesystem.Errorf(filesystem.CodeUnavailable, "ocr: down")
}

// requireProgram skips the test when program is not installed, naming it.
func requireProgram(t *testing.T, program string) {
	t.Helper()
	if _, err := exec.LookPath(program); err != nil {
		t.Skipf("%s is not installed (brew install poppler, apt install poppler-utils)", program)
	}
}

func TestPlugin_StoresInformationAndText(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithText()))
	ctx := t.Context()
	f, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(textDocument(reportInfo, "Revenue grew")), "report.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	m, ok := pdf.Of(f)
	if !ok || m.Title != "Q3 Report" || m.Author != "Jane Doe" || m.Created.Year() != 2026 || m.Encrypted {
		t.Fatalf("meta = %+v (%v)", m, f.Metadata)
	}
	if f.Metadata[records.MetaTitle] != "" {
		t.Fatal("the document's title was written over the application's display title")
	}
	key, ok := pdf.TextKey(f)
	if !ok || !strings.HasPrefix(key, f.Key+pdf.TextInfix) || f.Metadata[pdf.MetaTextCut] != "" || f.Metadata[pdf.MetaTextOCR] != "" {
		t.Fatalf("text key %q, metadata %v", key, f.Metadata)
	}
	if body, err := e.disk.ReadAll(ctx, key); err != nil || !strings.Contains(string(body), "Revenue grew") {
		t.Fatalf("text = %q, %v", body, err)
	}
	if st, err := e.disk.Stat(ctx, key); err != nil || st.ContentType != pdf.TextContentType {
		t.Fatalf("text object = %+v, %v", st, err)
	}
	next, err := e.store.Replace(ctx, scope, f.ID, bytes.NewReader(textDocument("", "Costs fell")), "", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	next = e.reread(t, next)
	nextKey, _ := pdf.TextKey(next)
	if m, _ := pdf.Of(next); m.Title != "" || nextKey == key || e.exists(t, key) || !e.exists(t, nextKey) {
		t.Fatalf("after replace: %+v, new %q, old exists %v", m, nextKey, e.exists(t, key))
	}
	if err := e.store.Purge(ctx, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if e.exists(t, nextKey) {
		t.Fatal("the text outlived its purged file")
	}

	// A scan read through WithOCR is marked as such, and a blank one
	// without OCR stores no text.
	ocr := newEnv(t, pdf.NewPlugin(pdf.Tool{MaxOCRPages: 1, OCRSide: 200}, pdf.WithOCR(&pageReader{})))
	g, err := ocr.store.Upload(ctx, scope, "media", bytes.NewReader(textDocument("", "", "")), "scan.pdf", docPolicy)
	if err == nil {
		g = ocr.reread(t, g)
	}
	if err != nil || g.Metadata[pdf.MetaTextOCR] != "true" || g.Metadata[pdf.MetaTextCut] != "true" {
		t.Fatalf("scan = %v, %v", g.Metadata, err)
	}
	g, err = e.store.Upload(ctx, scope, "media", bytes.NewReader(textDocument("", "")), "blank.pdf", docPolicy)
	if err == nil {
		g = e.reread(t, g)
	}
	if _, ok := pdf.TextKey(g); err != nil || ok {
		t.Fatalf("blank = %v, %v", g.Metadata, err)
	}
}

// testConverter stands in for contrib/office: it "converts" its content
// type by returning a fixed PDF, or fails as a converter does on a file
// it cannot read.
type testConverter struct {
	pdf    []byte
	reject bool
}

const convertedType = "application/x-test-document"

func (c testConverter) Converts(contentType string) bool { return contentType == convertedType }

func (c testConverter) ConvertToPDF(context.Context, io.Reader, string) (io.ReadCloser, error) {
	if c.reject {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "test: cannot convert")
	}
	return io.NopCloser(bytes.NewReader(c.pdf)), nil
}

func TestPlugin_DescribesConvertedFiles(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	conv := testConverter{pdf: textDocument(reportInfo, "Slide one", "Slide two")}
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{MaxSide: 100}, pdf.WithConverter(conv), pdf.WithPreview(), pdf.WithText()))
	in := records.NewFile{Scope: scope, Disk: "media", Key: "deck.x", Filename: "deck.x", ContentType: convertedType + "; v=1"}
	if _, err := e.disk.WriteBytes(t.Context(), in.Key, []byte("a deck"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	_, hasPreview := pdf.PreviewKey(f)
	_, hasText := pdf.TextKey(f)
	if m, ok := pdf.Of(f); !ok || m.Pages != 2 || m.Title != "Q3 Report" || !hasPreview || !hasText {
		t.Fatalf("converted = %+v (%v)", m, f.Metadata)
	}
	// A file the converter refuses is recorded, without metadata, and
	// marked failed: the conversion runs after the upload commits.
	r := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithConverter(testConverter{reject: true}), pdf.WithStrict()))
	if _, err := r.disk.WriteBytes(t.Context(), in.Key, []byte("a deck"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	g, err := r.store.CreateFile(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	g = r.reread(t, g)
	if g.Metadata != nil || g.Status != records.StatusFailed || g.StatusDetail != pdf.StatusFailedPrefix+string(filesystem.CodeRejected) {
		t.Fatalf("refused conversion: %v %q %q", g.Metadata, g.Status, g.StatusDetail)
	}
}

// hugePage is a Recognizer returning the same very long text for every
// page.
type hugePage struct{ text string }

func (h hugePage) Recognize(context.Context, io.Reader) (string, error) { return h.text, nil }

// TestText_OCRNeverCopiesPastMaxText pins that recognized text is cut
// while it is appended: a recognizer returning 64 MiB per page costs a
// few MiB of allocations at most, not a copy of each page.
func TestText_OCRNeverCopiesPastMaxText(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	rec := hugePage{text: strings.Repeat("a", 64<<20)}
	scan := textDocument("", "", "")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := pdf.Tool{MaxText: 1000, OCRSide: 100}.Text(t.Context(), bytes.NewReader(scan), rec)
	runtime.ReadMemStats(&after)
	if err != nil || len(got.Text) != 1000 || !got.Truncated {
		t.Fatalf("text = %d bytes, %v", len(got.Text), err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("%d MiB allocated reading a capped scan", alloc>>20)
	}
}

// probeReader is a Recognizer that, while it reads, looks at the store
// from outside any transaction: the uploaded file must already be
// committed, marked processing, and another tenant's upload must go
// through. Inside the upload's transaction the file would be invisible
// (or, on SQLite's one connection, the reads would wait until timeout).
type probeReader struct {
	e      *env
	seen   []records.File
	errs   []error
	called bool
}

func (r *probeReader) Recognize(context.Context, io.Reader) (string, error) {
	r.called = true
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	page, err := r.e.store.Files(ctx, records.FileQuery{Scope: scope})
	r.seen = page.Files
	r.errs = append(r.errs, err)
	_, err = r.e.store.Upload(ctx, "another-tenant", "media", strings.NewReader("notes"), "n.txt", docPolicy)
	r.errs = append(r.errs, err)
	return "scanned page", nil
}

func TestPlugin_UploadNeverWaitsOnTheRecognizer(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	probe := &probeReader{}
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{MaxOCRPages: 1, OCRSide: 100}, pdf.WithOCR(probe)))
	probe.e = e
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "")), "scan.pdf", docPolicy)
	if err != nil || !probe.called {
		t.Fatalf("upload: %v, recognizer called %v", err, probe.called)
	}
	if err := errors.Join(probe.errs...); err != nil {
		t.Fatalf("the store was busy while the recognizer ran: %v", err)
	}
	if len(probe.seen) != 1 || probe.seen[0].ID != f.ID || probe.seen[0].Status != records.StatusProcessing || probe.seen[0].StatusDetail != pdf.StatusDetail {
		t.Fatalf("while recognizing, the file was %+v", probe.seen)
	}
	if f = e.reread(t, f); f.Status != records.StatusReady || f.Metadata[pdf.MetaTextOCR] != "true" {
		t.Fatalf("after: %q %v", f.Status, f.Metadata)
	}
}

// TestProcess_FailureMarksTheFile pins that a failing recognizer leaves
// the upload recorded and marks the file failed with the reason, and
// that Process run again (a retry job) succeeds once the cause is gone.
func TestProcess_FailureMarksTheFile(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	rec := &switchReader{}
	p := pdf.NewPlugin(pdf.Tool{MaxOCRPages: 1, OCRSide: 100}, pdf.WithOCR(rec))
	e := newEnv(t, p)
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "")), "scan.pdf", docPolicy)
	if err != nil {
		t.Fatalf("a failing recognizer failed the upload: %v", err)
	}
	if f = e.reread(t, f); f.Status != records.StatusFailed || f.StatusDetail != pdf.StatusFailedPrefix+string(filesystem.CodeUnavailable) {
		t.Fatalf("status = %q %q", f.Status, f.StatusDetail)
	}
	rec.ok = true
	if err := p.Process(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if f = e.reread(t, f); f.Metadata[pdf.MetaTextOCR] != "true" || f.Status != records.StatusReady {
		t.Fatalf("after the retry: %v", f.Metadata)
	}
	// Running it again replaces the text rather than orphaning it.
	first, _ := pdf.TextKey(f)
	if err := p.Process(t.Context(), e.store, scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if second, _ := pdf.TextKey(e.reread(t, f)); second == first || e.exists(t, first) || !e.exists(t, second) {
		t.Fatalf("rerun: first %q exists %v, second %q", first, e.exists(t, first), second)
	}
}

// switchReader fails until ok is set.
type switchReader struct{ ok bool }

func (r *switchReader) Recognize(context.Context, io.Reader) (string, error) {
	if !r.ok {
		return "", filesystem.Errorf(filesystem.CodeUnavailable, "ocr: down")
	}
	return "scanned page", nil
}

// replacingReader replaces the file while it reads the first time, as a
// concurrent request would, and names each run's text.
type replacingReader struct {
	e     *env
	calls int
}

func (r *replacingReader) Recognize(context.Context, io.Reader) (string, error) {
	r.calls++
	run := r.calls
	if run == 1 {
		ctx := context.Background()
		page, err := r.e.store.Files(ctx, records.FileQuery{Scope: scope})
		if err != nil || len(page.Files) != 1 {
			return "", fmt.Errorf("files: %v %v", page.Files, err)
		}
		if _, err := r.e.store.Replace(ctx, scope, page.Files[0].ID, bytes.NewReader(textDocument("", "")), "", docPolicy); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("run %d", run), nil
}

// TestProcess_ContentReplacedMeanwhile pins that a run whose content was
// replaced while it worked records nothing and leaves no object behind:
// the replacement's own run wins.
func TestProcess_ContentReplacedMeanwhile(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	rec := &replacingReader{}
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{MaxOCRPages: 1, OCRSide: 100}, pdf.WithOCR(rec)))
	rec.e = e
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "")), "scan.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	key, ok := pdf.TextKey(f)
	if !ok || rec.calls != 2 {
		t.Fatalf("metadata %v after %d runs", f.Metadata, rec.calls)
	}
	if text, err := e.disk.ReadAll(t.Context(), key); err != nil || string(text) != "run 2\f" {
		t.Fatalf("text = %q, %v", text, err)
	}
	var keys []string
	for entry, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, entry.Key)
	}
	if len(keys) != 2 {
		t.Fatalf("objects = %v, want the content and its one text", keys)
	}
}

// TestResume_FinishesWhatACrashLeft simulates a crash after the commit:
// the row is processing and nothing in memory will run it. A store
// without the plugin installed stands in for the restarted process.
func TestResume_FinishesWhatACrashLeft(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	ctx := t.Context()
	disk := newDisk(t)
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	store, err := records.New(db, recordstest.Dialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(ctx, "r.pdf", textDocument("", "Revenue grew"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := store.CreateFile(ctx, records.NewFile{
		Scope: scope, Disk: "media", Key: "r.pdf", Filename: "r.pdf", ContentType: pdf.ContentType,
		Metadata: map[string]string{pdf.MetaPages: "1"}, Status: records.StatusProcessing, StatusDetail: pdf.StatusDetail,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithText())
	// Not yet older than the cutoff: left alone.
	if n, err := p.Resume(ctx, store, f.CreatedAt.Add(-time.Minute)); err != nil || n != 0 {
		t.Fatalf("too recent: %d, %v", n, err)
	}
	if got, _ := store.File(ctx, scope, f.ID); got.Status != records.StatusProcessing {
		t.Fatalf("status = %q", got.Status)
	}
	if n, err := p.Resume(ctx, store, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("resume: %d, %v", n, err)
	}
	got, err := store.File(ctx, scope, f.ID)
	if _, ok := pdf.TextKey(got); err != nil || !ok || got.Status != records.StatusReady {
		t.Fatalf("after resume: %q %v, %v", got.Status, got.Metadata, err)
	}
	if n, err := p.Resume(ctx, store, time.Now().Add(time.Minute)); err != nil || n != 0 {
		t.Fatalf("second resume: %d, %v", n, err)
	}
}

// TestDeferredProcessing_UploadReturnsFirst pins that with
// WithDeferredProcessing an upload returns before the recognizer runs,
// and Resume does the work.
func TestDeferredProcessing_UploadReturnsFirst(t *testing.T) {
	requirePrograms(t)
	requireProgram(t, pdf.DefaultPDFToText)
	rec := &pageReader{}
	p := pdf.NewPlugin(pdf.Tool{MaxOCRPages: 1, OCRSide: 100}, pdf.WithOCR(rec), pdf.WithDeferredProcessing())
	e := newEnv(t, p)
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(textDocument("", "")), "scan.pdf", docPolicy)
	if err != nil || rec.calls != 0 {
		t.Fatalf("upload: %v, recognizer ran %d times before it returned", err, rec.calls)
	}
	if f = e.reread(t, f); f.Status != records.StatusProcessing {
		t.Fatalf("status = %q", f.Status)
	}
	if n, err := p.Resume(t.Context(), e.store, time.Now().Add(time.Minute)); err != nil || n != 1 || rec.calls != 1 {
		t.Fatalf("resume: %d, %v, %d calls", n, err, rec.calls)
	}
	if f = e.reread(t, f); f.Status != records.StatusReady || f.Metadata[pdf.MetaTextOCR] != "true" {
		t.Fatalf("after resume: %q %v", f.Status, f.Metadata)
	}
}

// stuckConverter blocks until its context ends, standing in for a
// conversion that hangs.
type stuckConverter struct{}

func (stuckConverter) Converts(contentType string) bool { return contentType == convertedType }

func (stuckConverter) ConvertToPDF(ctx context.Context, _ io.Reader, _ string) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, filesystem.NewError(filesystem.CodeUnavailable, ctx.Err())
}

// TestProcess_TimeoutStillMarksFailed pins WithProcessTimeout and the
// status written after it: a run cut short by the bound returns its error
// and marks the file failed, rather than leaving it processing because
// the status was written on the expired context.
func TestProcess_TimeoutStillMarksFailed(t *testing.T) {
	const bound = 200 * time.Millisecond
	p := pdf.NewPlugin(pdf.Tool{}, pdf.WithConverter(stuckConverter{}), pdf.WithProcessTimeout(bound), pdf.WithDeferredProcessing())
	e := newEnv(t, p)
	in := records.NewFile{Scope: scope, Disk: "media", Key: "deck.x", Filename: "deck.x", ContentType: convertedType}
	if _, err := e.disk.WriteBytes(t.Context(), in.Key, []byte("a deck"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Process(t.Context(), e.store, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("a stuck conversion: %v, want CodeUnavailable", err)
	}
	if f = e.reread(t, f); f.Status != records.StatusFailed || f.StatusDetail != pdf.StatusFailedPrefix+string(filesystem.CodeUnavailable) {
		t.Fatalf("status = %q %q, want failed", f.Status, f.StatusDetail)
	}
}
