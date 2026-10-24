package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// heicHead is the start of an iPhone HEIC file: an ISO "ftyp" box with
// the heic brand, which is how the type is detected.
const heicHead = "\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic"

// photo encodes a w×h JPEG.
func photo(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 80, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// file is one part of a batch.
type file struct {
	name string
	data []byte
}

func setup(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	app, err := New(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)
	return srv.URL, dir
}

// post sends a batch as user and decodes the answer.
func post(t *testing.T, base, user string, files ...file) (int, batchResponse) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		w, _ := mw.CreateFormFile(formField, f.name)
		_, _ = w.Write(f.data)
	}
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+"/photos", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(headerUser, user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out batchResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// objects counts the files stored on the local disk.
func objects(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(filepath.Join(dir, "files"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestPartialBatch(t *testing.T) {
	base, dir := setup(t)
	code, out := post(t, base, "u1",
		file{"IMG_0001.jpg", photo(t, 64, 48)},
		file{"IMG_0002.HEIC", []byte(heicHead)},
		file{"notes.jpg", []byte("<html><script>")},
	)
	if code != http.StatusOK || len(out.Files) != 1 || len(out.Failed) != 2 {
		t.Fatalf("batch = %d %+v", code, out)
	}
	if out.Failed[0].Code != "rejected" || out.Failed[1].Code != "rejected" {
		t.Errorf("failures = %+v", out.Failed)
	}
	// A retry after a lost response stores nothing new.
	code, again := post(t, base, "u1", file{"IMG_0001.jpg", photo(t, 64, 48)})
	if code != http.StatusCreated || !again.Files[0].Duplicate || again.Files[0].FileID != out.Files[0].FileID {
		t.Errorf("retry = %d %+v", code, again)
	}
	if n := objects(t, dir); n != 1 {
		t.Errorf("%d objects stored, want 1", n)
	}
	// Another user's identical photo is theirs, not a duplicate.
	if _, other := post(t, base, "u2", file{"IMG_0001.jpg", photo(t, 64, 48)}); other.Files[0].Duplicate {
		t.Error("duplicate detected across users")
	}
}

func TestRequestFailures(t *testing.T) {
	base, _ := setup(t)
	if code, _ := post(t, base, "u1", file{"a.jpg", []byte("<html>")}); code != http.StatusUnsupportedMediaType {
		t.Errorf("all failed = %d", code)
	}
	var many []file
	for range maxFilesPerRequest + 1 {
		many = append(many, file{"x.jpg", photo(t, 8, 8)})
	}
	if code, _ := post(t, base, "u1", many...); code != http.StatusBadRequest {
		t.Errorf("too many files = %d", code)
	}
	if code, _ := post(t, base, "", file{"a.jpg", photo(t, 8, 8)}); code != http.StatusForbidden {
		t.Errorf("anonymous = %d", code)
	}
}

// TestDownscaleContract checks both sides of the limits endpoint: the
// server publishes what it enforces and refuses a photo over the longest
// side, and the same photo downscaled by the client with those limits is
// accepted.
func TestDownscaleContract(t *testing.T) {
	base, dir := setup(t)
	l, err := fetchLimits(t.Context(), http.DefaultClient, base)
	if err != nil {
		t.Fatal(err)
	}
	if l.MaxWidth != maxPhotoSide || l.MaxHeight != maxPhotoSide || l.MaxBytes != maxPhotoBytes || len(l.Allowed) != 3 {
		t.Fatalf("limits = %+v", l)
	}
	wide := photo(t, maxPhotoSide+500, 60)
	code, out := post(t, base, "u1", file{"pano.jpg", wide})
	if code != http.StatusUnsupportedMediaType || len(out.Failed) != 1 || out.Failed[0].Code != "rejected" {
		t.Fatalf("oversized photo = %d %+v", code, out)
	}
	if n := objects(t, dir); n != 0 {
		t.Errorf("refused photo left %d objects", n)
	}
	small, err := downscale(wide, l)
	if err != nil {
		t.Fatal(err)
	}
	if code, out := post(t, base, "u1", file{"pano.jpg", small}); code != http.StatusCreated || len(out.Files) != 1 {
		t.Fatalf("downscaled photo = %d %+v", code, out)
	}
	fitting := photo(t, 64, 48)
	if same, err := downscale(fitting, l); err != nil || !bytes.Equal(same, fitting) {
		t.Errorf("a photo that fits was changed (err %v)", err)
	}
}
