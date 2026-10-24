package webp_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	xwebp "golang.org/x/image/webp"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/webp"
)

// pattern is a 64 × 48 image of four flat quadrants, which survives lossy
// compression with small errors away from the edges.
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

// near reports whether every sampled quadrant centre of got is within tol
// of want in every channel.
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

func pipeline(opts ...webp.Options) *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(webp.Encoder(opts...)), imaging.WithDecoder(webp.Decoder()))
}

// TestEncodesWebP pins the output: a real WebP file that an independent
// decoder (golang.org/x/image/webp) reads back to the source's colours.
func TestEncodesWebP(t *testing.T) {
	src := pattern()
	res, err := pipeline().Process(bytes.NewReader(pngOf(t, src)), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatWebP, Quality: 80}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ContentType != "image/webp" || res.Width != 64 || res.Height != 48 || !webp.Decoder().Match(res.Data) {
		t.Fatalf("result %s %dx%d", res.ContentType, res.Width, res.Height)
	}
	got, err := xwebp.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	// Lossy WebP subsamples chroma (4:2:0), so flat colours come back
	// within a few levels, not exactly.
	if !near(got, src, 20) {
		t.Error("decoded colours are far from the source")
	}
	// Lower quality, smaller file.
	low, _ := pipeline().Process(bytes.NewReader(pngOf(t, src)), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatWebP, Quality: 5}, imaging.Options{})
	if len(low.Data) >= len(res.Data) {
		t.Errorf("quality 5 is %d bytes, quality 80 %d", len(low.Data), len(res.Data))
	}
}

// TestLossless pins exact pixels in lossless mode.
func TestLossless(t *testing.T) {
	src := pattern()
	res, err := pipeline(webp.Options{Lossless: true}).Process(bytes.NewReader(pngOf(t, src)), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatWebP}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := xwebp.Decode(bytes.NewReader(res.Data))
	if err != nil || !near(got, src, 0) {
		t.Errorf("lossless changed pixels: %v", err)
	}
}

// TestDecodesWebP pins the decoder: a WebP upload is resized like any
// other input, and probed from its header.
func TestDecodesWebP(t *testing.T) {
	var webpFile bytes.Buffer
	if err := webp.Encoder(webp.Options{Lossless: true}).Encode(&webpFile, pattern(), 0); err != nil {
		t.Fatal(err)
	}
	p := pipeline()
	info, err := p.Probe(bytes.NewReader(webpFile.Bytes()))
	if err != nil || info != (imaging.Info{Format: "webp", Width: 64, Height: 48}) {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	res, err := p.Process(bytes.NewReader(webpFile.Bytes()), imaging.Transform{Mode: imaging.ModeFit, Width: 32, Height: 32, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil || res.Width != 32 || res.Height != 24 {
		t.Fatalf("Process = %+v, %v", res, err)
	}
	if webp.Decoder().Match([]byte("RIFF\x00\x00\x00\x00WAVEfmt ")) || webp.Decoder().Match([]byte("RIFF")) {
		t.Error("Match accepted a non-WebP RIFF file")
	}
	_ = webp.Native() // reports, whichever backend this machine has
}

func TestEncoderDefaults(t *testing.T) {
	if webp.Encoder().Format() != imaging.FormatWebP {
		t.Error("format")
	}
	var out bytes.Buffer
	if err := webp.Encoder(webp.Options{Method: 6}).Encode(&out, pattern(), 50); err != nil || !bytes.HasPrefix(out.Bytes(), []byte("RIFF")) {
		t.Errorf("method 6: %v", err)
	}
}
