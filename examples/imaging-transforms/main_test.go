package main

import (
	"bytes"
	"flag"
	"image"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/imagingtest"
)

// update is the flag imagingtest.Golden reads: go test -update rewrites
// testdata/watermarked.golden.png from the current output.
var update = flag.Bool("update", false, "rewrite the golden images")

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fit  100x50 image/png", "fill 100x100 image/jpeg", "pad  100x100 image/png", "crop 100x100 image/png",
		"exif 6: stored 300x200, probe 200x300, output 200x300",
		"checkerboard kept: smart crop 50%, centre crop 0%",
		"watermarked 200x100, bottom right corner changed true",
		"gif: animated 3 frames, first frame 1",
		"dominant colour #969696", "over 10000 pixels: rejected", "custom processor ran 1 time(s)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestWatermarkGolden compares the watermarked output with a stored image
// by how it looks, so an encoder's one-level rounding change passes and a
// misplaced or missing watermark fails.
// ds:def id=testwatermarkgolden-n5c8r3tw
func TestWatermarkGolden(t *testing.T) {
	src, err := encodePNG(photo())
	if err != nil {
		t.Fatal(err)
	}
	p, tr, err := watermarked()
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Process(bytes.NewReader(src), tr, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := image.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	imagingtest.Golden(t, got, "testdata/watermarked.golden.png", imagingtest.Options{Update: *update})
}

// TestUnknownWatermark pins that a transform naming a watermark the
// Pipeline does not have is refused, not drawn without it.
func TestUnknownWatermark(t *testing.T) {
	tr := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG, Watermark: &imaging.Watermark{Name: "nope"}}
	src, err := encodePNG(photo())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imaging.Process(bytes.NewReader(src), tr, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("unknown watermark = %v, want invalid_argument", err)
	}
}

// TestWebPNeedsItsCodec pins that WebP output without the codec module is
// unsupported rather than silently written as another format.
func TestWebPNeedsItsCodec(t *testing.T) {
	tr := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatWebP}
	src, err := encodePNG(photo())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := imaging.Process(bytes.NewReader(src), tr, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("WebP without its codec = %v, want unsupported", err)
	}
}

// TestFitNeverEnlarges pins that ModeFit keeps a small image small.
func TestFitNeverEnlarges(t *testing.T) {
	small, err := encodePNG(image.NewNRGBA(image.Rect(0, 0, 20, 10)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := imaging.Process(bytes.NewReader(small), modes["fit"], imaging.Options{})
	if err != nil || res.Width != 20 || res.Height != 10 {
		t.Fatalf("fit of 20x10 = %dx%d, %v", res.Width, res.Height, err)
	}
}

// TestTooManyFrames pins the frame limit, read before any frame decodes.
func TestTooManyFrames(t *testing.T) {
	anim, err := animation()
	if err != nil {
		t.Fatal(err)
	}
	tr := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatGIF}
	if _, err := imaging.Process(bytes.NewReader(anim), tr, imaging.Options{MaxFrames: frames - 1}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("over MaxFrames = %v, want rejected", err)
	}
}
