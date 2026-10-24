// Package webp adds WebP output (and a WebP decoder with animation-safe
// EXIF handling) to an imaging.Pipeline, with the libwebp encoder compiled
// to Go (github.com/gen2brain/webp): no cgo.
//
//	p := imaging.NewPipeline(imaging.WithEncoder(webp.Encoder()), imaging.WithDecoder(webp.Decoder()))
//
// Two properties of the underlying library to know:
//
//   - When the program can load a system libwebp (libwebp.so, .dylib or
//     .dll), the library uses it instead of the compiled-in Go code, so
//     output bytes can differ between machines. Build with
//     -tags nodynamic to always use the pure-Go code; Native reports
//     which one runs.
//   - Importing it registers "webp" with the standard image package
//     (image.Decode), as most codec packages do.
package webp

import (
	"bytes"
	"image"
	"io"

	gowebp "github.com/gen2brain/webp"

	"github.com/ubgo/filesystem/contrib/imaging"
)

// DefaultMethod is the encoder's speed/size trade-off, 0 (fast) to 6
// (smallest): 4, libwebp's own default.
const DefaultMethod = gowebp.DefaultMethod

// Options configure the encoder.
type Options struct {
	// Lossless writes lossless WebP and ignores the Transform's quality:
	// for screenshots and drawings, where lossy WebP blurs edges.
	Lossless bool
	// Method is the speed/size trade-off, 0 to 6; 0 means DefaultMethod.
	Method int
}

// riffWebP is the container signature: "RIFF", a 4-byte size, "WEBP".
const (
	riffTag     = "RIFF"
	webpTag     = "WEBP"
	webpTagAt   = 8
	headerBytes = 12
)

type encoder struct{ o Options }

// Encoder returns the WebP encoder for imaging.WithEncoder.
func Encoder(opts ...Options) imaging.Encoder {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Method == 0 {
		o.Method = DefaultMethod
	}
	return encoder{o: o}
}

func (encoder) Format() imaging.Format { return imaging.FormatWebP }

// Encode writes img as WebP at quality (1 to 100), or lossless.
func (e encoder) Encode(w io.Writer, img image.Image, quality int) error {
	return gowebp.Encode(w, img, gowebp.Options{Quality: quality, Lossless: e.o.Lossless, Method: e.o.Method})
}

// maxMethod is libwebp's slowest, smallest method.
const maxMethod = 6

// EncodeTuned implements imaging.TunedEncoder: a transform's Lossless
// adds to the encoder's own (either makes the output lossless), and its
// Effort, 1 to imaging.MaxEffort, replaces Method, scaled onto 0..6.
func (e encoder) EncodeTuned(w io.Writer, img image.Image, s imaging.EncodeSettings) error {
	o := gowebp.Options{Quality: s.Quality, Lossless: e.o.Lossless || s.Lossless, Method: e.o.Method}
	if s.Effort > 0 {
		o.Method = methodFor(s.Effort)
	}
	return gowebp.Encode(w, img, o)
}

// methodFor scales an effort of 1..imaging.MaxEffort onto WebP's methods
// 0..6, rounding to the nearest.
func methodFor(effort int) int {
	return ((effort-1)*maxMethod*2 + imaging.MaxEffort - 1) / ((imaging.MaxEffort - 1) * 2)
}

type decoder struct{}

// Decoder returns the WebP decoder for imaging.WithDecoder. It replaces
// the built-in one (golang.org/x/image/webp), adding animated files (the
// first frame) and the EXIF orientation of photos saved as WebP.
func Decoder() imaging.Decoder { return decoder{} }

func (decoder) Name() string { return "webp" }

func (decoder) Match(head []byte) bool {
	return len(head) >= headerBytes && bytes.HasPrefix(head, []byte(riffTag)) && string(head[webpTagAt:headerBytes]) == webpTag
}

func (decoder) DecodeConfig(r io.Reader) (image.Config, error) { return gowebp.DecodeConfig(r) }

func (decoder) Decode(r io.Reader) (image.Image, error) {
	return gowebp.Decode(r, gowebp.Options{AutoRotate: true})
}

// Native reports whether the library uses a system libwebp rather than
// its compiled-in Go code (see the package documentation).
func Native() bool { return gowebp.Dynamic() == nil }
