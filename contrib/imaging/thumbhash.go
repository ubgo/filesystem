package imaging

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"math"

	"golang.org/x/image/draw"
)

// ThumbHash constants, from the format's reference implementation
// (github.com/evanw/thumbhash), which every decoder follows.
const (
	// thumbHashMaxSide is the largest side the encoder reads: the image is
	// scaled to fit 100 × 100 first, as the format requires.
	thumbHashMaxSide = 100
	// Luminance components along the longer side, with and without an
	// alpha channel (alpha takes the bits from luminance).
	thumbHashLumaOpaque = 7
	thumbHashLumaAlpha  = 5
	// The chroma channels use 3 × 3 components; alpha 5 × 5.
	thumbHashChroma = 3
	thumbHashAlpha  = 5
)

// ThumbHash returns the ThumbHash of img: about 25 bytes from which a
// browser draws a blurred placeholder with the image's colours, aspect
// ratio and alpha while the real image loads (decoders exist for
// JavaScript, Swift, Kotlin and Go). An image larger than 100 × 100 is
// scaled down to fit first. The output matches the reference encoder's
// header byte for byte; an AC component lying exactly half way between two
// quantisation steps may round the other way, because the last bit of
// Go's cosine can differ from a JavaScript engine's, which no decoder can
// show.
func ThumbHash(img image.Image) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > thumbHashMaxSide || h > thumbHashMaxSide {
		scale := float64(thumbHashMaxSide) / float64(max(w, h))
		w, h = max(1, int(float64(w)*scale+0.5)), max(1, int(float64(h)*scale+0.5))
	}
	nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(nrgba, nrgba.Bounds(), img, b.Min, draw.Src)
	} else {
		draw.CatmullRom.Scale(nrgba, nrgba.Bounds(), img, b, draw.Src, nil)
	}
	return thumbHashRGBA(w, h, nrgba.Pix)
}

// Preview decodes the image in r with proc (EXIF orientation applied,
// within opts' limits) and returns a copy that fits 100 × 100: what
// ThumbHash and DominantColour need, from one decode, without holding the
// full image afterwards. The errors are proc.Process's.
func Preview(proc Processor, r io.Reader, opts Options) (image.Image, error) {
	res, err := proc.Process(r, Transform{Mode: ModeFit, Width: thumbHashMaxSide, Height: thumbHashMaxSide, Format: FormatPNG}, opts)
	if err != nil {
		return nil, err
	}
	return png.Decode(bytes.NewReader(res.Data))
}

// ThumbHashOf returns the ThumbHash of the Preview of r.
func ThumbHashOf(proc Processor, r io.Reader, opts Options) ([]byte, error) {
	small, err := Preview(proc, r, opts)
	if err != nil {
		return nil, err
	}
	return ThumbHash(small), nil
}

// jsRound is JavaScript's Math.round for the non-negative values the
// format rounds: halves go up, as the reference encoder's do.
func jsRound(x float64) int { return int(math.Floor(x + 0.5)) }

// thumbHashRGBA encodes w × h non-premultiplied RGBA pixels (each side at
// most 100), following the reference rgbaToThumbHash step by step.
func thumbHashRGBA(w, h int, rgba []byte) []byte {
	n := w * h
	var avgR, avgG, avgB, avgA float64
	for i := range n {
		alpha := float64(rgba[4*i+3]) / 255
		avgR += alpha / 255 * float64(rgba[4*i])
		avgG += alpha / 255 * float64(rgba[4*i+1])
		avgB += alpha / 255 * float64(rgba[4*i+2])
		avgA += alpha
	}
	if avgA > 0 {
		avgR, avgG, avgB = avgR/avgA, avgG/avgA, avgB/avgA
	}
	hasAlpha := avgA < float64(n)
	limit := thumbHashLumaOpaque
	if hasAlpha {
		limit = thumbHashLumaAlpha
	}
	longest := float64(max(w, h))
	lx := max(1, jsRound(float64(limit*w)/longest))
	ly := max(1, jsRound(float64(limit*h)/longest))

	// Composite onto the average colour and split into luminance, two
	// chroma channels and alpha.
	l, p, q, a := make([]float64, n), make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range n {
		alpha := float64(rgba[4*i+3]) / 255
		r := avgR*(1-alpha) + alpha/255*float64(rgba[4*i])
		g := avgG*(1-alpha) + alpha/255*float64(rgba[4*i+1])
		bl := avgB*(1-alpha) + alpha/255*float64(rgba[4*i+2])
		l[i] = (r + g + bl) / 3
		p[i] = (r+g)/2 - bl
		q[i] = r - g
		a[i] = alpha
	}

	encode := func(channel []float64, nx, ny int) (dc float64, ac []float64, scale float64) {
		fx := make([]float64, w)
		for cy := range ny {
			for cx := 0; cx*ny < nx*(ny-cy); cx++ {
				for x := range w {
					fx[x] = math.Cos(math.Pi / float64(w) * float64(cx) * (float64(x) + 0.5))
				}
				f := 0.0
				for y := range h {
					fy := math.Cos(math.Pi / float64(h) * float64(cy) * (float64(y) + 0.5))
					for x := range w {
						f += channel[x+y*w] * fx[x] * fy
					}
				}
				f /= float64(n)
				if cx > 0 || cy > 0 {
					ac = append(ac, f)
					scale = math.Max(scale, math.Abs(f))
				} else {
					dc = f
				}
			}
		}
		if scale > 0 {
			for i := range ac {
				ac[i] = 0.5 + 0.5/scale*ac[i]
			}
		}
		return dc, ac, scale
	}
	lDC, lAC, lScale := encode(l, max(3, lx), max(3, ly))
	pDC, pAC, pScale := encode(p, thumbHashChroma, thumbHashChroma)
	qDC, qAC, qScale := encode(q, thumbHashChroma, thumbHashChroma)

	landscape := w > h
	header24 := jsRound(63*lDC) | jsRound(31.5+31.5*pDC)<<6 | jsRound(31.5+31.5*qDC)<<12 | jsRound(31*lScale)<<18
	if hasAlpha {
		header24 |= 1 << 23
	}
	side := lx
	if landscape {
		side = ly
	}
	header16 := side | jsRound(63*pScale)<<3 | jsRound(63*qScale)<<9
	if landscape {
		header16 |= 1 << 15
	}
	hash := []byte{byte(header24), byte(header24 >> 8), byte(header24 >> 16), byte(header16), byte(header16 >> 8)}
	channels := [][]float64{lAC, pAC, qAC}
	if hasAlpha {
		aDC, aAC, aScale := encode(a, thumbHashAlpha, thumbHashAlpha)
		hash = append(hash, byte(jsRound(15*aDC)|jsRound(15*aScale)<<4))
		channels = append(channels, aAC)
	}
	start := len(hash)
	i := 0
	for _, ac := range channels {
		for _, f := range ac {
			at := start + i>>1
			for len(hash) <= at {
				hash = append(hash, 0)
			}
			hash[at] |= byte(jsRound(15*f) << ((i & 1) << 2))
			i++
		}
	}
	return hash
}
