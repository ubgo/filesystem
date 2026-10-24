// Package imaging resizes and re-encodes images in pure Go, safely enough
// to run on user uploads: it checks an image's dimensions from its header
// before decoding it (a 1 KB file can declare a 100,000 × 100,000 image and
// exhaust memory when decoded), applies the EXIF orientation phones write,
// and re-encodes every output, which drops EXIF, GPS and any payload
// smuggled in the original.
//
// It is a contrib module because it depends on golang.org/x/image. The work
// is done by a Processor; the default, *Pipeline, decodes JPEG, PNG, GIF,
// WebP, BMP and TIFF and encodes JPEG and PNG in pure Go. Codec modules
// (contrib/imaging/webp, avif, heic) add decoders and encoders to a
// Pipeline of your own, and another backend can implement Processor.
package imaging

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"io"
	"slices"
	"strings"

	"golang.org/x/image/draw"

	"github.com/ubgo/filesystem"
)

// Mode is how an image is fitted to the requested box.
type Mode string

// The closed set of modes.
const (
	// ModeFit scales the image down to fit inside Width × Height, keeping
	// its aspect ratio. It never enlarges: a small image stays small.
	ModeFit Mode = "fit"
	// ModeFill scales the image to cover Width × Height and crops the
	// overflow from the centre, so the output is exactly that size.
	ModeFill Mode = "fill"
	// ModePad scales the image down to fit inside Width × Height, like
	// ModeFit, then centres it on a canvas of exactly that size: white for
	// JPEG, transparent for PNG. For grids of uniform tiles that must not
	// crop.
	ModePad Mode = "pad"
)

// ModeValues lists every Mode.
var ModeValues = []Mode{ModeFit, ModeFill, ModePad}

// Gravity is the part of the image ModeFill keeps when it crops.
type Gravity string

// The closed set of gravities. The zero value is the centre.
const (
	GravityCenter Gravity = ""
	GravityTop    Gravity = "top"
	GravityBottom Gravity = "bottom"
	GravityLeft   Gravity = "left"
	GravityRight  Gravity = "right"
	// GravitySmart keeps the most detailed part of the image: edges,
	// saturated colour and skin tones (see smartcrop.go), so a portrait's
	// face or a product rather than plain background survives the crop.
	GravitySmart Gravity = "smart"
	// GravityFocal keeps Transform.Focal as close to the centre of the
	// crop as the image allows; without a Focal it is the centre.
	GravityFocal Gravity = "focal"
)

// GravityValues lists every Gravity.
var GravityValues = []Gravity{GravityCenter, GravityTop, GravityBottom, GravityLeft, GravityRight, GravitySmart, GravityFocal}

// Format is an output encoding.
type Format string

// The closed set of output formats. JPEG and PNG are built in; WebP and
// AVIF need their codec module's Encoder in the Pipeline
// (contrib/imaging/webp, contrib/imaging/avif), and Process refuses them
// with CodeUnsupported otherwise.
const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
	FormatWebP Format = "webp"
	FormatAVIF Format = "avif"
	// FormatGIF keeps an animated GIF's animation (see animated.go); a
	// still input, or Transform.FirstFrame, gives a one-frame GIF.
	FormatGIF Format = "gif"
)

// FormatValues lists every Format.
var FormatValues = []Format{FormatJPEG, FormatPNG, FormatWebP, FormatAVIF, FormatGIF}

// formatType is a Format's MIME type and file extension.
type formatType struct{ contentType, extension string }

// formatTypes gives each Format of FormatValues its MIME type and file
// extension.
var formatTypes = map[Format]formatType{
	FormatJPEG: {"image/jpeg", ".jpg"},
	FormatPNG:  {"image/png", ".png"},
	FormatWebP: {"image/webp", ".webp"},
	FormatAVIF: {"image/avif", ".avif"},
	FormatGIF:  {"image/gif", ".gif"},
}

// Limits.
const (
	// MaxDimension bounds Width and Height of a Transform.
	MaxDimension = 8192
	// DefaultQuality is the JPEG quality when Transform.Quality is 0: the
	// usual point past which files grow faster than they look better.
	DefaultQuality = 82
	// DefaultMaxPixels refuses inputs larger than 50 megapixels, above any
	// phone camera, before decoding them.
	DefaultMaxPixels = 50_000_000
	// DefaultMaxBytes refuses inputs larger than 50 MiB.
	DefaultMaxBytes = 50 << 20
	// DefaultMaxFrames refuses an animated GIF with more frames, counted
	// from the file's structure before any frame is decoded.
	DefaultMaxFrames = 500
)

// algorithmVersion is part of every Digest. Bump it when resampling or
// encoding changes, so cached variants made the old way are regenerated.
const algorithmVersion = "1"

// Transform describes one output image.
type Transform struct {
	Mode   Mode
	Width  int
	Height int
	Format Format
	// Quality is the quality of a lossy format (JPEG, WebP, AVIF), 1 to
	// 100; 0 means DefaultQuality. Ignored for PNG.
	Quality int
	// Gravity is the part ModeFill keeps; the zero value is the centre
	// (GravityTop keeps the head of a portrait).
	Gravity Gravity
	// Crop, when not empty, is a region of the upright image, in its
	// pixels, taken before resizing: the square a user chose for an
	// avatar. A region outside the image is refused when applied.
	Crop image.Rectangle
	// Focal is the point GravityFocal keeps, per image (a records file's
	// stored focal point, see contrib/imaging/focal); nil means none.
	Focal *Focal
	// Watermark, when set, is drawn over the output (see watermark.go).
	Watermark *Watermark
	// FirstFrame makes an animated GIF output a still of its first frame;
	// otherwise FormatGIF keeps the animation (see animated.go).
	FirstFrame bool
	// Background, when set, fills what the output leaves transparent:
	// ModePad's border and the image's own transparent pixels, in every
	// output format (a brand colour behind a logo). Nil keeps today's
	// output: a transparent border, flattened to white by JPEG. An
	// animated GIF output keeps its frames' own transparency.
	Background *color.NRGBA
	// Lossless asks the encoder for lossless output (WebP, AVIF; PNG is
	// always lossless). Effort is the encoder's speed/size trade-off, 1
	// (fastest) to MaxEffort (smallest file); 0 leaves the encoder's own.
	// Both reach an encoder that implements TunedEncoder; one that does
	// not (JPEG, GIF) refuses a transform setting either with
	// CodeUnsupported rather than ignore it.
	Lossless bool
	Effort   int
	// KeepMetadata and KeepXMP carry metadata of the source into the
	// output, as Optimize's options of the same names do for an upload
	// (the same kinds, the same rebuilt-from-scratch EXIF and XMP, GPS
	// never): a resized variant that must keep its creator and copyright
	// (a photo marketplace, a news site). Both nil, the default, keep
	// nothing, as before. The output must be FormatJPEG or FormatPNG,
	// the formats whose metadata this package writes (CodeUnsupported
	// otherwise), and metadata is kept only from a source of the same
	// format: a PNG made from a JPEG has nothing in the form a PNG holds,
	// so it keeps nothing, as from a source without any. An unknown kind
	// or a property CheckXMP refuses is CodeInvalidArgument.
	KeepMetadata []Metadata
	KeepXMP      []XMPProperty
}

// MaxEffort bounds Transform.Effort.
const MaxEffort = 10

// Validate reports a transform that cannot be applied. A Format outside
// FormatValues is refused; ValidateFor also accepts the formats a
// Processor's own encoders write.
func (t Transform) Validate() error { return t.validate(nil) }

// ValidateFor is Validate for transforms that p will process: a Format
// outside FormatValues passes when p can say it writes it (an Encodes
// method, as *Pipeline has, true for a custom Encoder added with
// WithEncoder: JPEG XL, HEIF). A Processor that cannot say gets
// Validate's closed set.
func (t Transform) ValidateFor(p Processor) error {
	enc, ok := p.(interface{ Encodes(Format) bool })
	if !ok {
		return t.validate(nil)
	}
	return t.validate(enc.Encodes)
}

// validate checks t; encodes, when set, admits formats outside
// FormatValues.
func (t Transform) validate(encodes func(Format) bool) error {
	switch {
	case !slices.Contains(ModeValues, t.Mode):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: unknown mode %q", t.Mode)
	case !slices.Contains(FormatValues, t.Format) && (encodes == nil || !encodes(t.Format)):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: unknown format %q", t.Format)
	case t.Width < 1 || t.Height < 1 || t.Width > MaxDimension || t.Height > MaxDimension:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: size %dx%d is not within 1..%d", t.Width, t.Height, MaxDimension)
	case t.Quality < 0 || t.Quality > 100:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: quality %d is not within 0..100", t.Quality)
	case !slices.Contains(GravityValues, t.Gravity):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: unknown gravity %q", t.Gravity)
	case t.Crop != (image.Rectangle{}) && (t.Crop.Empty() || t.Crop.Min.X < 0 || t.Crop.Min.Y < 0):
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: crop %v is not a region of an image", t.Crop)
	case t.Effort < 0 || t.Effort > MaxEffort:
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: effort %d is not within 0..%d", t.Effort, MaxEffort)
	case t.Focal != nil && !t.Focal.Valid():
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: focal point %v is not within 0..1", *t.Focal)
	}
	if _, unknown, ok := keepOf(t.KeepMetadata); !ok {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: unknown metadata kind %q to keep", unknown)
	}
	if err := CheckXMP(t.KeepXMP); err != nil {
		return err
	}
	if t.Watermark != nil {
		return t.Watermark.validate()
	}
	return nil
}

func (t Transform) quality() int {
	if t.Quality == 0 {
		return DefaultQuality
	}
	return t.Quality
}

// Digest identifies the transform's output: equal transforms have equal
// digests, and any change (or a new algorithm version) gives a new one, so
// it can name cached results.
func (t Transform) Digest() string {
	q := t.quality()
	if t.Format == FormatPNG {
		q = 0
	}
	spec := fmt.Appendf(nil, "v%s|%s|%dx%d|%s|q%d", algorithmVersion, t.Mode, t.Width, t.Height, t.Format, q)
	// Later fields join only when set, so the digests of transforms that
	// predate them (and the cached variants they name) do not change.
	if t.Gravity != GravityCenter {
		spec = fmt.Appendf(spec, "|g%s", t.Gravity)
	}
	if t.Crop != (image.Rectangle{}) {
		spec = fmt.Appendf(spec, "|c%d,%d,%d,%d", t.Crop.Min.X, t.Crop.Min.Y, t.Crop.Max.X, t.Crop.Max.Y)
	}
	if t.Focal != nil {
		spec = fmt.Appendf(spec, "|f%g,%g", t.Focal.X, t.Focal.Y)
	}
	if w := t.Watermark; w != nil {
		spec = fmt.Appendf(spec, "|w%s,%s,%g,%g,%g", w.Name, w.Anchor, w.Opacity, w.Scale, w.Margin)
	}
	if t.FirstFrame {
		spec = append(spec, "|first"...)
	}
	if b := t.Background; b != nil {
		spec = fmt.Appendf(spec, "|bg%02x%02x%02x%02x", b.R, b.G, b.B, b.A)
	}
	if t.Lossless {
		spec = append(spec, "|lossless"...)
	}
	if t.Effort != 0 {
		spec = fmt.Appendf(spec, "|e%d", t.Effort)
	}
	if len(t.KeepMetadata) > 0 {
		kinds := make([]string, len(t.KeepMetadata))
		for i, m := range t.KeepMetadata {
			kinds[i] = string(m)
		}
		slices.Sort(kinds)
		spec = fmt.Appendf(spec, "|k%s", strings.Join(slices.Compact(kinds), ","))
	}
	if len(t.KeepXMP) > 0 {
		props := make([]string, len(t.KeepXMP))
		for i, p := range t.KeepXMP {
			props[i] = p.String()
		}
		spec = fmt.Appendf(spec, "|x%s", strings.Join(props, ","))
	}
	sum := sha256.Sum256(spec)
	return hex.EncodeToString(sum[:8])
}

// ContentType is the MIME type of the transform's output, for a Format of
// FormatValues; "" for any other (ask the Pipeline: FormatInfo).
func (t Transform) ContentType() string { return formatTypes[t.Format].contentType }

// Extension is the file extension of the output, with the dot, for a
// Format of FormatValues; "" for any other (ask the Pipeline:
// FormatInfo).
func (t Transform) Extension() string { return formatTypes[t.Format].extension }

// Options bound what Process accepts. Zero fields mean the defaults.
// MaxPixels bounds every decoded pixel: for an animation it is the canvas
// times the frame count.
type Options struct {
	MaxPixels int64
	MaxBytes  int64
	MaxFrames int
}

func (o Options) withDefaults() Options {
	if o.MaxPixels <= 0 {
		o.MaxPixels = DefaultMaxPixels
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxFrames <= 0 {
		o.MaxFrames = DefaultMaxFrames
	}
	return o
}

// Result is a processed image.
type Result struct {
	Data        []byte
	ContentType string
	Width       int
	Height      int
}

// Process reads an image from r and applies t with the default Pipeline
// (see Pipeline.Process).
func Process(r io.Reader, t Transform, opts Options) (Result, error) {
	return defaultPipeline.Process(r, t, opts)
}

// resize applies the transform's geometry with Catmull-Rom resampling.
func resize(src image.Image, t Transform) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if t.Mode == ModeFit || t.Mode == ModePad {
		scale := min(float64(t.Width)/float64(sw), float64(t.Height)/float64(sh), 1)
		w, h := max(1, int(float64(sw)*scale+0.5)), max(1, int(float64(sh)*scale+0.5))
		if t.Mode == ModeFit {
			dst := image.NewRGBA(image.Rect(0, 0, w, h))
			draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
			return dst
		}
		// Pad: a transparent canvas (JPEG encoding flattens it to white).
		dst := image.NewRGBA(image.Rect(0, 0, t.Width, t.Height))
		x0, y0 := (t.Width-w)/2, (t.Height-h)/2
		draw.CatmullRom.Scale(dst, image.Rect(x0, y0, x0+w, y0+h), src, sb, draw.Src, nil)
		return dst
	}
	// Fill: scale to cover, then crop the centre of the source.
	cw, ch := fillCrop(sb, t)
	x0, y0 := sb.Min.X+(sw-cw)/2, sb.Min.Y+(sh-ch)/2
	switch t.Gravity {
	case GravitySmart:
		x0, y0 = smartWindow(src, cw, ch)
	case GravityFocal:
		if t.Focal != nil {
			x0, y0 = focalWindow(sb, *t.Focal, cw, ch)
		}
	case GravityTop:
		y0 = sb.Min.Y
	case GravityBottom:
		y0 = sb.Max.Y - ch
	case GravityLeft:
		x0 = sb.Min.X
	case GravityRight:
		x0 = sb.Max.X - cw
	}
	dst := image.NewRGBA(image.Rect(0, 0, t.Width, t.Height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, image.Rect(x0, y0, x0+cw, y0+ch), draw.Src, nil)
	return dst
}

// fillCrop is the size, in sb's pixels, of the region ModeFill scales to
// t's box: as large as fits with the box's aspect ratio.
func fillCrop(sb image.Rectangle, t Transform) (int, int) {
	sw, sh := sb.Dx(), sb.Dy()
	scale := max(float64(t.Width)/float64(sw), float64(t.Height)/float64(sh))
	cw, ch := int(float64(t.Width)/scale+0.5), int(float64(t.Height)/scale+0.5)
	return min(max(cw, 1), sw), min(max(ch, 1), sh)
}

// flatten composites an image with transparency onto white for JPEG, which
// has no alpha channel (transparent pixels would otherwise turn black).
func flatten(img image.Image) image.Image { return onto(img, color.White) }

// onto composites img over a canvas of bg (Transform.Background, or
// white for JPEG).
func onto(img image.Image, bg color.Color) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(b)
	draw.Draw(dst, b, &image.Uniform{C: bg}, image.Point{}, draw.Src)
	draw.Draw(dst, b, img, b.Min, draw.Over)
	return dst
}
