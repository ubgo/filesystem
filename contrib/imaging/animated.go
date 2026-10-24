package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"

	"golang.org/x/image/draw"

	"github.com/ubgo/filesystem"
)

// GIF structure the frame counter walks (GIF89a specification).
const (
	gifHeaderBytes      = 13 // signature, version, logical screen descriptor
	gifFlagsAt          = 10 // packed fields of the screen descriptor
	gifColourTableFlag  = 0x80
	gifColourTableSize  = 0x07
	gifImageDescriptor  = 0x2C
	gifExtension        = 0x21
	gifTrailer          = 0x3B
	gifDescriptorBytes  = 10 // image separator to packed fields, inclusive
	gifDescriptorFlagAt = 9
)

func isGIF(data []byte) bool { return bytes.HasPrefix(data, []byte("GIF8")) }

// gifFrames counts the frames of a GIF from its block structure, decoding
// nothing, so an animation with a million tiny frames is refused before
// its first frame is decompressed.
func gifFrames(data []byte) (int, error) {
	broken := errors.New("imaging: broken GIF structure")
	if len(data) < gifHeaderBytes {
		return 0, broken
	}
	i := gifHeaderBytes
	if flags := data[gifFlagsAt]; flags&gifColourTableFlag != 0 {
		i += 3 << (flags&gifColourTableSize + 1)
	}
	frames := 0
	skipSubBlocks := func() bool {
		for i < len(data) {
			n := int(data[i])
			i++
			if n == 0 {
				return true
			}
			i += n
		}
		return false
	}
	for i < len(data) {
		switch data[i] {
		case gifTrailer:
			return frames, nil
		case gifExtension:
			i += 2 // introducer and label
			if !skipSubBlocks() {
				return 0, broken
			}
		case gifImageDescriptor:
			if i+gifDescriptorBytes > len(data) {
				return 0, broken
			}
			flags := data[i+gifDescriptorFlagAt]
			i += gifDescriptorBytes
			if flags&gifColourTableFlag != 0 {
				i += 3 << (flags&gifColourTableSize + 1)
			}
			i++ // LZW minimum code size
			if !skipSubBlocks() {
				return 0, broken
			}
			frames++
		default:
			return 0, broken
		}
	}
	return 0, broken // no trailer: the file was cut short
}

// processAnimation resizes every frame of an animated GIF and writes an
// animated GIF, keeping the delays and the loop count. Frames are
// composited onto the canvas as the format's disposal rules say before
// resizing (and cropped, when the transform crops), so partial and
// transparent frames come out right.
func processAnimation(data []byte, t Transform, o Options, mark *placedMark) (Result, error) {
	n, err := gifFrames(data)
	if err != nil {
		return Result{}, filesystem.NewError(filesystem.CodeRejected, err)
	}
	if n > o.MaxFrames {
		return Result{}, filesystem.Errorf(filesystem.CodeRejected, "imaging: %d frames exceeds %d", n, o.MaxFrames)
	}
	cfg, err := gif.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("imaging: unreadable gif header: %w", err))
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height)*int64(max(n, 1)) > o.MaxPixels {
		return Result{}, filesystem.Errorf(filesystem.CodeRejected, "imaging: %d frames of %dx%d exceed %d pixels", n, cfg.Width, cfg.Height, o.MaxPixels)
	}
	anim, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return Result{}, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("imaging: corrupt gif: %w", err))
	}
	canvas := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	// Every output frame is a whole picture, so each is disposed to the
	// background before the next: otherwise a viewer would show the
	// previous frame through the next one's transparent pixels.
	out := &gif.GIF{LoopCount: anim.LoopCount, Delay: anim.Delay, Disposal: make([]byte, len(anim.Image))}
	for i, frame := range anim.Image {
		var previous *image.RGBA
		if disposal(anim, i) == gif.DisposalPrevious {
			previous = image.NewRGBA(canvas.Bounds())
			copy(previous.Pix, canvas.Pix)
		}
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)
		region, err := applyCrop(canvas, t.Crop)
		if err != nil {
			return Result{}, err
		}
		if i == 0 && t.Mode == ModeFill && t.Gravity == GravitySmart {
			t = pinSmartCrop(region, t)
		}
		small := resize(region, t)
		mark.drawOn(small)
		paletted := image.NewPaletted(small.Bounds(), gifPalette)
		draw.FloydSteinberg.Draw(paletted, paletted.Bounds(), small, small.Bounds().Min)
		out.Image = append(out.Image, paletted)
		out.Disposal[i] = gif.DisposalBackground
		switch disposal(anim, i) {
		case gif.DisposalBackground:
			draw.Draw(canvas, frame.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			copy(canvas.Pix, previous.Pix)
		}
	}
	// Encoding cannot fail: a bytes.Buffer never fails a write, and the
	// frames, delays and disposals come from one decoded file.
	var buf bytes.Buffer
	_ = gif.EncodeAll(&buf, out)
	b := out.Image[0].Bounds()
	return Result{Data: buf.Bytes(), ContentType: t.ContentType(), Width: b.Dx(), Height: b.Dy()}, nil
}

// gifPalette is a transparent entry followed by the Plan 9 palette's
// first 255 colours, so transparent pixels of an animation stay
// transparent after quantising.
var gifPalette = append(color.Palette{color.Transparent}, palette.Plan9[:gifColours-1]...)

// pinSmartCrop chooses the smart crop window on the first frame and pins it
// for the rest as a focal point at its centre, so the crop does not jump
// as the detail moves between frames.
func pinSmartCrop(first image.Image, t Transform) Transform {
	b := first.Bounds()
	cw, ch := fillCrop(b, t)
	x0, y0 := smartWindow(first, cw, ch)
	t.Gravity = GravityFocal
	t.Focal = &Focal{X: float64(x0-b.Min.X+cw/2) / float64(b.Dx()), Y: float64(y0-b.Min.Y+ch/2) / float64(b.Dy())}
	return t
}

// disposal is frame i's disposal method; gif.DecodeAll records one for
// every frame.
func disposal(g *gif.GIF, i int) byte { return g.Disposal[i] }
