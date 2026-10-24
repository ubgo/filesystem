package imaging_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

// thumbHashVectors were produced by the reference JavaScript encoder
// (npm thumbhash 0.1.1, rgbaToThumbHash) for the same pixels, so a match
// means any ThumbHash decoder draws our hashes as the reference's.
var thumbHashVectors = []struct {
	name string
	w, h int
	px   func(x, y int) color.NRGBA
	want string
}{
	{"gradient", 32, 24, func(x, y int) color.NRGBA { return color.NRGBA{uint8(x * 8), uint8(y * 10), 128, 255} }, "nhcKNZpwd4dwiIiHiHiHh3BwCPeI"},
	{"alpha", 20, 30, func(x, y int) color.NRGBA {
		return color.NRGBA{200, uint8((x * 12) % 256), uint8((y * 8) % 256), uint8(((x + y) * 4) % 256)}
	}, "54iGIwgWkLGod4iDf/V2dXN3cIiHeHg="},
	{"checker", 100, 50, func(x, y int) color.NRGBA {
		if ((x>>3)+(y>>3))%2 == 1 {
			return color.NRGBA{250, 250, 250, 255}
		}
		return color.NRGBA{10, 40, 90, 255}
	}, "5eYBBIBmVVUSERFVVQBVAFUFUA=="},
	{"solid", 1, 1, func(int, int) color.NRGBA { return color.NRGBA{255, 0, 0, 255} }, "1fsrB38I9wiIh4hwj3CI+AiIgIAICIgA"},
	{"tall", 10, 100, func(x, y int) color.NRGBA {
		return color.NRGBA{uint8(x * 25), uint8(255 - y*2), uint8((x * y) % 256), 255}
	}, "HqkNSRhgemiIh4d3to8KF4g="},
}

func vectorImage(w, h int, px func(x, y int) color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, px(x, y))
		}
	}
	return img
}

// TestThumbHashMatchesTheReference compares with the reference encoder:
// the header (average colour, scales, aspect ratio, alpha flag) byte for
// byte, and every AC component within one quantisation step. A component
// that is exactly half way between two steps (a symmetric image makes
// many of them zero, which is 7.5 steps) rounds either way depending on
// the last bit of the cosine, and Go's math.Cos and the JavaScript
// engines' differ there; a decoder cannot show a difference of one step
// in fifteen.
func TestThumbHashMatchesTheReference(t *testing.T) {
	for _, v := range thumbHashVectors {
		got := imaging.ThumbHash(vectorImage(v.w, v.h, v.px))
		want, _ := base64.StdEncoding.DecodeString(v.want)
		header := 5
		if want[2]&0x80 != 0 {
			header = 6 // alpha adds a byte
		}
		if len(got) != len(want) || !bytes.Equal(got[:header], want[:header]) {
			t.Errorf("%s: %x, reference %x", v.name, got, want)
			continue
		}
		for i := header; i < len(want); i++ {
			for _, shift := range []uint{0, 4} {
				g, w := int(got[i]>>shift&0xf), int(want[i]>>shift&0xf)
				if g-w > 1 || w-g > 1 {
					t.Errorf("%s: component at byte %d is %d, reference %d", v.name, i, g, w)
				}
			}
		}
	}
}

// TestThumbHashScalesLargeImages pins that a large image is hashed from a
// copy fitting 100 × 100, keeping its aspect ratio in the header.
func TestThumbHashScalesLargeImages(t *testing.T) {
	big := vectorImage(400, 200, func(x, y int) color.NRGBA { return color.NRGBA{uint8(x), uint8(y), 50, 255} })
	h := imaging.ThumbHash(big)
	if len(h) < 5 || h[4]&0x80 == 0 {
		t.Fatalf("hash %x: not marked landscape", h)
	}
	// A sub-image whose bounds do not start at 0,0 hashes like a copy.
	sub := vectorImage(60, 60, func(x, y int) color.NRGBA { return color.NRGBA{uint8(x * 4), 0, uint8(y * 4), 255} })
	inner := sub.SubImage(image.Rect(10, 10, 50, 50))
	copyOf := vectorImage(40, 40, func(x, y int) color.NRGBA { return color.NRGBA{uint8((x + 10) * 4), 0, uint8((y + 10) * 4), 255} })
	if !bytes.Equal(imaging.ThumbHash(inner), imaging.ThumbHash(copyOf)) {
		t.Error("a sub-image hashes differently from its copy")
	}
}

func TestThumbHashOf(t *testing.T) {
	data := photo(t, 300, 150, pngEnc)
	h, err := imaging.ThumbHashOf(imaging.Default(), bytes.NewReader(data), imaging.Options{})
	if err != nil || len(h) < 5 || h[4]&0x80 == 0 {
		t.Fatalf("ThumbHashOf = %x, %v", h, err)
	}
	if _, err := imaging.ThumbHashOf(imaging.Default(), strings.NewReader("not an image"), imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("not an image = %v", err)
	}
	// A processor that returns bytes that are not a PNG.
	if _, err := imaging.ThumbHashOf(garbageProcessor{}, bytes.NewReader(data), imaging.Options{}); err == nil {
		t.Error("a broken processor's output was hashed")
	}
}

type garbageProcessor struct{ imaging.Processor }

func (garbageProcessor) Process(_ io.Reader, _ imaging.Transform, _ imaging.Options) (imaging.Result, error) {
	return imaging.Result{Data: []byte("garbage")}, nil
}
