package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

// corners builds a 3×2 image with red top-left, green top-right, blue
// bottom-left and white bottom-right, so every orientation is visible.
func corners() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	img.Set(2, 0, color.RGBA{0, 255, 0, 255})
	img.Set(0, 1, color.RGBA{0, 0, 255, 255})
	img.Set(2, 1, color.RGBA{255, 255, 255, 255})
	return img
}

var (
	red   = color.RGBA{255, 0, 0, 255}
	green = color.RGBA{0, 255, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
	white = color.RGBA{255, 255, 255, 255}
)

func TestOrient(t *testing.T) {
	cases := []struct {
		o              int
		w, h           int
		tl, tr, bl, br color.RGBA
	}{
		{orientNormal, 3, 2, red, green, blue, white},
		{orientFlipH, 3, 2, green, red, white, blue},
		{orientRotate180, 3, 2, white, blue, green, red},
		{orientFlipV, 3, 2, blue, white, red, green},
		{orientTranspose, 2, 3, red, blue, green, white},
		{orientRotate90, 2, 3, blue, red, white, green},
		{orientTransverse, 2, 3, white, green, blue, red},
		{orientRotate270, 2, 3, green, white, red, blue},
	}
	for _, c := range cases {
		out := orient(corners(), c.o)
		b := out.Bounds()
		if b.Dx() != c.w || b.Dy() != c.h {
			t.Errorf("orientation %d: size %dx%d, want %dx%d", c.o, b.Dx(), b.Dy(), c.w, c.h)
			continue
		}
		got := []color.Color{out.At(0, 0), out.At(c.w-1, 0), out.At(0, c.h-1), out.At(c.w-1, c.h-1)}
		want := []color.RGBA{c.tl, c.tr, c.bl, c.br}
		for i := range got {
			if rgba(got[i]) != want[i] {
				t.Errorf("orientation %d corner %d = %v, want %v", c.o, i, got[i], want[i])
			}
		}
	}
}

func rgba(c color.Color) color.RGBA {
	r, g, b, a := c.RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

// exifJPEG wraps an orientation value in a minimal JPEG APP1 EXIF segment,
// followed by an SOS marker, in the given byte order.
func exifJPEG(orientation uint16, order binary.AppendByteOrder, mark string) []byte {
	tiff := []byte(mark)
	tiff = order.AppendUint16(tiff, 42)
	tiff = order.AppendUint32(tiff, 8)
	tiff = order.AppendUint16(tiff, 1) // one entry
	tiff = order.AppendUint16(tiff, tagOrientation)
	tiff = order.AppendUint16(tiff, 3) // SHORT
	tiff = order.AppendUint32(tiff, 1)
	tiff = order.AppendUint16(tiff, orientation)
	tiff = append(tiff, 0, 0)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	out := []byte{0xFF, jpegSOI, 0xFF, 0xE0, 0, 4, 'J', 'F'} // an APP0 to skip first
	out = append(out, 0xFF, jpegAPP1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(seg)+2))
	out = append(out, seg...)
	return append(out, 0xFF, jpegSOS, 0, 2)
}

func TestExifOrientation(t *testing.T) {
	if got := exifOrientation(exifJPEG(6, binary.BigEndian, "MM")); got != orientRotate90 {
		t.Errorf("big-endian = %d", got)
	}
	if got := exifOrientation(exifJPEG(8, binary.LittleEndian, "II")); got != orientRotate270 {
		t.Errorf("little-endian = %d", got)
	}
	malformed := [][]byte{
		nil,
		[]byte("not a jpeg"),
		exifJPEG(9, binary.BigEndian, "MM"),
		exifJPEG(6, binary.BigEndian, "XX"),
		{0xFF, jpegSOI, 0x00, 0x00, 0, 4},
		{0xFF, jpegSOI, 0xFF, 0xE0, 0, 1},
		{0xFF, jpegSOI, 0xFF, 0xE0, 0, 200},
		{0xFF, jpegSOI, 0xFF, 0xE0, 0, 4, 1, 2},
		{0xFF, jpegSOI, 0xFF, jpegAPP1, 0, 10, 'E', 'x', 'i', 'f', 0, 0, 'M', 'M'},
	}
	for i, m := range malformed {
		if got := exifOrientation(m); got != orientNormal {
			t.Errorf("malformed %d = %d", i, got)
		}
	}
	// TIFF blocks with broken offsets.
	for i, tiff := range [][]byte{
		[]byte("MM\x00*\x00\x00\x00\x04"),                                                 // IFD offset inside the header
		[]byte("MM\x00*\x00\x00\x00\x08\x00\x05"),                                         // five entries, none present
		append([]byte("MM\x00*\x00\x00\x00\x08\x00\x01"), bytes.Repeat([]byte{0}, 12)...), // an entry that is not orientation
	} {
		if got := tiffOrientation(tiff); got != orientNormal {
			t.Errorf("tiff %d = %d", i, got)
		}
	}
}
