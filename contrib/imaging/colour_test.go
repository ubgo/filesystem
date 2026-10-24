package imaging_test

import (
	"image"
	"image/color"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
)

func fill(img *image.NRGBA, r image.Rectangle, c color.NRGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
}

// TestDominantColour pins IM-15 on a fixture: 70% teal, 30% orange gives
// teal, where the mean would be a muddy blend; transparent pixels never
// count.
func TestDominantColour(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 200, 100))
	teal, orange := color.NRGBA{0, 128, 128, 255}, color.NRGBA{255, 140, 0, 255}
	fill(img, image.Rect(0, 0, 140, 100), teal)
	fill(img, image.Rect(140, 0, 200, 100), orange)
	c, ok := imaging.DominantColour(img)
	if !ok || c != teal || imaging.Hex(c) != "#008080" {
		t.Fatalf("dominant = %v %s, %v", c, imaging.Hex(c), ok)
	}
	// Twice the same answer.
	if again, _ := imaging.DominantColour(img); again != c {
		t.Error("not deterministic")
	}
	// A logo on a transparent background: the logo's colour.
	logo := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	fill(logo, image.Rect(40, 40, 60, 60), orange)
	if c, ok := imaging.DominantColour(logo); !ok || c != orange {
		t.Errorf("logo = %v, %v", c, ok)
	}
	if _, ok := imaging.DominantColour(image.NewNRGBA(image.Rect(0, 0, 10, 10))); ok {
		t.Error("a fully transparent image has a dominant colour")
	}
	// Equal buckets: the lower one, whatever the map's order.
	split := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	fill(split, image.Rect(0, 0, 32, 64), color.NRGBA{255, 0, 0, 255})
	fill(split, image.Rect(32, 0, 64, 64), color.NRGBA{0, 0, 255, 255})
	for range 20 {
		if c, _ := imaging.DominantColour(split); c != (color.NRGBA{0, 0, 255, 255}) {
			t.Fatalf("tie = %v", c)
		}
	}
}
