package main

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/placeholder"
	"github.com/ubgo/filesystem/records"
)

// photo encodes a w×h JPEG.
func photo(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// put uploads data as the avatar of user, with an optional crop.
func put(t *testing.T, base, user, crop, name string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	url := base + "/users/" + user + "/avatar"
	if crop != "" {
		url += "?crop=" + crop
	}
	req, _ := http.NewRequest(http.MethodPut, url, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func setup(t *testing.T) (*App, string) {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	app, err := New(t.Context(), t.TempDir(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return app, srv.URL
}

// fetch follows the avatar redirect and probes the served image.
func fetch(t *testing.T, base, user, size string) (int, imaging.Info) {
	t.Helper()
	resp, err := http.Get(base + "/users/" + user + "/avatar?size=" + size)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, imaging.Info{}
	}
	data, _ := io.ReadAll(resp.Body)
	info, err := imaging.Probe(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, info
}

// fallbackOf asks for user's avatar without following the redirect and
// returns where it points when that is the generated fallback, "" otherwise.
func fallbackOf(t *testing.T, base, user, size string) string {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(base + "/users/" + user + "/avatar?size=" + size)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(loc, fallbackPrefix) {
		return ""
	}
	return loc
}

func TestAvatarLifecycle(t *testing.T) {
	app, base := setup(t)
	if resp := put(t, base, "u1", "", "me.jpg", photo(t, 800, 600)); resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	if code, info := fetch(t, base, "u1", "64"); code != 200 || info.Width != 64 || info.Height != 64 {
		t.Errorf("64 = %d %+v", code, info)
	}
	if code, info := fetch(t, base, "u1", "256"); code != 200 || info.Width != 256 {
		t.Errorf("256 = %d %+v", code, info)
	}
	if code, _ := fetch(t, base, "u1", "999"); code != http.StatusBadRequest {
		t.Errorf("undeclared size = %d", code)
	}

	// A cropped replacement leaves exactly one avatar, stored as a square.
	if resp := put(t, base, "u1", "100,50,400,400", "me.jpg", photo(t, 800, 600)); resp.StatusCode != http.StatusOK {
		t.Fatalf("replace = %d", resp.StatusCode)
	}
	list, err := app.Store.Attachments(t.Context(), "", records.Owner{Type: ownerUser, ID: "u1"}, roleAvatar)
	if err != nil || len(list) != 1 {
		t.Fatalf("attachments = %d, %v", len(list), err)
	}
	page, _ := app.Store.Files(t.Context(), records.FileQuery{})
	if len(page.Files) != 1 {
		t.Errorf("%d files after replace, want the old one purged", len(page.Files))
	}

	req, _ := http.NewRequest(http.MethodDelete, base+"/users/u1/avatar", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %v %v", resp, err)
	}
	if loc := fallbackOf(t, base, "u1", "64"); loc != fallbackPrefix+"u1.png?size=64&v="+placeholder.Version {
		t.Errorf("after delete, redirect = %q, want the fallback", loc)
	}
	// The fallback is a real 64 px image.
	if code, info := fetch(t, base, "u1", "64"); code != http.StatusOK || info.Width != 64 || info.Height != 64 {
		t.Errorf("fallback = %d %+v", code, info)
	}
}

func TestAvatarRefusals(t *testing.T) {
	_, base := setup(t)
	cases := []struct {
		name, crop, file string
		data             []byte
		want             int
	}{
		{"html disguised as jpg", "", "me.jpg", []byte("<html><script>alert(1)</script>"), http.StatusUnsupportedMediaType},
		{"crop outside the photo", "700,500,400,400", "me.jpg", photo(t, 800, 600), http.StatusUnsupportedMediaType},
		{"malformed crop", "1,2,3", "me.jpg", photo(t, 80, 60), http.StatusBadRequest},
		{"too large", "", "big.jpg", append(photo(t, 10, 10), make([]byte, maxAvatarBytes)...), http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		if resp := put(t, base, "u2", c.crop, c.file, c.data); resp.StatusCode != c.want {
			t.Errorf("%s = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	if fallbackOf(t, base, "u2", "64") == "" {
		t.Error("refused uploads left an avatar: no fallback redirect")
	}
}

func TestParseCrop(t *testing.T) {
	r, err := parseCrop("10,20,30,40")
	if err != nil || r != image.Rect(10, 20, 40, 60) {
		t.Errorf("parseCrop = %v, %v", r, err)
	}
	for _, bad := range []string{"a,b,c,d", "1,2,0,4", "-1,0,5,5"} {
		if _, err := parseCrop(bad); err == nil {
			t.Errorf("parseCrop(%q) accepted", bad)
		}
	}
}
