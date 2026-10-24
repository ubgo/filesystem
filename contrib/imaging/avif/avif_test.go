package avif_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/avif"
)

// pattern is a 64 × 48 image of four flat quadrants.
func pattern() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	quad := []color.NRGBA{{220, 30, 30, 255}, {30, 200, 40, 255}, {20, 40, 210, 255}, {240, 230, 30, 255}}
	for y := range 48 {
		for x := range 64 {
			img.SetNRGBA(x, y, quad[(y/24)*2+x/32])
		}
	}
	return img
}

func pngOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func near(got, want image.Image, tol int) bool {
	for _, p := range []image.Point{{16, 12}, {48, 12}, {16, 36}, {48, 36}} {
		r1, g1, b1, _ := got.At(p.X, p.Y).RGBA()
		r2, g2, b2, _ := want.At(p.X, p.Y).RGBA()
		for _, d := range []int{int(r1>>8) - int(r2>>8), int(g1>>8) - int(g2>>8), int(b1>>8) - int(b2>>8)} {
			if d > tol || -d > tol {
				return false
			}
		}
	}
	return true
}

func pipeline(opts ...avif.Options) *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(avif.Encoder(opts...)), imaging.WithDecoder(avif.Decoder()))
}

var toAVIF = imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatAVIF, Quality: 80}

// TestRoundTrip encodes the pattern as AVIF, then reads it back through
// the same Pipeline: Probe from the header, Process to PNG, and the
// decoded colours near the source.
func TestRoundTrip(t *testing.T) {
	src := pattern()
	p := pipeline()
	res, err := p.Process(bytes.NewReader(pngOf(t, src)), toAVIF, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "image/avif" || !avif.Decoder().Match(res.Data) {
		t.Fatalf("result %s, head %q", res.ContentType, res.Data[:12])
	}
	info, err := p.Probe(bytes.NewReader(res.Data))
	if err != nil || info != (imaging.Info{Format: "avif", Width: 64, Height: 48}) {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	back, err := p.Process(bytes.NewReader(res.Data), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := png.Decode(bytes.NewReader(back.Data))
	// Lossy AVIF subsamples chroma: flat colours come back near, not exact.
	if !near(got, src, 20) {
		t.Error("decoded colours are far from the source")
	}
	low, _ := p.Process(bytes.NewReader(pngOf(t, src)), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatAVIF, Quality: 5}, imaging.Options{})
	if len(low.Data) >= len(res.Data) {
		t.Errorf("quality 5 is %d bytes, quality 80 %d", len(low.Data), len(res.Data))
	}
}

func TestLossless(t *testing.T) {
	src := pattern()
	p := pipeline(avif.Options{Lossless: true, Speed: 8})
	res, err := p.Process(bytes.NewReader(pngOf(t, src)), toAVIF, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	back, err := p.Process(bytes.NewReader(res.Data), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := png.Decode(bytes.NewReader(back.Data))
	if !near(got, src, 0) {
		t.Error("lossless changed pixels")
	}
}

func TestMatch(t *testing.T) {
	d := avif.Decoder()
	for head, want := range map[string]bool{
		"\x00\x00\x00\x1cftypavif": true, "\x00\x00\x00\x1cftypavis": true,
		"\x00\x00\x00\x18ftypheic": false, "\x00\x00\x00\x1cftyp": false, "RIFF\x00\x00\x00\x00WEBP": false,
	} {
		if d.Match([]byte(head)) != want {
			t.Errorf("Match(%q) = %v", head, !want)
		}
	}
	if d.Name() != "avif" || avif.Encoder().Format() != imaging.FormatAVIF {
		t.Error("names")
	}
	_ = avif.Native()
}
