package filesystemtest_test

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"testing"

	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/sniff"
)

func TestFakeImages(t *testing.T) {
	for format, mime := range map[filesystemtest.ImageFormat]string{filesystemtest.PNG: "image/png", filesystemtest.JPEG: "image/jpeg", filesystemtest.GIF: "image/gif"} {
		data := filesystemtest.FakeImage(t, format, 40, 30)
		cfg, name, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width != 40 || cfg.Height != 30 || name != string(format) || sniff.Detect(data, "") != mime {
			t.Errorf("%s: %+v %s %v, detected %s", format, cfg, name, err, sniff.Detect(data, ""))
		}
		// Padded to an exact size, it still decodes and is detected.
		padded := filesystemtest.PadTo(t, data, len(data)+1000)
		if len(padded) != len(data)+1000 || sniff.Detect(padded, "") != mime {
			t.Errorf("%s padded: %d bytes, %s", format, len(padded), sniff.Detect(padded, ""))
		}
		if _, _, err := image.Decode(bytes.NewReader(padded)); err != nil {
			t.Errorf("%s padded does not decode: %v", format, err)
		}
	}
	b := filesystemtest.FakeBytes(1000)
	if len(b) != 1000 || sniff.Detect(b, "") != "application/octet-stream" || !bytes.Equal(b, filesystemtest.FakeBytes(1000)) {
		t.Errorf("fake bytes detected as %s", sniff.Detect(b, ""))
	}
}

func TestFakeImageRefusals(t *testing.T) {
	failed := func(fn func(tb testing.TB)) bool {
		r := &recorder{TB: t}
		fn(r)
		return r.fatal != ""
	}
	for name, fn := range map[string]func(tb testing.TB){
		"zero width": func(tb testing.TB) { filesystemtest.FakeImage(tb, filesystemtest.PNG, 0, 10) },
		"too tall": func(tb testing.TB) {
			filesystemtest.FakeImage(tb, filesystemtest.PNG, 10, filesystemtest.MaxFakeImageSide+1)
		},
		"unknown format": func(tb testing.TB) { filesystemtest.FakeImage(tb, "webp", 10, 10) },
		"pad too small":  func(tb testing.TB) { filesystemtest.PadTo(tb, []byte("abc"), 2) },
	} {
		if !failed(fn) {
			t.Errorf("%s did not fail the test", name)
		}
	}
}

func TestNewUploadRequest(t *testing.T) {
	png := filesystemtest.FakeImage(t, filesystemtest.PNG, 8, 8)
	req := filesystemtest.NewUploadRequest(t, "/upload", map[string]string{"caption": "hi", "album": "x"},
		filesystemtest.UploadFile{Field: "file", Name: "a.png", Content: png},
		filesystemtest.UploadFile{Field: "file", Name: "b.bin", Content: filesystemtest.FakeBytes(10)})
	if req.Method != http.MethodPost || req.URL.Path != "/upload" {
		t.Fatalf("request %s %s", req.Method, req.URL)
	}
	mr, err := req.MultipartReader()
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		seen = append(seen, part.FormName()+"/"+part.FileName())
		if part.FileName() == "a.png" && !bytes.Equal(data, png) {
			t.Error("file content differs")
		}
	}
	want := []string{"album/", "caption/", "file/a.png", "file/b.bin"}
	if len(seen) != len(want) {
		t.Fatalf("parts %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("parts %v, want %v", seen, want)
		}
	}
}
