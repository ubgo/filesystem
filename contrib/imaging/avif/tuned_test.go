package avif_test

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/avif"
)

// TestTransformLossless pins the per-transform setting: a lossy encoder
// writes a transform with Lossless exactly, at the fastest effort.
func TestTransformLossless(t *testing.T) {
	src := pattern()
	p := pipeline(avif.Options{Speed: 10})
	res, err := p.Process(bytes.NewReader(pngOf(t, src)), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatAVIF, Lossless: true, Effort: 1}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	back, err := p.Process(bytes.NewReader(res.Data), imaging.Transform{Mode: imaging.ModeFit, Width: 64, Height: 48, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := png.Decode(bytes.NewReader(back.Data))
	if !near(got, src, 0) {
		t.Error("Transform.Lossless did not reach the encoder")
	}
}
