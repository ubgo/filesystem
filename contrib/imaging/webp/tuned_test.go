package webp_test

import (
	"bytes"
	"image"
	"math/rand/v2"
	"testing"

	xwebp "golang.org/x/image/webp"

	"github.com/ubgo/filesystem/contrib/imaging"
)

// noise is an image lossy WebP cannot keep exactly.
func noise() *image.NRGBA {
	rng := rand.New(rand.NewPCG(3, 4))
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256))
		if i%4 == 3 {
			img.Pix[i] = 255
		}
	}
	return img
}

// TestTransformLossless pins the per-transform setting: one lossy
// encoder writes a transform with Lossless exactly and one without it
// lossily, and every Effort is accepted.
func TestTransformLossless(t *testing.T) {
	src := noise()
	run := func(tr imaging.Transform) image.Image {
		t.Helper()
		res, err := pipeline().Process(bytes.NewReader(pngOf(t, src)), tr, imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		img, err := xwebp.Decode(bytes.NewReader(res.Data))
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	exact := func(img image.Image) bool {
		for y := range 32 {
			for x := range 32 {
				r1, g1, b1, _ := img.At(x, y).RGBA()
				r2, g2, b2, _ := src.At(x, y).RGBA()
				if r1 != r2 || g1 != g2 || b1 != b2 {
					return false
				}
			}
		}
		return true
	}
	tr := imaging.Transform{Mode: imaging.ModeFit, Width: 32, Height: 32, Format: imaging.FormatWebP}
	if exact(run(tr)) {
		t.Fatal("the default encoder was lossless; the test cannot tell")
	}
	tr.Lossless = true
	if !exact(run(tr)) {
		t.Fatal("Transform.Lossless did not reach the encoder")
	}
	for _, e := range []int{1, imaging.MaxEffort} {
		tr.Effort = e
		run(tr)
	}
}
