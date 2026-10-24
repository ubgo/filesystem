package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"maps"
	"mime"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"

	"github.com/ubgo/filesystem"
)

// Processor turns an image into the output a Transform describes, and
// reads an image's header. It is the seam between this package's users
// (the variants and dimensions plugins, an application's own handlers)
// and the code that does the work: *Pipeline is the pure-Go
// implementation, and another backend (libvips, a remote image service)
// can implement the same two methods. An implementation must honour the
// Transform contract (Validate, the geometry of each Mode, ContentType)
// and the limits in Options, with the same error codes as Process.
type Processor interface {
	Process(r io.Reader, t Transform, opts Options) (Result, error)
	Probe(r io.Reader) (Info, error)
}

// Decoder reads one image format. The codec modules under
// contrib/imaging (webp, avif, heic) export decoders for formats the
// standard library cannot read.
type Decoder interface {
	// Name is the format's short name, reported as Info.Format.
	Name() string
	// Match reports whether head, the file's first bytes (at least 16
	// when the file has them), starts an image of this format.
	Match(head []byte) bool
	// DecodeConfig reads the dimensions without decoding pixels.
	DecodeConfig(r io.Reader) (image.Config, error)
	// Decode reads the whole image.
	Decode(r io.Reader) (image.Image, error)
}

// Encoder writes one output Format.
type Encoder interface {
	Format() Format
	// Encode writes img at quality (1 to 100; lossless formats ignore it).
	Encode(w io.Writer, img image.Image, quality int) error
}

// EncodeSettings are what a Transform asks of its encoder.
type EncodeSettings struct {
	// Quality is 1 to 100 (Transform.Quality, or DefaultQuality).
	Quality int
	// Lossless and Effort are the Transform's (0 Effort: the encoder's
	// own trade-off).
	Lossless bool
	Effort   int
}

// TunedEncoder is an Encoder that takes per-transform settings beyond
// quality: lossless output and a speed/size effort. The webp and avif
// codec modules' encoders are TunedEncoders; so is the built-in PNG one
// (Effort picks the zlib level). A Transform that sets Lossless or
// Effort needs one, and is refused with CodeUnsupported otherwise; one
// that sets neither is encoded with Encode, as before.
type TunedEncoder interface {
	Encoder
	EncodeTuned(w io.Writer, img image.Image, s EncodeSettings) error
}

// DescribedEncoder is an Encoder of a Format outside FormatValues (JPEG
// XL, HEIF): it declares what its output is, since the package's own
// table names only FormatValues. WithEncoder requires it for such a
// format; for a format of FormatValues the built-in type and extension
// win, so replacing the WebP encoder cannot relabel WebP.
type DescribedEncoder interface {
	Encoder
	// ContentType is the output's MIME type: an image/* type other than
	// image/svg+xml ("image/jxl"), which is what the stored variant is
	// served as.
	ContentType() string
	// Extension is the output's file extension with the dot (".jxl"):
	// a dot and 1 to 8 lower-case letters or digits.
	Extension() string
}

// extensionPattern is what DescribedEncoder.Extension must look like.
var extensionPattern = regexp.MustCompile(`^\.[a-z0-9]{1,8}$`)

// describe checks e's declaration for a format outside FormatValues. The
// type must be an image, and never SVG: a variant is served under it, and
// an encoder that declared text/html or image/svg+xml would make every
// variant a page that runs script.
func describe(e Encoder) (formatType, error) {
	d, ok := e.(DescribedEncoder)
	if !ok {
		return formatType{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: encoder for %q must declare its ContentType and Extension (DescribedEncoder)", e.Format())
	}
	ct, ext := strings.ToLower(strings.TrimSpace(d.ContentType())), d.Extension()
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mt, "image/") || mt == "image/svg+xml" {
		return formatType{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: encoder for %q declares content type %q; it must be an image type other than SVG", e.Format(), d.ContentType())
	}
	if !extensionPattern.MatchString(ext) {
		return formatType{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: encoder for %q declares extension %q; it must be a dot and 1 to 8 lower-case letters or digits", e.Format(), ext)
	}
	return formatType{contentType: mt, extension: ext}, nil
}

// Pipeline is the pure-Go Processor: decode with the first Decoder whose
// Match accepts the input, check its size from the header before decoding,
// apply the EXIF orientation, crop and resample (Catmull-Rom), and encode
// with the Encoder for the Transform's Format. A Pipeline is immutable and
// safe for concurrent use.
type Pipeline struct {
	decoders []Decoder
	encoders map[Format]Encoder
	// described holds the type and extension of each format outside
	// FormatValues, from its DescribedEncoder.
	described  map[Format]formatType
	watermarks map[string]*image.NRGBA
	// configErr is the first error an option recorded (Err).
	configErr error
	// colourManaged and convert: WithColourManagement.
	colourManaged bool
	convert       ColourConverter
}

// PipelineOption configures NewPipeline.
type PipelineOption func(*Pipeline)

// WithDecoder adds d. Added decoders are tried before the built-in ones,
// so a codec module can replace a built-in decoder for its format.
func WithDecoder(d Decoder) PipelineOption {
	return func(p *Pipeline) { p.decoders = append([]Decoder{d}, p.decoders...) }
}

// WithEncoder adds e, replacing any encoder for the same Format. An
// encoder of a Format outside FormatValues must be a DescribedEncoder
// whose declaration passes its checks; otherwise the Pipeline records a
// configuration error (Err) and Process refuses every call.
func WithEncoder(e Encoder) PipelineOption {
	return func(p *Pipeline) {
		f := e.Format()
		if !slices.Contains(FormatValues, f) {
			ft, err := describe(e)
			if err != nil {
				p.fail(err)
				return
			}
			p.described[f] = ft
		}
		p.encoders[f] = e
	}
}

// WithWatermark registers img under name, for Transform.Watermark. It is
// converted once here, so a request draws it without decoding anything.
// Registering a name twice keeps the last image. A nil image, or one
// with no pixels, is a configuration error the Pipeline records: Err
// reports it, and Process refuses every call with it.
func WithWatermark(name string, img image.Image) PipelineOption {
	return func(p *Pipeline) {
		if img == nil || img.Bounds().Empty() {
			p.fail(filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: watermark %q has no pixels", name))
			return
		}
		nrgba := image.NewNRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
		draw.Draw(nrgba, nrgba.Bounds(), img, img.Bounds().Min, draw.Src)
		p.watermarks[name] = nrgba
	}
}

// fail records an option's error; the first one wins.
func (p *Pipeline) fail(err error) {
	if p.configErr == nil {
		p.configErr = err
	}
}

// Err returns the first error an option met when the Pipeline was built,
// such as an empty watermark image, so an application can refuse to start
// with a broken Pipeline. Process returns the same error on every call.
func (p *Pipeline) Err() error { return p.configErr }

// NewPipeline returns a Pipeline with the built-in codecs (decoding JPEG,
// PNG, GIF, WebP, BMP and TIFF; encoding JPEG and PNG) plus the given
// ones.
func NewPipeline(options ...PipelineOption) *Pipeline {
	p := &Pipeline{
		decoders: []Decoder{
			builtin{"jpeg", "\xff\xd8", jpeg.Decode, jpeg.DecodeConfig},
			builtin{"png", "\x89PNG\r\n\x1a\n", png.Decode, png.DecodeConfig},
			builtin{"gif", "GIF8", gif.Decode, gif.DecodeConfig},
			builtin{"webp", "RIFF", webp.Decode, webp.DecodeConfig},
			builtin{"bmp", "BM", bmp.Decode, bmp.DecodeConfig},
			builtin{"tiff", "II*\x00", tiff.Decode, tiff.DecodeConfig},
			builtin{"tiff", "MM\x00*", tiff.Decode, tiff.DecodeConfig},
		},
		encoders:   map[Format]Encoder{},
		described:  map[Format]formatType{},
		watermarks: map[string]*image.NRGBA{},
	}
	for _, e := range []Encoder{jpegEncoder{}, pngEncoder{}, gifEncoder{}} {
		p.encoders[e.Format()] = e
	}
	for _, o := range options {
		o(p)
	}
	return p
}

// defaultPipeline backs the package-level Process, Probe and Sanitize. It
// is never changed after it is built: extra codecs go in a Pipeline of the
// caller's own, so no import changes what another package decodes.
var defaultPipeline = NewPipeline()

// Default returns the built-in Pipeline, for a Processor option that
// wants the default explicitly.
func Default() *Pipeline { return defaultPipeline }

// The Pipeline is the default Processor.
var _ Processor = (*Pipeline)(nil)

// Encodes reports whether the Pipeline can write f.
func (p *Pipeline) Encodes(f Format) bool {
	_, ok := p.encoders[f]
	return ok
}

// FormatInfo returns f's MIME type and file extension (with the dot):
// the package's own for FormatValues, the DescribedEncoder's for another
// format this Pipeline encodes. ok is false for a format it cannot name.
func (p *Pipeline) FormatInfo(f Format) (contentType, extension string, ok bool) {
	if ft, known := formatTypes[f]; known {
		return ft.contentType, ft.extension, true
	}
	ft, ok := p.described[f]
	return ft.contentType, ft.extension, ok
}

// Formats lists every format the Pipeline encodes, sorted.
func (p *Pipeline) Formats() []Format {
	return slices.Sorted(maps.Keys(p.encoders))
}

// builtin adapts the standard and x/image codecs. They are registered
// here rather than by blank imports, so this package never changes what
// image.Decode accepts elsewhere in the application.
type builtin struct {
	name, magic string
	decode      func(io.Reader) (image.Image, error)
	config      func(io.Reader) (image.Config, error)
}

func (b builtin) Name() string                                   { return b.name }
func (b builtin) Match(head []byte) bool                         { return bytes.HasPrefix(head, []byte(b.magic)) }
func (b builtin) DecodeConfig(r io.Reader) (image.Config, error) { return b.config(r) }
func (b builtin) Decode(r io.Reader) (image.Image, error)        { return b.decode(r) }

type jpegEncoder struct{}

func (jpegEncoder) Format() Format { return FormatJPEG }

// Encode flattens transparency onto white first: JPEG has no alpha, and
// transparent pixels would otherwise turn black.
func (jpegEncoder) Encode(w io.Writer, img image.Image, quality int) error {
	return jpeg.Encode(w, flatten(img), &jpeg.Options{Quality: quality})
}

type pngEncoder struct{}

func (pngEncoder) Format() Format { return FormatPNG }

func (pngEncoder) Encode(w io.Writer, img image.Image, _ int) error { return png.Encode(w, img) }

// EncodeTuned writes a PNG, which is always lossless; Effort picks the
// zlib level: up to pngFastEffort the fastest, from pngBestEffort the
// smallest, the default between.
func (pngEncoder) EncodeTuned(w io.Writer, img image.Image, s EncodeSettings) error {
	level := png.DefaultCompression
	switch {
	case s.Effort == 0:
	case s.Effort <= pngFastEffort:
		level = png.BestSpeed
	case s.Effort >= pngBestEffort:
		level = png.BestCompression
	}
	return (&png.Encoder{CompressionLevel: level}).Encode(w, img)
}

// The Effort thresholds of the PNG encoder's zlib levels.
const (
	pngFastEffort = 3
	pngBestEffort = 8
)

type gifEncoder struct{}

func (gifEncoder) Format() Format { return FormatGIF }

// Encode writes one frame, quantised to 256 colours with dithering.
func (gifEncoder) Encode(w io.Writer, img image.Image, _ int) error {
	return gif.Encode(w, img, &gif.Options{NumColors: gifColours, Drawer: draw.FloydSteinberg})
}

// gifColours is the size of a GIF palette.
const gifColours = 256

// match returns the decoder for data, or a CodeRejected error.
func (p *Pipeline) match(data []byte) (Decoder, error) {
	for _, d := range p.decoders {
		if d.Match(data) {
			return d, nil
		}
	}
	return nil, filesystem.NewError(filesystem.CodeRejected, errors.New("imaging: not a supported image format"))
}

// decode checks the header's dimensions against maxPixels before decoding.
func (p *Pipeline) decode(data []byte, maxPixels int64) (image.Image, error) {
	d, err := p.match(data)
	if err != nil {
		return nil, err
	}
	cfg, err := d.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("imaging: unreadable %s header: %w", d.Name(), err))
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "imaging: %dx%d exceeds %d pixels", cfg.Width, cfg.Height, maxPixels)
	}
	img, err := d.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("imaging: corrupt %s: %w", d.Name(), err))
	}
	return img, nil
}

// readLimited reads r up to max bytes, or fails with CodeTooLarge.
func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, filesystem.Errorf(filesystem.CodeTooLarge, "imaging: input exceeds %d bytes", max)
	}
	return data, nil
}

// Process reads an image from r and applies t. Errors: CodeInvalidArgument
// for a bad transform, CodeUnsupported for a Format no Encoder writes,
// CodeTooLarge for input over MaxBytes, CodeRejected for input that is not
// a supported image or has more than MaxPixels.
func (p *Pipeline) Process(r io.Reader, t Transform, opts Options) (Result, error) {
	if p.configErr != nil {
		return Result{}, p.configErr
	}
	if err := t.ValidateFor(p); err != nil {
		return Result{}, err
	}
	enc, ok := p.encoders[t.Format]
	if !ok {
		return Result{}, filesystem.Errorf(filesystem.CodeUnsupported, "imaging: no encoder for %s; add its codec module to the Pipeline (WithEncoder)", t.Format)
	}
	if _, tuned := enc.(TunedEncoder); !tuned && (t.Lossless || t.Effort != 0) {
		return Result{}, filesystem.Errorf(filesystem.CodeUnsupported, "imaging: the %s encoder takes no lossless or effort setting", t.Format)
	}
	keep, _, _ := keepOf(t.KeepMetadata) // validated above
	keep.xmp = t.KeepXMP
	if !keep.none() && t.Format != FormatJPEG && t.Format != FormatPNG {
		return Result{}, filesystem.Errorf(filesystem.CodeUnsupported, "imaging: metadata is kept only in JPEG and PNG output, not %s", t.Format)
	}
	o := opts.withDefaults()
	data, err := readLimited(r, o.MaxBytes)
	if err != nil {
		return Result{}, err
	}
	var mark *placedMark
	if t.Watermark != nil {
		if mark, err = p.mark(*t.Watermark); err != nil {
			return Result{}, err
		}
	}
	if t.Format == FormatGIF && !t.FirstFrame && isGIF(data) {
		return processAnimation(data, t, o, mark)
	}
	src, err := p.decode(data, o.MaxPixels)
	if err != nil {
		return Result{}, err
	}
	src, profile, err := p.manageColour(data, src)
	if err != nil {
		return Result{}, err
	}
	src = orient(src, exifOrientation(data))
	if src, err = applyCrop(src, t.Crop); err != nil {
		return Result{}, err
	}
	out := resize(src, t)
	mark.drawOn(out)
	if t.Background != nil {
		out = onto(out, *t.Background)
	}
	var buf bytes.Buffer
	if err := encode(enc, &buf, out, t); err != nil {
		return Result{}, err
	}
	b := out.Bounds()
	encoded := buf.Bytes()
	if profile != nil {
		encoded = embedProfile(t.Format, encoded, profile)
	}
	if !keep.none() {
		if encoded, err = carryMetadata(t.Format, data, encoded, keep); err != nil {
			return Result{}, err
		}
	}
	contentType, _, _ := p.FormatInfo(t.Format)
	return Result{Data: encoded, ContentType: contentType, Width: b.Dx(), Height: b.Dy()}, nil
}

// carryMetadata puts what keep keeps of the source data into encoded,
// an output of format f, right after its start (a JPEG's SOI, a PNG's
// IHDR), the place Optimize uses. Only a source of the same format has
// metadata in the form the output holds; any other keeps nothing. The
// kept segments are those stripJPEG and stripPNG rebuild, so EXIF and
// XMP are new blocks and never the source's.
func carryMetadata(f Format, data, encoded []byte, keep keepSet) ([]byte, error) {
	switch {
	case f == FormatJPEG && bytes.HasPrefix(data, []byte{jpegMarker, jpegSOI}):
		_, _, kept, err := stripJPEG(data, keep)
		if err != nil || len(kept) == 0 {
			return encoded, err
		}
		return append(append([]byte{jpegMarker, jpegSOI}, kept...), encoded[2:]...), nil
	case f == FormatPNG && bytes.HasPrefix(data, []byte(pngSignature)):
		_, _, kept, err := stripPNG(data, keep)
		if err != nil || len(kept) == 0 {
			return encoded, err
		}
		ihdrEnd := len(pngSignature) + chunkOverhead + pngIHDRBytes
		return append(append(slices.Clone(encoded[:ihdrEnd]), kept...), encoded[ihdrEnd:]...), nil
	}
	return encoded, nil
}

// encode writes img with enc under t's settings: EncodeTuned when t sets
// Lossless or Effort (Process has checked enc is a TunedEncoder), Encode
// otherwise, so a transform that sets neither encodes as it always did.
func encode(enc Encoder, w io.Writer, img image.Image, t Transform) error {
	var err error
	if tuned, ok := enc.(TunedEncoder); ok && (t.Lossless || t.Effort != 0) {
		err = tuned.EncodeTuned(w, img, EncodeSettings{Quality: t.quality(), Lossless: t.Lossless, Effort: t.Effort})
	} else {
		err = enc.Encode(w, img, t.quality())
	}
	if err != nil {
		return fmt.Errorf("imaging: encode %s: %w", t.Format, err)
	}
	return nil
}

// applyCrop cuts crop (in the upright image's pixels) out of src before
// resizing; an empty crop returns src.
func applyCrop(src image.Image, crop image.Rectangle) (image.Image, error) {
	if crop == (image.Rectangle{}) {
		return src, nil
	}
	region := crop.Add(src.Bounds().Min)
	if !region.In(src.Bounds()) {
		return nil, filesystem.Errorf(filesystem.CodeRejected, "imaging: crop %v is outside the %dx%d image", crop, src.Bounds().Dx(), src.Bounds().Dy())
	}
	cropped := image.NewRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
	draw.Draw(cropped, cropped.Bounds(), src, region.Min, draw.Src)
	return cropped, nil
}

// Probe reads an image's format and displayed dimensions from its first
// ProbeBytes, without decoding the pixels (see the package-level Probe).
func (p *Pipeline) Probe(r io.Reader) (Info, error) {
	data, err := io.ReadAll(io.LimitReader(r, ProbeBytes))
	if err != nil {
		return Info{}, err
	}
	d, err := p.match(data)
	if err != nil {
		return Info{}, err
	}
	cfg, err := d.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Info{}, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("imaging: unreadable %s header: %w", d.Name(), err))
	}
	info := Info{Format: d.Name(), Width: cfg.Width, Height: cfg.Height}
	if o := exifOrientation(data); o >= orientTranspose {
		info.Width, info.Height = info.Height, info.Width
	}
	return info, nil
}
