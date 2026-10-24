package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/records/recordshttp"
)

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

func setup(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	app, err := New(t.Context(), t.TempDir(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return srv.URL
}

// do sends a request, as the merchant when merchant is true, and decodes a
// JSON answer into out when out is not nil.
func do[T any](t *testing.T, method, url, contentType string, body io.Reader, merchant bool, out *T) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if merchant {
		req.Header.Set(headerMerchant, merchantToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func uploadImages(t *testing.T, base string, merchant bool, files ...[]byte) (int, recordshttp.ListResponse) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for i, f := range files {
		fw, _ := mw.CreateFormFile("file", string(rune('a'+i))+".jpg")
		_, _ = fw.Write(f)
	}
	_ = mw.Close()
	var out recordshttp.ListResponse
	code := do(t, http.MethodPost, base+"/api/owners/product/p1/gallery", mw.FormDataContentType(), &body, merchant, &out)
	return code, out
}

// probe fetches url, following redirects, and reads the image's size.
func probe(t *testing.T, url string) imaging.Info {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	info, err := imaging.Probe(resp.Body)
	if err != nil {
		t.Fatalf("%s: %d %v", url, resp.StatusCode, err)
	}
	return info
}

func TestGallery(t *testing.T) {
	base := setup(t)
	if code, _ := uploadImages(t, base, false, photo(t, 300, 200)); code != http.StatusForbidden {
		t.Errorf("shopper upload = %d", code)
	}
	code, up := uploadImages(t, base, true, photo(t, 800, 600), photo(t, 400, 900), photo(t, 500, 500))
	if code != http.StatusCreated || len(up.Files) != 3 {
		t.Fatalf("upload = %d %+v", code, up)
	}
	var view productView
	if do(t, http.MethodGet, base+"/products/p1", "", nil, false, &view) != 200 || view.Cover == nil || view.Cover.FileID != up.Files[0].FileID {
		t.Fatalf("view = %+v", view)
	}
	if info := probe(t, view.Cover.Thumb); info.Width != 200 || info.Height != 200 {
		t.Errorf("thumb = %+v", info)
	}
	if info := probe(t, view.Images[1].Card); info.Width != 600 || info.Height != 600 {
		t.Errorf("padded card = %+v", info)
	}
	if info := probe(t, base+view.Images[0].Zoom); info.Width != 800 {
		t.Errorf("zoom = %+v (never enlarged)", info)
	}

	// Reorder: the third image becomes the cover. A stale list is refused.
	ids := []string{up.Files[2].AttachmentID, up.Files[0].AttachmentID, up.Files[1].AttachmentID}
	order := func(ids []string) int {
		b, _ := json.Marshal(recordshttp.ReorderRequest{IDs: ids})
		return do[struct{}](t, http.MethodPut, base+"/api/owners/product/p1/gallery/order", "application/json", bytes.NewReader(b), true, nil)
	}
	if code := order(ids[:2]); code != http.StatusBadRequest {
		t.Errorf("stale reorder = %d", code)
	}
	if code := order(ids); code != http.StatusNoContent {
		t.Fatalf("reorder = %d", code)
	}
	if code := do[struct{}](t, http.MethodDelete, base+"/api/files/"+up.Files[2].FileID, "", nil, true, nil); code != http.StatusForbidden {
		t.Errorf("trash through the API = %d", code)
	}
	remove := base + "/products/p1/images/" + up.Files[2].FileID
	if code := do[struct{}](t, http.MethodDelete, remove, "", nil, false, nil); code != http.StatusForbidden {
		t.Errorf("shopper remove = %d", code)
	}
	if code := do[struct{}](t, http.MethodDelete, base+"/products/p2/images/"+up.Files[2].FileID, "", nil, true, nil); code != http.StatusNotFound {
		t.Errorf("remove from another product = %d", code)
	}
	if code := do[struct{}](t, http.MethodDelete, remove, "", nil, true, nil); code != http.StatusNoContent {
		t.Fatalf("remove = %d", code)
	}
	// Reorder works on the list the client now sees.
	if code := order([]string{up.Files[1].AttachmentID, up.Files[0].AttachmentID}); code != http.StatusNoContent {
		t.Errorf("reorder after remove = %d", code)
	}
	view = productView{}
	do(t, http.MethodGet, base+"/products/p1", "", nil, false, &view)
	if len(view.Images) != 2 || view.Cover.FileID != up.Files[1].FileID {
		t.Errorf("after reorder and trash = %+v", view)
	}
}

func TestGalleryRefusals(t *testing.T) {
	base := setup(t)
	// One bad file sinks the whole request: nothing is attached.
	code, _ := uploadImages(t, base, true, photo(t, 100, 100), []byte("<svg onload=alert(1)>"))
	if code != http.StatusUnsupportedMediaType {
		t.Errorf("mixed upload = %d", code)
	}
	var view productView
	do(t, http.MethodGet, base+"/products/p1", "", nil, false, &view)
	if len(view.Images) != 0 {
		t.Errorf("refused request left %d images", len(view.Images))
	}
	if code := do[struct{}](t, http.MethodGet, base+"/api/owners/user/u1/avatar", "", nil, true, nil); code != http.StatusForbidden {
		t.Errorf("other owner type = %d", code)
	}
}
