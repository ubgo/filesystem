package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"math"
)

// This file builds what a phone writes: a PNG whose iCCP chunk holds a
// Display P3 matrix/TRC profile. A real app gets such files from uploads;
// the demo makes one so it needs no binary test data.

// p3Colorants are Display P3's colorants adapted to D50, as in Apple's
// profile: the columns are red, green and blue in XYZ.
var p3Colorants = [3][3]float64{{0.5151, 0.2920, 0.1571}, {0.2412, 0.6922, 0.0666}, {-0.0011, 0.0419, 0.7841}}

// ICC layout constants (ICC.1:2022).
const (
	iccHeaderBytes = 128
	iccSpaceAt     = 16
	iccPCSAt       = 20
	iccTagBytes    = 12
	s15Fixed16     = 65536
	// pngIHDREnd is where the IHDR chunk ends: the signature (8), its
	// length and type (8), its body (13) and its CRC (4).
	pngIHDREnd = 33
)

// srgbCurve is sRGB's transfer function as a parametric curve of type 3.
func srgbCurve() []byte {
	b := append([]byte("para"), 0, 0, 0, 0)
	b = binary.BigEndian.AppendUint16(b, 3)
	b = append(b, 0, 0)
	for _, v := range []float64{2.4, 1 / 1.055, 0.055 / 1.055, 1 / 12.92, 0.04045} {
		b = binary.BigEndian.AppendUint32(b, uint32(int32(math.Round(v*s15Fixed16))))
	}
	return b
}

// xyzTag is one colorant as an XYZ tag.
func xyzTag(x, y, z float64) []byte {
	b := append([]byte("XYZ "), 0, 0, 0, 0)
	for _, v := range []float64{x, y, z} {
		b = binary.BigEndian.AppendUint32(b, uint32(int32(math.Round(v*s15Fixed16))))
	}
	return b
}

// p3Profile is a minimal RGB matrix/TRC profile with the P3 colorants.
func p3Profile() []byte {
	c, trc := p3Colorants, srgbCurve()
	tags := []struct {
		sig  string
		data []byte
	}{
		{"rXYZ", xyzTag(c[0][0], c[1][0], c[2][0])},
		{"gXYZ", xyzTag(c[0][1], c[1][1], c[2][1])},
		{"bXYZ", xyzTag(c[0][2], c[1][2], c[2][2])},
		{"rTRC", trc}, {"gTRC", trc}, {"bTRC", trc},
	}
	head := make([]byte, iccHeaderBytes)
	copy(head[iccSpaceAt:], "RGB ")
	copy(head[iccPCSAt:], "XYZ ")
	table := binary.BigEndian.AppendUint32(nil, uint32(len(tags)))
	var body []byte
	off := iccHeaderBytes + 4 + iccTagBytes*len(tags)
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

// displayP3PNG is a 4x4 PNG of one colour (red 180) tagged Display P3.
func displayP3PNG() ([]byte, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		copy(img.Pix[i:], []byte{180, 120, 90, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(p3Profile()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	chunkBody := append([]byte("Display P3\x00\x00"), z.Bytes()...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(chunkBody)))
	chunk = append(append(chunk, "iCCP"...), chunkBody...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("iCCP"), chunkBody...)))
	p := buf.Bytes()
	return append(append(append([]byte{}, p[:pngIHDREnd]...), chunk...), p[pngIHDREnd:]...), nil
}
