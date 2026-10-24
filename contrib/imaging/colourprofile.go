package imaging

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"io"
	"slices"
)

// ColourConverter turns img, whose pixels are in the colour space an ICC
// profile describes, into sRGB, the space browsers assume for an image
// without a profile. ok is false when it cannot handle that profile; the
// Pipeline then keeps the profile in the output instead. The pure-Go
// converter for RGB matrix profiles (Display P3, Adobe RGB, ProPhoto) is
// in contrib/imaging/icc; another engine (lcms2) can be passed the same
// way.
type ColourConverter func(profile []byte, img image.Image) (out image.Image, ok bool, err error)

// WithColourManagement makes the Pipeline handle embedded ICC colour
// profiles, which a re-encode otherwise drops (a Display P3 photo from a
// phone then looks washed out). An image with a profile is converted to
// sRGB by conv; when conv is nil, or cannot handle the profile, the
// profile is written into the output instead, for JPEG and PNG output, so
// its colours stay right. Without this option profiles are dropped, as
// before. Process and Sanitize (WithPipeline) both apply it.
func WithColourManagement(conv ColourConverter) PipelineOption {
	return func(p *Pipeline) {
		p.colourManaged, p.convert = true, conv
	}
}

// manageColour converts img to sRGB when the Pipeline manages colour and
// data carries a profile, or returns the profile to keep in the output.
func (p *Pipeline) manageColour(data []byte, img image.Image) (image.Image, []byte, error) {
	if !p.colourManaged {
		return img, nil, nil
	}
	profile, found := EmbeddedProfile(data)
	if !found {
		return img, nil, nil
	}
	if p.convert != nil {
		out, ok, err := p.convert(profile, img)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			return out, nil, nil
		}
	}
	return img, profile, nil
}

// ICC profile containers.
const (
	// iccChunkHeader is "ICC_PROFILE\0", then the chunk's sequence number
	// and the chunk count, in each JPEG APP2 segment of a profile.
	iccChunkHeader = len(iccTag) + 2
	// iccChunkMax is the most profile bytes one APP2 segment holds.
	iccChunkMax = 65535 - 2 - iccChunkHeader
	// iccProfileName names the profile in a PNG iCCP chunk.
	iccProfileName = "ICC Profile"
	// MaxProfileBytes bounds an embedded profile: real ones are a few
	// kilobytes (a lookup-table profile rarely passes 1 MiB), and a PNG's
	// compressed profile could otherwise inflate a thousandfold. It also
	// keeps a re-embedded profile within the 255 segments a JPEG allows.
	MaxProfileBytes = 4 << 20
)

// EmbeddedProfile returns the ICC profile embedded in a JPEG (APP2
// ICC_PROFILE segments, reassembled in sequence order) or a PNG (an iCCP
// chunk, decompressed); found is false when there is none or it is
// malformed, or larger than MaxProfileBytes. It reads only data's
// structure and never decodes pixels.
func EmbeddedProfile(data []byte) (profile []byte, found bool) {
	switch {
	case bytes.HasPrefix(data, []byte{jpegMarker, jpegSOI}):
		return jpegProfile(data)
	case bytes.HasPrefix(data, []byte(pngSignature)):
		return pngProfile(data)
	}
	return nil, false
}

func jpegProfile(data []byte) ([]byte, bool) {
	chunks := map[byte][]byte{}
	var count byte
	for i := 2; i+4 <= len(data) && data[i] == jpegMarker; {
		marker := data[i+1]
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		if marker == jpegSOS || size < 2 || i+2+size > len(data) {
			break
		}
		body := data[i+4 : i+2+size]
		if marker == jpegAPP2 && len(body) > iccChunkHeader && bytes.HasPrefix(body, []byte(iccTag)) {
			seq := body[len(iccTag)]
			count = body[len(iccTag)+1]
			chunks[seq] = body[iccChunkHeader:]
		}
		i += 2 + size
	}
	if count == 0 || len(chunks) != int(count) {
		return nil, false
	}
	var profile []byte
	for seq := byte(1); seq <= count; seq++ {
		chunk, ok := chunks[seq]
		if !ok || len(profile)+len(chunk) > MaxProfileBytes {
			return nil, false
		}
		profile = append(profile, chunk...)
	}
	return profile, true
}

func pngProfile(data []byte) ([]byte, bool) {
	for i := len(pngSignature); i+chunkOverhead <= len(data); {
		n := int(binary.BigEndian.Uint32(data[i:]))
		end := i + chunkOverhead + n
		if n < 0 || end > len(data) {
			return nil, false
		}
		kind, body := string(data[i+4:i+8]), data[i+8:i+8+n]
		switch kind {
		case "iCCP":
			// Name, NUL, compression method (0: zlib), compressed profile.
			nul := bytes.IndexByte(body, 0)
			if nul < 0 || nul+2 > len(body) {
				return nil, false
			}
			zr, err := zlib.NewReader(bytes.NewReader(body[nul+2:]))
			if err != nil {
				return nil, false
			}
			profile, err := io.ReadAll(io.LimitReader(zr, MaxProfileBytes+1))
			if err != nil || len(profile) > MaxProfileBytes {
				return nil, false
			}
			return profile, true
		case "IDAT", "IEND":
			return nil, false // the profile must come before the image data
		}
		i = end
	}
	return nil, false
}

// embedProfile writes profile into an encoded JPEG (APP2 segments after
// SOI) or PNG (an iCCP chunk after IHDR); other formats are returned as
// they are. profile comes from EmbeddedProfile, so it is at most
// MaxProfileBytes: 65 JPEG segments, well under the 255 the format counts.
func embedProfile(f Format, encoded, profile []byte) []byte {
	switch f {
	case FormatJPEG:
		out := slices.Clone(encoded[:2])
		count := (len(profile) + iccChunkMax - 1) / iccChunkMax
		for seq := range count {
			part := profile[seq*iccChunkMax : min((seq+1)*iccChunkMax, len(profile))]
			out = append(out, jpegMarker, jpegAPP2)
			out = binary.BigEndian.AppendUint16(out, uint16(2+iccChunkHeader+len(part)))
			out = append(out, iccTag...)
			out = append(out, byte(seq+1), byte(count))
			out = append(out, part...)
		}
		return append(out, encoded[2:]...)
	case FormatPNG:
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		_, _ = zw.Write(profile) // a bytes.Buffer never fails a write
		_ = zw.Close()
		body := append(append([]byte(iccProfileName), 0, 0), z.Bytes()...)
		chunk := binary.BigEndian.AppendUint32(nil, uint32(len(body)))
		chunk = append(chunk, "iCCP"...)
		chunk = append(chunk, body...)
		chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(append([]byte("iCCP"), body...)))
		ihdrEnd := len(pngSignature) + chunkOverhead + pngIHDRBytes
		return append(append(slices.Clone(encoded[:ihdrEnd]), chunk...), encoded[ihdrEnd:]...)
	}
	return encoded
}

// pngIHDRBytes is the length of a PNG's IHDR data.
const pngIHDRBytes = 13
