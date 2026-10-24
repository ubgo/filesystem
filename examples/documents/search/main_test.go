package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/contrib/pdf"
)

// textPDF is a letter-size PDF whose one page says text in large type,
// with a title.
func textPDF(title, text string) []byte {
	content := fmt.Sprintf("BT /F1 36 Tf 72 600 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents 5 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Title (" + title + ") >>",
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
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info 6 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestSearch(t *testing.T) {
	for _, p := range []string{pdf.DefaultPDFInfo, pdf.DefaultPDFToPPM, pdf.DefaultPDFToText, ocr.DefaultTesseract} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed (brew install poppler tesseract)", p)
		}
	}
	dir := t.TempDir()
	report := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(report, textPDF("Q3 Report", "Revenue grew"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The receipt is a photo of a page: an image with text in it.
	photo, err := pdf.Tool{MaxSide: 1700}.Preview(t.Context(), bytes.NewReader(textPDF("", "Total due")))
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(dir, "receipt.jpg")
	if err := os.WriteFile(receipt, photo, 0o600); err != nil {
		t.Fatal(err)
	}
	store, disk, err := newStore(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for _, p := range []string{report, receipt} {
		if err := upload1(t.Context(), &out, store, disk, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"title     Q3 Report", "(text layer): Revenue grew", "(OCR): Total due"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
