package filesystemtest

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	mimemultipart "mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// UploadFile is one file part of a multipart upload request built by
// NewUploadRequest.
type UploadFile struct {
	// Field is the form field; upload.Handler reads "file" by default.
	Field string
	// Name is the file name the client reports.
	Name string
	// Content is the file's bytes (FakeImage and FakeBytes make some).
	Content []byte
}

// NewUploadRequest builds a multipart/form-data POST to target carrying the
// form fields (in key order, before the files) and files, as a browser
// form would send it, for testing upload handlers without a browser:
//
//	req := filesystemtest.NewUploadRequest(t, "/upload", nil,
//		filesystemtest.UploadFile{Field: "file", Name: "a.png", Content: filesystemtest.FakeImage(t, filesystemtest.PNG, 64, 64)})
//	handler.ServeHTTP(rec, req)
//
// The request uses tb's context, so it is cancelled when the test ends.
func NewUploadRequest(tb testing.TB, target string, fields map[string]string, files ...UploadFile) *http.Request {
	tb.Helper()
	// Every write below goes to a bytes.Buffer, which never fails, and a
	// multipart.Writer fails only when its writer does, so none is checked.
	var body bytes.Buffer
	mw := mimemultipart.NewWriter(&body)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		_ = mw.WriteField(k, fields[k])
	}
	for _, f := range files {
		w, _ := mw.CreateFormFile(f.Field, f.Name)
		_, _ = w.Write(f.Content)
	}
	_ = mw.Close()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodPost, target, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// ImageFormat is an encoding FakeImage can produce.
type ImageFormat string

// The image formats FakeImage encodes, with the standard library's
// encoders.
const (
	PNG  ImageFormat = "png"
	JPEG ImageFormat = "jpeg"
	GIF  ImageFormat = "gif"
)

// MaxFakeImageSide bounds FakeImage's width and height, so a typo cannot
// allocate gigabytes in a test.
const MaxFakeImageSide = 8192

// FakeImage returns a real, decodable image of the given format and
// dimensions (a deterministic gradient), for tests of type detection, image
// processing and dimension checks. It fails the test for an unknown format
// or a side outside [1, MaxFakeImageSide]. To give it a particular byte
// size as well, pass it through PadTo.
func FakeImage(tb testing.TB, format ImageFormat, width, height int) []byte {
	tb.Helper()
	if width < 1 || height < 1 || width > MaxFakeImageSide || height > MaxFakeImageSide {
		tb.Fatalf("filesystemtest: fake image %dx%d is outside 1..%d", width, height, MaxFakeImageSide)
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: uint8(x * 255 / width), G: uint8(y * 255 / height), B: 128, A: 255})
		}
	}
	// The encoders fail only on a failing writer (a bytes.Buffer never
	// fails) or a side over 65,535, which MaxFakeImageSide rules out.
	var buf bytes.Buffer
	switch format {
	case PNG:
		_ = png.Encode(&buf, img)
	case JPEG:
		_ = jpeg.Encode(&buf, img, nil)
	case GIF:
		_ = gif.Encode(&buf, img, nil)
	default:
		tb.Fatalf("filesystemtest: unknown fake image format %q", format)
		return nil
	}
	return buf.Bytes()
}

// fakeByteCycle is the length of FakeBytes' repeating pattern.
const fakeByteCycle = 7

// FakeBytes returns size deterministic bytes of no recognisable type
// (detected as application/octet-stream), for size-limit tests.
func FakeBytes(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		// Control bytes 1 to 7 make content sniffers call it binary, and
		// no magic number starts with that run.
		b[i] = byte(1 + i%fakeByteCycle)
	}
	return b
}

// PadTo returns data followed by zero bytes up to size, for a file of an
// exact size that still decodes and is detected as its type: PNG, JPEG
// and GIF decoders stop at their end marker and ignore what follows. It
// fails the test when data is already longer than size.
func PadTo(tb testing.TB, data []byte, size int) []byte {
	tb.Helper()
	if len(data) > size {
		tb.Fatalf("filesystemtest: %d bytes do not fit in %d", len(data), size)
		return nil
	}
	return append(slices.Clone(data), make([]byte, size-len(data))...)
}
