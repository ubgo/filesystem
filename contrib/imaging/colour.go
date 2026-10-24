package imaging

import (
	"fmt"
	"image"
	"image/color"
)

// Dominant colour is found by sampling a grid of pixels, grouping them into
// colour buckets and averaging the fullest bucket, so a photo's main
// colour wins over the mean (which turns every photo brown).
const (
	// colourGrid is how many samples per side are taken at most.
	colourGrid = 64
	// colourBits is the precision of a bucket per channel: 16 levels.
	colourBits = 4
	// colourOpaque is the alpha below which a sample is ignored, so a
	// logo's transparent background does not win.
	colourOpaque = 128
)

// DominantColour returns the most common colour of img (the average of the
// fullest of 4,096 colour buckets over an even grid of at most 64 × 64
// samples), for a placeholder background or a theme colour. ok is false
// when no sampled pixel is at least half opaque. The result depends only
// on the pixels, so it is the same on every run.
func DominantColour(img image.Image) (c color.NRGBA, ok bool) {
	b := img.Bounds()
	stepX, stepY := max(1, b.Dx()/colourGrid), max(1, b.Dy()/colourGrid)
	type bucket struct{ n, r, g, b int }
	buckets := map[int]*bucket{}
	for y := b.Min.Y + stepY/2; y < b.Max.Y; y += stepY {
		for x := b.Min.X + stepX/2; x < b.Max.X; x += stepX {
			p := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if p.A < colourOpaque {
				continue
			}
			key := int(p.R>>(8-colourBits))<<(2*colourBits) | int(p.G>>(8-colourBits))<<colourBits | int(p.B>>(8-colourBits))
			bk := buckets[key]
			if bk == nil {
				bk = &bucket{}
				buckets[key] = bk
			}
			bk.n++
			bk.r += int(p.R)
			bk.g += int(p.G)
			bk.b += int(p.B)
		}
	}
	bestKey, best := -1, (*bucket)(nil)
	for key, bk := range buckets {
		// Ties go to the lower key, so the map's order cannot matter.
		if best == nil || bk.n > best.n || (bk.n == best.n && key < bestKey) {
			bestKey, best = key, bk
		}
	}
	if best == nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{uint8(best.r / best.n), uint8(best.g / best.n), uint8(best.b / best.n), 0xff}, true
}

// Hex writes c as a CSS colour, "#rrggbb" (alpha is dropped).
func Hex(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }
