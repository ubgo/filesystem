package imaging

import (
	"image"
	"testing"
)

// TestWatermarkIsScaledOnce pins the reuse of the scaled copy: drawing on
// frames of one size (an animation) scales the watermark once, and a new
// size scales it again.
func TestWatermarkIsScaledOnce(t *testing.T) {
	m := &placedMark{img: image.NewNRGBA(image.Rect(0, 0, 40, 20)), w: Watermark{Name: "logo"}}
	m.drawOn(image.NewRGBA(image.Rect(0, 0, 200, 100)))
	first := m.scaled
	m.drawOn(image.NewRGBA(image.Rect(0, 0, 200, 100)))
	if m.scaled != first {
		t.Error("the watermark was scaled again for a frame of the same size")
	}
	m.drawOn(image.NewRGBA(image.Rect(0, 0, 100, 50)))
	if m.scaled == first || m.scaled.Bounds().Dx() != 25 {
		t.Errorf("a new size reused the old copy: %v", m.scaled.Bounds())
	}
}
