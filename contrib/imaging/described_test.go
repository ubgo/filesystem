package imaging_test

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"slices"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

// formatJXL is a format outside FormatValues, as a JPEG XL codec module
// would add it.
const formatJXL imaging.Format = "jxl"

// jxlEncoder stands in for a JPEG XL encoder: it writes PNG bytes under
// whatever type and extension the test gives it.
type jxlEncoder struct{ contentType, extension string }

func (jxlEncoder) Format() imaging.Format { return formatJXL }
func (jxlEncoder) Encode(w io.Writer, img image.Image, _ int) error {
	return png.Encode(w, img)
}
func (e jxlEncoder) ContentType() string { return e.contentType }
func (e jxlEncoder) Extension() string   { return e.extension }

// bareEncoder writes a format outside FormatValues without declaring it.
type bareEncoder struct{}

func (bareEncoder) Format() imaging.Format                   { return formatJXL }
func (bareEncoder) Encode(io.Writer, image.Image, int) error { return nil }

// TestDescribedEncoder pins the custom-format seam: a DescribedEncoder
// adds a format the Pipeline then validates, encodes and names; an
// encoder that does not declare itself, or declares a non-image or SVG
// type or a bad extension, is a configuration error; the package's
// Validate and table stay the closed set.
func TestDescribedEncoder(t *testing.T) {
	p := imaging.NewPipeline(imaging.WithEncoder(jxlEncoder{"image/jxl", ".jxl"}))
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	tr := imaging.Transform{Mode: imaging.ModeFit, Width: 10, Height: 10, Format: formatJXL}
	res, err := p.Process(bytesReader(photo(t, 40, 20, pngEnc)), tr, imaging.Options{})
	if err != nil || res.ContentType != "image/jxl" || res.Width != 10 {
		t.Fatalf("Process = %+v, %v", res.ContentType, err)
	}
	if ct, ext, ok := p.FormatInfo(formatJXL); !ok || ct != "image/jxl" || ext != ".jxl" {
		t.Fatalf("FormatInfo = %q %q %v", ct, ext, ok)
	}
	if !slices.Contains(p.Formats(), formatJXL) || tr.ValidateFor(p) != nil {
		t.Fatalf("formats %v", p.Formats())
	}

	// The default is unchanged: Validate, the default pipeline and the
	// Transform's own table know only FormatValues.
	if filesystem.CodeOf(tr.Validate()) != filesystem.CodeInvalidArgument || filesystem.CodeOf(tr.ValidateFor(imaging.Default())) != filesystem.CodeInvalidArgument || tr.ContentType() != "" {
		t.Fatal("a format outside FormatValues passed without its encoder")
	}
	// A Processor that cannot say what it encodes gets the closed set.
	if filesystem.CodeOf(tr.ValidateFor(opaqueProcessor{p})) != filesystem.CodeInvalidArgument {
		t.Fatal("a processor without Encodes passed a format outside FormatValues")
	}
	if _, err := imaging.Process(bytesReader(photo(t, 40, 20, pngEnc)), tr, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("the default pipeline: %v", err)
	}

	for name, e := range map[string]imaging.Encoder{
		"undeclared":    bareEncoder{},
		"html":          jxlEncoder{"text/html", ".jxl"},
		"svg":           jxlEncoder{"image/svg+xml", ".svg"},
		"bad extension": jxlEncoder{"image/jxl", "jxl"},
		"path":          jxlEncoder{"image/jxl", ".j/xl"},
	} {
		bad := imaging.NewPipeline(imaging.WithEncoder(e))
		if filesystem.CodeOf(bad.Err()) != filesystem.CodeInvalidArgument || bad.Encodes(formatJXL) {
			t.Errorf("%s: Err = %v, encodes %v", name, bad.Err(), bad.Encodes(formatJXL))
		}
		if _, err := bad.Process(bytesReader(photo(t, 4, 4, pngEnc)), tr, imaging.Options{}); err == nil {
			t.Errorf("%s: a broken pipeline processed", name)
		}
	}
	// A declaration cannot relabel a built-in format.
	webp := imaging.NewPipeline(imaging.WithEncoder(relabel{}))
	if ct, _, _ := webp.FormatInfo(imaging.FormatPNG); ct != "image/png" {
		t.Fatalf("PNG relabelled as %q", ct)
	}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// relabel is a PNG encoder that declares another type.
type relabel struct{}

func (relabel) Format() imaging.Format                           { return imaging.FormatPNG }
func (relabel) Encode(w io.Writer, img image.Image, _ int) error { return png.Encode(w, img) }
func (relabel) ContentType() string                              { return "image/x-other" }
func (relabel) Extension() string                                { return ".other" }

// opaqueProcessor hides a Pipeline's Encodes method, as a Processor of an
// application's own would not have one.
type opaqueProcessor struct{ p *imaging.Pipeline }

func (o opaqueProcessor) Process(r io.Reader, t imaging.Transform, opts imaging.Options) (imaging.Result, error) {
	return o.p.Process(r, t, opts)
}

func (o opaqueProcessor) Probe(r io.Reader) (imaging.Info, error) { return o.p.Probe(r) }
