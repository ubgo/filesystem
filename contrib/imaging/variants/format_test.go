package variants_test

import (
	"image"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
)

const formatJXL imaging.Format = "jxl"

// jxl stands in for a JPEG XL codec module's encoder.
type jxl struct{}

func (jxl) Format() imaging.Format                           { return formatJXL }
func (jxl) Encode(w io.Writer, img image.Image, _ int) error { return png.Encode(w, img) }
func (jxl) ContentType() string                              { return "image/jxl" }
func (jxl) Extension() string                                { return ".jxl" }

// TestCustomFormatVariant pins a variant in a format outside
// imaging.FormatValues: accepted with a processor that encodes it, stored
// under its declared extension and type, kept by Prune, and refused at
// New without the encoder.
func TestCustomFormatVariant(t *testing.T) {
	tr := map[string]imaging.Transform{"thumb": {Mode: imaging.ModeFit, Width: 16, Height: 16, Format: formatJXL}}
	if _, err := variants.New(tr, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("without the encoder: %v", err)
	}
	p, err := variants.New(tr, imaging.Options{}, variants.WithProcessor(imaging.NewPipeline(imaging.WithEncoder(jxl{}))))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWithPlugin(t, memory.New(), p)
	f := e.image(t, "a.png", pngBytes(t, 64, 32), filesystem.VisibilityPrivate)
	v, err := p.Get(t.Context(), e.store, "t", f.ID, "thumb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(v.Key, ".jxl") || v.ContentType != "image/jxl" {
		t.Fatalf("variant %q as %q", v.Key, v.ContentType)
	}
	if n, err := p.Prune(t.Context(), e.store); err != nil || n != 0 {
		t.Fatalf("Prune removed %d (%v): a custom-format variant counted as stale", n, err)
	}
}
