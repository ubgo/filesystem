package office_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/office"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/recordstest"
	"github.com/ubgo/filesystem/upload"
)

// onePagePDF is a letter-size PDF whose one page says text.
func onePagePDF(text string) []byte {
	content := fmt.Sprintf("BT /F1 18 Tf 72 700 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
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

// TestPDFPlugin_DescribesOfficeFiles plugs a Tool into contrib/pdf's
// plugin as its Converter: an uploaded Word document is recorded with the
// page count and text of its PDF. soffice is a stand-in that writes a
// fixed PDF, so the test needs only Poppler.
func TestPDFPlugin_DescribesOfficeFiles(t *testing.T) {
	for _, p := range []string{pdf.DefaultPDFInfo, pdf.DefaultPDFToText} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed (brew install poppler, apt install poppler-utils)", p)
		}
	}
	fixture := filepath.Join(t.TempDir(), "converted.pdf")
	if err := os.WriteFile(fixture, onePagePDF("Quarterly results"), 0o600); err != nil {
		t.Fatal(err)
	}
	conv := office.Tool{SOffice: fakeSOffice(t, `cp `+fixture+` "$out/document.pdf"`)}
	disk, err := filesystem.NewDisk("media", memory.New())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := recordstest.Open(t)
	plugin := pdf.NewPlugin(pdf.Tool{}, pdf.WithConverter(conv), pdf.WithText())
	store, err := records.New(db, recordstest.Dialect(), mgr, records.WithPlugins(plugin))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	policy := upload.Policy{MaxSize: 1 << 20, Allowed: []string{office.DOCX}}
	f, err := store.Upload(t.Context(), "acme", "media", bytes.NewReader(docx(t, "Quarterly results")), "report.docx", policy)
	if err != nil {
		t.Fatal(err)
	}
	// The conversion runs after the upload commits; read the row it wrote.
	if f, err = store.File(t.Context(), f.Scope, f.ID); err != nil {
		t.Fatal(err)
	}
	m, ok := pdf.Of(f)
	key, hasText := pdf.TextKey(f)
	if !ok || m.Pages != 1 || !hasText {
		t.Fatalf("metadata = %v", f.Metadata)
	}
	if text, err := disk.ReadAll(t.Context(), key); err != nil || !strings.Contains(string(text), "Quarterly results") {
		t.Fatalf("text = %q, %v", text, err)
	}
}
