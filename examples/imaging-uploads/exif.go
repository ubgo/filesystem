package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"strings"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/upload"
)

// The photographer's authorship, and the camera and position the same
// EXIF block carries, which must not survive.
const (
	photoArtist    = "Ada Lens"
	photoCopyright = "(c) 2026 Ada Lens"
	cameraMake     = "Canon"
	gpsPosition    = "GPS 51.5007N 0.1246W"
)

// TIFF tags and types the camera's EXIF block uses (TIFF 6.0, EXIF 2.3).
const (
	tagMake          = 0x010F
	tagArtist        = 0x013B
	tagCopyright     = 0x8298
	tagGPSIFD        = 0x8825
	tagGPSLatRef     = 0x0001
	tagGPSProcessing = 0x001B
	typeASCII        = 2
	typeLong         = 4
	typeUndefined    = 7
	// tiffHeader is the byte-order mark, 42 and the offset of IFD0 (8).
	tiffHeader = 8
	// ifdEntry is the size of one IFD entry.
	ifdEntry = 12
	// app1 is the JPEG marker of the segment holding EXIF.
	app1 = 0xE1
	// exifHeader starts an APP1 segment holding EXIF.
	exifHeader = "Exif\x00\x00"
)

// keepAuthorship optimises photos and keeps who made them: the IPTC
// notice and the EXIF Artist and Copyright, never the GPS position.
// ds:def id=keepauthorship-n4v8k2qe
func keepAuthorship() upload.Processor {
	return imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{
		Quality:      80,
		KeepMetadata: []imaging.Metadata{imaging.MetadataCopyright, imaging.MetadataEXIFCopyright},
	})
}

// tiffField is one IFD entry of the camera's block.
type tiffField struct {
	tag, typ uint16
	value    []byte
}

// buildIFD writes the entries of one IFD at offset at of a big-endian
// TIFF block: values longer than four bytes go after the IFD, and the
// GPS pointer names gpsAt.
func buildIFD(fields []tiffField, at, gpsAt int) []byte {
	dataAt := at + 2 + len(fields)*ifdEntry + 4
	out := binary.BigEndian.AppendUint16(nil, uint16(len(fields)))
	var data []byte
	for _, f := range fields {
		out = binary.BigEndian.AppendUint16(out, f.tag)
		out = binary.BigEndian.AppendUint16(out, f.typ)
		switch {
		case f.tag == tagGPSIFD:
			out = binary.BigEndian.AppendUint32(out, 1)
			out = binary.BigEndian.AppendUint32(out, uint32(gpsAt))
		case len(f.value) <= 4:
			out = binary.BigEndian.AppendUint32(out, uint32(len(f.value)))
			out = append(out, f.value...)
			out = append(out, make([]byte, 4-len(f.value))...)
		default:
			out = binary.BigEndian.AppendUint32(out, uint32(len(f.value)))
			out = binary.BigEndian.AppendUint32(out, uint32(dataAt+len(data)))
			data = append(data, f.value...)
		}
	}
	out = binary.BigEndian.AppendUint32(out, 0)
	return append(out, data...)
}

// cameraEXIF is the block a camera writes: make, artist and copyright in
// IFD0, and a GPS IFD holding the position.
func cameraEXIF(artist, copyright string) []byte {
	text := func(s string) []byte { return append([]byte(s), 0) }
	var ifd0 []tiffField
	ifd0 = append(ifd0, tiffField{tagMake, typeASCII, text(cameraMake)})
	if artist != "" {
		ifd0 = append(ifd0, tiffField{tagArtist, typeASCII, text(artist)})
	}
	if copyright != "" {
		ifd0 = append(ifd0, tiffField{tagCopyright, typeASCII, text(copyright)})
	}
	ifd0 = append(ifd0, tiffField{tag: tagGPSIFD, typ: typeLong})
	gps := []tiffField{
		{tagGPSLatRef, typeASCII, text("N")},
		{tagGPSProcessing, typeUndefined, []byte("ASCII\x00\x00\x00" + gpsPosition)},
	}
	// IFD0's size is fixed by its entries, so the GPS IFD's offset is
	// known before IFD0 is written.
	first := buildIFD(ifd0, tiffHeader, 0)
	gpsAt := tiffHeader + len(first)
	out := append([]byte("MM\x00\x2a"), 0, 0, 0, tiffHeader)
	out = append(out, buildIFD(ifd0, tiffHeader, gpsAt)...)
	return append(out, buildIFD(gps, gpsAt, 0)...)
}

// photoWithEXIF is a small grey JPEG carrying block in an APP1 segment.
func photoWithEXIF(block []byte) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 7)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		return nil, err
	}
	seg := append([]byte(exifHeader), block...)
	out := append([]byte{}, buf.Bytes()[:2]...)
	out = append(out, 0xFF, app1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(seg)+2))
	out = append(out, seg...)
	return append(out, buf.Bytes()[2:]...), nil
}

// exifTags reads the IFD0 of the EXIF block in a JPEG: each tag, with its
// text for an ASCII field. It returns nil when the JPEG holds no EXIF.
func exifTags(jpg []byte) map[uint16]string {
	at := bytes.Index(jpg, []byte(exifHeader))
	if at < 0 {
		return nil
	}
	t := jpg[at+len(exifHeader):]
	order := binary.ByteOrder(binary.BigEndian)
	if bytes.HasPrefix(t, []byte("II")) {
		order = binary.LittleEndian
	}
	ifd := int(order.Uint32(t[4:]))
	tags := map[uint16]string{}
	for e := range int(order.Uint16(t[ifd:])) {
		off := ifd + 2 + e*ifdEntry
		tag, typ, count := order.Uint16(t[off:]), order.Uint16(t[off+2:]), int(order.Uint32(t[off+4:]))
		tags[tag] = ""
		if typ != typeASCII {
			continue
		}
		v := off + 8
		if count > 4 {
			v = int(order.Uint32(t[off+8:]))
		}
		tags[tag] = strings.TrimRight(string(t[v:v+count]), "\x00")
	}
	return tags
}

// optimised runs p over a JPEG the way an upload would.
func optimised(ctx context.Context, p upload.Processor, jpg []byte) ([]byte, error) {
	r, _, err := p(ctx, upload.Info{ContentType: "image/jpeg"}, bytes.NewReader(jpg))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// exifAuthorship optimises a camera photo twice, with the default and
// with keepAuthorship, and prints what of its EXIF survived.
// ds:def id=exifauthorship-w7c3m9tf
func exifAuthorship(ctx context.Context, w io.Writer) error {
	photo, err := photoWithEXIF(cameraEXIF(photoArtist, photoCopyright))
	if err != nil {
		return err
	}
	stripped, err := optimised(ctx, imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{Quality: 80}), photo)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "exif by default: kept %t\n", exifTags(stripped) != nil)

	kept, err := optimised(ctx, keepAuthorship(), photo)
	if err != nil {
		return err
	}
	tags := exifTags(kept)
	fmt.Fprintf(w, "exif with MetadataEXIFCopyright: %d fields, Artist %q, Copyright %q\n", len(tags), tags[tagArtist], tags[tagCopyright])
	_, gpsIFD := tags[tagGPSIFD]
	fmt.Fprintf(w, "exif GPS kept: %t, position anywhere in the file: %t, camera make kept: %t\n",
		gpsIFD, bytes.Contains(kept, []byte(gpsPosition)), bytes.Contains(kept, []byte(cameraMake)))

	bare, err := photoWithEXIF(cameraEXIF("", ""))
	if err != nil {
		return err
	}
	out, err := optimised(ctx, keepAuthorship(), bare)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "exif of a photo with neither field: kept %t\n", exifTags(out) != nil)

	unknown := imaging.Optimize(imaging.Options{}, imaging.OptimizeOptions{KeepMetadata: []imaging.Metadata{"xmp"}})
	_, err = optimised(ctx, unknown, photo)
	fmt.Fprintf(w, "keeping an unknown kind: %s\n", filesystem.CodeOf(err))
	return nil
}
