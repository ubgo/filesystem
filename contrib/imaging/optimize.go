package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"image/png"
	"io"
	"slices"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// DefaultOptimizeQuality is the JPEG quality Optimize recompresses to when
// OptimizeOptions.Quality is 0: the usual point past which files grow
// faster than they look better.
const DefaultOptimizeQuality = 82

// OptimizeOptions configure Optimize.
type OptimizeOptions struct {
	// Quality is the floor a JPEG may be recompressed to, 1 to 100; 0
	// means DefaultOptimizeQuality. A JPEG saved above it is re-encoded
	// at it when that makes the file smaller; one saved at or below it is
	// never re-encoded, so a photo never loses quality twice.
	Quality int
	// KeepMetadata names the metadata kept from what Optimize strips
	// (MetadataCopyright, MetadataComment, MetadataEXIFCopyright); nil
	// keeps none, the default. Stripping is a privacy floor, so this is
	// an allowlist of kinds that cannot carry a GPS position: XMP is never
	// kept, and EXIF only as a block rebuilt with two text fields. A kept
	// segment survives the re-encode of a rotated or recompressed JPEG
	// and of a recompressed PNG. A kind outside MetadataValues is
	// CodeInvalidArgument for every JPEG and PNG.
	KeepMetadata []Metadata
	// KeepXMP names the XMP properties kept (XMPWebStatement,
	// XMPLicensor, XMPCCLicense, or any other XMPProperty): a JPEG's or
	// PNG's XMP packet is replaced by one rebuilt from scratch holding
	// only their values, as text, so nothing unnamed survives; a source
	// with none of them keeps no XMP. Nil keeps no XMP, the default. A
	// property that may locate the photo (any EXIF property, where GPS
	// lives, or a place) is CodeInvalidArgument for every JPEG and PNG
	// (CheckXMP): GPS is never kept.
	KeepXMP []XMPProperty
}

// JPEG markers and segments Optimize reads.
const (
	jpegMarker = 0xFF
	jpegEOI    = 0xD9
	jpegRST0   = 0xD0
	jpegRST7   = 0xD7
	jpegDQT    = 0xDB
	jpegAPP0   = 0xE0
	jpegAPP2   = 0xE2
	jpegAPP14  = 0xEE
	jpegAPP15  = 0xEF
	jpegCOM    = 0xFE
	// iccTag starts an APP2 segment holding an ICC colour profile.
	iccTag = "ICC_PROFILE\x00"
	// dqtValues is the size of one quantisation table.
	dqtValues = 64
)

// stdLuminanceSum is the sum of the standard JPEG luminance quantisation
// table (ITU-T T.81 Annex K), which libjpeg and Go's encoder scale by
// quality; the ratio of a file's table to it gives the quality it was
// saved at.
const stdLuminanceSum = 3688

// PNG chunk bookkeeping.
const (
	pngSignature = "\x89PNG\r\n\x1a\n"
	// chunkOverhead is a chunk's length, type and CRC fields.
	chunkOverhead = 12
)

// keptPNGChunks are the ancillary chunks Optimize keeps: transparency,
// colour (without them a picture's colours shift), pixel density, and the
// APNG animation chunks. Every other ancillary chunk (text, EXIF, time
// stamps, application data) is dropped.
var keptPNGChunks = []string{"tRNS", "iCCP", "sRGB", "gAMA", "cHRM", "sBIT", "pHYs", "acTL", "fcTL", "fdAT"}

// pngColourChunks describe colour that Go's encoder cannot write back, and
// apngChunks an animation it would drop; either makes re-encoding unsafe.
var (
	pngColourChunks = []string{"iCCP", "sRGB", "gAMA", "cHRM", "sBIT"}
	apngChunks      = []string{"acTL", "fcTL", "fdAT"}
)

// Optimize returns an upload.Processor (for upload.Policy.Process) that
// makes JPEG and PNG uploads smaller without visible loss, lossless first:
//
//   - Metadata is removed without touching the pixels: EXIF (with the
//     GPS position), XMP, IPTC, comments, text chunks and anything appended
//     after the image. The colour profile and, for PNG, transparency and
//     APNG animation are kept.
//   - A JPEG that EXIF rotates is re-encoded upright, since removing EXIF
//     would otherwise turn it sideways.
//   - A JPEG saved above OptimizeOptions.Quality is recompressed at it,
//     and kept only when smaller. Its saved quality is read from its
//     quantisation table, so a file already at or below the floor is
//     never recompressed.
//   - A PNG is recompressed at the highest zlib level and kept only when
//     smaller, unless it carries a colour profile or an animation that
//     Go's encoder would drop.
//
// The result is never larger than the input, except for a rotated JPEG.
// Other content types pass through. Input over opts.MaxBytes is
// CodeTooLarge; a JPEG or PNG whose structure is broken is CodeRejected.
// A Quality outside 0..100 is CodeInvalidArgument for every JPEG.
func Optimize(opts Options, o OptimizeOptions) upload.Processor {
	lim := opts.withDefaults()
	quality := o.Quality
	if quality == 0 {
		quality = DefaultOptimizeQuality
	}
	keep, unknown, valid := keepOf(o.KeepMetadata)
	xmpErr := CheckXMP(o.KeepXMP)
	keep.xmp = slices.Clone(o.KeepXMP)
	return func(_ context.Context, in upload.Info, body io.Reader) (io.Reader, upload.Info, error) {
		if !valid && (in.ContentType == "image/jpeg" || in.ContentType == "image/png") {
			return nil, upload.Info{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: unknown metadata kind %q to keep", unknown)
		}
		if xmpErr != nil && (in.ContentType == "image/jpeg" || in.ContentType == "image/png") {
			return nil, upload.Info{}, xmpErr
		}
		switch in.ContentType {
		case "image/jpeg":
			if quality < 1 || quality > 100 {
				return nil, upload.Info{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "imaging: optimize quality %d is not within 1..100", quality)
			}
			data, err := readLimited(body, lim.MaxBytes)
			if err != nil {
				return nil, upload.Info{}, err
			}
			out, err := optimizeJPEG(data, quality, lim, keep)
			if err != nil {
				return nil, upload.Info{}, err
			}
			return bytes.NewReader(out), in, nil
		case "image/png":
			data, err := readLimited(body, lim.MaxBytes)
			if err != nil {
				return nil, upload.Info{}, err
			}
			out, err := optimizePNG(data, lim, keep)
			if err != nil {
				return nil, upload.Info{}, err
			}
			return bytes.NewReader(out), in, nil
		}
		return body, in, nil
	}
}

func optimizeJPEG(data []byte, quality int, lim Options, keep keepSet) ([]byte, error) {
	stripped, saved, kept, err := stripJPEG(data, keep)
	if err != nil {
		return nil, err
	}
	rotate := exifOrientation(data) != orientNormal
	if !rotate && saved <= quality {
		return stripped, nil
	}
	img, err := defaultPipeline.decode(data, lim.MaxPixels)
	if err != nil {
		return nil, err
	}
	img = orient(img, exifOrientation(data))
	// Never re-encode above the quality it was saved at: that only grows
	// the file.
	var buf bytes.Buffer
	_ = jpegEncoder{}.Encode(&buf, img, min(quality, saved))
	// The kept metadata goes right after the new file's start marker.
	encoded := append(append([]byte{jpegMarker, jpegSOI}, kept...), buf.Bytes()[2:]...)
	if rotate || len(encoded) < len(stripped) {
		return encoded, nil
	}
	return stripped, nil
}

// stripJPEG copies a JPEG without its metadata segments and without
// anything after its end, and returns the quality its first quantisation
// table was saved at (100 when it has none) and the metadata keep kept,
// as whole segments, which are also in the copy where they were.
func stripJPEG(data []byte, keep keepSet) ([]byte, int, []byte, error) {
	broken := func(why string) ([]byte, int, []byte, error) {
		return nil, 0, nil, filesystem.Errorf(filesystem.CodeRejected, "imaging: broken JPEG: %s", why)
	}
	var kept []byte
	if len(data) < 4 || data[0] != jpegMarker || data[1] != jpegSOI {
		return broken("no start marker")
	}
	out := []byte{jpegMarker, jpegSOI}
	saved := 100
	seenTable := false
	i := 2
	for {
		if i+4 > len(data) || data[i] != jpegMarker {
			return broken("segment expected")
		}
		marker := data[i+1]
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		if size < 2 || i+2+size > len(data) {
			return broken("segment overruns the file")
		}
		segment := data[i : i+2+size]
		body := segment[4:]
		if marker == jpegDQT && !seenTable && len(body) >= 1+dqtValues && body[0] == 0 {
			saved, seenTable = tableQuality(body[1:1+dqtValues]), true
		}
		if keepJPEGSegment(marker, body) {
			out = append(out, segment...)
		} else if seg := keep.jpegSegment(marker, body); seg != nil {
			out = append(out, seg...)
			kept = append(kept, seg...)
		}
		i += 2 + size
		if marker == jpegSOS {
			break
		}
	}
	// Entropy-coded data, with the later tables and scans of a
	// progressive JPEG, up to and including the end marker. A metadata
	// segment between scans is dropped too.
	from := i
	for i+1 < len(data) {
		if data[i] != jpegMarker {
			i++
			continue
		}
		m := data[i+1]
		switch {
		case m == 0x00 || m == jpegMarker || (m >= jpegRST0 && m <= jpegRST7):
			i += 2
		case m == jpegEOI:
			return append(out, data[from:i+2]...), saved, kept, nil
		default:
			if i+4 > len(data) {
				return broken("segment expected")
			}
			next := i + 2 + int(binary.BigEndian.Uint16(data[i+2:]))
			if next > len(data) {
				return broken("segment overruns the file")
			}
			if !keepJPEGSegment(m, data[min(i+4, next):next]) {
				out = append(out, data[from:i]...)
				from = next
			}
			i = next
		}
	}
	return broken("no end marker")
}

// keepJPEGSegment reports whether a segment before or between scans stays:
// everything but application segments and comments, except the JFIF
// header (APP0), the colour profile (APP2 ICC_PROFILE) and the Adobe
// segment (APP14), which says how CMYK and YCCK pixels are stored.
func keepJPEGSegment(marker byte, body []byte) bool {
	switch {
	case marker == jpegCOM:
		return false
	case marker < jpegAPP0 || marker > jpegAPP15:
		return true
	case marker == jpegAPP0, marker == jpegAPP14:
		return true
	case marker == jpegAPP2:
		return bytes.HasPrefix(body, []byte(iccTag))
	}
	return false
}

// tableQuality estimates the quality a luminance quantisation table was
// made with, inverting libjpeg's scaling of the standard table.
func tableQuality(table []byte) int {
	sum := 0
	for _, v := range table {
		sum += int(v)
	}
	scale := float64(sum) * 100 / stdLuminanceSum
	var q float64
	if scale <= 100 {
		q = (200 - scale) / 2
	} else {
		q = 5000 / scale
	}
	return min(max(int(q+0.5), 1), 100)
}

func optimizePNG(data []byte, lim Options, keep keepSet) ([]byte, error) {
	stripped, reencodable, kept, err := stripPNG(data, keep)
	if err != nil {
		return nil, err
	}
	if !reencodable {
		return stripped, nil
	}
	img, err := defaultPipeline.decode(data, lim.MaxPixels)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	_ = enc.Encode(&buf, img)
	// The kept text chunks go right after IHDR, which a PNG starts with.
	ihdrEnd := len(pngSignature) + chunkOverhead + pngIHDRBytes
	encoded := append(append(slices.Clone(buf.Bytes()[:ihdrEnd]), kept...), buf.Bytes()[ihdrEnd:]...)
	if len(encoded) < len(stripped) {
		return encoded, nil
	}
	return stripped, nil
}

// stripPNG copies a PNG without the ancillary chunks Optimize drops and
// without anything after IEND, and reports whether re-encoding it would
// lose nothing (no colour chunks, no animation) and the text chunks keep
// kept, which are also in the copy where they were.
func stripPNG(data []byte, keep keepSet) ([]byte, bool, []byte, error) {
	broken := func(why string) ([]byte, bool, []byte, error) {
		return nil, false, nil, filesystem.Errorf(filesystem.CodeRejected, "imaging: broken PNG: %s", why)
	}
	var kept []byte
	if !bytes.HasPrefix(data, []byte(pngSignature)) {
		return broken("no signature")
	}
	out := []byte(pngSignature)
	reencodable := true
	for i := len(pngSignature); ; {
		if i+chunkOverhead > len(data) {
			return broken("chunk expected")
		}
		n := int(binary.BigEndian.Uint32(data[i:]))
		end := i + chunkOverhead + n
		if n < 0 || end > len(data) || end < i {
			return broken("chunk overruns the file")
		}
		kind := string(data[i+4 : i+8])
		critical := kind[0] >= 'A' && kind[0] <= 'Z'
		switch {
		case critical || slices.Contains(keptPNGChunks, kind):
			out = append(out, data[i:end]...)
		case kind == "iTXt" && len(keep.xmp) > 0 && isPNGXMP(data[i+8:end-4]):
			// Rebuilt with the named properties, never copied.
			if chunk := pngXMPChunk(data[i+8:end-4], keep.xmp); chunk != nil {
				out = append(out, chunk...)
				kept = append(kept, chunk...)
			}
		case keep.keepsPNGChunk(kind, data[i+8:end-4]):
			out = append(out, data[i:end]...)
			kept = append(kept, data[i:end]...)
		case kind == pngEXIfChunk && keep.exifCopyright:
			// Rebuilt with the two kept fields, never copied.
			if chunk := pngEXIFChunk(data[i+8 : end-4]); chunk != nil {
				out = append(out, chunk...)
				kept = append(kept, chunk...)
			}
		}
		if slices.Contains(pngColourChunks, kind) || slices.Contains(apngChunks, kind) {
			reencodable = false
		}
		if kind == "IEND" {
			return out, reencodable, kept, nil
		}
		i = end
	}
}
