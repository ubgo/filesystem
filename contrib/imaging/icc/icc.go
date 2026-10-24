// Package icc converts images from the colour space of an embedded ICC
// profile to sRGB, in pure Go with only the standard library, for
// imaging.WithColourManagement:
//
//	p := imaging.NewPipeline(imaging.WithColourManagement(icc.ToSRGB))
//
// It handles RGB matrix/TRC profiles, the kind phones and cameras embed
// (Display P3, Adobe RGB, ProPhoto RGB, sRGB itself): three colorants and
// three tone curves. Profiles built from lookup tables (A2B0), and CMYK or
// grey profiles, are reported as unsupported, and imaging then keeps the
// profile in the output instead of converting. For those, a converter on
// lcms2 can be passed to WithColourManagement in its place.
package icc

import (
	"encoding/binary"
	"errors"
	"image"
	"math"

	"image/draw"
)

// ErrUnsupported is returned by Parse for a profile that is not an RGB
// matrix/TRC profile with an XYZ connection space.
var ErrUnsupported = errors.New("icc: not an RGB matrix/TRC profile")

// errMalformed is returned by Parse for a profile whose structure is
// broken.
var errMalformed = errors.New("icc: malformed profile")

// Layout of an ICC profile (ICC.1:2010, section 7).
const (
	headerBytes   = 128
	colourSpaceAt = 16
	pcsAt         = 20
	tagCountAt    = 128
	tagEntryBytes = 12
	typeHeader    = 8 // a tag's type signature and four reserved bytes
	// s15Fixed16 numbers have 16 fraction bits; u8Fixed8, 8.
	fixed16 = 1 << 16
	fixed8  = 1 << 8
	u16Max  = 0xffff
)

// Profile is a parsed RGB matrix/TRC profile.
type Profile struct {
	// toPCS turns linear RGB into XYZ (D50): its columns are the red,
	// green and blue colorants.
	toPCS [3][3]float64
	trc   [3]curve
}

// curve is a tone curve, encoded value 0..1 to linear 0..1.
type curve func(float64) float64

// Parse reads an ICC profile. A profile other than RGB with an XYZ
// connection space, colorants and tone curves is ErrUnsupported.
func Parse(data []byte) (*Profile, error) {
	if len(data) < headerBytes+4 || int(binary.BigEndian.Uint32(data)) > len(data) {
		return nil, errMalformed
	}
	if string(data[colourSpaceAt:colourSpaceAt+4]) != "RGB " || string(data[pcsAt:pcsAt+4]) != "XYZ " {
		return nil, ErrUnsupported
	}
	tags := map[string][]byte{}
	n := int(binary.BigEndian.Uint32(data[tagCountAt:]))
	for i := range n {
		at := tagCountAt + 4 + i*tagEntryBytes
		if at+tagEntryBytes > len(data) {
			return nil, errMalformed
		}
		off, size := int(binary.BigEndian.Uint32(data[at+4:])), int(binary.BigEndian.Uint32(data[at+8:]))
		if off < 0 || size < typeHeader || off+size > len(data) || off+size < off {
			return nil, errMalformed
		}
		tags[string(data[at:at+4])] = data[off : off+size]
	}
	var p Profile
	for c, names := range [3][2]string{{"rXYZ", "rTRC"}, {"gXYZ", "gTRC"}, {"bXYZ", "bTRC"}} {
		xyz, okX := tags[names[0]]
		trc, okT := tags[names[1]]
		if !okX || !okT {
			return nil, ErrUnsupported
		}
		v, err := parseXYZ(xyz)
		if err != nil {
			return nil, err
		}
		for row := range 3 {
			p.toPCS[row][c] = v[row]
		}
		if p.trc[c], err = parseCurve(trc); err != nil {
			return nil, err
		}
		if !finite(p.trc[c]) {
			return nil, errMalformed
		}
	}
	if !p.boundedProducts() {
		return nil, errMalformed
	}
	return &p, nil
}

// boundedProducts reports whether every output channel of the conversion
// stays finite for every 8-bit input: the sum of |matrix entry| times the
// largest |curve value| it can meet. Finite curves are not enough: a curve
// near 1e303 times a large colorant overflows, and two such terms of
// opposite sign make NaN.
func (p *Profile) boundedProducts() bool {
	m := p.toSRGBMatrix()
	var peak [3]float64
	for c := range 3 {
		for i := range curveSamples {
			peak[c] = math.Max(peak[c], math.Abs(p.trc[c](float64(i)/(curveSamples-1))))
		}
	}
	for row := range 3 {
		sum := 0.0
		for k := range 3 {
			sum += math.Abs(m[row][k]) * peak[k]
		}
		if math.IsInf(sum, 0) || math.IsNaN(sum) {
			return false
		}
	}
	return true
}

// toSRGBMatrix turns the profile's linear RGB into linear sRGB.
func (p *Profile) toSRGBMatrix() [3][3]float64 { return mul(invert(srgbToPCS), p.toPCS) }

// curveSamples is how many evenly spaced inputs finite tries.
const curveSamples = 256

// finite reports whether f gives a finite value for every 8-bit input. A
// crafted curve (a negative gamma, say) gives infinities or NaN, which
// would otherwise reach the conversion's table lookups.
func finite(f curve) bool {
	for i := range curveSamples {
		if v := f(float64(i) / (curveSamples - 1)); math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func s15(b []byte) float64 { return float64(int32(binary.BigEndian.Uint32(b))) / fixed16 }

func parseXYZ(b []byte) ([3]float64, error) {
	if string(b[:4]) != "XYZ " || len(b) < typeHeader+12 {
		return [3]float64{}, errMalformed
	}
	return [3]float64{s15(b[typeHeader:]), s15(b[typeHeader+4:]), s15(b[typeHeader+8:])}, nil
}

// paraCounts is how many parameters each parametricCurveType function
// has (ICC.1:2010, 10.18).
var paraCounts = [5]int{1, 3, 4, 5, 7}

func parseCurve(b []byte) (curve, error) {
	switch string(b[:4]) {
	case "curv":
		if len(b) < typeHeader+4 {
			return nil, errMalformed
		}
		n := int(binary.BigEndian.Uint32(b[typeHeader:]))
		entries := b[typeHeader+4:]
		switch {
		case n == 0:
			return func(x float64) float64 { return x }, nil
		case len(entries) < 2*n:
			return nil, errMalformed
		case n == 1:
			g := float64(binary.BigEndian.Uint16(entries)) / fixed8
			return func(x float64) float64 { return math.Pow(x, g) }, nil
		}
		table := make([]float64, n)
		for i := range table {
			table[i] = float64(binary.BigEndian.Uint16(entries[2*i:])) / u16Max
		}
		return func(x float64) float64 {
			pos := x * float64(n-1)
			i := min(int(pos), n-2)
			return table[i] + (table[i+1]-table[i])*(pos-float64(i))
		}, nil
	case "para":
		if len(b) < typeHeader+4 {
			return nil, errMalformed
		}
		kind := int(binary.BigEndian.Uint16(b[typeHeader:]))
		if kind >= len(paraCounts) || len(b) < typeHeader+4+4*paraCounts[kind] {
			return nil, errMalformed
		}
		var v [7]float64
		for i := range paraCounts[kind] {
			v[i] = s15(b[typeHeader+4+4*i:])
		}
		return parametric(kind, v), nil
	}
	return nil, errMalformed
}

// parametric is a parametricCurveType function (ICC.1:2010, table 68).
func parametric(kind int, v [7]float64) curve {
	g, a, b, c, d, e, f := v[0], v[1], v[2], v[3], v[4], v[5], v[6]
	pow := func(x float64) float64 { return math.Pow(math.Max(a*x+b, 0), g) }
	switch kind {
	case 1:
		return func(x float64) float64 {
			if x >= -b/a {
				return pow(x)
			}
			return 0
		}
	case 2:
		return func(x float64) float64 {
			if x >= -b/a {
				return pow(x) + c
			}
			return c
		}
	case 3:
		return func(x float64) float64 {
			if x >= d {
				return pow(x)
			}
			return c * x
		}
	case 4:
		return func(x float64) float64 {
			if x >= d {
				return pow(x) + e
			}
			return c*x + f
		}
	}
	return func(x float64) float64 { return math.Pow(x, g) }
}

// sRGB's colorants adapted to the D50 connection space (the sRGB IEC
// 61966-2.1 profile's rXYZ, gXYZ and bXYZ), as columns.
var srgbToPCS = [3][3]float64{
	{0.4360747, 0.3850649, 0.1430804},
	{0.2225045, 0.7168786, 0.0606169},
	{0.0139322, 0.0971045, 0.7141733},
}

// Output encoding table size: linear 0..1 in this many steps.
const encodeSteps = 4096

// ToSRGB returns img converted to sRGB as an *image.NRGBA (alpha kept),
// with p's tone curves and colorants and the sRGB curve on output; colours
// outside sRGB are clipped.
func (p *Profile) ToSRGB(img image.Image) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	m := p.toSRGBMatrix()
	var decode [3][256]float64
	for c := range 3 {
		for v := range 256 {
			decode[c][v] = p.trc[c](float64(v) / 255)
		}
	}
	var encode [encodeSteps + 1]uint8
	for i := range encode {
		encode[i] = uint8(math.Round(255 * srgbEncode(float64(i)/encodeSteps)))
	}
	for i := 0; i < len(out.Pix); i += 4 {
		r, g, bl := decode[0][out.Pix[i]], decode[1][out.Pix[i+1]], decode[2][out.Pix[i+2]]
		for c := range 3 {
			lin := m[c][0]*r + m[c][1]*g + m[c][2]*bl
			out.Pix[i+c] = encode[int(math.Round(clampUnit(lin)*encodeSteps))]
		}
	}
	return out
}

// clampUnit limits v to 0..1, and maps NaN to 0: Parse refuses profiles
// that could produce one, and this keeps a table index valid regardless,
// since converting NaN to an integer is undefined (it panics on amd64).
func clampUnit(v float64) float64 {
	if !(v > 0) { // also NaN, which math.Max would pass through
		return 0
	}
	return math.Min(v, 1)
}

// ToSRGB is an imaging.ColourConverter: it converts img when profile is a
// supported RGB matrix/TRC profile, and reports ok false (so imaging keeps
// the profile) for any other or malformed one. It never fails.
func ToSRGB(profile []byte, img image.Image) (image.Image, bool, error) {
	p, err := Parse(profile)
	if err != nil {
		return nil, false, nil
	}
	return p.ToSRGB(img), true, nil
}

// srgbEncode is the sRGB transfer function, linear to encoded.
func srgbEncode(x float64) float64 {
	if x <= 0.0031308 {
		return 12.92 * x
	}
	return 1.055*math.Pow(x, 1/2.4) - 0.055
}

func mul(a, b [3][3]float64) [3][3]float64 {
	var out [3][3]float64
	for i := range 3 {
		for j := range 3 {
			for k := range 3 {
				out[i][j] += a[i][k] * b[k][j]
			}
		}
	}
	return out
}

// invert inverts a non-singular 3 × 3 matrix by its adjugate.
func invert(m [3][3]float64) [3][3]float64 {
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) - m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) + m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	return [3][3]float64{
		{(m[1][1]*m[2][2] - m[1][2]*m[2][1]) / det, (m[0][2]*m[2][1] - m[0][1]*m[2][2]) / det, (m[0][1]*m[1][2] - m[0][2]*m[1][1]) / det},
		{(m[1][2]*m[2][0] - m[1][0]*m[2][2]) / det, (m[0][0]*m[2][2] - m[0][2]*m[2][0]) / det, (m[0][2]*m[1][0] - m[0][0]*m[1][2]) / det},
		{(m[1][0]*m[2][1] - m[1][1]*m[2][0]) / det, (m[0][1]*m[2][0] - m[0][0]*m[2][1]) / det, (m[0][0]*m[1][1] - m[0][1]*m[1][0]) / det},
	}
}
