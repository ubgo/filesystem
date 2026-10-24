package pdf_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// requirePrograms skips the test when Poppler is not installed: the only
// reason a test of this package may skip.
func requirePrograms(t *testing.T) {
	t.Helper()
	for _, p := range []string{pdf.DefaultPDFInfo, pdf.DefaultPDFToPPM} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed (brew install poppler, apt install poppler-utils)", p)
		}
	}
}

// document builds a PDF of n pages of w×h points, each with a filled
// rectangle, with a correct cross-reference table.
func document(n int, w, h float64) []byte {
	var objs []string
	kids := make([]string, n)
	for i := range n {
		kids[i] = fmt.Sprintf("%d 0 R", 3+2*i)
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	content := fmt.Sprintf("0.2 0.4 0.8 rg 36 36 %g %g re f", w-72, h-72)
	for i := range n {
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %g %g] /Contents %d 0 R >>", w, h, 4+2*i))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
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
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// Page sizes of the fixtures, in points.
const (
	letterW, letterH = 612, 792
	a4W, a4H         = 595, 842
)

func TestProbe(t *testing.T) {
	requirePrograms(t)
	info, err := pdf.Tool{}.Probe(t.Context(), bytes.NewReader(document(3, letterW, letterH)))
	if err != nil || info.Pages != 3 || info.Width != letterW || info.Height != letterH {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestProbe_Refuses(t *testing.T) {
	requirePrograms(t)
	cases := map[string][]byte{
		"text":      []byte("quarterly numbers\n"),
		"truncated": document(2, letterW, letterH)[:200],
	}
	if _, err := exec.LookPath("qpdf"); err == nil {
		dir := t.TempDir()
		in, out := filepath.Join(dir, "in.pdf"), filepath.Join(dir, "locked.pdf")
		if err := os.WriteFile(in, document(1, letterW, letterH), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, err := exec.CommandContext(t.Context(), "qpdf", "--encrypt", "user-secret", "owner-secret", "256", "--", in, out).CombinedOutput(); err != nil {
			t.Fatalf("qpdf: %v %s", err, b)
		}
		locked, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		cases["locked with a user password"] = locked
	} else {
		t.Log("qpdf is not installed: the locked-PDF case is not run")
	}
	for name, body := range cases {
		_, err := pdf.Tool{}.Probe(t.Context(), bytes.NewReader(body))
		if filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s: err = %v, want rejected", name, err)
		}
		var pe *pdf.ProgramError
		if msg := fshttp.ClientMessage(err); errors.As(err, &pe) && (msg != pe.Error() || pe.Detail == "" || strings.Contains(msg, pe.Detail)) {
			t.Errorf("%s: client message %q is not the safe text", name, fshttp.ClientMessage(err))
		}
	}
}

func TestPreview_FitsMaxSide(t *testing.T) {
	requirePrograms(t)
	for name, tc := range map[string]struct {
		w, h  float64
		side  int
		wantW int
		wantH int
	}{
		"portrait letter": {letterW, letterH, 200, 155, 200},
		// A page claiming to be 200 inches wide renders no larger.
		"huge page": {14400, 720, 300, 300, 15},
	} {
		jpg, err := pdf.Tool{MaxSide: tc.side}.Preview(t.Context(), bytes.NewReader(document(1, tc.w, tc.h)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
		if err != nil || img.Width != tc.wantW || img.Height != tc.wantH {
			t.Errorf("%s: preview %dx%d, %v; want %dx%d", name, img.Width, img.Height, err, tc.wantW, tc.wantH)
		}
	}
}

func TestTool_Bounds(t *testing.T) {
	requirePrograms(t)
	body := document(1, letterW, letterH)
	if _, err := (pdf.Tool{MaxInput: 100}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("MaxInput: %v", err)
	}
	if _, err := (pdf.Tool{Timeout: time.Nanosecond}).Probe(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("Timeout: %v", err)
	}
	if _, err := (pdf.Tool{MaxPreview: 100}).Preview(t.Context(), bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("MaxPreview: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := (pdf.Tool{}).Probe(ctx, bytes.NewReader(body)); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("cancelled: %v", err)
	}
}

func TestTool_MissingProgram(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "pdfinfo")
	_, err := pdf.Tool{PDFInfo: missing}.Probe(t.Context(), strings.NewReader("x"))
	if !errors.Is(err, pdf.ErrNoProgram) || filesystem.CodeOf(err) != filesystem.CodeUnavailable || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v", err)
	}
	if _, err := (pdf.Tool{PDFToPPM: missing}).Preview(t.Context(), strings.NewReader("x")); !errors.Is(err, pdf.ErrNoProgram) {
		t.Fatalf("preview err = %v", err)
	}
}

func newDisk(t *testing.T) *filesystem.Disk {
	t.Helper()
	d, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var docPolicy = upload.Policy{MaxSize: 10 << 20, Allowed: []string{pdf.ContentType, "text/plain"}}

func TestInspect(t *testing.T) {
	requirePrograms(t)
	d := newDisk(t)
	p := docPolicy
	p.Inspect = pdf.Tool{}.Inspect(pdf.Limits{MaxPages: 2})
	if _, err := upload.Receive(t.Context(), d, bytes.NewReader(document(3, a4W, a4H)), "long.pdf", p); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("too many pages: %v", err)
	}
	if _, err := upload.Receive(t.Context(), d, bytes.NewReader(document(2, a4W, a4H)), "ok.pdf", p); err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Receive(t.Context(), d, strings.NewReader("quarterly numbers\n"), "notes.txt", p); err != nil {
		t.Fatalf("a text file must pass untouched: %v", err)
	}
	n := 0
	for _, err := range d.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("%d objects stored, want the accepted PDF and the text file", n)
	}
}

const scope = "acme"

type env struct {
	disk  *filesystem.Disk
	store *records.Store
}

func newEnv(t *testing.T, p *pdf.Plugin) *env {
	t.Helper()
	disk := newDisk(t)
	mgr, err := filesystem.NewManager(disk)
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
	return &env{disk: disk, store: store}
}

// reread returns f as stored now: what Process recorded after the
// upload's commit.
func (e *env) reread(t *testing.T, f records.File) records.File {
	t.Helper()
	got, err := e.store.File(t.Context(), f.Scope, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *env) exists(t *testing.T, key string) bool {
	t.Helper()
	ok, err := e.disk.Exists(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestPlugin_RecordsPagesAndAPreview(t *testing.T) {
	requirePrograms(t)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{MaxSide: 300}, pdf.WithPreview()))
	f, err := e.store.Upload(t.Context(), scope, "media", bytes.NewReader(document(4, a4W, a4H)), "report.pdf", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	f = e.reread(t, f)
	if f.Status != records.StatusReady {
		t.Fatalf("status after processing = %q %q", f.Status, f.StatusDetail)
	}
	if m, ok := pdf.Of(f); !ok || m.Pages != 4 || m.PageWidth != a4W || m.PageHeight != a4H {
		t.Fatalf("meta = %+v %v (%v)", m, ok, f.Metadata)
	}
	preview, ok := pdf.PreviewKey(f)
	if !ok || !strings.HasPrefix(preview, f.Key+pdf.PreviewInfix) {
		t.Fatalf("preview key = %q", preview)
	}
	if st, err := e.disk.Stat(t.Context(), preview); err != nil || st.ContentType != pdf.PreviewContentType {
		t.Fatalf("preview = %+v, %v", st, err)
	}
	next, err := e.store.Replace(t.Context(), scope, f.ID, bytes.NewReader(document(1, letterW, letterH)), "", docPolicy)
	if err != nil {
		t.Fatal(err)
	}
	next = e.reread(t, next)
	nextPreview, _ := pdf.PreviewKey(next)
	if m, _ := pdf.Of(next); m.Pages != 1 || nextPreview == preview || !e.exists(t, nextPreview) || e.exists(t, preview) {
		t.Fatalf("after replace: %+v, new %q, old exists %v", m, nextPreview, e.exists(t, preview))
	}
	if err := e.store.Purge(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if e.exists(t, nextPreview) {
		t.Fatal("the preview outlived its purged file")
	}
}

func TestPlugin_RollbackDeletesThePreview(t *testing.T) {
	requirePrograms(t)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview()))
	errAbort := errors.New("abort")
	err := e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if _, err := e.store.Upload(ctx, scope, "media", bytes.NewReader(document(1, a4W, a4H)), "r.pdf", docPolicy); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatal(err)
	}
	for _, err := range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("an object outlived the rolled-back upload")
	}
}

func TestPlugin_UnreadableAndOtherFiles(t *testing.T) {
	requirePrograms(t)
	e := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithPreview()))
	f, err := e.store.Upload(t.Context(), scope, "media", strings.NewReader("quarterly numbers\n"), "notes.txt", docPolicy)
	if err != nil || f.Metadata != nil {
		t.Fatalf("text = %v, %v", f.Metadata, err)
	}
	broken := document(2, a4W, a4H)[:200]
	in := records.NewFile{Scope: scope, Disk: "media", Key: "broken.pdf", Filename: "broken.pdf", ContentType: pdf.ContentType, Metadata: map[string]string{pdf.MetaPages: "stale", "app": "kept"}}
	if _, err := e.disk.WriteBytes(t.Context(), in.Key, broken, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if f, err = e.store.CreateFile(t.Context(), in); err != nil || len(f.Metadata) != 1 || f.Metadata["app"] != "kept" {
		t.Fatalf("broken = %v, %v", f.Metadata, err)
	}
	strict := newEnv(t, pdf.NewPlugin(pdf.Tool{}, pdf.WithStrict()))
	if _, err := strict.disk.WriteBytes(t.Context(), in.Key, broken, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := strict.store.CreateFile(t.Context(), in); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("strict: %v", err)
	}
	missing := newEnv(t, pdf.NewPlugin(pdf.Tool{PDFInfo: "/nonexistent/pdfinfo"}))
	if _, err := missing.store.Upload(t.Context(), scope, "media", bytes.NewReader(document(1, a4W, a4H)), "r.pdf", docPolicy); !errors.Is(err, pdf.ErrNoProgram) {
		t.Fatalf("missing program: %v", err)
	}
}

func TestOf_WithoutMetadata(t *testing.T) {
	if _, ok := pdf.Of(records.File{}); ok {
		t.Fatal("Of reported metadata for a file without any")
	}
	if _, ok := pdf.PreviewKey(records.File{}); ok {
		t.Fatal("PreviewKey reported a preview for a file without one")
	}
	if pdf.NewPlugin(pdf.Tool{}).Name() != pdf.PluginName {
		t.Fatal("name")
	}
}

// TestProbe_AcceptsAnOwnerPasswordOnly pins that a PDF restricted only by
// an owner password (no printing, no copying) opens and is read.
func TestProbe_AcceptsAnOwnerPasswordOnly(t *testing.T) {
	requirePrograms(t)
	if _, err := exec.LookPath("qpdf"); err != nil {
		t.Skip("qpdf is not installed: it builds the restricted PDF")
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.pdf"), filepath.Join(dir, "restricted.pdf")
	if err := os.WriteFile(in, document(2, a4W, a4H), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.CommandContext(t.Context(), "qpdf", "--encrypt", "", "owner-secret", "256", "--print=none", "--extract=n", "--", in, out).CombinedOutput(); err != nil {
		t.Fatalf("qpdf: %v %s", err, b)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := (pdf.Tool{}).Probe(t.Context(), bytes.NewReader(body)); err != nil || info.Pages != 2 || !info.Encrypted {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if jpg, err := (pdf.Tool{}).Preview(t.Context(), bytes.NewReader(body)); err != nil || len(jpg) == 0 {
		t.Fatalf("preview: %d bytes, %v", len(jpg), err)
	}
}
