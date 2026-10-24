// Package avif adds AVIF output and input to an imaging.Pipeline, with
// libavif compiled to WebAssembly and run by wazero
// (github.com/gen2brain/avif): no cgo.
//
//	p := imaging.NewPipeline(imaging.WithEncoder(avif.Encoder()), imaging.WithDecoder(avif.Decoder()))
//
// AVIF files are often half the size of a JPEG of the same quality, but
// encoding is slow: seconds for a large photo, much slower than JPEG. Make
// AVIF variants eagerly (variants.WithEager) or in a job, not on a page's
// first request. Two properties of the underlying library to know:
//
//   - When the program can load a system libavif, the library uses it
//     instead of the WebAssembly build, so output bytes can differ between
//     machines. Build with -tags nodynamic to always use the WebAssembly
//     build; Native reports which one runs.
//   - Importing it registers "avif" with the standard image package
//     (image.Decode).
package avif

import (
	"bytes"
	"image"
	"io"
	"slices"

	goavif "github.com/gen2brain/avif"

	"github.com/ubgo/filesystem/contrib/imaging"
)

// DefaultSpeed is the encoder's speed, 1 (slowest, smallest) to 10
// (fastest): 10, since AVIF is slow to encode even at its fastest.
const DefaultSpeed = goavif.DefaultSpeed

// Options configure the encoder.
type Options struct {
	// Speed is 1 (slowest, smallest file) to 10 (fastest); 0 means
	// DefaultSpeed.
	Speed int
	// Lossless writes lossless AVIF (4:4:4) and ignores the quality.
	Lossless bool
}

// The ISO base media file header: a box size, "ftyp", then the brand.
const (
	ftypAt      = 4
	brandAt     = 8
	headerBytes = 12
)

// brands are the major brands of AVIF still images and image sequences.
var brands = []string{"avif", "avis"}

type encoder struct{ o Options }

// Encoder returns the AVIF encoder for imaging.WithEncoder.
func Encoder(opts ...Options) imaging.Encoder {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Speed == 0 {
		o.Speed = DefaultSpeed
	}
	return encoder{o: o}
}

func (encoder) Format() imaging.Format { return imaging.FormatAVIF }

// Encode writes img as AVIF at quality (1 to 100; alpha at the same
// quality), or lossless.
func (e encoder) Encode(w io.Writer, img image.Image, quality int) error {
	return goavif.Encode(w, img, goavif.Options{Quality: quality, QualityAlpha: quality, Speed: e.o.Speed, Lossless: e.o.Lossless})
}

// EncodeTuned implements imaging.TunedEncoder: a transform's Lossless
// adds to the encoder's own (either makes the output lossless), and its
// Effort, 1 (fastest) to imaging.MaxEffort (smallest), replaces Speed,
// which runs the other way: effort 1 is speed 10.
func (e encoder) EncodeTuned(w io.Writer, img image.Image, s imaging.EncodeSettings) error {
	o := goavif.Options{Quality: s.Quality, QualityAlpha: s.Quality, Speed: e.o.Speed, Lossless: e.o.Lossless || s.Lossless}
	if s.Effort > 0 {
		o.Speed = imaging.MaxEffort + 1 - s.Effort
	}
	return goavif.Encode(w, img, o)
}

type decoder struct{}

// Decoder returns the AVIF decoder for imaging.WithDecoder. Decoded images
// are turned upright by their irot/imir boxes; DecodeConfig (and so
// imaging's Probe) reports the stored size before that rotation.
func Decoder() imaging.Decoder { return decoder{} }

func (decoder) Name() string { return "avif" }

func (decoder) Match(head []byte) bool {
	return len(head) >= headerBytes && bytes.Equal(head[ftypAt:brandAt], []byte("ftyp")) && slices.Contains(brands, string(head[brandAt:headerBytes]))
}

func (decoder) DecodeConfig(r io.Reader) (image.Config, error) { return goavif.DecodeConfig(r) }

func (decoder) Decode(r io.Reader) (image.Image, error) {
	return goavif.Decode(r, goavif.Options{AutoRotate: true})
}

// Native reports whether the library uses a system libavif rather than
// its WebAssembly build (see the package documentation).
func Native() bool { return goavif.Dynamic() == nil }
