package variants_test

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
)

// webpStandIn encodes FormatWebP as PNG bytes, counting calls, standing in
// for the WebP codec module.
type webpStandIn struct{ calls *int }

func (webpStandIn) Format() imaging.Format { return imaging.FormatWebP }
func (e webpStandIn) Encode(w io.Writer, img image.Image, _ int) error {
	*e.calls++
	return png.Encode(w, img)
}

// bareProcessor hides the Pipeline's Encodes, as a third-party backend
// without that method would.
type bareProcessor struct{ imaging.Processor }

var webpThumb = map[string]imaging.Transform{"thumb": {Mode: imaging.ModeFit, Width: 20, Height: 20, Format: imaging.FormatWebP}}

func TestWithProcessor(t *testing.T) {
	if _, err := variants.New(webpThumb, imaging.Options{}); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Errorf("WebP without its encoder = %v, want unsupported at New", err)
	}
	if _, err := variants.New(transforms, imaging.Options{}, variants.WithProcessor(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("nil processor = %v", err)
	}
	// A processor that cannot say what it encodes is trusted at New.
	if _, err := variants.New(webpThumb, imaging.Options{}, variants.WithProcessor(bareProcessor{imaging.Default()})); err != nil {
		t.Errorf("bare processor = %v", err)
	}

	calls := 0
	proc := imaging.NewPipeline(imaging.WithEncoder(webpStandIn{calls: &calls}))
	p, err := variants.New(webpThumb, imaging.Options{}, variants.WithProcessor(proc))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWithPlugin(t, memory.New(), p)
	f := e.image(t, "a.png", pngBytes(t, 40, 40), filesystem.VisibilityPrivate)
	v, err := e.plugin.Get(t.Context(), e.store, "t", f.ID, "thumb")
	if err != nil || calls != 1 || v.ContentType != "image/webp" || !bytes.HasSuffix([]byte(v.Key), []byte(".webp")) {
		t.Fatalf("Get = %+v, %v (encoder calls %d)", v, err, calls)
	}
}
