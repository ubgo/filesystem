package ocr_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/ocr"
	"github.com/ubgo/filesystem/contrib/pdf"
	"github.com/ubgo/filesystem/fshttp"
)

// fakeTesseract writes an executable shell script standing in for
// tesseract, so a test controls what it prints and how it exits.
func fakeTesseract(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tesseract")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// smallPNG is a w×h white PNG.
func smallPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRecognize_PassesTheImageAndLanguages(t *testing.T) {
	log := filepath.Join(t.TempDir(), "args")
	tool := ocr.Tool{Tesseract: fakeTesseract(t, `printf '%s\n' "$@" > `+log+`; printf '  hello\nworld \n\n'`), Languages: []string{"eng", "chi_sim"}}
	text, err := tool.Recognize(t.Context(), bytes.NewReader(smallPNG(t, 4, 4)))
	if err != nil || text != "hello\nworld" {
		t.Fatalf("text = %q, %v", text, err)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if fields := strings.Fields(string(args)); len(fields) != 4 || fields[1] != "stdout" || fields[2] != "-l" || fields[3] != "eng+chi_sim" {
		t.Fatalf("arguments = %q", fields)
	}
	if text, err := (ocr.Tool{Tesseract: fakeTesseract(t, "true")}).Recognize(t.Context(), bytes.NewReader(smallPNG(t, 4, 4))); err != nil || text != "" {
		t.Fatalf("no text = %q, %v", text, err)
	}
}

// TestRecognize_RefusesBeforeRunning pins every refusal that must happen
// before the program sees anything: a language that could be an option
// or a path, a file that is not an accepted image (tesseract reads a text
// file as a list of images to open), too many bytes, too many pixels.
func TestRecognize_RefusesBeforeRunning(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	prog := fakeTesseract(t, "touch "+ran+"; echo text")
	for name, tc := range map[string]struct {
		tool  ocr.Tool
		input []byte
		code  filesystem.Code
	}{
		"option as a language":  {ocr.Tool{Languages: []string{"--tessdata-dir=/etc"}}, smallPNG(t, 4, 4), filesystem.CodeInvalidArgument},
		"path as a language":    {ocr.Tool{Languages: []string{"eng", "../../x"}}, smallPNG(t, 4, 4), filesystem.CodeInvalidArgument},
		"empty language":        {ocr.Tool{Languages: []string{""}}, smallPNG(t, 4, 4), filesystem.CodeInvalidArgument},
		"list of image paths":   {ocr.Tool{}, []byte("/etc/passwd\n/home/user/secret.png\n"), filesystem.CodeRejected},
		"svg":                   {ocr.Tool{}, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), filesystem.CodeRejected},
		"too many bytes":        {ocr.Tool{MaxInput: 10}, smallPNG(t, 4, 4), filesystem.CodeTooLarge},
		"too many pixels":       {ocr.Tool{MaxPixels: 399}, smallPNG(t, 20, 20), filesystem.CodeTooLarge},
		"header claims a giant": {ocr.Tool{}, giantHeader(t), filesystem.CodeTooLarge},
	} {
		tc.tool.Tesseract = prog
		if _, err := tc.tool.Recognize(t.Context(), bytes.NewReader(tc.input)); filesystem.CodeOf(err) != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Fatalf("%s: the program ran", name)
		}
	}
	if _, err := (ocr.Tool{Tesseract: prog, MaxPixels: 400}).Recognize(t.Context(), bytes.NewReader(smallPNG(t, 20, 20))); err != nil {
		t.Fatalf("exactly MaxPixels: %v", err)
	}
}

// giantHeader is a PNG whose header declares 100000×100000 pixels: what
// the pixel bound is for, refused from the header alone.
func giantHeader(t *testing.T) []byte {
	t.Helper()
	b := smallPNG(t, 1, 1)
	// IHDR's width and height follow the 8-byte signature, the chunk
	// length and the chunk type; its checksum covers the type and the 13
	// data bytes.
	copy(b[16:24], []byte{0, 1, 0x86, 0xa0, 0, 1, 0x86, 0xa0})
	binary.BigEndian.PutUint32(b[29:33], crc32.ChecksumIEEE(b[12:29]))
	return b
}

func TestRecognize_Bounds(t *testing.T) {
	img := smallPNG(t, 4, 4)
	// Endless text is stopped.
	tool := ocr.Tool{Tesseract: fakeTesseract(t, "yes text"), MaxText: 1000, Timeout: 10 * time.Second}
	if _, err := tool.Recognize(t.Context(), bytes.NewReader(img)); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Fatalf("endless text: %v", err)
	}
	// A run past Timeout is stopped.
	tool = ocr.Tool{Tesseract: fakeTesseract(t, "exec sleep 300"), Timeout: 300 * time.Millisecond}
	start := time.Now()
	if _, err := tool.Recognize(t.Context(), bytes.NewReader(img)); filesystem.CodeOf(err) != filesystem.CodeUnavailable || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	// A failing run keeps only the start of its messages, and its client
	// message names no path.
	tool = ocr.Tool{Tesseract: fakeTesseract(t, `i=0; while [ $i -lt 2000 ]; do echo "Error in pixRead $i" >&2; i=$((i+1)); done; exit 1`)}
	_, err := tool.Recognize(t.Context(), bytes.NewReader(img))
	var pe *ocr.ProgramError
	if filesystem.CodeOf(err) != filesystem.CodeRejected || !errors.As(err, &pe) || len(pe.Detail) == 0 || len(pe.Detail) > 4<<10 || pe.Program != "tesseract" {
		t.Fatalf("failing run: %v", err)
	}
	if strings.Contains(fshttp.ClientMessage(err), string(filepath.Separator)) {
		t.Fatalf("the client message names a path: %q", fshttp.ClientMessage(err))
	}
	if _, err := (ocr.Tool{Tesseract: "/nonexistent/tesseract"}).Recognize(t.Context(), bytes.NewReader(img)); !errors.Is(err, ocr.ErrNoProgram) || filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("missing program: %v", err)
	}
	if _, err := (ocr.Tool{Tesseract: tool.Tesseract, TempDir: filepath.Join(t.TempDir(), "missing")}).Recognize(t.Context(), bytes.NewReader(img)); err == nil {
		t.Fatal("a missing TempDir was not reported")
	}
}

// requirePrograms skips the test when one of programs is not installed,
// naming it: the only reason a test of this package may skip.
func requirePrograms(t *testing.T, programs ...string) {
	t.Helper()
	for _, p := range programs {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s is not installed (brew install tesseract poppler, apt install tesseract-ocr poppler-utils)", p)
		}
	}
}

// textPDF is a letter-size PDF whose one page says text in large type.
func textPDF(text string) []byte {
	content := fmt.Sprintf("BT /F1 36 Tf 72 600 Td (%s) Tj ET", text)
	return pdfOf("<< /Font << /F1 3 0 R >> >>", content, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
}

// scanPDF is a PDF whose one page is a grayscale image and nothing else:
// a scan, with no text layer.
func scanPDF(t *testing.T, img *image.Gray) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(img.Pix); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	xobj := fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream", w, h, z.Len(), z.String())
	return pdfOf("<< /XObject << /Im1 3 0 R >> >>", "q 612 0 0 792 0 0 cm /Im1 Do Q", xobj)
}

// pdfOf is a one-page letter PDF with the page's resources, its content
// stream and a third object they refer to as 3 0 R.
func pdfOf(resources, content, third string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [4 0 R] /Count 1 >>",
		third,
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources " + resources + " /Contents 5 0 R >>",
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

// pageImage renders a PDF page saying text with Poppler, as a JPEG the
// engine can read.
func pageImage(t *testing.T, text string) []byte {
	t.Helper()
	img, err := pdf.Tool{MaxSide: 1700}.Preview(t.Context(), bytes.NewReader(textPDF(text)))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestRecognize_Tesseract(t *testing.T) {
	requirePrograms(t, ocr.DefaultTesseract, pdf.DefaultPDFToPPM)
	text, err := ocr.Tool{}.Recognize(t.Context(), bytes.NewReader(pageImage(t, "Quarterly results")))
	if err != nil || !strings.Contains(text, "Quarterly results") {
		t.Fatalf("text = %q, %v", text, err)
	}
	if _, err := (ocr.Tool{Languages: []string{"zzz"}}).Recognize(t.Context(), bytes.NewReader(pageImage(t, "x"))); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a language that is not installed: %v", err)
	}
}

// TestPDFText_ReadsAScanWithTesseract plugs a Tool into contrib/pdf as
// its Recognizer: a PDF that is only an image of a page is read.
func TestPDFText_ReadsAScanWithTesseract(t *testing.T) {
	requirePrograms(t, ocr.DefaultTesseract, pdf.DefaultPDFInfo, pdf.DefaultPDFToPPM, pdf.DefaultPDFToText)
	page, _, err := image.Decode(bytes.NewReader(pageImage(t, "Invoice total")))
	if err != nil {
		t.Fatal(err)
	}
	gray := image.NewGray(page.Bounds())
	draw.Draw(gray, gray.Bounds(), page, page.Bounds().Min, draw.Src)
	got, err := pdf.Tool{}.Text(t.Context(), bytes.NewReader(scanPDF(t, gray)), ocr.Tool{})
	if err != nil || !got.OCR || !strings.Contains(got.Text, "Invoice total") {
		t.Fatalf("scan = %+v, %v", got, err)
	}
}

// TestRecognize_PassesOnlyAnAllowlistedEnvironment pins that tesseract
// sees PATH and the variables of Tool.Env, never the application's
// secrets.
func TestRecognize_PassesOnlyAnAllowlistedEnvironment(t *testing.T) {
	t.Setenv("FS_TEST_SECRET", "planted-secret")
	tool := ocr.Tool{Tesseract: fakeTesseract(t, "env"), Env: []string{"TESSDATA_PREFIX=/opt/tessdata"}}
	got, err := tool.Recognize(t.Context(), bytes.NewReader(smallPNG(t, 4, 4)))
	if err != nil || strings.Contains(got, "planted-secret") || !strings.Contains(got, "PATH=") || !strings.Contains(got, "TESSDATA_PREFIX=/opt/tessdata") {
		t.Fatalf("environment:\n%s\n%v", got, err)
	}
}
