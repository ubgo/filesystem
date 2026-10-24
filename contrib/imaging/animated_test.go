package imaging_test

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

var testPalette = color.Palette{color.Transparent, color.NRGBA{255, 0, 0, 255}, color.NRGBA{0, 255, 0, 255}, color.NRGBA{0, 0, 255, 255}}

// frame is a paletted frame of r filled with palette index c.
func frame(r image.Rectangle, c uint8) *image.Paletted {
	p := image.NewPaletted(r, testPalette)
	for i := range p.Pix {
		p.Pix[i] = c
	}
	return p
}

func encodeGIF(t *testing.T, g *gif.GIF) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, g); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// threeFrames is a 40 × 40 animation: red, green, blue.
func threeFrames(t *testing.T) []byte {
	full := image.Rect(0, 0, 40, 40)
	return encodeGIF(t, &gif.GIF{Image: []*image.Paletted{frame(full, 1), frame(full, 2), frame(full, 3)}, Delay: []int{10, 20, 30}, LoopCount: 0})
}

var toGIF = imaging.Transform{Mode: imaging.ModeFit, Width: 20, Height: 20, Format: imaging.FormatGIF}

// TestAnimatedGIFKeepsItsFrames pins IM-19: every frame resized, delays
// and loop kept; FirstFrame and other formats give a still.
func TestAnimatedGIFKeepsItsFrames(t *testing.T) {
	res, err := imaging.Process(bytes.NewReader(threeFrames(t)), toGIF, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := gif.DecodeAll(bytes.NewReader(res.Data))
	if err != nil || len(out.Image) != 3 || res.ContentType != "image/gif" || res.Width != 20 {
		t.Fatalf("frames %d, %+v, %v", len(out.Image), res, err)
	}
	for i, want := range [][3]int{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}} {
		if got := rgb8(out.Image[i].At(10, 10)); got != want || out.Delay[i] != []int{10, 20, 30}[i] || out.Image[i].Bounds().Dx() != 20 {
			t.Errorf("frame %d: %v delay %d", i, got, out.Delay[i])
		}
	}
	still := toGIF
	still.FirstFrame = true
	res, _ = imaging.Process(bytes.NewReader(threeFrames(t)), still, imaging.Options{})
	if out, err := gif.DecodeAll(bytes.NewReader(res.Data)); err != nil || len(out.Image) != 1 {
		t.Errorf("FirstFrame: %v", err)
	}
	if still.Digest() == toGIF.Digest() {
		t.Error("FirstFrame is not in the digest")
	}
	png := imaging.Transform{Mode: imaging.ModeFit, Width: 20, Height: 20, Format: imaging.FormatPNG}
	if res, err := imaging.Process(bytes.NewReader(threeFrames(t)), png, imaging.Options{}); err != nil || res.ContentType != "image/png" {
		t.Errorf("animated to PNG: %v", err)
	}
	// A still input to GIF output, and a crop applied to every frame.
	if res, err := imaging.Process(bytes.NewReader(photo(t, 30, 30, pngEnc)), toGIF, imaging.Options{}); err != nil || res.ContentType != "image/gif" {
		t.Errorf("still to GIF: %v", err)
	}
	cropped := toGIF
	cropped.Crop = image.Rect(0, 0, 20, 10)
	if res, err := imaging.Process(bytes.NewReader(threeFrames(t)), cropped, imaging.Options{}); err != nil || res.Width != 20 || res.Height != 10 {
		t.Errorf("crop = %+v, %v", res, err)
	}
	cropped.Crop = image.Rect(0, 0, 50, 50)
	if _, err := imaging.Process(bytes.NewReader(threeFrames(t)), cropped, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("crop outside = %v", err)
	}
}

// TestAnimatedGIFDisposal pins compositing: a frame drawn after a frame
// disposed to background shows transparency, not the previous frame, and
// DisposalPrevious restores the canvas.
func TestAnimatedGIFDisposal(t *testing.T) {
	full, corner := image.Rect(0, 0, 40, 40), image.Rect(0, 0, 10, 10)
	data := encodeGIF(t, &gif.GIF{
		Image:    []*image.Paletted{frame(full, 1), frame(corner, 3), frame(corner, 2), frame(image.Rect(30, 30, 40, 40), 3)},
		Delay:    []int{5, 5, 5, 5},
		Disposal: []byte{gif.DisposalBackground, gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone},
	})
	same := imaging.Transform{Mode: imaging.ModeFit, Width: 40, Height: 40, Format: imaging.FormatGIF}
	res, err := imaging.Process(bytes.NewReader(data), same, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := gif.DecodeAll(bytes.NewReader(res.Data))
	// Every output frame is whole and disposed to the background, so a
	// viewer never shows the previous frame through transparent pixels.
	for i, d := range out.Disposal {
		if d != gif.DisposalBackground {
			t.Errorf("output frame %d disposal = %d", i, d)
		}
	}
	if _, _, _, a := out.Image[1].At(30, 30).RGBA(); a != 0 {
		t.Errorf("frame 2 kept frame 1's red after a background disposal: %v", out.Image[1].At(30, 30))
	}
	// Frame 3 (green corner) was disposed to previous: frame 4 shows frame
	// 2's blue corner again.
	if got := rgb8(out.Image[3].At(5, 5)); got != [3]int{0, 0, 255} {
		t.Errorf("frame 4 corner = %v, want the restored blue", got)
	}
}

// TestAnimatedGIFLimits pins the pixel-bomb rule over every frame: the
// frame count is read from the structure before decoding, and the canvas
// times the frames must fit MaxPixels.
func TestAnimatedGIFLimits(t *testing.T) {
	data := threeFrames(t)
	for name, o := range map[string]imaging.Options{"frames": {MaxFrames: 2}, "pixels": {MaxPixels: 40*40*3 - 1}} {
		if _, err := imaging.Process(bytes.NewReader(data), toGIF, o); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s limit = %v", name, err)
		}
	}
	// header is a 1 × 1 GIF89a screen with a two-colour global table.
	header := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff")
	with := func(tail ...byte) []byte { return append(append([]byte{}, header...), tail...) }
	for name, bad := range map[string][]byte{
		"no frames":     with(0x3B),
		"cut extension": with(0x21, 0xF9, 0x04, 0x00),
		"cut image":     with(0x2C, 0x00, 0x00),
		"bad version":   append([]byte("GIF8xa"), with(0x3B)[6:]...),
		"short":         []byte("GIF89a\x01"),
		"bad block":     append(append([]byte{}, data[:13]...), 0x99),
		"cut frame":     data[:len(data)-20],
		"cut desc":      append(append([]byte{}, data[:13+3*4]...), 0x2C, 0, 0),
		"bad lzw":       append(append([]byte{}, data[:len(data)-1]...), 0x2C, 0, 0, 0, 0, 1, 0, 1, 0, 0, 2, 2, 0xff, 0xff, 0),
	} {
		if _, err := imaging.Process(bytes.NewReader(bad), toGIF, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s = %v", name, err)
		}
	}
	// A file without its trailer was cut short.
	if _, err := imaging.Process(bytes.NewReader(data[:len(data)-1]), toGIF, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("no trailer = %v", err)
	}
}

// TestAnimatedSmartCropIsPinned pins that GravitySmart chooses its window
// on the first frame and keeps it: the detail moving to the other side in
// frame 2 must not make the crop jump after it.
func TestAnimatedSmartCropIsPinned(t *testing.T) {
	noisy := func(x0 int) *image.Paletted {
		p := image.NewPaletted(image.Rect(0, 0, 120, 40), testPalette)
		for i := range p.Pix {
			p.Pix[i] = 1
		}
		for y := 0; y < 40; y++ {
			for x := x0; x < x0+40; x++ {
				p.Pix[y*120+x] = uint8(1 + (x*7+y*13)%3)
			}
		}
		return p
	}
	data := encodeGIF(t, &gif.GIF{Image: []*image.Paletted{noisy(0), noisy(80)}, Delay: []int{5, 5}})
	tr := imaging.Transform{Mode: imaging.ModeFill, Width: 40, Height: 40, Format: imaging.FormatGIF, Gravity: imaging.GravitySmart}
	res, err := imaging.Process(bytes.NewReader(data), tr, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := gif.DecodeAll(bytes.NewReader(res.Data))
	// Frame 1's crop is its noisy left; frame 2 keeps that window, now
	// flat red, instead of following the noise to the right.
	if colours(out.Image[0]) < 3 || colours(out.Image[1]) != 1 {
		t.Errorf("%d and %d colours: the crop moved between frames", colours(out.Image[0]), colours(out.Image[1]))
	}
}

// colours counts the distinct colours of img.
func colours(img image.Image) int {
	seen := map[color.Color]bool{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			seen[img.At(x, y)] = true
		}
	}
	return len(seen)
}
