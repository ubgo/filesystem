package heic_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/heic"
	"github.com/ubgo/filesystem/upload"
)

// quadrants.heic is a 64 × 48 picture of four flat quadrants (the colours
// below), saved as HEIC at quality 80 by macOS (sips) from a PNG this
// test package's pattern drew: no HEIC encoder runs in Go to make one
// in-test.
var quadrants = []color.NRGBA{{220, 30, 30, 255}, {30, 200, 40, 255}, {20, 40, 210, 255}, {240, 230, 30, 255}}

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/quadrants.heic")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// near checks each quadrant's centre against its colour within tol.
func near(t *testing.T, img image.Image, scale float64, tol int) {
	t.Helper()
	for i, p := range []image.Point{{16, 12}, {48, 12}, {16, 36}, {48, 36}} {
		x, y := int(float64(p.X)*scale), int(float64(p.Y)*scale)
		r, g, b, _ := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
		want := quadrants[i]
		for _, d := range []int{int(r>>8) - int(want.R), int(g>>8) - int(want.G), int(b>>8) - int(want.B)} {
			if d > tol || -d > tol {
				t.Errorf("quadrant %d at (%d,%d) is %d,%d,%d, want %v ± %d", i, x, y, r>>8, g>>8, b>>8, want, tol)
				break
			}
		}
	}
}

var pipeline = imaging.NewPipeline(imaging.WithDecoder(heic.Decoder()))

func TestProbeAndDecode(t *testing.T) {
	data := fixture(t)
	info, err := pipeline.Probe(bytes.NewReader(data))
	if err != nil || info != (imaging.Info{Format: "heic", Width: 64, Height: 48}) {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	img, err := heic.Decoder().Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// Lossy HEVC with 4:2:0 chroma: near the source colours, not exact.
	near(t, img, 1, 20)
}

// TestResizeToJPEG pins the use the module exists for: a HEIC photo made
// into a JPEG variant through the ordinary Pipeline.
func TestResizeToJPEG(t *testing.T) {
	res, err := pipeline.Process(bytes.NewReader(fixture(t)), imaging.Transform{Mode: imaging.ModeFit, Width: 32, Height: 32, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil || res.Width != 32 || res.Height != 24 {
		t.Fatalf("Process = %+v, %v", res, err)
	}
	img, _ := png.Decode(bytes.NewReader(res.Data))
	near(t, img, 0.5, 24)
}

// TestSanitizeTurnsHEICIntoJPEG pins Sanitize with the HEIC decoder: an
// iPhone upload is stored as an upright JPEG named .jpg.
func TestSanitizeTurnsHEICIntoJPEG(t *testing.T) {
	proc := imaging.Sanitize(imaging.Options{}, imaging.WithPipeline(pipeline))
	r, info, err := proc(t.Context(), upload.Info{Filename: "IMG_0042.HEIC", ContentType: heic.ContentType}, bytes.NewReader(fixture(t)))
	if err != nil || info.ContentType != "image/jpeg" || info.Filename != "IMG_0042.jpg" {
		t.Fatalf("Sanitize = %+v, %v", info, err)
	}
	var out bytes.Buffer
	_, _ = out.ReadFrom(r)
	if !bytes.HasPrefix(out.Bytes(), []byte("\xff\xd8")) {
		t.Error("not a JPEG")
	}
}

func TestMatch(t *testing.T) {
	d := heic.Decoder()
	for head, want := range map[string]bool{
		"\x00\x00\x00\x24ftypheic": true, "\x00\x00\x00\x24ftypheix": true, "\x00\x00\x00\x24ftyphevc": true,
		"\x00\x00\x00\x1cftypavif": false, "\x00\x00\x00\x24ftyp": false, "\xff\xd8\xff\xe0": false,
	} {
		if d.Match([]byte(head)) != want {
			t.Errorf("Match(%q) = %v", head, !want)
		}
	}
	if d.Name() != "heic" {
		t.Error("name")
	}
	_ = heic.Native()
}
