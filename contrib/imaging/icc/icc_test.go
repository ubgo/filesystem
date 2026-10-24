package icc_test

import (
	"encoding/binary"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging/icc"
)

// fixed writes v as s15Fixed16.
func fixed(v float64) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(int32(math.Round(v*65536))))
}

func xyzTag(x, y, z float64) []byte {
	b := append([]byte("XYZ "), 0, 0, 0, 0)
	return append(append(append(b, fixed(x)...), fixed(y)...), fixed(z)...)
}

// para builds a parametricCurveType tag.
func para(kind uint16, params ...float64) []byte {
	b := append([]byte("para"), 0, 0, 0, 0)
	b = binary.BigEndian.AppendUint16(b, kind)
	b = append(b, 0, 0)
	for _, p := range params {
		b = append(b, fixed(p)...)
	}
	return b
}

// curv builds a curveType tag of raw 16-bit entries.
func curv(entries ...uint16) []byte {
	b := append([]byte("curv"), 0, 0, 0, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		b = binary.BigEndian.AppendUint16(b, e)
	}
	return b
}

// srgbCurve is sRGB's transfer function as parametric type 3.
var srgbCurve = para(3, 2.4, 1/1.055, 0.055/1.055, 1/12.92, 0.04045)

// profile assembles an ICC profile with the given colour space, PCS,
// colorants (columns) and one curve for all three channels.
func profile(space, pcs string, colorants [3][3]float64, trc []byte, extra ...string) []byte {
	tags := []struct {
		sig  string
		data []byte
	}{
		{"rXYZ", xyzTag(colorants[0][0], colorants[1][0], colorants[2][0])},
		{"gXYZ", xyzTag(colorants[0][1], colorants[1][1], colorants[2][1])},
		{"bXYZ", xyzTag(colorants[0][2], colorants[1][2], colorants[2][2])},
		{"rTRC", trc}, {"gTRC", trc}, {"bTRC", trc},
	}
	tags = tags[:6-len(extra)]
	head := make([]byte, 128)
	copy(head[16:], space)
	copy(head[20:], pcs)
	table := binary.BigEndian.AppendUint32(nil, uint32(len(tags)))
	var body []byte
	off := 128 + 4 + 12*len(tags)
	for _, tg := range tags {
		table = append(table, tg.sig...)
		table = binary.BigEndian.AppendUint32(table, uint32(off+len(body)))
		table = binary.BigEndian.AppendUint32(table, uint32(len(tg.data)))
		body = append(body, tg.data...)
	}
	out := append(append(head, table...), body...)
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	return out
}

var (
	srgbColorants = [3][3]float64{{0.4360747, 0.3850649, 0.1430804}, {0.2225045, 0.7168786, 0.0606169}, {0.0139322, 0.0971045, 0.7141733}}
	// Display P3 colorants adapted to D50, as in Apple's profile.
	p3Colorants = [3][3]float64{{0.5151, 0.2920, 0.1571}, {0.2412, 0.6922, 0.0666}, {-0.0011, 0.0419, 0.7841}}
)

func swatch(c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return img
}

func convert(t *testing.T, prof []byte, c color.NRGBA) color.NRGBA {
	t.Helper()
	out, ok, err := icc.ToSRGB(prof, swatch(c))
	if !ok || err != nil {
		t.Fatalf("ToSRGB ok %v, %v", ok, err)
	}
	return out.(*image.NRGBA).NRGBAAt(1, 1)
}

func near(a, b color.NRGBA, tol int) bool {
	for _, d := range []int{int(a.R) - int(b.R), int(a.G) - int(b.G), int(a.B) - int(b.B), int(a.A) - int(b.A)} {
		if d > tol || -d > tol {
			return false
		}
	}
	return true
}

func srgbDecode(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func srgbEncode(v float64) uint8 {
	v = math.Min(math.Max(v, 0), 1)
	if v <= 0.0031308 {
		return uint8(math.Round(255 * 12.92 * v))
	}
	return uint8(math.Round(255 * (1.055*math.Pow(v, 1/2.4) - 0.055)))
}

// TestSRGBIsIdentity pins that an sRGB profile changes nothing visible.
func TestSRGBIsIdentity(t *testing.T) {
	prof := profile("RGB ", "XYZ ", srgbColorants, srgbCurve)
	for _, c := range []color.NRGBA{{0, 0, 0, 255}, {255, 255, 255, 255}, {200, 100, 50, 128}, {13, 140, 250, 255}} {
		if got := convert(t, prof, c); !near(got, c, 1) {
			t.Errorf("%v → %v", c, got)
		}
	}
}

// TestDisplayP3 checks the conversion against the published linear
// Display P3 to sRGB matrix (IEC 61966-2-1 primaries, D65), an
// independent reference: a P3 photo's colours must come out as sRGB shows
// them.
func TestDisplayP3(t *testing.T) {
	ref := [3][3]float64{{1.2249, -0.2247, 0}, {-0.0420, 1.0419, 0}, {-0.0197, -0.0786, 1.0979}}
	prof := profile("RGB ", "XYZ ", p3Colorants, srgbCurve)
	for _, c := range []color.NRGBA{{180, 120, 90, 255}, {120, 160, 140, 255}, {128, 128, 128, 255}, {90, 100, 200, 255}} {
		lin := [3]float64{srgbDecode(float64(c.R) / 255), srgbDecode(float64(c.G) / 255), srgbDecode(float64(c.B) / 255)}
		var want color.NRGBA
		want.A = 255
		for i, dst := range []*uint8{&want.R, &want.G, &want.B} {
			*dst = srgbEncode(ref[i][0]*lin[0] + ref[i][1]*lin[1] + ref[i][2]*lin[2])
		}
		if got := convert(t, prof, c); !near(got, want, 3) {
			t.Errorf("P3 %v → %v, reference %v", c, got, want)
		}
	}
}

// TestToneCurves pins every curve encoding: with sRGB colorants the
// output is the sRGB encoding of the curve's linear value.
func TestToneCurves(t *testing.T) {
	gamma := func(g float64) func(float64) float64 { return func(x float64) float64 { return math.Pow(x, g) } }
	cases := map[string]struct {
		tag []byte
		f   func(float64) float64
	}{
		"curv identity": {curv(), func(x float64) float64 { return x }},
		"curv gamma":    {curv(0x0233), gamma(563.0 / 256)}, // Adobe RGB's 2.2
		"curv table":    {curv(0, 0x4000, 0xffff), func(x float64) float64 { return interp(x) }},
		"para 0":        {para(0, 1.8), gamma(1.8)},
		"para 1":        {para(1, 2.0, 1, 0), gamma(2.0)},
		"para 2":        {para(2, 2.0, 1, 0, 0.1), func(x float64) float64 { return x*x + 0.1 }},
		"para 3":        {srgbCurve, srgbDecode},
		"para 4": {para(4, 2.0, 1, 0, 0.5, 0.2, 0.05, 0.01), func(x float64) float64 {
			if x >= 0.2 {
				return x*x + 0.05
			}
			return 0.5*x + 0.01
		}},
		"para 1 below": {para(1, 1.0, 1, -0.5), func(x float64) float64 {
			if x >= 0.5 {
				return x - 0.5
			}
			return 0
		}},
		"para 2 below": {para(2, 1.0, 1, -0.5, 0.1), func(x float64) float64 {
			if x >= 0.5 {
				return x - 0.5 + 0.1
			}
			return 0.1
		}},
	}
	for name, c := range cases {
		prof := profile("RGB ", "XYZ ", srgbColorants, c.tag)
		for _, v := range []uint8{0, 5, 30, 100, 128, 200, 255} {
			want := srgbEncode(c.f(float64(v) / 255))
			if got := convert(t, prof, color.NRGBA{v, v, v, 255}); !near(got, color.NRGBA{want, want, want, 255}, 1) {
				t.Errorf("%s at %d: %v, want %d", name, v, got, want)
			}
		}
	}
}

// interp is the piecewise-linear table 0, 0x4000, 0xffff.
func interp(x float64) float64 {
	if x < 0.5 {
		return x * 2 * 0x4000 / 65535
	}
	return (0x4000 + (x-0.5)*2*(65535-0x4000)) / 65535
}

func TestUnsupportedAndMalformed(t *testing.T) {
	good := profile("RGB ", "XYZ ", srgbColorants, srgbCurve)
	unsupported := map[string][]byte{
		"cmyk":         profile("CMYK", "XYZ ", srgbColorants, srgbCurve),
		"lab pcs":      profile("RGB ", "Lab ", srgbColorants, srgbCurve),
		"missing tags": profile("RGB ", "XYZ ", srgbColorants, srgbCurve, "drop"),
	}
	for name, p := range unsupported {
		if _, err := icc.Parse(p); err != icc.ErrUnsupported {
			t.Errorf("%s = %v", name, err)
		}
	}
	short := func(b []byte, n int) []byte { return b[:n] }
	badSize := append([]byte{}, good...)
	binary.BigEndian.PutUint32(badSize, uint32(len(good)+1))
	badTag := append([]byte{}, good...)
	binary.BigEndian.PutUint32(badTag[128+4+4:], uint32(len(good))) // first tag's offset past the end
	manyTags := append([]byte{}, good...)
	binary.BigEndian.PutUint32(manyTags[128:], 1000)
	// A header that announces one tag and ends before its entry.
	cut := append(append([]byte{}, good[:128]...), 0, 0, 0, 1)
	binary.BigEndian.PutUint32(cut, uint32(len(cut)))
	malformed := map[string][]byte{
		"cut table":   cut,
		"short":       short(good, 100),
		"size":        badSize,
		"tag offset":  badTag,
		"tag table":   manyTags,
		"xyz type":    profile("RGB ", "XYZ ", srgbColorants, srgbCurve)[:0],
		"bad xyz":     withTag(good, "rXYZ", []byte("curv\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00")),
		"short xyz":   withTag(good, "rXYZ", []byte("XYZ \x00\x00\x00\x00")),
		"curve type":  profile("RGB ", "XYZ ", srgbColorants, []byte("text\x00\x00\x00\x00")),
		"short curv":  profile("RGB ", "XYZ ", srgbColorants, []byte("curv\x00\x00\x00\x00")),
		"curv count":  profile("RGB ", "XYZ ", srgbColorants, []byte("curv\x00\x00\x00\x00\x00\x00\x00\x05\x00\x01")),
		"short para":  profile("RGB ", "XYZ ", srgbColorants, []byte("para\x00\x00\x00\x00")),
		"para kind":   profile("RGB ", "XYZ ", srgbColorants, para(9, 1)),
		"para params": profile("RGB ", "XYZ ", srgbColorants, para(4, 1, 1)),
		// A negative gamma is infinite at 0: refused at Parse, never a NaN
		// index into the conversion's tables.
		"negative gamma": profile("RGB ", "XYZ ", srgbColorants, para(0, -1)),
		// Finite curves (32767·x to the 68th is 1.1e307 at full scale) times
		// huge colorants overflow, and terms of opposite sign make NaN in
		// the matrix step: refused at Parse too.
		"overflowing products": profile("RGB ", "XYZ ", [3][3]float64{{30000, 0, 0}, {0, 30000, 0}, {0, 0, 30000}}, para(1, 68, 32767, 0)),
	}
	for name, p := range malformed {
		if _, err := icc.Parse(p); err == nil || err == icc.ErrUnsupported {
			t.Errorf("%s = %v", name, err)
		}
		if out, ok, err := icc.ToSRGB(p, swatch(color.NRGBA{})); ok || out != nil || err != nil {
			t.Errorf("%s: ToSRGB = %v %v", name, ok, err)
		}
	}
}

// withTag replaces the data of tag sig, keeping its size field honest by
// appending the new data at the end.
func withTag(p []byte, sig string, data []byte) []byte {
	out := append([]byte{}, p...)
	n := int(binary.BigEndian.Uint32(out[128:]))
	for i := range n {
		at := 128 + 4 + 12*i
		if string(out[at:at+4]) == sig {
			binary.BigEndian.PutUint32(out[at+4:], uint32(len(out)))
			binary.BigEndian.PutUint32(out[at+8:], uint32(len(data)))
		}
	}
	out = append(out, data...)
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	return out
}
