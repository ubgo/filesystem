package imaging_test

import (
	"bytes"
	"flag"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/imagingtest"
)

// -update rewrites testdata/*.golden.png from the current output.
var _ = flag.Bool("update", false, "rewrite golden images")

// scene is a 300 × 120 test picture: a grey background with a noisy,
// detailed block on the right and a flat red block on the left, so the
// smart, focal and watermarked crops each land somewhere recognisable.
func scene(t *testing.T) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 120))
	fill(img, img.Bounds(), color.NRGBA{128, 128, 128, 255})
	fill(img, image.Rect(20, 30, 80, 90), color.NRGBA{200, 40, 40, 255})
	rng := rand.New(rand.NewPCG(5, 6))
	for y := 20; y < 100; y++ {
		for x := 210; x < 290; x++ {
			v := uint8(rng.IntN(256))
			img.SetNRGBA(x, y, color.NRGBA{v, uint8(255 - int(v)), 90, 255})
		}
	}
	return encodePNG(t, img)
}

// TestGoldenTransforms pins the look of IMAGING-ADV's transforms with
// perceptual goldens (imagingtest): a wrong crop or a moved watermark
// fails, an encoder rounding a level differently does not.
func TestGoldenTransforms(t *testing.T) {
	logo := image.NewNRGBA(image.Rect(0, 0, 30, 10))
	fill(logo, logo.Bounds(), color.NRGBA{255, 255, 255, 255})
	p := imaging.NewPipeline(imaging.WithWatermark("logo", logo))
	base := imaging.Transform{Mode: imaging.ModeFill, Width: 120, Height: 120, Format: imaging.FormatPNG}
	cases := map[string]func(imaging.Transform) imaging.Transform{
		"smart": func(tr imaging.Transform) imaging.Transform { tr.Gravity = imaging.GravitySmart; return tr },
		"focal": func(tr imaging.Transform) imaging.Transform {
			tr.Gravity, tr.Focal = imaging.GravityFocal, &imaging.Focal{X: 0.15, Y: 0.5}
			return tr
		},
		"watermark": func(tr imaging.Transform) imaging.Transform {
			tr.Watermark = &imaging.Watermark{Name: "logo", Anchor: imaging.AnchorTopLeft, Opacity: 0.8, Margin: 0.05}
			return tr
		},
	}
	for name, mk := range cases {
		res, err := p.Process(bytes.NewReader(scene(t)), mk(base), imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := png.Decode(bytes.NewReader(res.Data))
		if err != nil {
			t.Fatal(err)
		}
		imagingtest.Golden(t, got, "testdata/"+name+".golden.png")
	}
}
