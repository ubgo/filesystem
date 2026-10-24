package imaging

import (
	"image"
	"math"
)

// Focal is a point of interest in an image, as fractions of its upright
// width and height: {0.5, 0.5} is the centre, {0, 0} the top left. It is
// resolution-independent, so one stored point serves every size.
type Focal struct {
	X, Y float64
}

// Valid reports whether both coordinates are within 0..1.
func (f Focal) Valid() bool {
	return f.X >= 0 && f.X <= 1 && f.Y >= 0 && f.Y <= 1 && !math.IsNaN(f.X) && !math.IsNaN(f.Y)
}

// focalWindow places a cw × ch crop of sb with the focal point as near its
// centre as the image's edges allow.
func focalWindow(sb image.Rectangle, f Focal, cw, ch int) (int, int) {
	cx := sb.Min.X + int(f.X*float64(sb.Dx())+0.5)
	cy := sb.Min.Y + int(f.Y*float64(sb.Dy())+0.5)
	x0 := min(max(cx-cw/2, sb.Min.X), sb.Max.X-cw)
	y0 := min(max(cy-ch/2, sb.Min.Y), sb.Max.Y-ch)
	return x0, y0
}
