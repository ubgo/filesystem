package imaging_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"io"
	"runtime"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/upload"
)

// fakeProfile is a stand-in ICC profile: imaging treats profiles as opaque
// bytes, only the converter reads them. It is long enough to need two
// JPEG segments.
var fakeProfile = bytes.Repeat([]byte("ICCDATA."), 9000)

// withICCJPEG inserts profile into a JPEG as APP2 segments in reverse
// order, so reassembly must follow the sequence numbers.
func withICCJPEG(t *testing.T, jpg, profile []byte) []byte {
	t.Helper()
	const max = 65519
	var segs [][]byte
	count := (len(profile) + max - 1) / max
	for i := range count {
		part := profile[i*max : min((i+1)*max, len(profile))]
		s := []byte{0xFF, 0xE2}
		s = binary.BigEndian.AppendUint16(s, uint16(2+14+len(part)))
		s = append(append(append(s, "ICC_PROFILE\x00"...), byte(i+1), byte(count)), part...)
		segs = append([][]byte{s}, segs...)
	}
	out := append([]byte{}, jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

func withICCPNG(t *testing.T, p, profile []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(profile)
	_ = zw.Close()
	body := append([]byte("P3\x00\x00"), z.Bytes()...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	chunk = append(append(chunk, "iCCP"...), body...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("iCCP"), body...)))
	return append(append(append([]byte{}, p[:33]...), chunk...), p[33:]...)
}

// shift is a fake ColourConverter: it adds 50 to red, so the test can see
// whether it ran; unsupported profiles are those starting "CMYK".
type shift struct{ calls int }

func (s *shift) convert(profile []byte, img image.Image) (image.Image, bool, error) {
	s.calls++
	if bytes.HasPrefix(profile, []byte("CMYK")) {
		return nil, false, nil
	}
	if bytes.HasPrefix(profile, []byte("FAIL")) {
		return nil, false, errors.New("converter broke")
	}
	b := img.Bounds()
	out := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			c.R = uint8(min(int(c.R)+50, 255))
			out.SetNRGBA(x, y, c)
		}
	}
	return out, true, nil
}

func greyPNG(t *testing.T) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	fill(img, img.Bounds(), color.NRGBA{100, 100, 100, 255})
	return encodePNG(t, img)
}

func TestEmbeddedProfile(t *testing.T) {
	jpg := withICCJPEG(t, photo(t, 8, 8, jpegEnc), fakeProfile)
	if p, ok := imaging.EmbeddedProfile(jpg); !ok || !bytes.Equal(p, fakeProfile) {
		t.Errorf("JPEG: %d bytes, %v", len(p), ok)
	}
	if p, ok := imaging.EmbeddedProfile(withICCPNG(t, greyPNG(t), fakeProfile)); !ok || !bytes.Equal(p, fakeProfile) {
		t.Errorf("PNG: %d bytes, %v", len(p), ok)
	}
	plainPNG := greyPNG(t)
	missingChunk := withICCJPEG(t, photo(t, 8, 8, jpegEnc), fakeProfile)
	// Drop the second segment's sequence: mark it chunk 3 of 2.
	missingChunk[2+4+12] = 3
	badZlib := withICCPNG(t, plainPNG, fakeProfile)
	badZlib[33+8+4] ^= 0xff // corrupt the zlib header
	// Over MaxProfileBytes: a PNG profile that inflates past it (a few
	// kilobytes of zlib), and JPEG segments that add up past it.
	bomb := withICCPNG(t, plainPNG, make([]byte, imaging.MaxProfileBytes+1))
	bigJPEG := withICCJPEG(t, photo(t, 8, 8, jpegEnc), make([]byte, imaging.MaxProfileBytes+1))
	if len(bomb) > 64<<10 {
		t.Fatalf("the bomb is %d bytes; it should be small", len(bomb))
	}
	for name, data := range map[string][]byte{
		"inflates past the cap": bomb,
		"segments past the cap": bigJPEG,
		"no profile JPEG":       photo(t, 8, 8, jpegEnc),
		"no profile PNG":        plainPNG,
		"GIF":                   []byte("GIF89a"),
		"missing chunk":         missingChunk,
		"bad zlib":              badZlib,
		"cut zlib":              cutZlib(plainPNG),
		"no NUL":                append(append([]byte{}, plainPNG[:33]...), 0, 0, 0, 2, 'i', 'C', 'C', 'P', 'a', 'b', 0, 0, 0, 0),
		"cut chunk":             append(append([]byte{}, plainPNG[:33]...), 0, 0, 0, 99, 'i', 'C', 'C', 'P'),
		"short PNG":             plainPNG[:20],
	} {
		if _, ok := imaging.EmbeddedProfile(data); ok {
			t.Errorf("%s: found a profile", name)
		}
	}
}

func TestColourManagement(t *testing.T) {
	src := withICCPNG(t, greyPNG(t), fakeProfile)
	toPNG := imaging.Transform{Mode: imaging.ModeFit, Width: 8, Height: 8, Format: imaging.FormatPNG}
	red := func(data []byte) uint8 {
		img := decodeOrFail(t, data)
		return uint8(rgb8(img.At(4, 4))[0])
	}
	// Off by default: no conversion, profile dropped.
	res, _ := imaging.Process(bytes.NewReader(src), toPNG, imaging.Options{})
	if _, ok := imaging.EmbeddedProfile(res.Data); ok || red(res.Data) != 100 {
		t.Errorf("default kept or converted the profile")
	}
	// Converted to sRGB: the converter ran and no profile is written.
	conv := &shift{}
	p := imaging.NewPipeline(imaging.WithColourManagement(conv.convert))
	res, err := p.Process(bytes.NewReader(src), toPNG, imaging.Options{})
	if _, ok := imaging.EmbeddedProfile(res.Data); err != nil || ok || red(res.Data) != 150 || conv.calls != 1 {
		t.Errorf("converted: %v, red %d, calls %d", err, red(res.Data), conv.calls)
	}
	// No profile: the converter is not called.
	if _, err := p.Process(bytes.NewReader(greyPNG(t)), toPNG, imaging.Options{}); err != nil || conv.calls != 1 {
		t.Errorf("no profile: %v, calls %d", err, conv.calls)
	}
	// Unsupported profile, or no converter: the profile is kept, in JPEG
	// and PNG output.
	cmyk := append([]byte("CMYK"), fakeProfile...)
	for name, pl := range map[string]*imaging.Pipeline{"unsupported": p, "keep only": imaging.NewPipeline(imaging.WithColourManagement(nil))} {
		for _, f := range []imaging.Format{imaging.FormatPNG, imaging.FormatJPEG} {
			tr := toPNG
			tr.Format = f
			res, err := pl.Process(bytes.NewReader(withICCPNG(t, greyPNG(t), cmyk)), tr, imaging.Options{})
			if kept, ok := imaging.EmbeddedProfile(res.Data); err != nil || !ok || !bytes.Equal(kept, cmyk) || red(res.Data) < 95 || red(res.Data) > 105 {
				t.Errorf("%s %s: kept %v (%d bytes), %v", name, f, ok, len(kept), err)
			}
		}
	}
	// Formats that cannot carry a profile pass through without one.
	gif := toPNG
	gif.Format = imaging.FormatGIF
	if res, err := imaging.NewPipeline(imaging.WithColourManagement(nil)).Process(bytes.NewReader(src), gif, imaging.Options{}); err != nil || res.ContentType != "image/gif" {
		t.Errorf("GIF: %v", err)
	}
	// A converter error fails the call.
	if _, err := p.Process(bytes.NewReader(withICCPNG(t, greyPNG(t), append([]byte("FAIL"), 1))), toPNG, imaging.Options{}); err == nil {
		t.Error("a converter error was ignored")
	}
}

// TestSanitizeManagesColour pins the same in Sanitize with WithPipeline.
func TestSanitizeManagesColour(t *testing.T) {
	conv := &shift{}
	proc := imaging.Sanitize(imaging.Options{}, imaging.WithPipeline(imaging.NewPipeline(imaging.WithColourManagement(conv.convert))))
	sanitize := func(data []byte, ct string) []byte {
		r, _, err := proc(t.Context(), upload.Info{Filename: "a", ContentType: ct}, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(r)
		return out
	}
	if out := sanitize(withICCPNG(t, greyPNG(t), fakeProfile), "image/png"); conv.calls != 1 || rgb8(decodeOrFail(t, out).At(1, 1))[0] != 150 {
		t.Errorf("converted = calls %d", conv.calls)
	}
	cmyk := append([]byte("CMYK"), fakeProfile...)
	out := sanitize(withICCJPEG(t, photo(t, 8, 8, jpegEnc), cmyk), "image/jpeg")
	if kept, ok := imaging.EmbeddedProfile(out); !ok || !bytes.Equal(kept, cmyk) {
		t.Errorf("JPEG kept %v", ok)
	}
	if _, _, err := proc(t.Context(), upload.Info{ContentType: "image/png"}, bytes.NewReader(withICCPNG(t, greyPNG(t), []byte("FAIL")))); err == nil {
		t.Error("a converter error was ignored")
	}
}

// cutZlib is png with an iCCP chunk whose zlib stream stops part-way.
func cutZlib(p []byte) []byte {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(fakeProfile)
	_ = zw.Close()
	body := append([]byte("P3\x00\x00"), z.Bytes()[:20]...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	chunk = append(append(chunk, "iCCP"...), body...)
	chunk = append(chunk, 0, 0, 0, 0)
	return append(append(append([]byte{}, p[:33]...), chunk...), p[33:]...)
}

func decodeOrFail(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestProfileBombIsNotInflated pins the read bound: a PNG profile that
// would inflate to 128 MiB costs at most a little over MaxProfileBytes.
func TestProfileBombIsNotInflated(t *testing.T) {
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	zero := make([]byte, 1<<20)
	for range 128 {
		_, _ = zw.Write(zero)
	}
	_ = zw.Close()
	body := append([]byte("bomb\x00\x00"), z.Bytes()...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	chunk = append(append(chunk, "iCCP"...), body...)
	chunk = append(chunk, 0, 0, 0, 0)
	p := greyPNG(t)
	data := append(append(append([]byte{}, p[:33]...), chunk...), p[33:]...)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, ok := imaging.EmbeddedProfile(data); ok {
		t.Fatal("a 128 MiB profile was accepted")
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 4*imaging.MaxProfileBytes {
		t.Errorf("reading the bomb allocated %d bytes", grew)
	}
}
