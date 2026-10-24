package imaging_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/upload"
)

// rawMagic starts the toy format the tests register: "RAW1", a width and
// height byte, then one grey byte per pixel.
const rawMagic = "RAW1"

// rawDecoder decodes the toy format, standing in for a codec module.
type rawDecoder struct{}

func (rawDecoder) Name() string           { return "raw" }
func (rawDecoder) Match(head []byte) bool { return bytes.HasPrefix(head, []byte(rawMagic)) }
func (rawDecoder) DecodeConfig(r io.Reader) (image.Config, error) {
	head := make([]byte, len(rawMagic)+2)
	if _, err := io.ReadFull(r, head); err != nil {
		return image.Config{}, err
	}
	return image.Config{ColorModel: color.GrayModel, Width: int(head[4]), Height: int(head[5])}, nil
}
func (rawDecoder) Decode(r io.Reader) (image.Image, error) {
	data, err := io.ReadAll(r)
	if err != nil || len(data) < 6 {
		return nil, errors.New("short raw image")
	}
	w, h := int(data[4]), int(data[5])
	if len(data) != 6+w*h {
		return nil, errors.New("raw image size mismatch")
	}
	img := image.NewGray(image.Rect(0, 0, w, h))
	copy(img.Pix, data[6:])
	return img, nil
}

func rawImage(w, h int, grey byte) []byte {
	return append([]byte{'R', 'A', 'W', '1', byte(w), byte(h)}, bytes.Repeat([]byte{grey}, w*h)...)
}

// webpStandIn is an Encoder for FormatWebP that writes PNG bytes, so the
// test can check which encoder ran without a real WebP encoder.
type webpStandIn struct{ called *int }

func (webpStandIn) Format() imaging.Format { return imaging.FormatWebP }
func (e webpStandIn) Encode(w io.Writer, img image.Image, _ int) error {
	*e.called++
	return png.Encode(w, img)
}

type failingEncoder struct{}

func (failingEncoder) Format() imaging.Format                   { return imaging.FormatPNG }
func (failingEncoder) Encode(io.Writer, image.Image, int) error { return errors.New("encoder broke") }

func TestPipelineCodecs(t *testing.T) {
	webp := imaging.Transform{Mode: imaging.ModeFit, Width: 4, Height: 4, Format: imaging.FormatWebP}
	// The default Pipeline has no WebP encoder and no raw decoder.
	if _, err := imaging.Process(bytes.NewReader(photo(t, 8, 8, pngEnc)), webp, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("WebP on the default pipeline = %v", err)
	}
	if imaging.Default().Encodes(imaging.FormatWebP) || !imaging.Default().Encodes(imaging.FormatJPEG) {
		t.Error("Encodes on the default pipeline")
	}
	if _, err := imaging.Probe(bytes.NewReader(rawImage(3, 2, 7))); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("raw on the default pipeline = %v", err)
	}

	called := 0
	p := imaging.NewPipeline(imaging.WithDecoder(rawDecoder{}), imaging.WithEncoder(webpStandIn{called: &called}))
	res, err := p.Process(bytes.NewReader(rawImage(8, 8, 200)), webp, imaging.Options{})
	if err != nil || called != 1 || res.ContentType != "image/webp" || res.Width != 4 {
		t.Fatalf("Process = %+v, %v (encoder calls %d)", res, err, called)
	}
	info, err := p.Probe(bytes.NewReader(rawImage(3, 2, 7)))
	if err != nil || info != (imaging.Info{Format: "raw", Width: 3, Height: 2}) {
		t.Errorf("Probe = %+v, %v", info, err)
	}
	// The built-ins still work alongside.
	if _, err := p.Process(bytes.NewReader(photo(t, 8, 8, pngEnc)), imaging.Transform{Mode: imaging.ModeFit, Width: 4, Height: 4, Format: imaging.FormatJPEG}, imaging.Options{}); err != nil {
		t.Errorf("JPEG on an extended pipeline = %v", err)
	}
	// Header and body errors of an added decoder are CodeRejected.
	for _, bad := range [][]byte{[]byte("RAW1"), append(rawImage(2, 2, 1), 9)} {
		if _, err := p.Process(bytes.NewReader(bad), webp, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("bad raw %q = %v", bad, err)
		}
	}
	if _, err := p.Probe(bytes.NewReader([]byte("RAW1"))); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("Probe of a short raw header = %v", err)
	}
	if _, err := p.Probe(failingReader{}); err == nil {
		t.Error("Probe ignored a read error")
	}
}

// TestAddedDecoderTakesPriority pins that a codec module can replace a
// built-in decoder for its format.
func TestAddedDecoderTakesPriority(t *testing.T) {
	var seen []string
	p := imaging.NewPipeline(imaging.WithDecoder(spyPNG{seen: &seen}))
	if _, err := p.Probe(bytes.NewReader(photo(t, 2, 2, pngEnc))); err != nil || len(seen) != 1 {
		t.Errorf("Probe = %v, spy saw %v", err, seen)
	}
}

type spyPNG struct{ seen *[]string }

func (spyPNG) Name() string           { return "png" }
func (spyPNG) Match(head []byte) bool { return bytes.HasPrefix(head, []byte("\x89PNG")) }
func (s spyPNG) DecodeConfig(r io.Reader) (image.Config, error) {
	*s.seen = append(*s.seen, "config")
	return png.DecodeConfig(r)
}
func (spyPNG) Decode(r io.Reader) (image.Image, error) { return png.Decode(r) }

func TestEncoderFailure(t *testing.T) {
	p := imaging.NewPipeline(imaging.WithEncoder(failingEncoder{}))
	_, err := p.Process(bytes.NewReader(photo(t, 4, 4, pngEnc)), imaging.Transform{Mode: imaging.ModeFit, Width: 2, Height: 2, Format: imaging.FormatPNG}, imaging.Options{})
	if err == nil || !strings.Contains(err.Error(), "encoder broke") {
		t.Errorf("Process = %v", err)
	}
}

// TestPipelineIsAProcessor pins that a Processor value can stand in for
// the package functions.
func TestPipelineIsAProcessor(t *testing.T) {
	var p imaging.Processor = imaging.Default()
	if _, err := p.Probe(bytes.NewReader(photo(t, 2, 2, pngEnc))); err != nil {
		t.Error(err)
	}
}

// TestSanitizeWithPipeline pins that Sanitize decodes with the Pipeline
// it is given, so a codec module's formats are sanitized too.
func TestSanitizeWithPipeline(t *testing.T) {
	var seen []string
	proc := imaging.Sanitize(imaging.Options{}, imaging.WithPipeline(imaging.NewPipeline(imaging.WithDecoder(spyPNG{seen: &seen}))))
	if _, _, err := proc(t.Context(), upload.Info{Filename: "a.png", ContentType: "image/png"}, bytes.NewReader(photo(t, 4, 4, pngEnc))); err != nil || len(seen) == 0 {
		t.Errorf("Sanitize = %v; the given pipeline was not used (%v)", err, seen)
	}
}

// TestSanitizeTurnsCameraFormatsIntoJPEG pins that a photo in a lossy
// camera format (HEIC here, decoded by an added decoder) becomes a JPEG
// named .jpg, not a PNG several times its size.
func TestSanitizeTurnsCameraFormatsIntoJPEG(t *testing.T) {
	proc := imaging.Sanitize(imaging.Options{}, imaging.WithPipeline(imaging.NewPipeline(imaging.WithDecoder(rawDecoder{}))))
	r, info, err := proc(t.Context(), upload.Info{Filename: "IMG_0001.HEIC", ContentType: "image/heic"}, bytes.NewReader(rawImage(4, 4, 90)))
	if err != nil || info.ContentType != "image/jpeg" || info.Filename != "IMG_0001.jpg" {
		t.Fatalf("Sanitize = %+v, %v", info, err)
	}
	out, _ := io.ReadAll(r)
	if !bytes.HasPrefix(out, []byte("\xff\xd8")) {
		t.Error("output is not a JPEG")
	}
}
