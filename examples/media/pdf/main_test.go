package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// twoPages is a two-page A4 PDF with a correct cross-reference table.
func twoPages() []byte {
	content := "0.2 0.4 0.8 rg 36 36 523 770 re f"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 4 0 R >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Contents 4 0 R >>",
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

// TestRun uploads a two-page PDF and checks the report. It skips only
// when Poppler is not installed.
func TestRun(t *testing.T) {
	for _, p := range []string{"pdfinfo", "pdftoppm"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed", p)
		}
	}
	var out strings.Builder
	if err := run(t.Context(), &out, bytes.NewReader(twoPages()), "report.pdf"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"application/pdf", "pages     2", "page 1    595x842 pt", ".preview-", "image/jpeg"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	if err := run(t.Context(), &out, strings.NewReader("%PDF-1.4 cut short"), "broken.pdf"); err == nil {
		t.Fatal("a broken PDF was stored")
	}
}
