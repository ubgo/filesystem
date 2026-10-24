package imaging

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
)

// realJPEG is a decodable w×h JPEG carrying an EXIF orientation.
func realJPEG(t *testing.T, w, h int, orientation uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	segments := exifJPEG(orientation, binary.BigEndian, "MM")
	segments = segments[2 : len(segments)-4] // the APP0 and APP1 segments, without SOI and SOS
	plain := buf.Bytes()
	return append(append(append([]byte{}, plain[:2]...), segments...), plain[2:]...)
}

func TestProbe(t *testing.T) {
	for orientation, want := range map[uint16][2]int{1: {40, 20}, 3: {40, 20}, 5: {20, 40}, 6: {20, 40}, 8: {20, 40}} {
		info, err := Probe(bytes.NewReader(realJPEG(t, 40, 20, orientation)))
		if err != nil || info.Format != "jpeg" || [2]int{info.Width, info.Height} != want {
			t.Errorf("orientation %d: %+v, %v, want %v", orientation, info, err, want)
		}
	}
	// A pixel bomb's header is read, never its pixels.
	if info, err := Probe(bytes.NewReader(bombPNGHeader(100000, 100000))); err != nil || info.Width != 100000 || info.Format != "png" {
		t.Errorf("bomb header = %+v, %v", info, err)
	}
	for name, data := range map[string]string{
		"not an image": "<svg/>",
		"bad header":   "\x89PNG\r\n\x1a\nbroken",
	} {
		if _, err := Probe(strings.NewReader(data)); filesystem.CodeOf(err) != filesystem.CodeRejected {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Probe(errReader{}); err == nil {
		t.Error("a read error was ignored")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, filesystem.ErrUnavailable }

// bombPNGHeader is a PNG signature and IHDR claiming w×h pixels.
func bombPNGHeader(w, h uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	chunk := append([]byte("IHDR"), ihdr...)
	out := []byte("\x89PNG\r\n\x1a\n")
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)))
	out = append(out, chunk...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(chunk))
}
