package imaging

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// Metadata names a kind of embedded metadata Optimize may keep. Optimize
// strips every kind by default, because metadata is where a photo leaks
// its owner's location and device; keeping one is an opt-in that names
// it (OptimizeOptions.KeepMetadata, Transform.KeepMetadata). No kind
// ever keeps a GPS position: EXIF is kept only as a block rebuilt from
// scratch with two text fields (MetadataEXIFCopyright), and XMP only as a
// packet rebuilt from scratch with the properties an allowlist names
// (KeepXMP, see XMPProperty), which can never name a location.
type Metadata string

// The closed set of keepable metadata.
const (
	// MetadataCopyright keeps who owns and made the image: in a JPEG the
	// IPTC copyright notice, by-line (creator), credit and source fields
	// (IPTC IIM 2:116, 2:80, 2:110, 2:115, with the record version 2:0),
	// rewritten into a Photoshop segment that holds nothing else; in a
	// PNG the text chunks keyed "Copyright" and "Author". imgproxy keeps
	// the same by default.
	MetadataCopyright Metadata = "copyright"
	// MetadataComment keeps free-text comments: a JPEG's COM segments, a
	// PNG's text chunks keyed "Comment" and "Description". A comment is
	// whatever the author typed, so keep it only when that is wanted.
	MetadataComment Metadata = "comment"
	// MetadataEXIFCopyright keeps the EXIF Artist (TIFF tag 0x013B) and
	// Copyright (0x8298) fields: a JPEG's EXIF APP1 segment and a PNG's
	// eXIf chunk are replaced by a new, minimal EXIF block that holds
	// those two text fields and nothing else. The block is written from
	// scratch, never filtered, so the source's GPS position, camera,
	// time stamps, thumbnail and orientation cannot survive; a source
	// with neither field keeps no EXIF at all. Separate from
	// MetadataCopyright (IPTC and PNG text) so that naming one never
	// changes what the other keeps.
	MetadataEXIFCopyright Metadata = "exif-copyright"
	// MetadataCaption keeps a JPEG's IPTC caption (IPTC IIM 2:120,
	// caption/abstract, with the record version 2:0) in the same rebuilt
	// Photoshop segment as MetadataCopyright. A caption is whatever the
	// author typed, like a comment.
	MetadataCaption Metadata = "caption"
)

// MetadataValues lists every Metadata.
var MetadataValues = []Metadata{MetadataCopyright, MetadataComment, MetadataEXIFCopyright, MetadataCaption}

// IPTC IIM fields MetadataCopyright keeps (record 2, the application
// record).
const (
	iptcRecordApplication = 2
	iptcRecordVersion     = 0
	iptcByLine            = 80
	iptcCredit            = 110
	iptcSource            = 115
	iptcCopyright         = 116
	iptcCaption           = 120
	// iptcTag starts every IIM dataset.
	iptcTag = 0x1C
	// iptcExtendedSize marks a dataset whose size field is itself a
	// length; such a dataset (over 32 KiB) is never a copyright field.
	iptcExtendedSize = 0x8000
)

// iptcKept are the application-record datasets MetadataCopyright keeps,
// and iptcCaptionKept those MetadataCaption keeps.
var (
	iptcKept        = []byte{iptcRecordVersion, iptcByLine, iptcCredit, iptcSource, iptcCopyright}
	iptcCaptionKept = []byte{iptcRecordVersion, iptcCaption}
)

// Photoshop image resource blocks, which carry IPTC in a JPEG's APP13.
const (
	jpegAPP13         = 0xED
	photoshopTag      = "Photoshop 3.0\x00"
	photoshopResource = "8BIM"
	// photoshopIPTC is the resource ID of the IPTC-NAA record.
	photoshopIPTC = 0x0404
)

// PNG text chunk keywords each kind keeps.
var pngKeptKeywords = map[Metadata][]string{
	MetadataCopyright: {"Copyright", "Author"},
	MetadataComment:   {"Comment", "Description"},
}

// pngTextChunks are the chunk types holding keyword and text.
var pngTextChunks = []string{"tEXt", "zTXt", "iTXt"}

// keepSet is a resolved KeepMetadata, with the KeepXMP allowlist.
type keepSet struct {
	copyright, comment, exifCopyright, caption bool
	// xmp names the XMP properties kept, CheckXMP having passed; nil
	// keeps no XMP.
	xmp []XMPProperty
}

// none reports whether k keeps nothing.
func (k keepSet) none() bool {
	return !k.copyright && !k.comment && !k.exifCopyright && !k.caption && len(k.xmp) == 0
}

// keepOf resolves kinds, reporting the first one outside MetadataValues.
func keepOf(kinds []Metadata) (keepSet, Metadata, bool) {
	var k keepSet
	for _, m := range kinds {
		switch m {
		case MetadataCopyright:
			k.copyright = true
		case MetadataComment:
			k.comment = true
		case MetadataEXIFCopyright:
			k.exifCopyright = true
		case MetadataCaption:
			k.caption = true
		default:
			return keepSet{}, m, false
		}
	}
	return k, "", true
}

// jpegSegment returns what of a metadata segment (marker, body) k keeps,
// as a whole segment, or nil: the COM segment for comments, for
// copyright a new APP13 holding only the kept IPTC fields, and for EXIF
// copyright a new APP1 holding only the EXIF Artist and Copyright.
func (k keepSet) jpegSegment(marker byte, body []byte) []byte {
	switch {
	case marker == jpegCOM && k.comment:
		return jpegSegmentOf(marker, body)
	case marker == jpegAPP1 && len(k.xmp) > 0 && bytes.HasPrefix(body, []byte(jpegXMPHeader)):
		return jpegXMPSegment(body, k.xmp)
	case marker == jpegAPP1 && k.exifCopyright:
		return jpegEXIFSegment(body)
	case marker == jpegAPP13 && (k.copyright || k.caption) && bytes.HasPrefix(body, []byte(photoshopTag)):
		iptc := keptIPTC(photoshopIPTCRecord(body[len(photoshopTag):]), k.iptcDatasets())
		if len(iptc) == 0 {
			return nil
		}
		out := append([]byte(photoshopTag), photoshopResource...)
		out = binary.BigEndian.AppendUint16(out, photoshopIPTC)
		out = append(out, 0, 0) // an empty, padded name
		out = binary.BigEndian.AppendUint32(out, uint32(len(iptc)))
		out = append(out, iptc...)
		if len(iptc)%2 == 1 {
			out = append(out, 0)
		}
		if len(out)+jpegLengthBytes > maxJPEGSegment {
			return nil
		}
		return jpegSegmentOf(marker, out)
	}
	return nil
}

// A JPEG segment's length field is jpegLengthBytes long and counts itself
// with the body, so it holds at most maxJPEGSegment (a uint16): a body
// longer than maxJPEGSegment-jpegLengthBytes cannot be written as one
// segment, and a kept block that large is dropped rather than truncated.
const (
	jpegLengthBytes = 2
	maxJPEGSegment  = 0xFFFF
)

// jpegSegmentOf builds a JPEG segment; the body fits (see maxJPEGSegment).
func jpegSegmentOf(marker byte, body []byte) []byte {
	out := []byte{jpegMarker, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(jpegLengthBytes+len(body)))
	return append(out, body...)
}

// photoshopIPTCRecord returns the data of the IPTC-NAA resource among
// Photoshop image resource blocks, or nil. A malformed block ends the
// walk: nothing after it is trusted.
func photoshopIPTCRecord(b []byte) []byte {
	for len(b) >= 4+2+2+4 && string(b[:4]) == photoshopResource {
		id := binary.BigEndian.Uint16(b[4:])
		nameLen := int(b[6])
		name := 1 + nameLen + (1+nameLen)%2 // a Pascal string padded to even
		at := 6 + name
		if at+4 > len(b) {
			return nil
		}
		size := int(binary.BigEndian.Uint32(b[at:]))
		at += 4
		if size < 0 || at+size > len(b) {
			return nil
		}
		if id == photoshopIPTC {
			return b[at : at+size]
		}
		at += size + size%2
		if at > len(b) {
			return nil
		}
		b = b[at:]
	}
	return nil
}

// iptcDatasets are the application-record datasets k keeps.
func (k keepSet) iptcDatasets() []byte {
	var out []byte
	if k.copyright {
		out = append(out, iptcKept...)
	}
	if k.caption {
		out = append(out, iptcCaptionKept...)
	}
	return out
}

// keptIPTC copies the datasets of record that are in kept.
func keptIPTC(record, kept []byte) []byte {
	var out []byte
	for len(record) >= 5 && record[0] == iptcTag {
		size := int(binary.BigEndian.Uint16(record[3:]))
		if size&iptcExtendedSize != 0 || 5+size > len(record) {
			break
		}
		if record[1] == iptcRecordApplication && slices.Contains(kept, record[2]) {
			out = append(out, record[:5+size]...)
		}
		record = record[5+size:]
	}
	return out
}

// keepsPNGChunk reports whether k keeps a text chunk of kind with data.
func (k keepSet) keepsPNGChunk(kind string, data []byte) bool {
	if !slices.Contains(pngTextChunks, kind) {
		return false
	}
	keyword, _, ok := bytes.Cut(data, []byte{0})
	if !ok {
		return false
	}
	return (k.copyright && slices.Contains(pngKeptKeywords[MetadataCopyright], string(keyword))) ||
		(k.comment && slices.Contains(pngKeptKeywords[MetadataComment], string(keyword)))
}
