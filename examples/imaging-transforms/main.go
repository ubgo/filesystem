// Command imaging-transforms shows what contrib/imaging does to one image,
// with no database and no disk: the three resize modes and a crop region,
// EXIF orientation, a smart crop, a watermark, an animated GIF kept
// animated, the dominant colour, the pixel-bomb limits, and a Processor of
// its own wrapped around the Pipeline. The images are drawn in code, so
// the program needs no files.
//
//	go run ./imaging-transforms
//
// It prints what each step returned; main_test.go runs the same steps and
// compares one output with a golden image.
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"sync/atomic"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
)

// Decisions of this demo, not of the library.
const (
	// photoW and photoH are the size of the drawn landscape photo.
	photoW, photoH = 400, 200
	// box is the side of the square every transform targets.
	box = 100
	// logoName is the name the watermark is registered under.
	logoName = "logo"
	// frames is how many frames the drawn animation has.
	frames = 3
	// tinyPixels is a pixel budget far below the photo, to show the limit.
	tinyPixels = 10_000
)

// photo draws a landscape image: a plain grey sky on the left and a
// detailed red and yellow checkerboard on the right, the part a smart crop
// should keep.
func photo() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, photoW, photoH))
	for y := range photoH {
		for x := range photoW {
			c := color.NRGBA{150, 150, 150, 255}
			if x >= photoW*3/4 {
				c = color.NRGBA{220, 30, 30, 255}
				if (x/4+y/4)%2 == 0 {
					c = color.NRGBA{250, 220, 40, 255}
				}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// encodePNG returns img as PNG bytes.
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	err := png.Encode(&buf, img)
	return buf.Bytes(), err
}

// modes resizes the 400x200 photo into a 100x100 box with each mode, and
// takes a crop region first for the last one: the square a user picked for
// an avatar. ModeFit never enlarges, so it is also what other libraries
// call "limit".
// ds:def id=modes-t8w2k5nr
var modes = map[string]imaging.Transform{
	"fit":  {Mode: imaging.ModeFit, Width: box, Height: box, Format: imaging.FormatPNG},
	"fill": {Mode: imaging.ModeFill, Width: box, Height: box, Format: imaging.FormatJPEG, Quality: 80},
	"pad":  {Mode: imaging.ModePad, Width: box, Height: box, Format: imaging.FormatPNG},
	"crop": {Mode: imaging.ModeFill, Width: box, Height: box, Format: imaging.FormatPNG, Crop: image.Rect(300, 0, 400, 100)},
}

// resizeAll applies every transform of modes to src and reports the
// output size of each.
// ds:def id=resizeall-m3q7v9cz
func resizeAll(w io.Writer, src []byte) error {
	for _, name := range []string{"fit", "fill", "pad", "crop"} {
		res, err := imaging.Process(bytes.NewReader(src), modes[name], imaging.Options{})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-4s %dx%d %s\n", name, res.Width, res.Height, res.ContentType)
	}
	return nil
}

// smartCard is a square card that keeps the most detailed part of the
// photo instead of its centre.
// ds:def id=smartcard-x5r8n2wk
var smartCard = imaging.Transform{
	Mode: imaging.ModeFill, Width: box, Height: box, Format: imaging.FormatPNG,
	Gravity: imaging.GravitySmart,
}

// logo draws the watermark: a white square with a black border.
func logo() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for y := range 40 {
		for x := range 40 {
			c := color.NRGBA{255, 255, 255, 255}
			if x < 4 || y < 4 || x >= 36 || y >= 36 {
				c = color.NRGBA{0, 0, 0, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// watermarked registers the logo once on a Pipeline and returns it with a
// transform that draws it in the bottom right corner at half opacity, a
// fifth of the output's width, with a small margin.
// ds:def id=watermarked-c9v4h7ma
func watermarked() (*imaging.Pipeline, imaging.Transform, error) {
	p := imaging.NewPipeline(imaging.WithWatermark(logoName, logo()))
	if err := p.Err(); err != nil {
		return nil, imaging.Transform{}, err
	}
	t := imaging.Transform{
		Mode: imaging.ModeFit, Width: 200, Height: 200, Format: imaging.FormatPNG,
		Watermark: &imaging.Watermark{Name: logoName, Anchor: imaging.AnchorBottomRight, Opacity: 0.5, Scale: 0.2, Margin: 0.02},
	}
	return p, t, nil
}

// animation draws a GIF of frames solid frames, red, green and blue.
func animation() ([]byte, error) {
	g := &gif.GIF{LoopCount: 0}
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}}
	for i := range frames {
		f := image.NewPaletted(image.Rect(0, 0, 60, 40), pal)
		for p := range f.Pix {
			f.Pix[p] = uint8(i)
		}
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	err := gif.EncodeAll(&buf, g)
	return buf.Bytes(), err
}

// gifThumbs makes two thumbnails of an animated GIF: FormatGIF keeps the
// animation (every frame resized, delays and loop count kept), and
// FirstFrame makes a still. It returns the frame count of each.
// ds:def id=gifthumbs-p2k8w4xd
func gifThumbs(src []byte) (animated, still int, err error) {
	t := imaging.Transform{Mode: imaging.ModeFit, Width: 30, Height: 30, Format: imaging.FormatGIF}
	moving, err := imaging.Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return 0, 0, err
	}
	t.FirstFrame = true
	first, err := imaging.Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return 0, 0, err
	}
	a, err := gif.DecodeAll(bytes.NewReader(moving.Data))
	if err != nil {
		return 0, 0, err
	}
	s, err := gif.DecodeAll(bytes.NewReader(first.Data))
	if err != nil {
		return 0, 0, err
	}
	return len(a.Image), len(s.Image), nil
}

// placeholderColour is the background a page paints while the image
// loads: its most common colour as #rrggbb. Transparent pixels are
// ignored; an image with none opaque has no colour.
// ds:def id=placeholdercolour-w7n3c5qe
func placeholderColour(img image.Image) string {
	c, ok := imaging.DominantColour(img)
	if !ok {
		return ""
	}
	return imaging.Hex(c)
}

// tooBig processes src under a pixel budget smaller than the image. The
// size is read from the header and refused before a pixel is decoded, so
// a small file declaring a huge image cannot exhaust memory.
// ds:def id=toobig-r4j9m2vt
func tooBig(src []byte) error {
	_, err := imaging.Process(bytes.NewReader(src), modes["fit"], imaging.Options{MaxPixels: tinyPixels})
	return err
}

// countingProcessor is a Processor of its own: it wraps another one and
// counts the images it processed. Anything that accepts an
// imaging.Processor (variants.WithProcessor, dimensions, CheckDimensions)
// takes it, so metrics, a cache or a different engine slot in here.
// ds:def id=countingprocessor-h6t2q8zn
type countingProcessor struct {
	next imaging.Processor
	n    atomic.Int64
}

func (c *countingProcessor) Process(r io.Reader, t imaging.Transform, o imaging.Options) (imaging.Result, error) {
	c.n.Add(1)
	return c.next.Process(r, t, o)
}

func (c *countingProcessor) Probe(r io.Reader) (imaging.Info, error) { return c.next.Probe(r) }

// The wrapper is a Processor; the compiler checks it.
var _ imaging.Processor = (*countingProcessor)(nil)

// run performs every step and prints what it got.
func run(w io.Writer) error {
	src, err := encodePNG(photo())
	if err != nil {
		return err
	}
	if err := resizeAll(w, src); err != nil {
		return err
	}
	if err := orientation(w); err != nil {
		return err
	}
	return effects(w, src)
}

// orientation shows a portrait phone photo stored landscape with an EXIF
// orientation of 6: Probe and Process both see it upright.
func orientation(w io.Writer) error {
	rotated, err := exifJPEG(image.NewNRGBA(image.Rect(0, 0, 300, 200)), orientRotate90)
	if err != nil {
		return err
	}
	info, err := imaging.Probe(bytes.NewReader(rotated))
	if err != nil {
		return err
	}
	res, err := imaging.Process(bytes.NewReader(rotated), imaging.Transform{Mode: imaging.ModeFit, Width: 1000, Height: 1000, Format: imaging.FormatJPEG}, imaging.Options{})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "exif 6: stored 300x200, probe %dx%d, output %dx%d\n", info.Width, info.Height, res.Width, res.Height)
	return nil
}

// effects runs the smart crop, the watermark, the GIF, the colour, the
// limit and the wrapped processor.
func effects(w io.Writer, src []byte) error {
	smart, err := imaging.Process(bytes.NewReader(src), smartCard, imaging.Options{})
	if err != nil {
		return err
	}
	centre, err := imaging.Process(bytes.NewReader(src), modes["fill"], imaging.Options{})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "checkerboard kept: smart crop %d%%, centre crop %d%%\n", detailShare(smart.Data), detailShare(centre.Data))
	p, t, err := watermarked()
	if err != nil {
		return err
	}
	marked, err := p.Process(bytes.NewReader(src), t, imaging.Options{})
	if err != nil {
		return err
	}
	plain, err := p.Process(bytes.NewReader(src), imaging.Transform{Mode: t.Mode, Width: t.Width, Height: t.Height, Format: t.Format}, imaging.Options{})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "watermarked %dx%d, bottom right corner changed %t\n", marked.Width, marked.Height, cornerOf(marked.Data) != cornerOf(plain.Data))
	anim, err := animation()
	if err != nil {
		return err
	}
	moving, still, err := gifThumbs(anim)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "gif: animated %d frames, first frame %d\n", moving, still)
	fmt.Fprintf(w, "dominant colour %s\n", placeholderColour(photo()))
	err = tooBig(src)
	fmt.Fprintf(w, "over %d pixels: %s\n", tinyPixels, filesystem.CodeOf(err))
	counter := &countingProcessor{next: imaging.Default()}
	if _, err := counter.Process(bytes.NewReader(src), modes["fit"], imaging.Options{}); err != nil {
		return err
	}
	fmt.Fprintf(w, "custom processor ran %d time(s)\n", counter.n.Load())
	return nil
}

// detailShare is the percentage of an encoded image's pixels that are not
// the grey sky: how much of the checkerboard a crop kept. An image that
// does not decode counts as none.
func detailShare(data []byte) int {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0
	}
	b := img.Bounds()
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := nrgbaAt(img, x, y)
			if c.R > c.B+40 {
				n++
			}
		}
	}
	return n * 100 / (b.Dx() * b.Dy())
}

// cornerOf is the colour of a pixel 10 pixels in from the bottom right
// corner, where the watermark sits.
func cornerOf(data []byte) string {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err.Error()
	}
	b := img.Bounds()
	return imaging.Hex(nrgbaAt(img, b.Max.X-10, b.Max.Y-10))
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

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// jpegDecoderRegistered keeps image.Decode able to read the JPEG outputs
// in colourOf; the png and gif packages register themselves the same way.
var _ = jpeg.Decode
