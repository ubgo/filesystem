package imaging_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

func photo(t *testing.T, w, h int, encode func(*bytes.Buffer, image.Image) error) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngEnc(b *bytes.Buffer, img image.Image) error  { return png.Encode(b, img) }
func jpegEnc(b *bytes.Buffer, img image.Image) error { return jpeg.Encode(b, img, nil) }
func gifEnc(b *bytes.Buffer, img image.Image) error  { return gif.Encode(b, img, nil) }

func size(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Width, cfg.Height
}

func TestProcessGeometry(t *testing.T) {
	src := photo(t, 400, 200, pngEnc)
	cases := []struct {
		tr   imaging.Transform
		w, h int
	}{
		{imaging.Transform{Mode: imaging.ModeFit, Width: 100, Height: 100, Format: imaging.FormatJPEG}, 100, 50},
		{imaging.Transform{Mode: imaging.ModeFit, Width: 1000, Height: 1000, Format: imaging.FormatPNG}, 400, 200},
		{imaging.Transform{Mode: imaging.ModeFill, Width: 100, Height: 100, Format: imaging.FormatJPEG, Quality: 60}, 100, 100},
		{imaging.Transform{Mode: imaging.ModeFill, Width: 800, Height: 100, Format: imaging.FormatPNG}, 800, 100},
	}
	for _, c := range cases {
		res, err := imaging.Process(bytes.NewReader(src), c.tr, imaging.Options{})
		if err != nil {
			t.Fatalf("%+v: %v", c.tr, err)
		}
		w, h := size(t, res.Data)
		if w != c.w || h != c.h || res.Width != c.w || res.Height != c.h || res.ContentType != c.tr.ContentType() {
			t.Errorf("%+v => %dx%d (%dx%d) %s, want %dx%d", c.tr, w, h, res.Width, res.Height, res.ContentType, c.w, c.h)
		}
	}
}

func TestProcessInputs(t *testing.T) {
	fit := imaging.Transform{Mode: imaging.ModeFit, Width: 50, Height: 50, Format: imaging.FormatJPEG}
	for name, data := range map[string][]byte{
		"jpeg": photo(t, 60, 40, jpegEnc),
		"gif":  photo(t, 60, 40, gifEnc),
		"png":  photo(t, 60, 40, pngEnc),
	} {
		if _, err := imaging.Process(bytes.NewReader(data), fit, imaging.Options{}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A transparent PNG becomes a JPEG on white, not black.
	clear := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	_ = png.Encode(&buf, clear)
	res, err := imaging.Process(&buf, imaging.Transform{Mode: imaging.ModeFit, Width: 4, Height: 4, Format: imaging.FormatJPEG}, imaging.Options{})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := jpeg.Decode(bytes.NewReader(res.Data))
	if r, _, _, _ := img.At(1, 1).RGBA(); r>>8 < 240 {
		t.Errorf("transparency flattened to %v, want white", img.At(1, 1))
	}
}

// bombPNG is a PNG whose valid header claims w×h pixels and that has no
// image data: decoding it would allocate w×h×4 bytes.
func bombPNG(w, h uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	chunk := append([]byte("IHDR"), ihdr...)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)))
	out = append(out, chunk...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(chunk))
}

func TestProcessRefusals(t *testing.T) {
	fit := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG}
	cases := []struct {
		name string
		data []byte
		opts imaging.Options
		code filesystem.Code
	}{
		{"pixel bomb", bombPNG(100000, 100000), imaging.Options{}, filesystem.CodeRejected},
		{"over a custom pixel limit", photo(t, 100, 100, pngEnc), imaging.Options{MaxPixels: 9999}, filesystem.CodeRejected},
		{"too many bytes", photo(t, 100, 100, pngEnc), imaging.Options{MaxBytes: 10}, filesystem.CodeTooLarge},
		{"not an image", []byte("<svg><script/></svg>"), imaging.Options{}, filesystem.CodeRejected},
		{"bad header", []byte("\x89PNG\r\n\x1a\nbroken"), imaging.Options{}, filesystem.CodeRejected},
		{"corrupt body", append(bombPNG(10, 10), []byte("garbage")...), imaging.Options{}, filesystem.CodeRejected},
	}
	for _, c := range cases {
		_, err := imaging.Process(bytes.NewReader(c.data), fit, c.opts)
		if filesystem.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	if _, err := imaging.Process(failingReader{}, fit, imaging.Options{}); err == nil {
		t.Error("read error ignored")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, filesystem.ErrUnavailable }

func TestTransform(t *testing.T) {
	bad := []imaging.Transform{
		{Mode: "stretch", Width: 1, Height: 1, Format: imaging.FormatPNG},
		{Mode: imaging.ModeFit, Width: 1, Height: 1, Format: "tiff"},
		{Mode: imaging.ModeFit, Width: 0, Height: 1, Format: imaging.FormatPNG},
		{Mode: imaging.ModeFit, Width: 1, Height: imaging.MaxDimension + 1, Format: imaging.FormatPNG},
		{Mode: imaging.ModeFit, Width: 1, Height: 1, Format: imaging.FormatJPEG, Quality: 101},
	}
	for _, tr := range bad {
		if filesystem.CodeOf(tr.Validate()) != filesystem.CodeInvalidArgument {
			t.Errorf("Validate(%+v) accepted", tr)
		}
		if _, err := imaging.Process(strings.NewReader(""), tr, imaging.Options{}); err == nil {
			t.Errorf("Process(%+v) accepted", tr)
		}
	}
	a := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatJPEG}
	b := a
	b.Quality = imaging.DefaultQuality
	if a.Digest() != b.Digest() {
		t.Error("default quality changes the digest")
	}
	c := a
	c.Width = 11
	if a.Digest() == c.Digest() {
		t.Error("different sizes share a digest")
	}
	p1 := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG, Quality: 5}
	p2 := p1
	p2.Quality = 90
	if p1.Digest() != p2.Digest() || p1.Extension() != ".png" || a.Extension() != ".jpg" {
		t.Error("PNG digest depends on quality, or wrong extension")
	}
	if len(imaging.ModeValues) != 3 || len(imaging.FormatValues) != 5 || len(imaging.GravityValues) != 7 {
		t.Error("value lists incomplete")
	}
	// A transform without the newer fields keeps the digest it always had,
	// so cached variants are not made again.
	old := sha256.Sum256([]byte("v1|fit|10x10|jpeg|q82"))
	if a.Digest() != hex.EncodeToString(old[:8]) {
		t.Error("the digest of an existing transform changed")
	}
	g, cr := a, a
	g.Gravity = imaging.GravityTop
	cr.Crop = image.Rect(0, 0, 5, 5)
	if g.Digest() == a.Digest() || cr.Digest() == a.Digest() || g.Digest() == cr.Digest() {
		t.Error("gravity or crop does not change the digest")
	}
	for _, bad := range []imaging.Transform{
		{Mode: imaging.ModeFill, Width: 1, Height: 1, Format: imaging.FormatPNG, Gravity: "north"},
		{Mode: imaging.ModeFill, Width: 1, Height: 1, Format: imaging.FormatPNG, Crop: image.Rect(-1, 0, 5, 5)},
		{Mode: imaging.ModeFill, Width: 1, Height: 1, Format: imaging.FormatPNG, Crop: image.Rect(3, 3, 3, 9)},
	} {
		if filesystem.CodeOf(bad.Validate()) != filesystem.CodeInvalidArgument {
			t.Errorf("Validate(%+v) accepted", bad)
		}
	}
}

// pixel decodes a PNG output and returns its colour at x, y.
func pixel(t *testing.T, data []byte, x, y int) color.NRGBA {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
}

// halves is a w×h PNG whose top half is red and bottom half blue.
func halves(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.NRGBA{255, 0, 0, 255}
			if y >= h/2 {
				c = color.NRGBA{0, 0, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestPadGravityCrop(t *testing.T) {
	src := halves(t, 40, 80) // portrait: red above blue
	run := func(tr imaging.Transform) imaging.Result {
		t.Helper()
		res, err := imaging.Process(bytes.NewReader(src), tr, imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	pad := run(imaging.Transform{Mode: imaging.ModePad, Width: 40, Height: 40, Format: imaging.FormatPNG})
	if pad.Width != 40 || pad.Height != 40 || pixel(t, pad.Data, 0, 20).A != 0 || pixel(t, pad.Data, 20, 5).R < 200 {
		t.Fatalf("pad = %dx%d, edge %v, centre %v", pad.Width, pad.Height, pixel(t, pad.Data, 0, 20), pixel(t, pad.Data, 20, 5))
	}
	top := run(imaging.Transform{Mode: imaging.ModeFill, Width: 20, Height: 20, Format: imaging.FormatPNG, Gravity: imaging.GravityTop})
	bottom := run(imaging.Transform{Mode: imaging.ModeFill, Width: 20, Height: 20, Format: imaging.FormatPNG, Gravity: imaging.GravityBottom})
	if pixel(t, top.Data, 10, 10).R < 200 || pixel(t, bottom.Data, 10, 10).B < 200 {
		t.Fatalf("top keeps %v, bottom keeps %v", pixel(t, top.Data, 10, 10), pixel(t, bottom.Data, 10, 10))
	}
	// A landscape image, red on the left and blue on the right.
	wide := image.NewNRGBA(image.Rect(0, 0, 80, 20))
	for y := range 20 {
		for x := range 80 {
			c := color.NRGBA{255, 0, 0, 255}
			if x >= 40 {
				c = color.NRGBA{0, 0, 255, 255}
			}
			wide.Set(x, y, c)
		}
	}
	var wideBuf bytes.Buffer
	_ = png.Encode(&wideBuf, wide)
	for g, red := range map[imaging.Gravity]bool{imaging.GravityLeft: true, imaging.GravityRight: false} {
		res, err := imaging.Process(bytes.NewReader(wideBuf.Bytes()), imaging.Transform{Mode: imaging.ModeFill, Width: 20, Height: 20, Format: imaging.FormatPNG, Gravity: g}, imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if p := pixel(t, res.Data, 10, 10); (p.R > 200) != red {
			t.Fatalf("%s keeps %v", g, p)
		}
	}
	crop := run(imaging.Transform{Mode: imaging.ModeFit, Width: 100, Height: 100, Format: imaging.FormatPNG, Crop: image.Rect(0, 50, 40, 80)})
	if crop.Width != 40 || crop.Height != 30 || pixel(t, crop.Data, 20, 15).B < 200 {
		t.Fatalf("crop = %dx%d, %v", crop.Width, crop.Height, pixel(t, crop.Data, 20, 15))
	}
	if _, err := imaging.Process(bytes.NewReader(src), imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG, Crop: image.Rect(0, 0, 41, 10)}, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Fatalf("a crop outside the image = %v", err)
	}
}
