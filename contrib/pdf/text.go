package pdf

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"unicode"
)

// DefaultPDFToText is pdftotext's name, looked up on PATH when
// Tool.PDFToText is empty.
const DefaultPDFToText = "pdftotext"

// Default text limits, used when Tool leaves a field zero.
const (
	// DefaultMaxText bounds extracted text: 1 MiB is some 500 pages of
	// prose, plenty for a search index.
	DefaultMaxText int64 = 1 << 20
	// DefaultMaxTextPages bounds the pages pdftotext reads.
	DefaultMaxTextPages = 2000
	// DefaultMaxOCRPages bounds the pages of a scanned PDF sent to a
	// Recognizer.
	DefaultMaxOCRPages = 20
	// DefaultOCRSide is the longest side of a page rendered for a
	// Recognizer: an A4 page at 300 dpi, the resolution OCR engines are
	// tuned for.
	DefaultOCRSide = 3508
)

// maxOCRImage bounds one rendered page sent to a Recognizer. A PNG of a
// DefaultOCRSide page is a few MiB; a page that renders larger is
// CodeTooLarge rather than memory spent.
const maxOCRImage = 64 << 20

// pageBreak separates pages in Extract.Text, as pdftotext prints them.
const pageBreak = "\f"

// TextContentType is the type of a stored text derivative.
const TextContentType = "text/plain; charset=utf-8"

// Recognizer reads the text in an image (OCR). Tool.Text uses one for a
// PDF without a text layer (a scan), rendering each page as a PNG and
// passing it in. contrib/ocr's Tool is one; a cloud OCR service is
// another. It must bound its own work and output.
type Recognizer interface {
	Recognize(ctx context.Context, image io.Reader) (string, error)
}

// Extract is a PDF's text.
type Extract struct {
	// Text is the document's text as UTF-8, pages separated by form feeds
	// ("\f"); "" when it has none.
	Text string
	// Truncated reports that Text stops short of the document: it reached
	// MaxText bytes, or the document has more pages than MaxTextPages
	// (MaxOCRPages when OCR).
	Truncated bool
	// OCR reports that Text came from a Recognizer because the PDF had no
	// text layer.
	OCR bool
}

// Text extracts r's text with pdftotext, for a search index or a
// summary. When the PDF has no text layer (no letter or digit in it, as
// in a scan) and rec is not nil, its first MaxOCRPages pages are rendered
// and read by rec instead. A file pdfinfo cannot read is CodeRejected.
func (t Tool) Text(ctx context.Context, r io.Reader, rec Recognizer) (Extract, error) {
	var out Extract
	err := t.withWorkFile(r, func(path string) error {
		info, err := t.infoPath(ctx, path)
		if err != nil {
			return err
		}
		out, err = t.textPath(ctx, path, info.Pages, rec)
		return err
	})
	return out, err
}

// textPath extracts the text of path, a PDF of pages pages.
func (t Tool) textPath(ctx context.Context, path string, pages int, rec Recognizer) (Extract, error) {
	last := min(pages, t.maxTextPages())
	stdout := &capped{max: t.maxText()}
	args := []string{"-enc", textEncoding, "-l", strconv.Itoa(last), path, "-"}
	if err := t.runTo(ctx, program(t.PDFToText, DefaultPDFToText), args, stdout); err != nil && !stdout.over {
		return Extract{}, err
	}
	out := Extract{Text: validText(stdout.buf.Bytes()), Truncated: stdout.over || pages > last}
	if rec == nil || hasText(out.Text) {
		return out, nil
	}
	return t.ocrPath(ctx, path, pages, rec)
}

// ocrPath renders the first MaxOCRPages pages of path and reads each with
// rec, keeping at most MaxText bytes: a page is cut to the room left
// before it is kept, so memory never holds more than MaxText of text
// besides the one page being added.
func (t Tool) ocrPath(ctx context.Context, path string, pages int, rec Recognizer) (Extract, error) {
	last := min(pages, t.maxOCRPages())
	out := Extract{OCR: true, Truncated: pages > last}
	var b bytes.Buffer
	for page := 1; page <= last; page++ {
		img, err := t.renderPage(ctx, path, page, t.ocrSide(), formatPNG, maxOCRImage)
		if err != nil {
			return Extract{}, err
		}
		text, err := rec.Recognize(ctx, bytes.NewReader(img))
		if err != nil {
			return Extract{}, err
		}
		if !appendCapped(&b, text, t.maxText()) || !appendCapped(&b, pageBreak, t.maxText()) {
			out.Truncated = true
			break
		}
	}
	out.Text = validText(b.Bytes())
	return out, nil
}

// appendCapped appends s to b without letting b pass limit bytes: it keeps
// what fits and reports false when s did not fit whole. Nothing past the
// cap is ever copied, so a recognizer returning megabytes for one page
// costs no more memory than its own string.
func appendCapped(b *bytes.Buffer, s string, limit int64) bool {
	room := max(limit-int64(b.Len()), 0)
	if int64(len(s)) > room {
		b.WriteString(s[:room])
		return false
	}
	b.WriteString(s)
	return true
}

// validText returns text as valid UTF-8, dropping a rune a cap cut in
// half.
func validText(text []byte) string {
	return strings.ToValidUTF8(string(text), "")
}

// hasText reports whether text holds a letter or a digit: pdftotext's
// output for a scan is form feeds and blank lines.
func hasText(text string) bool {
	return strings.IndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

func (t Tool) maxText() int64 {
	if t.MaxText <= 0 {
		return DefaultMaxText
	}
	return t.MaxText
}

func (t Tool) maxTextPages() int {
	if t.MaxTextPages <= 0 {
		return DefaultMaxTextPages
	}
	return t.MaxTextPages
}

func (t Tool) maxOCRPages() int {
	if t.MaxOCRPages <= 0 {
		return DefaultMaxOCRPages
	}
	return t.MaxOCRPages
}

func (t Tool) ocrSide() int {
	if t.OCRSide <= 0 {
		return DefaultOCRSide
	}
	return t.OCRSide
}
