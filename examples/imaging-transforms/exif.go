package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
)

// orientRotate90 is the EXIF orientation of a phone photo taken in
// portrait and stored landscape: rotate 90 degrees clockwise to display.
const orientRotate90 = 6

// EXIF layout written by exifJPEG: an APP1 segment holding "Exif\0\0",
// a big-endian TIFF header and one IFD with one entry, the orientation.
const (
	jpegAPP1         = 0xE1
	tiffOrientation  = 0x0112
	tiffTypeShort    = 3
	tiffFirstIFDAt   = 8
	soiBytes         = 2
	exifHeader       = "Exif\x00\x00"
	tiffBigEndianTag = "MM\x00*"
)

// exifJPEG encodes img as a JPEG whose EXIF says orientation o, as a
// camera writes it: the pixels stay as stored and the tag says how to
// turn them.
func exifJPEG(img image.Image, o uint16) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		return nil, err
	}
	tiff := []byte(tiffBigEndianTag)
	tiff = binary.BigEndian.AppendUint32(tiff, tiffFirstIFDAt)
	tiff = binary.BigEndian.AppendUint16(tiff, 1) // one entry
	tiff = binary.BigEndian.AppendUint16(tiff, tiffOrientation)
	tiff = binary.BigEndian.AppendUint16(tiff, tiffTypeShort)
	tiff = binary.BigEndian.AppendUint32(tiff, 1) // one value
	tiff = binary.BigEndian.AppendUint16(tiff, o)
	tiff = binary.BigEndian.AppendUint16(tiff, 0) // value padding
	tiff = binary.BigEndian.AppendUint32(tiff, 0) // no next IFD
	body := append([]byte(exifHeader), tiff...)
	seg := []byte{0xFF, jpegAPP1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(body)+2))
	seg = append(seg, body...)
	data := buf.Bytes()
	out := append([]byte{}, data[:soiBytes]...)
	out = append(out, seg...)
	return append(out, data[soiBytes:]...), nil
}
