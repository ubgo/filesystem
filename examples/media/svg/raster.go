package main

import (
	"bytes"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/svg/raster"
)

// rasterSide is the longest side, in pixels, an SVG is drawn at before
// the transform resizes it: enough for the largest raster variant.
const rasterSide = 512

// svgPipeline reads SVG as well as the raster formats: every document is
// sanitised, then drawn by the pure-Go renderer. Hand it to
// variants.WithProcessor and dimensions.WithProcessor so an uploaded logo
// gets PNG variants, a size and a placeholder like any image.
// ds:def id=svgpipeline-b8x3k7qw
func svgPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithDecoder(raster.Decoder(raster.Options{Side: rasterSide})))
}

// thumbnail draws a stored SVG as a PNG of at most side pixels.
// ds:def id=thumbnail-v5n2c8rj
func thumbnail(doc []byte, side int) (imaging.Result, error) {
	t := imaging.Transform{Mode: imaging.ModeFit, Width: side, Height: side, Format: imaging.FormatPNG}
	return svgPipeline().Process(bytes.NewReader(doc), t, imaging.Options{})
}
