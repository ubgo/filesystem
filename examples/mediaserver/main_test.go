package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

const png = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-avatar"

func postAvatar(t *testing.T, base, user, name, data string) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, _ := mw.CreateFormFile("file", name)
	_, _ = io.WriteString(w, data)
	_ = mw.Close()
	resp, err := http.Post(base+"/users/"+user+"/avatar", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

func TestAvatarFlow(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	defer srv.Close()
	app, err := New(t.Context(), t.TempDir(), srv.URL, bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Mux

	if resp := postAvatar(t, srv.URL, "u1", "me.png", png); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	if resp := postAvatar(t, srv.URL, "u1", "evil.png", "<html><script>"); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("disguised HTML = %d", resp.StatusCode)
	}
	// Replacing the avatar trashes the old file.
	if resp := postAvatar(t, srv.URL, "u1", "new.png", png+"-v2"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("replace = %d", resp.StatusCode)
	}
	resp, err := http.Get(srv.URL + "/users/u1/avatar")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != png+"-v2" || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("avatar = %d %q %s", resp.StatusCode, got, resp.Header.Get("Content-Type"))
	}
	if resp, _ := http.Get(srv.URL + "/users/nobody/avatar"); resp.StatusCode != 404 {
		t.Errorf("missing avatar = %d", resp.StatusCode)
	}
}
