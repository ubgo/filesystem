package sniff_test

import (
	"strings"
	"testing"

	"github.com/ubgo/filesystem/sniff"
)

func TestDetect(t *testing.T) {
	zip := "PK\x03\x04" + strings.Repeat("\x00", 26)
	cases := []struct {
		head, name, want string
	}{
		{"\x89PNG\r\n\x1a\n", "x.exe", "image/png"},
		{"\xff\xd8\xff\xe0", "", "image/jpeg"},
		{"\x00\x00\x00\x1cftypavif\x00\x00", "a.png", sniff.AVIF},
		{"\x00\x00\x00\x18ftypheic", "", sniff.HEIC},
		{"\x00\x00\x00\x18ftypmif1", "", sniff.HEIF},
		{"\x00\x00\x00\x14ftypqt  ", "", sniff.QuickTime},
		{"\x00\x00\x00\x18ftypisom", "", "video/mp4"},
		{`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg">`, "logo.png", sniff.SVG},
		{`<svg><script>alert(1)</script></svg>`, "", sniff.SVG},
		{"a,b,c\n1,2,3\n", "data.csv", sniff.CSV},
		{`{"a": 1}`, "x.json", sniff.JSON},
		{"# Title", "README.md", sniff.Markdown},
		{"plain words", "notes.html", sniff.TextPlain},
		{"plain words", "", sniff.TextPlain},
		{zip, "report.docx", sniff.DOCX},
		{zip, "sheet.XLSX", sniff.XLSX},
		{zip, "deck.pptx", sniff.PPTX},
		{zip, "book.epub", sniff.EPUB},
		{zip, "archive.zip", sniff.Zip},
		{"\x00\x01\x02\x03", "x.json", sniff.OctetStream},
		{"", "", sniff.TextPlain},
	}
	for _, c := range cases {
		if got := sniff.Detect([]byte(c.head), c.name); got != c.want {
			t.Errorf("Detect(%q, %q) = %q, want %q", c.head, c.name, got, c.want)
		}
	}
	long := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 2*sniff.HeadSize)...)
	if got := sniff.Detect(long, ""); got != "image/png" {
		t.Errorf("long head = %q", got)
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		mime, pattern string
		want          bool
	}{
		{"image/png", "image/png", true},
		{"image/png", "image/*", true},
		{"IMAGE/PNG", "image/*", true},
		{"text/plain; charset=utf-8", "text/plain", true},
		{"image/png", "video/*", false},
		{"image/png", "*/*", true},
		{"", "*/*", false},
		{"imagex/png", "image/*", false},
	}
	for _, c := range cases {
		if got := sniff.Matches(c.mime, c.pattern); got != c.want {
			t.Errorf("Matches(%q, %q) = %v", c.mime, c.pattern, got)
		}
	}
	if !sniff.MatchesAny("image/png", []string{"video/*", "image/*"}) || sniff.MatchesAny("image/png", nil) {
		t.Error("MatchesAny wrong")
	}
}

func TestExtension(t *testing.T) {
	for mime, want := range map[string]string{
		"image/jpeg":                ".jpg",
		"text/plain; charset=utf-8": ".txt",
		sniff.DOCX:                  ".docx",
		"application/x-unknown":     "",
	} {
		if got := sniff.Extension(mime); got != want {
			t.Errorf("Extension(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestTypeByExtension(t *testing.T) {
	for name, want := range map[string]string{
		"a.png": "image/png", "PHOTO.JPG": "image/jpeg", "x.jpeg": "image/jpeg", "dir/report.PDF": "application/pdf",
		"page.htm": "text/html", "song.ogg": "audio/ogg", "clip.MOV": sniff.QuickTime, "a.docx": sniff.DOCX,
		"no-extension": "", "weird.xyz": "", ".hidden": "",
	} {
		if got := sniff.TypeByExtension(name); got != want {
			t.Errorf("TypeByExtension(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIsActive(t *testing.T) {
	cases := map[string]bool{
		"text/html; charset=utf-8": true, sniff.SVG: true, "application/xml": true,
		// Any structured XML type can carry XHTML script.
		"application/atom+xml": true, "application/rss+xml": true, "Application/MathML+XML; charset=utf-8": true, "image/foo+xml": true,
		"text/xsl": true, "application/xslt+xml": true,
		// Legacy JavaScript names.
		"application/x-javascript": true, "text/ecmascript": true, "application/ecmascript": true, "text/jscript": true, "text/javascript1.5": true,
		"image/png": false, "text/plain": false, "application/json": false, "application/ld+json": false, "application/xmlfoo": false,
	}
	for mime, want := range cases {
		if sniff.IsActive(mime) != want {
			t.Errorf("IsActive(%q) != %v", mime, want)
		}
	}
}

// TestDetectWith pins the custom-detector hook: it labels only what Detect
// calls octet-stream, never relabels a known type, and an empty or
// malformed answer stays octet-stream.
func TestDetectWith(t *testing.T) {
	const unknown, custom = "\x00\x01FAKE3D\x00", "model/x-fake3d"
	answer := func(a string) sniff.Detector { return func([]byte, string) string { return a } }
	cases := []struct {
		name   string
		detect sniff.Detector
		head   string
		want   string
	}{
		{"labels unknown content", answer(custom), unknown, custom},
		{"keeps a known type", answer(custom), "\x89PNG\r\n\x1a\n", "image/png"},
		{"keeps HTML", answer(custom), "<!DOCTYPE html><p>x", "text/html; charset=utf-8"},
		{"empty answer", answer(""), unknown, sniff.OctetStream},
		{"blank answer", answer("  "), unknown, sniff.OctetStream},
		{"malformed answer", answer("not a type;;"), unknown, sniff.OctetStream},
		{"nil detector is Detect", nil, unknown, sniff.OctetStream},
	}
	for _, c := range cases {
		if got := sniff.DetectWith(c.detect, []byte(c.head), "f.bin"); got != c.want {
			t.Errorf("%s: DetectWith = %q, want %q", c.name, got, c.want)
		}
	}
	// The detector sees at most HeadSize bytes, as Detect does.
	var seen int
	sniff.DetectWith(func(head []byte, _ string) string { seen = len(head); return "" }, []byte(unknown+strings.Repeat("\x00", 2*sniff.HeadSize)), "")
	if seen != sniff.HeadSize {
		t.Errorf("detector saw %d bytes, want %d", seen, sniff.HeadSize)
	}
}
