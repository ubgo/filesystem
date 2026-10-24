// Command imaging-codecs shows the codec modules of contrib/imaging: a
// Pipeline that writes WebP and AVIF as well as JPEG and PNG, and one that
// converts photos with an embedded Display P3 colour profile to sRGB. All
// three modules are pure Go (WebAssembly through wazero, no cgo), so the
// program builds with CGO_ENABLED=0.
//
//	go run .
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"os"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/avif"
	"github.com/ubgo/filesystem/contrib/imaging/icc"
	"github.com/ubgo/filesystem/contrib/imaging/webp"
)

// Decisions of this demo, not of the library.
const (
	// thumbSide is the box every thumbnail fits in.
	thumbSide = 200
	// webpQuality and avifQuality are the lossy qualities; AVIF looks as
	// good as WebP at a lower number.
	webpQuality = 75
	avifQuality = 60
)

// modernPipeline decodes and encodes WebP and AVIF on top of the built-in
// JPEG, PNG and GIF. Added decoders are tried before the built-in ones, so
// a codec module can also replace a built-in decoder. Pass the Pipeline to
// variants.WithProcessor so WebP and AVIF variants can be declared.
// ds:def id=modernpipeline-v4k9t2xs
func modernPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(
		imaging.WithEncoder(webp.Encoder()), imaging.WithDecoder(webp.Decoder()),
		imaging.WithEncoder(avif.Encoder()), imaging.WithDecoder(avif.Decoder()),
	)
}

// thumbnails makes the same thumbnail in every format the Pipeline
// writes, and reports the type and size of each.
// ds:def id=thumbnails-c7m2w8rq
func thumbnails(w io.Writer, p *imaging.Pipeline, src []byte) error {
	for _, t := range []imaging.Transform{
		{Mode: imaging.ModeFit, Width: thumbSide, Height: thumbSide, Format: imaging.FormatJPEG},
		{Mode: imaging.ModeFit, Width: thumbSide, Height: thumbSide, Format: imaging.FormatWebP, Quality: webpQuality},
		{Mode: imaging.ModeFit, Width: thumbSide, Height: thumbSide, Format: imaging.FormatAVIF, Quality: avifQuality},
	} {
		res, err := p.Process(bytes.NewReader(src), t, imaging.Options{})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-10s %dx%d %s\n", res.ContentType, res.Width, res.Height, t.Extension())
	}
	return nil
}

// managedPipeline converts photos whose embedded profile is an RGB matrix
// profile (a phone's Display P3, Adobe RGB) to sRGB, so they do not look
// washed out once re-encoded. A profile icc cannot convert is written into
// JPEG and PNG output instead.
// ds:def id=managedpipeline-h3w8n5kc
func managedPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithColourManagement(icc.ToSRGB))
}

// convertP3 processes a Display P3 photo with and without colour
// management and returns the red channel of each output and whether the
// output still carries a profile.
func convertP3(src []byte) (plainRed, managedRed uint8, kept bool, err error) {
	t := imaging.Transform{Mode: imaging.ModeFit, Width: 4, Height: 4, Format: imaging.FormatPNG}
	plain, err := imaging.Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return 0, 0, false, err
	}
	managed, err := managedPipeline().Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return 0, 0, false, err
	}
	_, kept = imaging.EmbeddedProfile(managed.Data)
	pr, err := redAt(plain.Data)
	if err != nil {
		return 0, 0, false, err
	}
	mr, err := redAt(managed.Data)
	return pr, mr, kept, err
}

// redAt is the red channel of the pixel at (1, 1) of a PNG.
func redAt(data []byte) (uint8, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	return nrgbaAt(img, 1, 1).R, nil
}

// nrgbaAt is the pixel at (x, y) as non-premultiplied 8-bit RGBA, the form
// the checks compare channels in. NRGBAModel.Convert always returns a
// color.NRGBA; the checked assertion keeps a model that ever did not from
// panicking, and the zero colour it falls back to fails every check.
func nrgbaAt(img image.Image, x, y int) color.NRGBA {
	c, ok := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	if !ok {
		return color.NRGBA{}
	}
	return c
}

// photo draws a 400x300 gradient and encodes it as PNG.
func photo() ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 400, 300))
	for y := range 300 {
		for x := range 400 {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x * 255 / 400), uint8(y * 255 / 300), 128, 255})
		}
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	return buf.Bytes(), err
}

// run performs every step and prints what it got.
func run(w io.Writer) error {
	src, err := photo()
	if err != nil {
		return err
	}
	if err := thumbnails(w, modernPipeline(), src); err != nil {
		return err
	}
	p3, err := displayP3PNG()
	if err != nil {
		return err
	}
	plain, managed, kept, err := convertP3(p3)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "display p3 red 180: dropped profile %d, converted to sRGB %d, profile kept %t\n", plain, managed, kept)
	return nil
}

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}
