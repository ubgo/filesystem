package imaging

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ubgo/filesystem"
)

// iim is one IPTC IIM dataset of the application record.
func iim(dataset byte, value string) []byte {
	out := []byte{iptcTag, iptcRecordApplication, dataset}
	out = binary.BigEndian.AppendUint16(out, uint16(len(value)))
	return append(out, value...)
}

// photoshopBlock is one Photoshop image resource block.
func photoshopBlock(id uint16, data []byte) []byte {
	out := append([]byte(photoshopResource), 0, 0)
	binary.BigEndian.PutUint16(out[4:], id)
	out = append(out, 0, 0)
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// photoshopIPTCSegment is an APP13 holding an XMP resource with a GPS
// position, then the IPTC record with copyright, creator and a city.
func photoshopIPTCSegment() []byte {
	record := append(append(iim(iptcCopyright, "(c) Aman Bansal"), iim(iptcByLine, "Aman Bansal")...), iim(90, "Paris")...)
	body := append([]byte(photoshopTag), photoshopBlock(0x0424, []byte("<xmp GPSLatitude=48/>"))...)
	body = append(body, photoshopBlock(photoshopIPTC, record)...)
	return segment(jpegAPP13, string(body))
}

// TestOptimizeKeepMetadata pins the metadata allowlist: by default every
// kind goes; MetadataCopyright keeps the IPTC copyright and creator and
// nothing else of the segment (no city, no XMP, no GPS); MetadataComment
// keeps comments; kept metadata survives a re-encode; PNG keeps the named
// text chunks only; an unknown kind fails closed.
func TestOptimizeKeepMetadata(t *testing.T) {
	exif := exifJPEG(1, binary.BigEndian, "MM")
	exif = exif[2 : len(exif)-4]
	in := withSegments(encodeJPEG(t, noisy(32, 32), 60), "",
		exif,
		segment(jpegAPP1, "http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta GPSLatitude=1/>"),
		photoshopIPTCSegment(),
		segment(jpegCOM, "shot for the spring issue"),
	)
	gone := []string{"Exif", "xmpmeta", "GPS", "Paris"}
	check := func(name string, out []byte, want, absent []string) {
		t.Helper()
		for _, s := range want {
			if !bytes.Contains(out, []byte(s)) {
				t.Errorf("%s: %q was dropped", name, s)
			}
		}
		for _, s := range append(absent, gone...) {
			if bytes.Contains(out, []byte(s)) {
				t.Errorf("%s: %q survived", name, s)
			}
		}
	}
	out, err := optimize(t, OptimizeOptions{}, "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	check("default", out, nil, []string{"Aman", "spring"})

	out, err = optimize(t, OptimizeOptions{KeepMetadata: []Metadata{MetadataCopyright}}, "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	check("copyright", out, []string{"(c) Aman Bansal", photoshopTag}, []string{"spring"})
	if _, _, _, err := stripJPEG(out, keepSet{}); err != nil {
		t.Fatalf("the output is not a well-formed JPEG: %v", err)
	}

	out, err = optimize(t, OptimizeOptions{KeepMetadata: []Metadata{MetadataComment}}, "image/jpeg", in)
	if err != nil {
		t.Fatal(err)
	}
	check("comment", out, []string{"spring"}, []string{"Aman"})

	// Recompressed above the floor: the kept segment is spliced back in.
	high := withSegments(encodeJPEG(t, noisy(64, 64), 95), "", photoshopIPTCSegment())
	out, err = optimize(t, OptimizeOptions{Quality: 50, KeepMetadata: MetadataValues}, "image/jpeg", high)
	if err != nil || len(out) >= len(high) {
		t.Fatalf("recompress: %v, %d → %d", err, len(high), len(out))
	}
	check("recompressed", out, []string{"(c) Aman Bansal"}, nil)

	png := withChunks(flatPNG(t, 64, 64), "", pngChunk("tEXt", "Copyright\x00(c) Aman Bansal"), pngChunk("tEXt", "Location\x00Paris"), pngChunk("eXIf", "MM\x00*GPS"))
	out, err = optimize(t, OptimizeOptions{KeepMetadata: []Metadata{MetadataCopyright}}, "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	check("png", out, []string{"(c) Aman Bansal"}, nil)
	if out, _ = optimize(t, OptimizeOptions{}, "image/png", png); bytes.Contains(out, []byte("Aman")) {
		t.Error("png default kept the copyright chunk")
	}

	if _, err := optimize(t, OptimizeOptions{KeepMetadata: []Metadata{"exif"}}, "image/jpeg", in); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("an unknown kind: %v", err)
	}
}

// TestMetadataMalformed pins the walks over embedded metadata on input
// they must not trust: truncated, oversized or unpadded Photoshop blocks
// and IIM datasets end the walk with nothing kept, an APP13 whose kept
// IPTC would be empty or too large for one segment is dropped, and a PNG
// text chunk without a keyword is not kept.
func TestMetadataMalformed(t *testing.T) {
	k := keepSet{copyright: true}
	app13 := func(blocks ...[]byte) []byte {
		return append([]byte(photoshopTag), bytes.Join(blocks, nil)...)
	}
	// Only a city: nothing to keep, so no segment.
	if seg := k.jpegSegment(jpegAPP13, app13(photoshopBlock(photoshopIPTC, iim(90, "Paris")))); seg != nil {
		t.Errorf("segment for a record with nothing kept: %q", seg)
	}
	// An odd-length kept record is padded to even.
	seg := k.jpegSegment(jpegAPP13, app13(photoshopBlock(photoshopIPTC, iim(iptcCopyright, "ab"))))
	if seg == nil || (len(seg)-4)%2 != 0 {
		t.Errorf("odd record: %q", seg)
	}
	// Kept datasets too large for one segment: dropped, not cut.
	big := append(iim(iptcCopyright, string(bytes.Repeat([]byte("c"), 30000))), iim(iptcByLine, string(bytes.Repeat([]byte("b"), 30000)))...)
	big = append(big, iim(iptcCredit, string(bytes.Repeat([]byte("r"), 30000)))...)
	if seg := k.jpegSegment(jpegAPP13, app13(photoshopBlock(photoshopIPTC, big))); seg != nil {
		t.Errorf("an oversized segment was built (%d bytes)", len(seg))
	}

	block := func(id uint16, nameLen byte, size uint32, data []byte) []byte {
		out := append([]byte(photoshopResource), byte(id>>8), byte(id), nameLen)
		out = append(out, make([]byte, int(nameLen)+(1+int(nameLen))%2)...)
		out = binary.BigEndian.AppendUint32(out, size)
		return append(out, data...)
	}
	for name, b := range map[string][]byte{
		"name past the end":       append([]byte(photoshopResource), 0x04, 0x04, 10, 0, 0, 0, 0, 0),
		"size past the end":       block(photoshopIPTC, 0, 1000, []byte("short")),
		"unpadded odd last block": block(0x0424, 0, 3, []byte("xyz")),
		"no IPTC block":           photoshopBlock(0x0424, []byte("xmp!")),
	} {
		if got := photoshopIPTCRecord(b); got != nil {
			t.Errorf("%s: record %q", name, got)
		}
	}
	// An extended-size dataset ends the IIM walk: what follows is not read.
	extended := append([]byte{iptcTag, iptcRecordApplication, iptcCopyright, 0x80, 0x04, 0, 0, 0, 5}, iim(iptcByLine, "after")...)
	if got := keptIPTC(append(iim(iptcCopyright, "kept"), extended...), iptcKept); !bytes.Equal(got, iim(iptcCopyright, "kept")) {
		t.Errorf("kept past an extended dataset: %q", got)
	}
	if k.keepsPNGChunk("tEXt", []byte("Copyright without a separator")) {
		t.Error("a text chunk without a keyword was kept")
	}
}
