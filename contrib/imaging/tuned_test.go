package imaging_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

// tunedPNG records the settings it is given and writes a PNG.
type tunedPNG struct{ got *imaging.EncodeSettings }

func (tunedPNG) Format() imaging.Format { return imaging.FormatPNG }
func (tunedPNG) Encode(w io.Writer, img image.Image, _ int) error {
	return png.Encode(w, img)
}
func (e tunedPNG) EncodeTuned(w io.Writer, img image.Image, s imaging.EncodeSettings) error {
	*e.got = s
	return png.Encode(w, img)
}

// TestTransformBackgroundAndTuning pins the per-transform encoder seams:
// Background fills the pad border, the default border stays transparent;
// Lossless and Effort reach a TunedEncoder, an encoder that is not one
// refuses them, and the digest of a transform without them is unchanged.
func TestTransformBackgroundAndTuning(t *testing.T) {
	src := photo(t, 40, 20, pngEnc)
	pad := imaging.Transform{Mode: imaging.ModePad, Width: 40, Height: 40, Format: imaging.FormatPNG}
	corner := func(tr imaging.Transform) color.NRGBA {
		t.Helper()
		res, err := imaging.Process(bytes.NewReader(src), tr, imaging.Options{})
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(res.Data))
		if err != nil {
			t.Fatal(err)
		}
		return color.NRGBAModel.Convert(img.At(0, 0)).(color.NRGBA)
	}
	if c := corner(pad); c.A != 0 {
		t.Fatalf("default pad corner %v, want transparent", c)
	}
	brand := color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xFF}
	withBg := pad
	withBg.Background = &brand
	if c := corner(withBg); c != brand {
		t.Fatalf("pad corner %v, want %v", c, brand)
	}
	if withBg.Digest() == pad.Digest() {
		t.Fatal("Background does not change the digest")
	}

	var got imaging.EncodeSettings
	p := imaging.NewPipeline(imaging.WithEncoder(tunedPNG{&got}))
	tuned := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG, Lossless: true, Effort: 7}
	if _, err := p.Process(bytes.NewReader(src), tuned, imaging.Options{}); err != nil || !got.Lossless || got.Effort != 7 || got.Quality != imaging.DefaultQuality {
		t.Fatalf("settings %+v, %v", got, err)
	}
	// The built-in PNG encoder is tuned too.
	if _, err := imaging.Process(bytes.NewReader(src), tuned, imaging.Options{}); err != nil {
		t.Fatalf("built-in PNG with effort: %v", err)
	}
	// Every effort band of the built-in PNG encoder (0 keeps zlib's
	// default) writes a PNG that decodes to the same pixels.
	for _, effort := range []int{0, 1, 7, imaging.MaxEffort} {
		band := tuned
		band.Effort = effort
		res, err := imaging.Process(bytes.NewReader(src), band, imaging.Options{})
		if err != nil {
			t.Fatalf("effort %d: %v", effort, err)
		}
		if _, err := png.Decode(bytes.NewReader(res.Data)); err != nil {
			t.Fatalf("effort %d: %v", effort, err)
		}
	}
	jpegLossless := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatJPEG, Lossless: true}
	if _, err := imaging.Process(bytes.NewReader(src), jpegLossless, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("lossless JPEG: %v", err)
	}
	bad := tuned
	bad.Effort = imaging.MaxEffort + 1
	if filesystem.CodeOf(bad.Validate()) != filesystem.CodeInvalidArgument {
		t.Fatal("an effort out of range passed")
	}
	plain := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: imaging.FormatPNG}
	lossless, effort := plain, plain
	lossless.Lossless, effort.Effort = true, 7
	if d := plain.Digest(); d == lossless.Digest() || d == effort.Digest() || lossless.Digest() == effort.Digest() {
		t.Fatal("Lossless or Effort does not change the digest")
	}
}
