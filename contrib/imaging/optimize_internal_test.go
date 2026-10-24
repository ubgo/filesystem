package imaging

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

func optimize(t *testing.T, o OptimizeOptions, contentType string, data []byte) ([]byte, error) {
	t.Helper()
	r, info, err := Optimize(Options{}, o)(t.Context(), upload.Info{Filename: "f", ContentType: contentType}, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if info.ContentType != contentType || info.Filename != "f" {
		t.Errorf("info changed: %+v", info)
	}
	b, _ := io.ReadAll(r)
	return b, nil
}

// noisy is a w × h image of seeded noise, which compresses badly, so a
// recompression at a lower quality is clearly smaller.
func noisy(w, h int) *image.RGBA {
	rng := rand.New(rand.NewPCG(1, 2))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(rng.IntN(256))
	}
	return img
}

func encodeJPEG(t *testing.T, img image.Image, q int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// segment is one JPEG marker segment.
func segment(marker byte, body string) []byte {
	out := []byte{jpegMarker, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(len(body)+2))
	return append(out, body...)
}

// withSegments inserts segs after SOI, and payload after the end.
func withSegments(jpg []byte, payload string, segs ...[]byte) []byte {
	out := append([]byte{}, jpg[:2]...)
	for _, s := range segs {
		out = append(out, s...)
	}
	out = append(out, jpg[2:]...)
	return append(out, payload...)
}

func samePixels(t *testing.T, a, b []byte) bool {
	t.Helper()
	ia, err1 := jpeg.Decode(bytes.NewReader(a))
	ib, err2 := jpeg.Decode(bytes.NewReader(b))
	if err1 != nil || err2 != nil || ia.Bounds() != ib.Bounds() {
		t.Fatalf("decode: %v %v", err1, err2)
	}
	for y := ia.Bounds().Min.Y; y < ia.Bounds().Max.Y; y++ {
		for x := ia.Bounds().Min.X; x < ia.Bounds().Max.X; x++ {
			if ia.At(x, y) != ib.At(x, y) {
				return false
			}
		}
	}
	return true
}

// TestOptimizeJPEGStripsLosslessly pins the lossless path: a JPEG at or
// below the floor loses its metadata and trailing payload, keeps its
// colour profile and Adobe segment, and keeps every pixel.
func TestOptimizeJPEGStripsLosslessly(t *testing.T) {
	base := encodeJPEG(t, noisy(32, 32), 60)
	exif := exifJPEG(1, binary.BigEndian, "MM")
	exif = exif[2 : len(exif)-4] // APP0 + APP1 EXIF
	in := withSegments(base, "SMUGGLED PAYLOAD",
		exif,
		segment(jpegAPP1, "http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta GPSLatitude=1/>"),
		segment(jpegAPP2, iccTag+"profile-bytes"),
		segment(0xED, "Photoshop 3.0\x00IPTC"),
		segment(jpegAPP14, "Adobe\x00\x64\x00\x00\x00\x00\x01"),
		segment(jpegCOM, "camera serial 1234"),
	)
	out, err := optimize(t, OptimizeOptions{}, "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"Exif", "xmpmeta", "Photoshop", "serial", "SMUGGLED"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("%q survived", gone)
		}
	}
	for _, kept := range []string{iccTag + "profile-bytes", "Adobe"} {
		if !bytes.Contains(out, []byte(kept)) {
			t.Errorf("%q was dropped", kept)
		}
	}
	if !samePixels(t, base, out) || len(out) >= len(in) {
		t.Errorf("pixels changed or the file did not shrink (%d → %d)", len(in), len(out))
	}
	// A comment between the scan and the end marker goes too.
	late := append(append(append([]byte{}, base[:len(base)-2]...), segment(jpegCOM, "late comment")...), base[len(base)-2:]...)
	out, err = optimize(t, OptimizeOptions{}, "image/jpeg", late)
	if err != nil || bytes.Contains(out, []byte("late comment")) || !samePixels(t, base, out) {
		t.Errorf("late comment: %v", err)
	}
}

// TestOptimizeJPEGRecompressesAboveTheFloor pins that a JPEG saved above
// the floor is recompressed at it, and one at or below is not.
func TestOptimizeJPEGRecompressesAboveTheFloor(t *testing.T) {
	high := encodeJPEG(t, noisy(64, 64), 95)
	out, err := optimize(t, OptimizeOptions{Quality: 70}, "image/jpeg", high)
	if err != nil || len(out) >= len(high) {
		t.Fatalf("recompressed %d → %d, %v", len(high), len(out), err)
	}
	if _, q, _, _ := stripJPEG(out, keepSet{}); q != 70 {
		t.Errorf("recompressed at %d, want the floor 70", q)
	}
	if _, q, _, _ := stripJPEG(encodeJPEG(t, noisy(8, 8), 30), keepSet{}); q != 30 {
		t.Errorf("quality 30 read as %d", q)
	}
	low := encodeJPEG(t, noisy(64, 64), 50)
	out, _ = optimize(t, OptimizeOptions{Quality: 70}, "image/jpeg", low)
	if !samePixels(t, low, out) {
		t.Error("a JPEG below the floor was recompressed")
	}
	// When recompression would not shrink it, the stripped file is kept.
	tiny := encodeJPEG(t, image.NewRGBA(image.Rect(0, 0, 1, 1)), 95)
	out, _ = optimize(t, OptimizeOptions{Quality: 94}, "image/jpeg", tiny)
	if !bytes.Equal(out, tiny) {
		t.Errorf("a file recompression cannot shrink changed: %d → %d", len(tiny), len(out))
	}
}

// TestOptimizeJPEGRotates pins that removing EXIF never turns a photo
// sideways: an oriented JPEG is re-encoded upright.
func TestOptimizeJPEGRotates(t *testing.T) {
	in := realJPEG(t, 40, 20, orientRotate90)
	out, err := optimize(t, OptimizeOptions{}, "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 20 || cfg.Height != 40 || bytes.Contains(out, []byte("Exif")) {
		t.Errorf("rotated = %+v, %v", cfg, err)
	}
}

// pngChunk builds one PNG chunk with its CRC.
func pngChunk(kind, data string) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, kind...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE([]byte(kind+data)))
}

// withChunks inserts chunks after IHDR, and payload after IEND.
func withChunks(p []byte, payload string, chunks ...[]byte) []byte {
	ihdrEnd := len(pngSignature) + chunkOverhead + 13
	out := append([]byte{}, p[:ihdrEnd]...)
	for _, c := range chunks {
		out = append(out, c...)
	}
	out = append(out, p[ihdrEnd:]...)
	return append(out, payload...)
}

func flatPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 7 * 30)
	}
	var b bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	_ = enc.Encode(&b, img)
	return b.Bytes()
}

func TestOptimizePNG(t *testing.T) {
	base := flatPNG(t, 64, 64)
	in := withChunks(base, "TRAILER", pngChunk("tEXt", "Comment\x00secret"), pngChunk("eXIf", "MM\x00*GPS"), pngChunk("tIME", "\x07\xea\x0a\x06\x00\x00\x00"), pngChunk("pHYs", "\x00\x00\x0b\x13\x00\x00\x0b\x13\x01"))
	out, err := optimize(t, OptimizeOptions{}, "image/png", in)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"secret", "GPS", "tIME", "TRAILER"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Errorf("%q survived", gone)
		}
	}
	// No colour chunk: recompressed (the input was stored uncompressed).
	if len(out) >= len(base) {
		t.Errorf("not recompressed: %d → %d", len(base), len(out))
	}
	a, _ := png.Decode(bytes.NewReader(base))
	b, err := png.Decode(bytes.NewReader(out))
	if err != nil || a.At(5, 5) != b.At(5, 5) || a.At(63, 63) != b.At(63, 63) {
		t.Errorf("pixels changed: %v", err)
	}

	// A colour profile or an animation forbids re-encoding: the IDAT
	// bytes are kept as they were, and so are those chunks.
	for _, keep := range []string{"iCCP", "acTL"} {
		in := withChunks(base, "", pngChunk(keep, "data"), pngChunk("tEXt", "x\x00y"))
		out, err := optimize(t, OptimizeOptions{}, "image/png", in)
		if err != nil || !bytes.Contains(out, []byte(keep+"data")) || bytes.Contains(out, []byte("tEXt")) || len(out) != len(base)+chunkOverhead+4 {
			t.Errorf("%s: %d bytes, %v", keep, len(out), err)
		}
	}
	// An already-tight PNG keeps its own bytes.
	var tight bytes.Buffer
	_ = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&tight, image.NewGray(image.Rect(0, 0, 4, 4)))
	if out, _ := optimize(t, OptimizeOptions{}, "image/png", tight.Bytes()); !bytes.Equal(out, tight.Bytes()) {
		t.Error("an already-tight PNG changed")
	}
}

func TestOptimizeRefusalsAndPassThrough(t *testing.T) {
	jpg := encodeJPEG(t, noisy(8, 8), 90)
	broken := map[string][]string{
		"image/jpeg": {"", "\xff\xd8", "\xff\xd8\x00\x00\x00\x00", "\xff\xd8\xff\xe0\x00\x10ab", string(jpg[:len(jpg)-2]), string(jpg[:len(jpg)-2]) + "\xff\xc4", string(jpg[:len(jpg)-2]) + "\xff\xc4\xff\xff"},
		"image/png":  {"", pngSignature, pngSignature + "\x00\x00\x00\x01IHDR", pngSignature + "\x7f\xff\xff\xffIDAT\x00\x00\x00\x00"},
	}
	for ct, list := range broken {
		for _, data := range list {
			if _, err := optimize(t, OptimizeOptions{}, ct, []byte(data)); filesystem.CodeOf(err) != filesystem.CodeRejected {
				t.Errorf("%s %q = %v", ct, data, err)
			}
		}
	}
	if _, err := optimize(t, OptimizeOptions{Quality: 101}, "image/jpeg", jpg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("quality 101 = %v", err)
	}
	r, _, err := Optimize(Options{MaxBytes: 10}, OptimizeOptions{})(t.Context(), upload.Info{ContentType: "image/png"}, bytes.NewReader(flatPNG(t, 8, 8)))
	if filesystem.CodeOf(err) != filesystem.CodeTooLarge || r != nil {
		t.Errorf("over MaxBytes = %v", err)
	}
	if _, _, err := Optimize(Options{MaxBytes: 10}, OptimizeOptions{})(t.Context(), upload.Info{ContentType: "image/jpeg"}, bytes.NewReader(jpg)); filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("JPEG over MaxBytes = %v", err)
	}
	// A pixel bomb is refused before decoding, on both re-encode paths.
	if _, _, err := Optimize(Options{MaxPixels: 10}, OptimizeOptions{})(t.Context(), upload.Info{ContentType: "image/png"}, bytes.NewReader(flatPNG(t, 8, 8))); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("PNG over MaxPixels = %v", err)
	}
	if _, _, err := Optimize(Options{MaxPixels: 10}, OptimizeOptions{})(t.Context(), upload.Info{ContentType: "image/jpeg"}, bytes.NewReader(jpg)); filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("JPEG over MaxPixels = %v", err)
	}
	// Other types pass through untouched.
	body := strings.NewReader("hello")
	r, info, err := Optimize(Options{}, OptimizeOptions{})(t.Context(), upload.Info{ContentType: "text/plain"}, body)
	if err != nil || r != body || info.ContentType != "text/plain" {
		t.Errorf("text = %v", err)
	}
}
