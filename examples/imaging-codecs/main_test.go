package main

import (
	"bytes"
	"image"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/webp"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"image/jpeg 200x150 .jpg", "image/webp 200x150 .webp", "image/avif 200x150 .avif",
		"dropped profile 180, converted to sRGB 190, profile kept false",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestRoundTrip decodes the WebP and AVIF outputs with the same Pipeline,
// so a variant written in either format can be read back (probed for its
// dimensions, processed again).
func TestRoundTrip(t *testing.T) {
	src, err := photo()
	if err != nil {
		t.Fatal(err)
	}
	p := modernPipeline()
	for _, f := range []imaging.Format{imaging.FormatWebP, imaging.FormatAVIF} {
		res, err := p.Process(bytes.NewReader(src), imaging.Transform{Mode: imaging.ModeFit, Width: 50, Height: 50, Format: f}, imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		info, err := p.Probe(bytes.NewReader(res.Data))
		if err != nil || info.Width != 50 || info.Height != 38 {
			t.Fatalf("%s probe = %+v, %v", f, info, err)
		}
	}
}

// TestLosslessWebP pins that a lossless encoder keeps every pixel: the
// choice for screenshots and drawings.
func TestLosslessWebP(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7)
	}
	var buf bytes.Buffer
	enc := webp.Encoder(webp.Options{Lossless: true})
	if err := enc.Encode(&buf, img, 0); err != nil {
		t.Fatal(err)
	}
	got, err := webp.Decoder().Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 8 {
		for x := range 8 {
			r1, g1, b1, a1 := img.At(x, y).RGBA()
			r2, g2, b2, a2 := got.At(x, y).RGBA()
			if a1 == 0xffff && (r1>>8 != r2>>8 || g1>>8 != g2>>8 || b1>>8 != b2>>8 || a1 != a2) {
				t.Fatalf("pixel %d,%d changed", x, y)
			}
		}
	}
}
