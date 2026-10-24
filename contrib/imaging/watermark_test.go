package imaging_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

func rgb8(c color.Color) [3]int {
	r, g, b, _ := c.RGBA()
	return [3]int{int(r >> 8), int(g >> 8), int(b >> 8)}
}

// TestWatermark pins IM-18: a registered image drawn at its anchor, its
// size a fraction of the output, at its opacity.
func TestWatermark(t *testing.T) {
	base := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	fill(base, base.Bounds(), color.NRGBA{255, 255, 255, 255})
	logo := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	fill(logo, logo.Bounds(), color.NRGBA{255, 0, 0, 255})
	p := imaging.NewPipeline(imaging.WithWatermark("logo", logo))
	render := func(w imaging.Watermark) image.Image {
		t.Helper()
		res, err := p.Process(bytes.NewReader(encodePNG(t, base)), imaging.Transform{Mode: imaging.ModeFit, Width: 200, Height: 100, Format: imaging.FormatPNG, Watermark: &w}, imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		img, _ := png.Decode(bytes.NewReader(res.Data))
		return img
	}
	// Default: bottom right, a quarter of the width (50 × 25), opaque.
	img := render(imaging.Watermark{Name: "logo"})
	if rgb8(img.At(190, 90)) != [3]int{255, 0, 0} || rgb8(img.At(140, 90)) != [3]int{255, 255, 255} || rgb8(img.At(10, 10)) != [3]int{255, 255, 255} {
		t.Errorf("default placement: %v %v %v", img.At(190, 90), img.At(140, 90), img.At(10, 10))
	}
	img = render(imaging.Watermark{Name: "logo", Anchor: imaging.AnchorTopLeft, Opacity: 0.5, Margin: 0.05})
	if got := rgb8(img.At(15, 15)); got[0] != 255 || got[1] < 120 || got[1] > 135 || rgb8(img.At(3, 3)) != [3]int{255, 255, 255} {
		t.Errorf("half opacity top left with margin: %v, margin pixel %v", got, img.At(3, 3))
	}
	for _, a := range []imaging.Anchor{imaging.AnchorTopRight, imaging.AnchorBottomLeft, imaging.AnchorCenter} {
		img := render(imaging.Watermark{Name: "logo", Anchor: a, Scale: 0.5})
		at := map[imaging.Anchor]image.Point{imaging.AnchorTopRight: {190, 5}, imaging.AnchorBottomLeft: {5, 90}, imaging.AnchorCenter: {100, 50}}[a]
		if rgb8(img.At(at.X, at.Y)) != [3]int{255, 0, 0} {
			t.Errorf("%s: %v at %v", a, img.At(at.X, at.Y), at)
		}
	}
	// A watermark taller than the output is fitted to its height.
	tall := image.NewNRGBA(image.Rect(0, 0, 10, 1000))
	fill(tall, tall.Bounds(), color.NRGBA{0, 0, 255, 255})
	pt := imaging.NewPipeline(imaging.WithWatermark("tall", tall))
	res, err := pt.Process(bytes.NewReader(encodePNG(t, base)), imaging.Transform{Mode: imaging.ModeFit, Width: 200, Height: 100, Format: imaging.FormatPNG, Watermark: &imaging.Watermark{Name: "tall"}}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	img = decodeOrFail(t, res.Data)
	if rgb8(img.At(199, 0)) != [3]int{0, 0, 255} || rgb8(img.At(199, 99)) != [3]int{0, 0, 255} || rgb8(img.At(150, 50)) != [3]int{255, 255, 255} {
		t.Errorf("tall watermark: %v %v %v", img.At(199, 0), img.At(199, 99), img.At(150, 50))
	}
	// An empty or nil image is a configuration error: Err says so at
	// once, and Process refuses every call, watermarked or not.
	for name, bad := range map[string]image.Image{"empty": image.NewNRGBA(image.Rect(0, 0, 0, 0)), "nil": nil} {
		broken := imaging.NewPipeline(imaging.WithWatermark(name, bad))
		if filesystem.CodeOf(broken.Err()) != filesystem.CodeInvalidArgument {
			t.Errorf("%s watermark: Err = %v", name, broken.Err())
		}
		if _, err := broken.Process(bytes.NewReader(encodePNG(t, base)), imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG}, imaging.Options{}); err != broken.Err() {
			t.Errorf("%s watermark: Process = %v", name, err)
		}
	}
	if p.Err() != nil {
		t.Errorf("a valid pipeline reports %v", p.Err())
	}
	// Unknown name, bad settings, and the digest.
	if _, err := p.Process(bytes.NewReader(encodePNG(t, base)), imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG, Watermark: &imaging.Watermark{Name: "nope"}}, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("unknown watermark = %v", err)
	}
	for _, w := range []imaging.Watermark{{}, {Name: "logo", Anchor: "middle"}, {Name: "logo", Opacity: 2}, {Name: "logo", Scale: -1}, {Name: "logo", Margin: 0.6}} {
		tr := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG, Watermark: &w}
		if filesystem.CodeOf(tr.Validate()) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v accepted", w)
		}
	}
	plain := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG}
	marked := plain
	marked.Watermark = &imaging.Watermark{Name: "logo"}
	if plain.Digest() == marked.Digest() {
		t.Error("the watermark is not in the digest")
	}
}
