// Package raster turns SVG into pixels for imaging: a Decoder for an
// imaging.Pipeline, so an uploaded SVG gets PNG or JPEG thumbnails,
// variants, dimensions and a ThumbHash like any raster image.
//
//	p := imaging.NewPipeline(imaging.WithDecoder(raster.Decoder(raster.Options{})))
//
// Every document is sanitised first (contrib/svg: scripts, event handlers,
// external references and foreign content removed), so the renderer only
// ever sees a clean, bounded document. The default renderer is pure Go
// (github.com/srwiley/oksvg); it draws shapes, paths, gradients and
// transforms, but not text or filters, which come out missing. A different
// renderer (resvg compiled to WebAssembly, say) can be passed as
// Options.Render.
package raster

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/svg"
)

// Limits and defaults.
const (
	// DefaultSide is the longest side, in pixels, an SVG is drawn at when
	// Options.Side is 0: enough for any thumbnail or variant to be scaled
	// down from, rather than up.
	DefaultSide = 2048
	// MaxSide bounds Options.Side.
	MaxSide = 8192
	// DefaultTimeout bounds one rendering when Options.Timeout is 0.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxRenders bounds the renderings in flight when
	// Options.MaxRenders is 0.
	DefaultMaxRenders = 4
	// headBytes is how much of the input Match and DecodeConfig look at
	// for the root element.
	headBytes = 4096
	// fallbackWidth and fallbackHeight are the size of an SVG that states
	// neither a size nor a viewBox, as browsers draw it.
	fallbackWidth, fallbackHeight = 300, 150
)

// Renderer draws a sanitised SVG document into a w × h image, stretching
// its viewBox to fill it (the Decoder chooses w and h with the document's
// own aspect ratio).
type Renderer func(doc io.Reader, w, h int) (*image.RGBA, error)

// Options configure the Decoder. The zero value is safe.
type Options struct {
	// Side is the longest side, in pixels, the document is drawn at; 0
	// means DefaultSide, at most MaxSide. The Pipeline's MaxPixels still
	// applies to the result.
	Side int
	// Timeout bounds one rendering; 0 means DefaultTimeout. A rendering
	// that takes longer fails with CodeRejected. The renderer cannot be
	// interrupted, so it finishes in the background, bounded by the
	// sanitiser's limits and Side.
	Timeout time.Duration
	// MaxRenders bounds the renderings in flight at once, counting those
	// that timed out but still run; 0 means DefaultMaxRenders. A Decode
	// while that many run is refused at once with CodeUnavailable, so
	// slow documents cannot pile up abandoned renderings and their
	// images in memory.
	MaxRenders int
	// Sanitize bounds and tunes the sanitising that runs first (input
	// size, nesting, element count, embedded raster images).
	Sanitize svg.Options
	// Render draws the document; nil means the pure-Go oksvg renderer.
	Render Renderer
}

// decoder holds the slots of the renderings in flight: one Decoder value,
// shared by every Pipeline it is added to, bounds them all.
type decoder struct {
	o     Options
	slots chan struct{}
}

// Decoder returns the SVG decoder for imaging.WithDecoder. A Side outside
// 0..MaxSide is clamped into it.
func Decoder(o Options) imaging.Decoder {
	if o.Side <= 0 {
		o.Side = DefaultSide
	}
	o.Side = min(o.Side, MaxSide)
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Render == nil {
		o.Render = renderOKSVG
	}
	if o.MaxRenders <= 0 {
		o.MaxRenders = DefaultMaxRenders
	}
	return decoder{o: o, slots: make(chan struct{}, o.MaxRenders)}
}

func (decoder) Name() string { return "svg" }

// Match reports whether head starts an SVG document: after a byte order
// mark and white space, an <svg> root, possibly after an XML declaration,
// comments or a doctype.
func (decoder) Match(head []byte) bool {
	_, ok := rootSize(head)
	return ok
}

// DecodeConfig reports the size the document is drawn at.
func (d decoder) DecodeConfig(r io.Reader) (image.Config, error) {
	head, err := io.ReadAll(io.LimitReader(r, headBytes))
	if err != nil {
		return image.Config{}, err
	}
	size, ok := rootSize(head)
	if !ok {
		return image.Config{}, errors.New("raster: no svg root element")
	}
	w, h := d.scaled(size)
	return image.Config{ColorModel: color.RGBAModel, Width: w, Height: h}, nil
}

// Decode sanitises the document and draws it.
func (d decoder) Decode(r io.Reader) (image.Image, error) {
	var clean bytes.Buffer
	if _, err := svg.Sanitize(r, &clean, d.o.Sanitize); err != nil {
		return nil, err
	}
	size, _ := rootSize(clean.Bytes()) // the sanitiser guarantees an svg root
	w, h := d.scaled(size)
	select {
	case d.slots <- struct{}{}:
	default:
		return nil, filesystem.Errorf(filesystem.CodeUnavailable, "raster: %d SVG renderings already in progress", cap(d.slots))
	}
	type result struct {
		img *image.RGBA
		err error
	}
	done := make(chan result, 1)
	go func() {
		// The slot is freed when the rendering ends, not when Decode
		// gives up on it.
		defer func() { <-d.slots }()
		// A renderer that panics (a bug in it, a document it chokes on)
		// fails this decode, never the process: nothing else recovers in
		// this goroutine.
		defer func() {
			if v := recover(); v != nil {
				done <- result{nil, filesystem.Errorf(filesystem.CodeUnknown, "raster: the renderer panicked: %v", v)}
			}
		}()
		img, err := d.o.Render(bytes.NewReader(clean.Bytes()), w, h)
		done <- result{img, err}
	}()
	select {
	case res := <-done:
		return res.img, res.err
	case <-time.After(d.o.Timeout):
		return nil, filesystem.Errorf(filesystem.CodeRejected, "raster: rendering took longer than %s", d.o.Timeout)
	}
}

// scaled is size scaled so its longest side is Side, keeping its ratio.
func (d decoder) scaled(size [2]float64) (int, int) {
	k := float64(d.o.Side) / max(size[0], size[1])
	return max(1, int(size[0]*k+0.5)), max(1, int(size[1]*k+0.5))
}

// rootSize finds the svg root element and its intrinsic size: width and
// height in user units (px), else the viewBox's, else the browser default.
func rootSize(head []byte) ([2]float64, bool) {
	dec := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return [2]float64{}, false
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local != "svg" {
				return [2]float64{}, false
			}
			return intrinsic(el.Attr), true
		case xml.CharData:
			if len(bytes.TrimSpace(el)) > 0 {
				return [2]float64{}, false
			}
		}
	}
}

func intrinsic(attrs []xml.Attr) [2]float64 {
	var w, h float64
	var box []float64
	for _, a := range attrs {
		switch a.Name.Local {
		case "width":
			w = length(a.Value)
		case "height":
			h = length(a.Value)
		case "viewBox":
			for _, f := range strings.FieldsFunc(a.Value, func(r rune) bool { return r == ' ' || r == ',' }) {
				v, _ := strconv.ParseFloat(f, 64)
				box = append(box, v)
			}
		}
	}
	switch {
	case w > 0 && h > 0:
		return [2]float64{w, h}
	case len(box) == 4 && box[2] > 0 && box[3] > 0:
		return [2]float64{box[2], box[3]}
	}
	return [2]float64{fallbackWidth, fallbackHeight}
}

// length reads a width or height in user units; other units (%, em, cm)
// give 0, so the viewBox decides.
func length(v string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(v), "px"), 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// renderOKSVG draws with oksvg, ignoring elements it does not support rather
// than failing on them.
func renderOKSVG(doc io.Reader, w, h int) (*image.RGBA, error) {
	icon, err := oksvg.ReadIconStream(doc, oksvg.IgnoreErrorMode)
	if err != nil {
		return nil, filesystem.NewError(filesystem.CodeRejected, fmt.Errorf("raster: %w", err))
	}
	icon.SetTarget(0, 0, float64(w), float64(h))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	icon.Draw(rasterx.NewDasher(w, h, rasterx.NewScannerGV(w, h, img, img.Bounds())), 1)
	return img, nil
}
