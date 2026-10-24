package imaging

import (
	"bytes"
	"encoding/binary"
	"image/jpeg"
	"image/png"
	"slices"
	"strings"
	"testing"
)

// GPS IFD tags the fixture carries (TIFF/EXIF 2.3).
const (
	tagGPSPointer     = 0x8825
	tagMake           = 0x010F
	tagGPSLatitudeRef = 0x0001
	tagGPSLatitude    = 0x0002
	tagGPSProcessing  = 0x001B
	tiffTypeRational  = 5
	tiffTypeLong      = 4
	tiffTypeShort     = 3
	gpsSecret         = "GPS-SECRET-48.8584N"
)

// tiffEntry is one IFD entry of a fixture: value holds the bytes of the
// value, put inline when four bytes or fewer, else at an offset.
type tiffEntry struct {
	tag, typ uint16
	count    uint32
	value    []byte
}

// buildTIFF writes a TIFF block in order whose IFD0 holds ifd0 and, when
// gps is non-empty, a GPS IFD that IFD0's 0x8825 entry points at.
func buildTIFF(order binary.AppendByteOrder, mark string, ifd0, gps []tiffEntry) []byte {
	if len(gps) > 0 {
		ifd0 = append(ifd0, tiffEntry{tag: tagGPSPointer, typ: tiffTypeLong, count: 1, value: make([]byte, 4)})
	}
	ifdSize := func(n int) int { return 2 + n*ifdEntryBytes + 4 }
	ifd0At := tiffHeaderBytes
	gpsAt := ifd0At + ifdSize(len(ifd0))
	dataAt := gpsAt
	if len(gps) > 0 {
		dataAt += ifdSize(len(gps))
	}
	var data []byte
	write := func(entries []tiffEntry, next uint32) []byte {
		out := order.AppendUint16(nil, uint16(len(entries)))
		for _, e := range entries {
			out = order.AppendUint16(out, e.tag)
			out = order.AppendUint16(out, e.typ)
			out = order.AppendUint32(out, e.count)
			switch {
			case e.tag == tagGPSPointer:
				out = order.AppendUint32(out, uint32(gpsAt))
			case len(e.value) <= 4:
				v := make([]byte, 4)
				copy(v, e.value)
				out = append(out, v...)
			default:
				out = order.AppendUint32(out, uint32(dataAt+len(data)))
				data = append(data, e.value...)
			}
		}
		return order.AppendUint32(out, next)
	}
	out := []byte(mark)
	out = order.AppendUint16(out, 42)
	out = order.AppendUint32(out, uint32(ifd0At))
	out = append(out, write(ifd0, 0)...)
	if len(gps) > 0 {
		out = append(out, write(gps, 0)...)
	}
	return append(out, data...)
}

func ascii(tag uint16, s string) tiffEntry {
	return tiffEntry{tag: tag, typ: tiffTypeASCII, count: uint32(len(s) + 1), value: append([]byte(s), 0)}
}

// gpsEXIF is a source EXIF block carrying the two kept fields among a
// camera make, an orientation and a GPS IFD with a position.
func gpsEXIF(order binary.AppendByteOrder, mark string) []byte {
	lat := order.AppendUint32(nil, 48)
	lat = order.AppendUint32(lat, 1)
	lat = order.AppendUint32(lat, 51)
	lat = order.AppendUint32(lat, 1)
	lat = order.AppendUint32(lat, 30)
	lat = order.AppendUint32(lat, 1)
	return buildTIFF(order, mark,
		[]tiffEntry{
			ascii(tagMake, "CanonSecretCamera"),
			{tag: tagOrientation, typ: tiffTypeShort, count: 1, value: order.AppendUint16(nil, orientRotate90)},
			ascii(exifTagArtist, "Aman Bansal"),
			ascii(exifTagCopyright, "(c) 2026 Aman Bansal"),
		},
		[]tiffEntry{
			ascii(tagGPSLatitudeRef, "N"),
			{tag: tagGPSLatitude, typ: tiffTypeRational, count: 3, value: lat},
			ascii(tagGPSProcessing, gpsSecret),
		})
}

// ifd0Tags parses the IFD0 tags of a TIFF block, failing on a malformed one.
func ifd0Tags(t *testing.T, tiff []byte) []uint16 {
	t.Helper()
	order, ok := tiffByteOrder(tiff)
	if !ok || binary.BigEndian.Uint16(tiff[2:]) != tiffMagic {
		t.Fatalf("not a TIFF block: %q", tiff)
	}
	ifd := int(order.Uint32(tiff[4:]))
	n := int(order.Uint16(tiff[ifd:]))
	var tags []uint16
	for e := range n {
		tags = append(tags, order.Uint16(tiff[ifd+2+e*ifdEntryBytes:]))
	}
	if next := order.Uint32(tiff[ifd+2+n*ifdEntryBytes:]); next != 0 {
		t.Errorf("the kept block links to IFD at %d", next)
	}
	return tags
}

// TestKeptEXIF pins the rewrite: from either byte order it builds a block
// with exactly Artist and Copyright, ascending, whose text round-trips
// through tiffOrientation-style parsing; the GPS IFD, its position, the
// camera make and the orientation never appear in the output.
func TestKeptEXIF(t *testing.T) {
	for name, src := range map[string][]byte{
		"big-endian":    gpsEXIF(binary.BigEndian, "MM"),
		"little-endian": gpsEXIF(binary.LittleEndian, "II"),
	} {
		if !bytes.Contains(src, []byte(gpsSecret)) {
			t.Fatalf("%s: the fixture carries no GPS", name)
		}
		out := keptEXIF(src)
		if out == nil {
			t.Fatalf("%s: nothing kept", name)
		}
		for _, absent := range []string{gpsSecret, "CanonSecretCamera"} {
			if bytes.Contains(out, []byte(absent)) {
				t.Errorf("%s: %q survived", name, absent)
			}
		}
		if tags := ifd0Tags(t, out); !slices.Equal(tags, exifKeptTags) {
			t.Errorf("%s: IFD0 tags = %#x, want %#x", name, tags, exifKeptTags)
		}
		if tiffOrientation(out) != orientNormal {
			t.Errorf("%s: an orientation survived", name)
		}
		for _, want := range []string{"Aman Bansal\x00", "(c) 2026 Aman Bansal\x00"} {
			if !bytes.Contains(out, []byte(want)) {
				t.Errorf("%s: %q was dropped", name, want)
			}
		}
	}
}

// TestKeptEXIFEdges pins short (inline) values, the per-field cap, source
// order, and the inputs that keep nothing: no kept field, a field of the
// wrong type, offsets past the end, an empty text, a broken header.
func TestKeptEXIFEdges(t *testing.T) {
	// A three-letter artist sits inline; Copyright before Artist in the
	// source still comes out ascending.
	out := keptEXIF(buildTIFF(binary.BigEndian, "MM", []tiffEntry{ascii(exifTagCopyright, "(c) X"), ascii(exifTagArtist, "Abe")}, nil))
	if tags := ifd0Tags(t, out); !slices.Equal(tags, exifKeptTags) {
		t.Errorf("tags = %#x", tags)
	}
	if !bytes.Contains(out, []byte("Abe\x00")) || !bytes.Contains(out, []byte("(c) X\x00")) {
		t.Errorf("short values: %q", out)
	}

	long := strings.Repeat("c", 3*exifMaxField)
	out = keptEXIF(buildTIFF(binary.BigEndian, "MM", []tiffEntry{ascii(exifTagCopyright, long)}, nil))
	if len(out) > tiffHeaderBytes+2+ifdEntryBytes+4+exifMaxField+1 || !bytes.Contains(out, []byte(strings.Repeat("c", exifMaxField-1)+"\x00")) {
		t.Errorf("a long field was not capped at %d bytes: %d", exifMaxField, len(out))
	}

	wrongType := tiffEntry{tag: exifTagCopyright, typ: tiffTypeShort, count: 1, value: []byte{0, 1}}
	pastEnd := buildTIFF(binary.BigEndian, "MM", []tiffEntry{ascii(exifTagCopyright, "(c) far away")}, nil)
	binary.BigEndian.PutUint32(pastEnd[tiffHeaderBytes+2+8:], 1<<20)
	for name, src := range map[string][]byte{
		"no kept field":  buildTIFF(binary.BigEndian, "MM", []tiffEntry{ascii(tagMake, "Canon")}, []tiffEntry{ascii(tagGPSProcessing, gpsSecret)}),
		"wrong type":     buildTIFF(binary.BigEndian, "MM", []tiffEntry{wrongType}, nil),
		"value past end": pastEnd,
		"empty text":     buildTIFF(binary.BigEndian, "MM", []tiffEntry{ascii(exifTagArtist, "")}, nil),
		"bad mark":       buildTIFF(binary.BigEndian, "XX", []tiffEntry{ascii(exifTagArtist, "Aman")}, nil),
		"short":          []byte("MM\x00*"),
		"ifd past end":   append([]byte("MM\x00*"), 0, 0, 1, 0),
		"truncated ifd":  buildTIFF(binary.BigEndian, "MM", []tiffEntry{ascii(exifTagArtist, "Aman Bansal")}, nil)[:tiffHeaderBytes+4],
	} {
		if got := keptEXIF(src); got != nil {
			t.Errorf("%s: kept %q", name, got)
		}
	}
	if jpegEXIFSegment([]byte("http://ns.adobe.com/xap/1.0/\x00<x/>")) != nil {
		t.Error("an XMP APP1 was kept as EXIF")
	}
}

// TestOptimizeKeepEXIFCopyright pins the opt-in end to end. JPEG: the
// default drops EXIF; MetadataEXIFCopyright keeps a rebuilt APP1 with
// Artist and Copyright only, through the re-encode an EXIF rotation
// forces, and the output never contains the source's GPS. PNG: an eXIf
// chunk is rebuilt the same way with a valid CRC. MetadataCopyright alone
// keeps no EXIF, so naming one kind never widens the other.
func TestOptimizeKeepEXIFCopyright(t *testing.T) {
	src := gpsEXIF(binary.BigEndian, "MM")
	in := withSegments(encodeJPEG(t, noisy(20, 40), 60), "", segment(jpegAPP1, exifHeader+string(src)))
	keep := OptimizeOptions{KeepMetadata: []Metadata{MetadataEXIFCopyright}}

	for name, o := range map[string]OptimizeOptions{"default": {}, "iptc copyright only": {KeepMetadata: []Metadata{MetadataCopyright}}} {
		out, err := optimize(t, o, "image/jpeg", in)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("Aman")) {
			t.Errorf("%s kept EXIF", name)
		}
	}

	out, err := optimize(t, keep, "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(gpsSecret)) || bytes.Contains(out, []byte("Canon")) {
		t.Fatal("the source's GPS or camera survived")
	}
	if !bytes.Contains(out, []byte("(c) 2026 Aman Bansal")) || !bytes.Contains(out, []byte("Aman Bansal\x00")) {
		t.Error("the copyright or artist was dropped")
	}
	// The rotation was applied and is not re-applied by a viewer.
	if exifOrientation(out) != orientNormal {
		t.Error("an orientation survived")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 40 || cfg.Height != 20 {
		t.Errorf("rotated output = %+v, %v", cfg, err)
	}
	if _, _, _, err := stripJPEG(out, keepSet{}); err != nil {
		t.Errorf("the output is not a well-formed JPEG: %v", err)
	}

	pngIn := withChunks(flatPNG(t, 64, 64), "", pngChunk(pngEXIfChunk, string(gpsEXIF(binary.LittleEndian, "II"))))
	out, err = optimize(t, keep, "image/png", pngIn)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(gpsSecret)) || !bytes.Contains(out, []byte("(c) 2026 Aman Bansal")) {
		t.Errorf("png: GPS kept or copyright dropped")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("png output does not decode (bad CRC?): %v", err)
	}
	if out, _ = optimize(t, OptimizeOptions{}, "image/png", pngIn); bytes.Contains(out, []byte("eXIf")) {
		t.Error("png default kept the eXIf chunk")
	}
}

// TestEXIFBlocksWithoutKeptFields: an EXIF block holding neither Artist
// nor Copyright (here only the camera's make) gives no JPEG segment and no
// PNG chunk at all, rather than an empty EXIF block.
func TestEXIFBlocksWithoutKeptFields(t *testing.T) {
	tiff := buildTIFF(binary.LittleEndian, "II", []tiffEntry{ascii(tagMake, "CanonSecretCamera")}, nil)
	if keptEXIF(tiff) != nil {
		t.Fatal("the block keeps a field; the test proves nothing")
	}
	if seg := jpegEXIFSegment(append([]byte(exifHeader), tiff...)); seg != nil {
		t.Errorf("JPEG segment = %x, want none", seg)
	}
	if chunk := pngEXIFChunk(tiff); chunk != nil {
		t.Errorf("PNG chunk = %x, want none", chunk)
	}
}
