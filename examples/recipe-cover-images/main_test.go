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
	"strings"
	"testing"
)

// banner encodes a w×h JPEG with a gradient, so the smart crop has detail
// to find.
func banner(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// setup starts the app on a test server whose URL is also the public base.
func setup(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	var app *App
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.Handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	var err error
	if app, err = New(t.Context(), dir, srv.URL); err != nil {
		t.Fatal(err)
	}
	return srv, dir
}

// put uploads data as the channel's cover.
func put(t *testing.T, base, channel string, data []byte) (int, coverResponse, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", "banner.jpg")
	_, _ = w.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPut, base+"/channels/"+channel+"/cover", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(t, req)
}

// do sends req and decodes a cover answer, keeping the raw body.
func do(t *testing.T, req *http.Request) (int, coverResponse, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	var out coverResponse
	_ = json.Unmarshal(raw.Bytes(), &out)
	return resp.StatusCode, out, raw.String()
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

func TestCoverLifecycle(t *testing.T) {
	srv, dir := setup(t)
	code, out, raw := put(t, srv.URL, "c1", banner(t, 2400, 1000))
	if code != http.StatusOK {
		t.Fatalf("upload = %d %s", code, raw)
	}
	if !strings.Contains(out.SrcSet, " 600w, ") || !strings.HasSuffix(out.SrcSet, " 1800w") || len(out.URLs) != 3 {
		t.Fatalf("srcset = %q urls = %v", out.SrcSet, out.URLs)
	}
	if n := objects(t, dir); n != 4 {
		t.Errorf("%d objects after upload, want the cover and three widths", n)
	}
	// Each width is a real, public JPEG of that width.
	resp, err := http.Get(out.URLs["cover"])
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(resp.Body)
	_ = resp.Body.Close()
	if err != nil || cfg.Width != 600 || cfg.Height != 200 {
		t.Errorf("cover is %dx%d (%v)", cfg.Width, cfg.Height, err)
	}

	// A replacement purges the old cover and its variants.
	if code, _, raw := put(t, srv.URL, "c1", banner(t, 1500, 500)); code != http.StatusOK {
		t.Fatalf("replace = %d %s", code, raw)
	}
	if n := objects(t, dir); n != 4 {
		t.Errorf("%d objects after replacing, want 4", n)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/channels/c1/cover", nil)
	if code, got, _ := do(t, req); code != http.StatusOK || !strings.Contains(got.SrcSet, " 1500w") {
		// A 1500-pixel source gives a 1500-pixel "cover-3x": a density
		// version never enlarges, and SrcSet reports the real width.
		t.Errorf("get = %d %q", code, got.SrcSet)
	}

	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/channels/c1/cover", nil)
	if code, _, _ := do(t, req); code != http.StatusNoContent {
		t.Errorf("delete = %d", code)
	}
	if n := objects(t, dir); n != 0 {
		t.Errorf("%d objects after delete", n)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/channels/c1/cover", nil)
	if code, _, _ := do(t, req); code != http.StatusNotFound {
		t.Errorf("get after delete = %d", code)
	}
}

func TestRefusals(t *testing.T) {
	srv, dir := setup(t)
	if code, _, raw := put(t, srv.URL, "c1", banner(t, 800, 300)); code != http.StatusUnsupportedMediaType || !strings.Contains(raw, `"rejected"`) {
		t.Errorf("too small = %d %s", code, raw)
	}
	if code, _, raw := put(t, srv.URL, "c1", []byte("<html><script>")); code != http.StatusUnsupportedMediaType {
		t.Errorf("html = %d %s", code, raw)
	}
	if n := objects(t, dir); n != 0 {
		t.Errorf("refused uploads left %d objects", n)
	}
}
