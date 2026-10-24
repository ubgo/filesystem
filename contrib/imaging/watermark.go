package imaging

import (
	"image"
	"image/color"
	"math"
	"slices"

	"golang.org/x/image/draw"

	"github.com/ubgo/filesystem"
)

// Anchor is where a watermark sits on the output.
type Anchor string

// The closed set of anchors. The zero value is the bottom right corner,
// where a watermark hides the least.
const (
	AnchorBottomRight Anchor = ""
	AnchorBottomLeft  Anchor = "bottom_left"
	AnchorTopRight    Anchor = "top_right"
	AnchorTopLeft     Anchor = "top_left"
	AnchorCenter      Anchor = "center"
)

// AnchorValues lists every Anchor.
var AnchorValues = []Anchor{AnchorBottomRight, AnchorBottomLeft, AnchorTopRight, AnchorTopLeft, AnchorCenter}

// DefaultWatermarkScale is the watermark's width, as a fraction of the
// output's, when Watermark.Scale is 0.
const DefaultWatermarkScale = 0.25

// Watermark draws a registered image over a transform's output: a logo on
// every product photo, "preview" across a paid image.
type Watermark struct {
	// Name is the image registered on the Pipeline with WithWatermark;
	// an unknown name fails Process with CodeInvalidArgument.
	Name string
	// Anchor is where it sits.
	Anchor Anchor
	// Opacity is 0 to 1; 0 means 1 (opaque).
	Opacity float64
	// Scale is its width as a fraction of the output's width, keeping its
	// aspect ratio; 0 means DefaultWatermarkScale.
	Scale float64
	// Margin is its distance from the anchored edges, as a fraction of
	// the output's width; 0 puts it against them.
	Margin float64
}

func (w Watermark) validate() error {
	switch {
	case w.Name == "":
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: a watermark needs the name it was registered under")
	case !slices.Contains(AnchorValues, w.Anchor):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: unknown watermark anchor %q", w.Anchor)
	case !between(w.Opacity, 0, 1) || !between(w.Scale, 0, 1) || !between(w.Margin, 0, 0.5):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: watermark opacity and scale must be within 0..1 and margin within 0..0.5")
	}
	return nil
}

func between(v, lo, hi float64) bool { return v >= lo && v <= hi && !math.IsNaN(v) }

// placedMark is a registered watermark with its settings, ready to draw.
// It keeps the scaled copy it last drew, so the frames of an animation
// (all one size) scale the watermark once.
type placedMark struct {
	img    *image.NRGBA
	w      Watermark
	scaled *image.NRGBA
}

// mark looks up w's image on the Pipeline.
func (p *Pipeline) mark(w Watermark) (*placedMark, error) {
	img, ok := p.watermarks[w.Name]
	if !ok {
		return nil, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: no watermark %q in the pipeline (see WithWatermark)", w.Name)
	}
	return &placedMark{img: img, w: w}, nil
}

// drawOn draws the watermark over dst; a nil mark draws nothing.
func (m *placedMark) drawOn(dst *image.RGBA) {
	if m == nil {
		return
	}
	db, mb := dst.Bounds(), m.img.Bounds()
	scale, opacity := m.w.Scale, m.w.Opacity
	if scale == 0 {
		scale = DefaultWatermarkScale
	}
	if opacity == 0 {
		opacity = 1
	}
	w := max(1, int(float64(db.Dx())*scale+0.5))
	h := max(1, int(float64(w)*float64(mb.Dy())/float64(mb.Dx())+0.5))
	if h > db.Dy() {
		// A watermark taller than the output is fitted to its height, so
		// a tall, thin image never allocates more than the output.
		h = db.Dy()
		w = max(1, int(float64(h)*float64(mb.Dx())/float64(mb.Dy())+0.5))
	}
	margin := int(float64(db.Dx())*m.w.Margin + 0.5)
	var at image.Point
	switch m.w.Anchor {
	case AnchorTopLeft:
		at = image.Pt(db.Min.X+margin, db.Min.Y+margin)
	case AnchorTopRight:
		at = image.Pt(db.Max.X-margin-w, db.Min.Y+margin)
	case AnchorBottomLeft:
		at = image.Pt(db.Min.X+margin, db.Max.Y-margin-h)
	case AnchorCenter:
		at = image.Pt(db.Min.X+(db.Dx()-w)/2, db.Min.Y+(db.Dy()-h)/2)
	default:
		at = image.Pt(db.Max.X-margin-w, db.Max.Y-margin-h)
	}
	if m.scaled == nil || m.scaled.Bounds().Dx() != w || m.scaled.Bounds().Dy() != h {
		m.scaled = image.NewNRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(m.scaled, m.scaled.Bounds(), m.img, mb, draw.Src, nil)
	}
	scaled := m.scaled
	alpha := &image.Uniform{C: color.Alpha{A: uint8(opacity*0xff + 0.5)}}
	draw.DrawMask(dst, image.Rectangle{Min: at, Max: at.Add(image.Pt(w, h))}, scaled, image.Point{}, alpha, image.Point{}, draw.Over)
}
