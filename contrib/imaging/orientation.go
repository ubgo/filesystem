package imaging

import (
	"encoding/binary"
	"image"
)

// EXIF orientation values (TIFF tag 0x0112). Phones store the sensor's
// pixels as captured and record how to rotate them for display.
const (
	orientNormal     = 1
	orientFlipH      = 2
	orientRotate180  = 3
	orientFlipV      = 4
	orientTranspose  = 5
	orientRotate90   = 6
	orientTransverse = 7
	orientRotate270  = 8
)

// JPEG and TIFF constants the parser needs.
const (
	jpegSOI         = 0xD8
	jpegSOS         = 0xDA
	jpegAPP1        = 0xE1
	tagOrientation  = 0x0112
	tiffHeaderBytes = 8
	ifdEntryBytes   = 12
)

// exifOrientation returns the EXIF orientation of a JPEG, or orientNormal
// when there is none or the data is not a well-formed JPEG with EXIF. It
// bounds-checks every offset, because the input is untrusted.
func exifOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != jpegSOI {
		return orientNormal
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			return orientNormal
		}
		marker := data[i+1]
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		if marker == jpegSOS || size < 2 || i+2+size > len(data) {
			return orientNormal
		}
		segment := data[i+4 : i+2+size]
		if marker == jpegAPP1 && len(segment) > 6 && string(segment[:6]) == "Exif\x00\x00" {
			return tiffOrientation(segment[6:])
		}
		i += 2 + size
	}
	return orientNormal
}

// tiffOrientation reads the orientation tag from IFD0 of a TIFF block.
func tiffOrientation(t []byte) int {
	if len(t) < tiffHeaderBytes {
		return orientNormal
	}
	var order binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return orientNormal
	}
	ifd := int(order.Uint32(t[4:]))
	if ifd < tiffHeaderBytes || ifd+2 > len(t) {
		return orientNormal
	}
	n := int(order.Uint16(t[ifd:]))
	for e := range n {
		off := ifd + 2 + e*ifdEntryBytes
		if off+ifdEntryBytes > len(t) {
			return orientNormal
		}
		if order.Uint16(t[off:]) == tagOrientation {
			v := int(order.Uint16(t[off+8:]))
			if v >= orientNormal && v <= orientRotate270 {
				return v
			}
			return orientNormal
		}
	}
	return orientNormal
}

// orient returns img as it should be displayed for an EXIF orientation.
func orient(img image.Image, o int) image.Image {
	if o == orientNormal {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= orientTranspose {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			sx, sy := sourcePixel(o, x, y, w, h)
			dst.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// sourcePixel maps a displayed pixel to the stored pixel it comes from.
// w and h are the stored image's size.
func sourcePixel(o, x, y, w, h int) (int, int) {
	switch o {
	case orientFlipH:
		return w - 1 - x, y
	case orientRotate180:
		return w - 1 - x, h - 1 - y
	case orientFlipV:
		return x, h - 1 - y
	case orientTranspose:
		return y, x
	case orientRotate90:
		return y, h - 1 - x
	case orientTransverse:
		return w - 1 - y, h - 1 - x
	}
	// orientRotate270
	return w - 1 - y, x
}
