// Package heic adds HEIC input to an imaging.Pipeline, so photos from
// iPhones (and other phones that save HEIF) can be resized, measured and
// sanitized like JPEGs. It wraps github.com/gen2brain/heic, a decoder
// compiled to WebAssembly and run by wazero: no cgo. There is no HEIC
// encoder; HEIC uploads become JPEG variants (or, with imaging.Sanitize
// and WithPipeline, a JPEG original).
//
//	p := imaging.NewPipeline(imaging.WithDecoder(heic.Decoder()))
//
// Two properties of the underlying library to know:
//
//   - When the program can load a system libheif, the library uses it
//     instead of the WebAssembly build. Build with -tags nodynamic to
//     always use the WebAssembly build; Native reports which one runs.
//   - Importing it registers "heic" with the standard image package
//     (image.Decode).
package heic

import (
	"bytes"
	"image"
	"io"
	"slices"

	goheic "github.com/gen2brain/heic"

	"github.com/ubgo/filesystem/contrib/imaging"
)

// ContentType is the MIME type HEIC uploads are detected as (sniff.HEIC);
// pass it to dimensions.WithProcessor so they are measured.
const ContentType = "image/heic"

// The ISO base media file header: a box size, "ftyp", then the brand.
const (
	ftypAt      = 4
	brandAt     = 8
	headerBytes = 12
)

// brands are the major brands of HEVC-coded HEIF files, as the decoder
// registers them.
var brands = []string{"heic", "heix", "hevc", "hevx", "msf1"}

type decoder struct{}

// Decoder returns the HEIC decoder for imaging.WithDecoder. The decoded
// image is upright (the decoder applies the file's rotation and mirror
// boxes).
func Decoder() imaging.Decoder { return decoder{} }

func (decoder) Name() string { return "heic" }

func (decoder) Match(head []byte) bool {
	return len(head) >= headerBytes && bytes.Equal(head[ftypAt:brandAt], []byte("ftyp")) && slices.Contains(brands, string(head[brandAt:headerBytes]))
}

func (decoder) DecodeConfig(r io.Reader) (image.Config, error) { return goheic.DecodeConfig(r) }

func (decoder) Decode(r io.Reader) (image.Image, error) { return goheic.Decode(r) }

// Native reports whether the library uses a system libheif rather than
// its WebAssembly build (see the package documentation).
func Native() bool { return goheic.Dynamic() == nil }
