package imaging

import (
	"image"
	"math"

	"golang.org/x/image/draw"
)

// Smart crop scores every pixel of a small copy of the image and keeps the
// crop window with the highest total. The score follows smartcrop.js
// (github.com/jwagner/smartcrop.js): detail (edges), skin tones and
// saturated colour. Skin tones make it favour people, but it is not a face
// detector: an application that runs one stores the face as a focal point
// (contrib/imaging/focal) and uses GravityFocal instead.
const (
	// smartSide is the longest side of the copy the scores are computed
	// on: enough to find the subject, cheap at any input size.
	smartSide = 128
	// Weights of the three signals.
	smartEdgeWeight = 1.0
	smartSkinWeight = 1.8
	smartSatWeight  = 0.3
	// A pixel counts as skin or saturated only within these thresholds,
	// and not when it is near black or white.
	smartSkinThreshold = 0.8
	smartSatThreshold  = 0.4
	smartLumaMin       = 0.2
	smartLumaMax       = 0.95
)

// smartSkin is the normalised RGB direction of skin tones.
var smartSkin = [3]float64{0.78, 0.57, 0.44}

// smartWindow returns the top-left corner of the cw × ch crop of src with
// the most detail, skin and saturation. Ties go to the window nearest the
// centre, so a featureless image is cropped like GravityCenter.
func smartWindow(src image.Image, cw, ch int) (int, int) {
	sb := src.Bounds()
	k := min(1, float64(smartSide)/float64(max(sb.Dx(), sb.Dy())))
	w, h := max(1, int(float64(sb.Dx())*k+0.5)), max(1, int(float64(sb.Dy())*k+0.5))
	small := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), src, sb, draw.Src, nil)

	// Summed-area table of the scores, one row and column of zeros first.
	sat := make([]float64, (w+1)*(h+1))
	luma := make([]float64, w*h)
	for y := range h {
		for x := range w {
			i := small.PixOffset(x, y)
			r, g, b := float64(small.Pix[i])/255, float64(small.Pix[i+1])/255, float64(small.Pix[i+2])/255
			luma[y*w+x] = 0.2126*r + 0.7152*g + 0.0722*b
		}
	}
	for y := range h {
		for x := range w {
			i := small.PixOffset(x, y)
			score := smartScore(float64(small.Pix[i])/255, float64(small.Pix[i+1])/255, float64(small.Pix[i+2])/255, luma, w, h, x, y)
			sat[(y+1)*(w+1)+x+1] = score + sat[y*(w+1)+x+1] + sat[(y+1)*(w+1)+x] - sat[y*(w+1)+x]
		}
	}
	ww, wh := min(w, max(1, int(float64(cw)*k+0.5))), min(h, max(1, int(float64(ch)*k+0.5)))
	bestX, bestY, best, bestDist := 0, 0, -1.0, math.MaxFloat64
	cxMid, cyMid := float64(w-ww)/2, float64(h-wh)/2
	for y := 0; y+wh <= h; y++ {
		for x := 0; x+ww <= w; x++ {
			total := sat[(y+wh)*(w+1)+x+ww] - sat[y*(w+1)+x+ww] - sat[(y+wh)*(w+1)+x] + sat[y*(w+1)+x]
			dist := math.Hypot(float64(x)-cxMid, float64(y)-cyMid)
			if total > best+1e-9 || (math.Abs(total-best) <= 1e-9 && dist < bestDist) {
				bestX, bestY, best, bestDist = x, y, total, dist
			}
		}
	}
	x0 := min(max(sb.Min.X+int(float64(bestX)/k+0.5), sb.Min.X), sb.Max.X-cw)
	y0 := min(max(sb.Min.Y+int(float64(bestY)/k+0.5), sb.Min.Y), sb.Max.Y-ch)
	return x0, y0
}

// smartScore is one pixel's interest: its edge strength (the luma
// Laplacian), how close it is to a skin tone, and its saturation.
func smartScore(r, g, b float64, luma []float64, w, h, x, y int) float64 {
	at := func(x, y int) float64 { return luma[min(max(y, 0), h-1)*w+min(max(x, 0), w-1)] }
	l := at(x, y)
	edge := math.Abs(4*l - at(x-1, y) - at(x+1, y) - at(x, y-1) - at(x, y+1))
	score := smartEdgeWeight * edge
	if l < smartLumaMin || l > smartLumaMax {
		return score
	}
	if mag := math.Sqrt(r*r + g*g + b*b); mag > 0 {
		d := math.Sqrt((r/mag-smartSkin[0])*(r/mag-smartSkin[0]) + (g/mag-smartSkin[1])*(g/mag-smartSkin[1]) + (b/mag-smartSkin[2])*(b/mag-smartSkin[2]))
		if skin := 1 - d; skin > smartSkinThreshold {
			score += smartSkinWeight * (skin - smartSkinThreshold) / (1 - smartSkinThreshold)
		}
	}
	// hi > 0 here: a pixel that bright is never black.
	hi, lo := max(r, g, b), min(r, g, b)
	if s := (hi - lo) / hi; s > smartSatThreshold {
		score += smartSatWeight * (s - smartSatThreshold) / (1 - smartSatThreshold)
	}
	return score
}
