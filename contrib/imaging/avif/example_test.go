package avif_test

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"log"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/avif"
)

// avifPipeline writes AVIF as well as JPEG and PNG. AVIF is slow to
// encode: declare AVIF variants with variants.WithEager, so they are made
// after the upload rather than on a page's first request.
// ds:def id=avifpipeline-vkyty8te
func avifPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(avif.Encoder()), imaging.WithDecoder(avif.Decoder()))
}

// Example makes an AVIF thumbnail of a PNG.
func Example() {
	var original bytes.Buffer
	if err := png.Encode(&original, image.NewGray(image.Rect(0, 0, 300, 200))); err != nil {
		log.Fatal(err)
	}
	thumb := imaging.Transform{Mode: imaging.ModeFit, Width: 150, Height: 150, Format: imaging.FormatAVIF, Quality: 60}
	res, err := avifPipeline().Process(&original, thumb, imaging.Options{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.ContentType, res.Width, res.Height, thumb.Extension())
	// Output: image/avif 150 100 .avif
}
