package webp_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/webp"
)

// webpPipeline is a Pipeline that writes WebP as well as JPEG and PNG.
// Pass it to variants.WithProcessor so WebP variants can be declared.
// ds:def id=webppipeline-yw2vxhuh
func webpPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithEncoder(webp.Encoder()), imaging.WithDecoder(webp.Decoder()))
}

// Example makes a WebP thumbnail of a PNG with a Pipeline that has the
// WebP codec.
func Example() {
	img := image.NewNRGBA(image.Rect(0, 0, 400, 300))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	img.SetNRGBA(0, 0, color.NRGBA{255, 0, 0, 255})
	var original bytes.Buffer
	if err := png.Encode(&original, img); err != nil {
		log.Fatal(err)
	}
	thumb := imaging.Transform{Mode: imaging.ModeFit, Width: 200, Height: 200, Format: imaging.FormatWebP, Quality: 75}
	res, err := webpPipeline().Process(&original, thumb, imaging.Options{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.ContentType, res.Width, res.Height, thumb.Extension())
	// Output: image/webp 200 150 .webp
}
