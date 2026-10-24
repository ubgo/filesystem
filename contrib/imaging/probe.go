package imaging

import (
	"io"
)

// ProbeBytes is how much of a file Probe reads: every supported format
// states its dimensions near the start, and a JPEG's EXIF segment (where
// the orientation is) is at most 64 KiB and comes before the image data.
const ProbeBytes = 128 << 10

// Info is what an image's header says.
type Info struct {
	// Format is the decoder's name: jpeg, png, gif, webp, bmp or tiff.
	Format string
	// Width and Height are as displayed: swapped for the EXIF
	// orientations that rotate by 90 degrees, as a phone's portrait photo
	// stored landscape needs.
	Width  int
	Height int
}

// Probe reads an image's format and dimensions from its first ProbeBytes,
// without decoding the pixels, so it is cheap and safe on any upload.
// Anything that is not a supported image with a readable header is
// CodeRejected. A TIFF whose header points past ProbeBytes is refused too.
func Probe(r io.Reader) (Info, error) {
	return defaultPipeline.Probe(r)
}
