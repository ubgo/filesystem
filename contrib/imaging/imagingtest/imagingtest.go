// Package imagingtest compares images with stored golden files in tests,
// by how different they look rather than byte for byte: encoders, resampling
// and dithering may change a pixel by a level or two between Go versions or
// machines without anyone being able to see it, and a byte-exact golden
// would fail on every such change while a perceptual one keeps catching
// real regressions (a crop in the wrong place, a missing watermark, a
// colour shift).
//
//	imagingtest.Golden(t, got, "testdata/card.golden.png")
//
// Run the tests with -update to write the golden files from the current
// output (the flag is the test binary's own "update" flag, when the test
// package defines one, as most golden-file tests do; Options.Update does
// the same from code).
package imagingtest

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Defaults of the comparison.
const (
	// DefaultMaxMean is the largest mean difference allowed, in levels of
	// 0..255 on the perceptual distance below: under one level is
	// invisible.
	DefaultMaxMean = 1.0
	// DefaultPixelThreshold is the distance at which one pixel counts as
	// visibly different.
	DefaultPixelThreshold = 16.0
	// DefaultMaxDifferent is the largest fraction of visibly different
	// pixels allowed: 0.1%, room for dithering at edges.
	DefaultMaxDifferent = 0.001
)

// Options tune Golden. Zero fields mean the defaults.
type Options struct {
	MaxMean        float64
	PixelThreshold float64
	MaxDifferent   float64
	// Update writes got as the golden file instead of comparing, as the
	// -update flag does.
	Update bool
}

// Diff is how different two images look.
type Diff struct {
	// Mean is the mean per-pixel distance, 0..255.
	Mean float64
	// Different is the fraction of pixels farther apart than the
	// threshold Compare was given.
	Different float64
	// Max is the largest per-pixel distance.
	Max float64
}

// Compare measures how different a and b look: per pixel, the Euclidean
// distance of their colours composited over mid grey (so a transparent
// pixel compares by what shows through), each channel weighted by its
// share of perceived brightness (Rec. 709 luma: green counts most), scaled
// to 0..255. Images of different sizes are an error.
func Compare(a, b image.Image, threshold float64) (Diff, error) {
	ab, bb := a.Bounds(), b.Bounds()
	if ab.Dx() != bb.Dx() || ab.Dy() != bb.Dy() {
		return Diff{}, fmt.Errorf("imagingtest: sizes differ: %dx%d and %dx%d", ab.Dx(), ab.Dy(), bb.Dx(), bb.Dy())
	}
	var sum float64
	var d Diff
	n := ab.Dx() * ab.Dy()
	for y := range ab.Dy() {
		for x := range ab.Dx() {
			dist := distance(a.At(ab.Min.X+x, ab.Min.Y+y).RGBA, b.At(bb.Min.X+x, bb.Min.Y+y).RGBA)
			sum += dist
			d.Max = math.Max(d.Max, dist)
			if dist > threshold {
				d.Different++
			}
		}
	}
	if n > 0 {
		d.Mean, d.Different = sum/float64(n), d.Different/float64(n)
	}
	return d, nil
}

// Rec. 709 luma weights: the share of each channel in perceived
// brightness.
const (
	weightR = 0.2126
	weightG = 0.7152
	weightB = 0.0722
	// grey is the background transparent pixels are composited over.
	grey = 0.5
)

// distance of two premultiplied 16-bit colours, as described on Compare.
func distance(a, b func() (r, g, b, a uint32)) float64 {
	ra, ga, ba, aa := a()
	rb, gb, bb, ab := b()
	over := func(c, alpha uint32) float64 {
		return float64(c)/0xffff + grey*(1-float64(alpha)/0xffff)
	}
	dr, dg, db := over(ra, aa)-over(rb, ab), over(ga, aa)-over(gb, ab), over(ba, aa)-over(bb, ab)
	return 255 * math.Sqrt(weightR*dr*dr+weightG*dg*dg+weightB*db*db)
}

// Golden compares got with the PNG at path and fails t when they look
// different beyond the options' limits, saving got in a new directory
// under os.TempDir for inspection (left in place; the message names it). With -update (or Options.Update) it writes got
// to path instead, creating its directory. A missing golden file fails
// with a hint to run with -update.
func Golden(t testing.TB, got image.Image, path string, opts ...Options) {
	t.Helper()
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Update || updateFlag() {
		var buf bytes.Buffer
		if err := png.Encode(&buf, got); err != nil {
			t.Fatalf("imagingtest: encode %s: %v", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("imagingtest: %v", err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatalf("imagingtest: %v", err)
		}
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("imagingtest: %v (run the test with -update to create it)", err)
	}
	defer func() { _ = f.Close() }()
	want, err := png.Decode(f)
	if err != nil {
		t.Fatalf("imagingtest: golden %s: %v", path, err)
	}
	d, err := Compare(got, want, or(o.PixelThreshold, DefaultPixelThreshold))
	if err != nil {
		t.Fatalf("imagingtest: %s: %v", path, err)
	}
	if d.Mean <= or(o.MaxMean, DefaultMaxMean) && d.Different <= or(o.MaxDifferent, DefaultMaxDifferent) {
		return
	}
	saved := "(not saved)"
	if dir, err := os.MkdirTemp("", "imagingtest-"); err == nil {
		var buf bytes.Buffer
		_ = png.Encode(&buf, got)
		saved = filepath.Join(dir, filepath.Base(path))
		_ = os.WriteFile(saved, buf.Bytes(), 0o644)
	}
	t.Errorf("imagingtest: %s differs: mean %.2f, %.3f%% of pixels visibly different, max %.1f; got saved as %s", path, d.Mean, d.Different*100, d.Max, saved)
}

func or(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

// updateFlag reports the test binary's -update flag, when it defines one.
func updateFlag() bool {
	f := flag.Lookup("update")
	return f != nil && f.Value.String() == "true"
}
