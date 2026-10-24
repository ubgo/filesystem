package imaging_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func process(t *testing.T, data []byte, tr imaging.Transform) image.Image {
	t.Helper()
	res, err := imaging.Process(bytes.NewReader(data), tr, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// spread is the luma standard deviation of img: high for detail, near 0
// for a flat area.
func spread(img image.Image) float64 {
	b := img.Bounds()
	var sum, sq float64
	n := float64(b.Dx() * b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			l := (float64(r) + float64(g) + float64(bl)) / 3 / 257
			sum += l
			sq += l * l
		}
	}
	mean := sum / n
	return sq/n - mean*mean
}

var square = imaging.Transform{Mode: imaging.ModeFill, Width: 100, Height: 100, Format: imaging.FormatPNG}

// TestSmartCropFindsDetail pins IM-16 on fixtures: the crop of a wide,
// mostly flat image keeps its detailed corner, and a skin-toned patch
// beats flat background, where the centre crop shows neither.
func TestSmartCropFindsDetail(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	fill(img, img.Bounds(), color.NRGBA{128, 128, 128, 255})
	rng := rand.New(rand.NewPCG(3, 4))
	for y := 20; y < 80; y++ {
		for x := 220; x < 280; x++ {
			v := uint8(rng.IntN(256))
			img.SetNRGBA(x, y, color.NRGBA{v, v, v, 255})
		}
	}
	data := encodePNG(t, img)
	smart := square
	smart.Gravity = imaging.GravitySmart
	if s, c := spread(process(t, data, smart)), spread(process(t, data, square)); s < 100 || c > 1 {
		t.Errorf("smart crop spread %.1f (want the detail), centre %.1f", s, c)
	}

	// A skin-toned patch and a grey one of the same brightness and size,
	// so their edges score alike: skin decides.
	skin := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	fill(skin, skin.Bounds(), color.NRGBA{40, 40, 40, 255})
	// Skin on the right: a tie would go to the left (the first window).
	fill(skin, image.Rect(10, 20, 70, 80), color.NRGBA{181, 181, 181, 255})
	fill(skin, image.Rect(230, 20, 290, 80), color.NRGBA{224, 172, 138, 255})
	out := process(t, encodePNG(t, skin), smart)
	if c := rgb8(out.At(50, 50)); c[0]-c[2] < 50 {
		t.Errorf("smart crop missed the skin-toned area: centre pixel %v", out.At(50, 50))
	}
	// A saturated block beats grey background too.
	bold := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	fill(bold, bold.Bounds(), color.NRGBA{120, 120, 120, 255})
	fill(bold, image.Rect(230, 20, 290, 80), color.NRGBA{40, 90, 230, 255})
	if got := dominantChannel(process(t, encodePNG(t, bold), smart).At(50, 50)); got != 2 {
		t.Errorf("smart crop missed the saturated block: %v", process(t, encodePNG(t, bold), smart).At(50, 50))
	}
	// A featureless image is cropped from the centre, deterministically.
	flat := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	fill(flat, flat.Bounds(), color.NRGBA{90, 90, 90, 255})
	fill(flat, image.Rect(100, 0, 200, 100), color.NRGBA{91, 90, 90, 255})
	a, b := process(t, encodePNG(t, flat), smart), process(t, encodePNG(t, flat), square)
	if a.At(50, 50) != b.At(50, 50) {
		t.Errorf("flat image: smart %v, centre %v", a.At(50, 50), b.At(50, 50))
	}
}

// thirds is 300 × 100: red, green and blue thirds.
func thirds(t *testing.T) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	fill(img, image.Rect(0, 0, 100, 100), color.NRGBA{255, 0, 0, 255})
	fill(img, image.Rect(100, 0, 200, 100), color.NRGBA{0, 255, 0, 255})
	fill(img, image.Rect(200, 0, 300, 100), color.NRGBA{0, 0, 255, 255})
	return encodePNG(t, img)
}

func dominantChannel(c color.Color) int {
	r, g, b, _ := c.RGBA()
	switch {
	case r > g && r > b:
		return 0
	case g > b:
		return 1
	}
	return 2
}

// TestFocalCrop pins IM-17: the crop centres on the focal point, clamped
// to the image, and without a point GravityFocal is the centre.
func TestFocalCrop(t *testing.T) {
	data := thirds(t)
	focal := square
	focal.Gravity = imaging.GravityFocal
	for _, c := range []struct {
		f    *imaging.Focal
		want int
	}{{&imaging.Focal{X: 0.85, Y: 0.5}, 2}, {&imaging.Focal{X: 0, Y: 0}, 0}, {&imaging.Focal{X: 1, Y: 1}, 2}, {nil, 1}} {
		tr := focal
		tr.Focal = c.f
		out := process(t, data, tr)
		// Every column of the crop is inside the image: an edge point is
		// clamped, never padded with nothing.
		for _, x := range []int{2, 50, 97} {
			if got := dominantChannel(out.At(x, 50)); got != c.want {
				t.Errorf("focal %v: channel %d at x=%d, want %d", c.f, got, x, c.want)
			}
		}
	}
	a, b := focal, focal
	a.Focal, b.Focal = &imaging.Focal{X: 0.2, Y: 0.5}, &imaging.Focal{X: 0.8, Y: 0.5}
	if a.Digest() == b.Digest() || a.Digest() == focal.Digest() {
		t.Error("the focal point is not in the digest")
	}
	for _, bad := range []imaging.Focal{{X: -0.1}, {Y: 1.1}} {
		tr := focal
		tr.Focal = &bad
		if filesystem.CodeOf(tr.Validate()) != filesystem.CodeInvalidArgument {
			t.Errorf("focal %v accepted", bad)
		}
	}
}
