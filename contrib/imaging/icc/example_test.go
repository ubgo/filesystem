package icc_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"log"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/icc"
)

// managedPipeline converts photos with an embedded RGB profile (a phone's
// Display P3) to sRGB, and keeps any other profile in the output.
func managedPipeline() *imaging.Pipeline {
	return imaging.NewPipeline(imaging.WithColourManagement(icc.ToSRGB))
}

// Example converts a Display P3 PNG to an sRGB thumbnail: the same
// numbers mean a more saturated red in P3, so sRGB needs a higher one.
func Example() {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		copy(img.Pix[i:], []byte{180, 120, 90, 255})
	}
	var p3 bytes.Buffer
	if err := png.Encode(&p3, img); err != nil {
		log.Fatal(err)
	}
	original := embed(p3.Bytes(), profile("RGB ", "XYZ ", p3Colorants, srgbCurve))
	res, err := managedPipeline().Process(bytes.NewReader(original), imaging.Transform{Mode: imaging.ModeFit, Width: 4, Height: 4, Format: imaging.FormatPNG}, imaging.Options{})
	if err != nil {
		log.Fatal(err)
	}
	out, _ := png.Decode(bytes.NewReader(res.Data))
	_, kept := imaging.EmbeddedProfile(res.Data)
	c := color.NRGBAModel.Convert(out.At(1, 1)).(color.NRGBA)
	fmt.Println(c.R > 180, kept)
	// Output: true false
}

// embed puts profile into a PNG's iCCP chunk, as an editor saving in P3
// does.
func embed(p, profile []byte) []byte {
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write(profile)
	_ = zw.Close()
	body := append([]byte("Display P3\x00\x00"), z.Bytes()...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
	chunk = append(append(chunk, "iCCP"...), body...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("iCCP"), body...)))
	return append(append(append([]byte{}, p[:33]...), chunk...), p[33:]...)
}
