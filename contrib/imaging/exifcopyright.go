package imaging

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"slices"
)

// EXIF fields MetadataEXIFCopyright keeps, from IFD0 of the source's TIFF
// block. They are listed in ascending tag order, the order TIFF requires
// of the entries of an IFD.
const (
	// exifTagArtist is TIFF tag 0x013B, the person who made the image.
	exifTagArtist = 0x013B
	// exifTagCopyright is TIFF tag 0x8298, the copyright notice.
	exifTagCopyright = 0x8298
	// tiffTypeASCII is the TIFF field type of NUL-terminated text, the
	// only type either field may have; a field of any other type is
	// malformed and not kept.
	tiffTypeASCII = 2
	// exifMaxField caps the bytes kept of one field. Real notices are a
	// line; the cap keeps two fields far inside one JPEG segment
	// (65,533 bytes), so a kept block never has to be cut.
	exifMaxField = 4096
	// tiffInlineBytes is the size of an IFD entry's value field: a value
	// this short sits in the entry, a longer one at an offset.
	tiffInlineBytes = 4
	// exifHeader starts an APP1 segment holding EXIF.
	exifHeader = "Exif\x00\x00"
	// pngEXIfChunk is the PNG chunk holding a bare TIFF block of EXIF.
	pngEXIfChunk = "eXIf"
)

// exifKeptTags are the IFD0 tags MetadataEXIFCopyright copies, ascending.
var exifKeptTags = []uint16{exifTagArtist, exifTagCopyright}

// exifField is one kept ASCII field: its tag and its text, NUL-terminated.
type exifField struct {
	tag  uint16
	text []byte
}

// keptEXIF reads the Artist and Copyright fields from IFD0 of the TIFF
// block t and returns a new TIFF block holding those two fields and
// nothing else, or nil when t holds neither or is malformed.
//
// Invariant (the GPS floor): nothing of t is copied but the two fields'
// text. The source's other entries, its sub-IFD pointers (EXIF, GPS,
// interoperability), the IFD1 thumbnail and every byte between are never
// read into the output, because the output is built from scratch rather
// than filtered. Every offset is bounds-checked: the input is untrusted.
func keptEXIF(t []byte) []byte {
	order, ok := tiffByteOrder(t)
	if !ok {
		return nil
	}
	ifd := int(order.Uint32(t[4:]))
	if ifd < tiffHeaderBytes || ifd+2 > len(t) {
		return nil
	}
	n := int(order.Uint16(t[ifd:]))
	var fields []exifField
	for e := range n {
		off := ifd + 2 + e*ifdEntryBytes
		if off+ifdEntryBytes > len(t) {
			return nil
		}
		tag := order.Uint16(t[off:])
		if !slices.Contains(exifKeptTags, tag) || order.Uint16(t[off+2:]) != tiffTypeASCII {
			continue
		}
		count := int(order.Uint32(t[off+4:]))
		at := off + 8
		if count > tiffInlineBytes {
			at = int(order.Uint32(t[off+8:]))
		}
		if count < 1 || at < 0 || at+count > len(t) || at+count < at {
			continue
		}
		text := bytes.TrimRight(t[at:at+count], "\x00")
		if len(text) == 0 {
			continue
		}
		text = append(bytes.Clone(text[:min(len(text), exifMaxField-1)]), 0)
		fields = append(fields, exifField{tag: tag, text: text})
	}
	if len(fields) == 0 {
		return nil
	}
	// Tags come back in the source's order; TIFF wants them ascending,
	// and exifKeptTags is.
	ordered := make([]exifField, 0, len(fields))
	for _, tag := range exifKeptTags {
		for _, f := range fields {
			if f.tag == tag {
				ordered = append(ordered, f)
				break
			}
		}
	}
	return tiffBlock(ordered)
}

// tiffByteOrder reads the byte order of a TIFF header.
func tiffByteOrder(t []byte) (binary.ByteOrder, bool) {
	if len(t) < tiffHeaderBytes {
		return nil, false
	}
	switch string(t[:2]) {
	case "II":
		return binary.LittleEndian, true
	case "MM":
		return binary.BigEndian, true
	}
	return nil, false
}

// tiffMagic is the second field of every TIFF header.
const tiffMagic = 42

// tiffBlock writes a big-endian TIFF block whose only IFD holds fields
// (ASCII, ascending tags) and links to no other IFD.
func tiffBlock(fields []exifField) []byte {
	order := binary.BigEndian
	out := []byte("MM")
	out = order.AppendUint16(out, tiffMagic)
	out = order.AppendUint32(out, tiffHeaderBytes)
	out = order.AppendUint16(out, uint16(len(fields)))
	data := tiffHeaderBytes + 2 + len(fields)*ifdEntryBytes + 4
	var tail []byte
	for _, f := range fields {
		out = order.AppendUint16(out, f.tag)
		out = order.AppendUint16(out, tiffTypeASCII)
		out = order.AppendUint32(out, uint32(len(f.text)))
		if len(f.text) <= tiffInlineBytes {
			value := make([]byte, tiffInlineBytes)
			copy(value, f.text)
			out = append(out, value...)
			continue
		}
		out = order.AppendUint32(out, uint32(data+len(tail)))
		tail = append(tail, f.text...)
		if len(tail)%2 == 1 {
			tail = append(tail, 0) // values start on a word boundary
		}
	}
	out = order.AppendUint32(out, 0) // no next IFD
	return append(out, tail...)
}

// jpegEXIFSegment returns a new APP1 segment holding only the kept EXIF
// fields of the APP1 body, or nil.
func jpegEXIFSegment(body []byte) []byte {
	if !bytes.HasPrefix(body, []byte(exifHeader)) {
		return nil
	}
	t := keptEXIF(body[len(exifHeader):])
	if t == nil {
		return nil
	}
	return jpegSegmentOf(jpegAPP1, append([]byte(exifHeader), t...))
}

// pngEXIFChunk returns a new eXIf chunk holding only the kept EXIF fields
// of an eXIf chunk's data, or nil.
func pngEXIFChunk(data []byte) []byte {
	t := keptEXIF(data)
	if t == nil {
		return nil
	}
	return pngChunkOf(pngEXIfChunk, t)
}

// pngChunkOf builds a PNG chunk: length, type, data and the CRC of type
// and data.
func pngChunkOf(kind string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, kind...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}
