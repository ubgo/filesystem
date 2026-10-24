package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// picture encodes a w×h PNG.
func picture(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), 40, uint8(y), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

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

// put uploads data to a tenant's slot.
func put(t *testing.T, base, tenant, slot string, data []byte) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", "brand.png")
	_, _ = w.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPut, base+"/tenants/"+tenant+"/branding/"+slot, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(t, req)
}

func do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
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

// size fetches a public URL and returns the image's dimensions.
func size(t *testing.T, url string) (int, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	cfg, _, err := image.DecodeConfig(resp.Body)
	if err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	return cfg.Width, cfg.Height
}

func TestBranding(t *testing.T) {
	srv, dir := setup(t)
	if code, body := put(t, srv.URL, "acme", slotLogo, picture(t, 800, 200)); code != http.StatusOK {
		t.Fatalf("logo = %d %s", code, body)
	}
	if code, body := put(t, srv.URL, "acme", slotFavicon, picture(t, 256, 256)); code != http.StatusOK {
		t.Fatalf("favicon = %d %s", code, body)
	}
	code, body := put(t, srv.URL, "acme", slotEmailHeader, picture(t, 1600, 400))
	if code != http.StatusOK {
		t.Fatalf("email header = %d %s", code, body)
	}
	var b branding
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	for slot, want := range map[string]map[string][2]int{
		slotLogo:        {"logo-header": {512, 128}},
		slotFavicon:     {"favicon-32": {32, 32}, "favicon-180": {180, 180}},
		slotEmailHeader: {"email-header-600": {1200, 300}},
	} {
		for name, wh := range want {
			u := b[slot][name]
			if !strings.HasPrefix(u, srv.URL+publicPrefix+"/") || strings.Contains(u, "sig=") {
				t.Errorf("%s %s url = %q, want a public URL", slot, name, u)
				continue
			}
			if w, h := size(t, u); w != wh[0] || h != wh[1] {
				t.Errorf("%s %s is %dx%d, want %dx%d", slot, name, w, h, wh[0], wh[1])
			}
		}
	}
	// 3 originals and 4 sizes; replacing the logo purges the old one and its size.
	if n := objects(t, dir); n != 7 {
		t.Errorf("%d objects, want 7", n)
	}
	if code, body := put(t, srv.URL, "acme", slotLogo, picture(t, 400, 100)); code != http.StatusOK {
		t.Fatalf("replace logo = %d %s", code, body)
	}
	if n := objects(t, dir); n != 7 {
		t.Errorf("%d objects after replacing the logo, want 7", n)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/tenants/acme/branding/"+slotFavicon, nil)
	if code, _ := do(t, req); code != http.StatusNoContent {
		t.Errorf("delete favicon = %d", code)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/tenants/acme/branding", nil)
	if _, body := do(t, req); strings.Contains(body, "favicon") || !strings.Contains(body, "logo-header") {
		t.Errorf("after delete = %s", body)
	}
	// Another tenant has no branding of its own.
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/tenants/globex/branding", nil)
	if _, body := do(t, req); body != "{}\n" {
		t.Errorf("other tenant = %s", body)
	}
}

func TestSlotRules(t *testing.T) {
	srv, dir := setup(t)
	for _, c := range []struct {
		slot string
		data []byte
		code int
		want string
	}{
		{slotFavicon, picture(t, 300, 200), http.StatusUnsupportedMediaType, "rejected"},
		{slotFavicon, picture(t, 64, 64), http.StatusUnsupportedMediaType, "rejected"},
		{slotEmailHeader, picture(t, 400, 100), http.StatusUnsupportedMediaType, "rejected"},
		{slotLogo, []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), http.StatusUnsupportedMediaType, "rejected"},
		{"wallpaper", picture(t, 300, 300), http.StatusNotFound, "not_found"},
	} {
		code, body := put(t, srv.URL, "acme", c.slot, c.data)
		if code != c.code || !strings.Contains(body, `"`+c.want+`"`) {
			t.Errorf("%s: %d %s", c.slot, code, body)
		}
	}
	if n := objects(t, dir); n != 0 {
		t.Errorf("refused uploads left %d objects", n)
	}
}
