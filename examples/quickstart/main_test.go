package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/signedurl"
)

// testSide is the width and height of the generated test photo.
const testSide = 16

// testServer starts the app on a temporary directory behind an httptest
// server, whose URL becomes the base of signed links.
// ds:def id=quickstart-testserver-6kckt4sx
func testServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	a, err := newApp(t.Context(), config{BaseURL: srv.URL, DataDir: dir, Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	srv.Config.Handler = a.Handler()
	return srv, dir
}

// pngBytes is a small, real PNG.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, testSide, testSide))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// do sends one request as user and returns the status and body.
func do(t *testing.T, srv *httptest.Server, method, path, user string, body io.Reader, contentType string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if user != "" {
		req.Header.Set(userHeader, user)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// uploadAs posts data as the form file name.
func uploadAs(t *testing.T, srv *httptest.Server, user, name string, data []byte) (int, []byte) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, err := mw.CreateFormFile(formField, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return do(t, srv, http.MethodPost, "/photos", user, &b, mw.FormDataContentType())
}

// decode unmarshals a JSON body or fails the test.
func decode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// TestPhotoLifecycle walks one photo through the whole tutorial: upload,
// list, open through a signed link, trash, restore, trash, purge.
// ds:def id=quickstart-lifecycle-test-murxdrz5
func TestPhotoLifecycle(t *testing.T) {
	srv, dir := testServer(t)
	img := pngBytes(t)

	status, body := uploadAs(t, srv, "alice", "beach.png", img)
	if status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, body)
	}
	p := decode[photo](t, body)
	if p.ContentType != "image/png" || p.Size != int64(len(img)) || p.Filename != "beach.png" {
		t.Fatalf("uploaded %+v", p)
	}
	stored, err := filepath.Glob(filepath.Join(dir, "files", rolePhotos, "*.png"))
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored files = %v, %v", stored, err)
	}

	if _, body = do(t, srv, http.MethodGet, "/photos", "alice", nil, ""); len(decode[[]photo](t, body)) != 1 {
		t.Fatalf("alice's photos = %s", body)
	}
	if _, body = do(t, srv, http.MethodGet, "/photos", "bob", nil, ""); len(decode[[]photo](t, body)) != 0 {
		t.Fatalf("bob sees %s", body)
	}
	if status, _ = do(t, srv, http.MethodGet, "/photos/"+p.ID, "bob", nil, ""); status != http.StatusNotFound {
		t.Fatalf("bob opening alice's photo = %d", status)
	}
	if status, body = do(t, srv, http.MethodGet, "/photos/"+p.ID, "alice", nil, ""); status != http.StatusOK || !bytes.Equal(body, img) {
		t.Fatalf("download through the signed link = %d, %d bytes", status, len(body))
	}

	trashAndPurge(t, srv, p.ID, stored[0])
}

// trashAndPurge checks the second half of the lifecycle.
func trashAndPurge(t *testing.T, srv *httptest.Server, id, storedPath string) {
	t.Helper()
	if status, _ := do(t, srv, http.MethodDelete, "/trash/"+id, "alice", nil, ""); status != http.StatusBadRequest {
		t.Fatalf("purge of a live photo = %d", status)
	}
	if status, _ := do(t, srv, http.MethodDelete, "/photos/"+id, "alice", nil, ""); status != http.StatusNoContent {
		t.Fatalf("trash = %d", status)
	}
	if _, body := do(t, srv, http.MethodGet, "/photos", "alice", nil, ""); len(decode[[]photo](t, body)) != 0 {
		t.Fatalf("trashed photo still listed: %s", body)
	}
	if _, body := do(t, srv, http.MethodGet, "/trash", "alice", nil, ""); len(decode[[]photo](t, body)) != 1 {
		t.Fatalf("trash = %s", body)
	}
	if status, _ := do(t, srv, http.MethodPost, "/photos/"+id+"/restore", "alice", nil, ""); status != http.StatusNoContent {
		t.Fatalf("restore = %d", status)
	}
	if _, body := do(t, srv, http.MethodGet, "/photos", "alice", nil, ""); len(decode[[]photo](t, body)) != 1 {
		t.Fatalf("restored photo not listed: %s", body)
	}
	_, _ = do(t, srv, http.MethodDelete, "/photos/"+id, "alice", nil, "")
	if status, _ := do(t, srv, http.MethodDelete, "/trash/"+id, "alice", nil, ""); status != http.StatusNoContent {
		t.Fatalf("purge = %d", status)
	}
	if _, err := os.Stat(storedPath); !os.IsNotExist(err) {
		t.Fatalf("purged bytes still on disk: %v", err)
	}
	if status, _ := do(t, srv, http.MethodGet, "/photos/"+id, "alice", nil, ""); status != http.StatusNotFound {
		t.Fatalf("purged photo = %d", status)
	}
}

// TestRefusals checks what the policy and the handlers refuse, and that a
// refused upload leaves nothing on disk.
func TestRefusals(t *testing.T) {
	srv, dir := testServer(t)
	cases := []struct {
		name   string
		file   string
		data   []byte
		status int
		code   filesystem.Code
	}{
		{"text named as a photo", "cat.png", []byte("just some text"), http.StatusUnsupportedMediaType, filesystem.CodeRejected},
		{"png named as a jpeg", "cat.jpg", pngBytes(t), http.StatusUnsupportedMediaType, filesystem.CodeRejected},
		{"too large", "big.png", append(pngBytes(t), make([]byte, maxPhotoSize)...), http.StatusRequestEntityTooLarge, filesystem.CodeTooLarge},
	}
	for _, c := range cases {
		status, body := uploadAs(t, srv, "alice", c.file, c.data)
		if status != c.status || !strings.Contains(string(body), string(c.code)) {
			t.Errorf("%s: %d %s", c.name, status, body)
		}
	}
	if status, _ := do(t, srv, http.MethodGet, "/photos", "", nil, ""); status != http.StatusUnauthorized {
		t.Errorf("no user = %d", status)
	}
	if status, _ := do(t, srv, http.MethodPost, "/photos", "alice", strings.NewReader("x"), "text/plain"); status != http.StatusBadRequest {
		t.Errorf("not a form = %d", status)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "files", rolePhotos, "*")); len(left) != 0 {
		t.Errorf("refused uploads left %v", left)
	}
}

// TestMemoryDSN runs the app with its bytes in memory: the same code, a
// different DSN.
func TestMemoryDSN(t *testing.T) {
	srv := httptest.NewServer(nil)
	defer srv.Close()
	a, err := newApp(t.Context(), config{BaseURL: srv.URL, DataDir: t.TempDir(), StorageDSN: "mem://", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	srv.Config.Handler = a.Handler()
	if status, body := uploadAs(t, srv, "alice", "a.png", pngBytes(t)); status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, body)
	}
}

// TestBadWiring checks that a short secret and a bad DSN fail at start-up.
func TestBadWiring(t *testing.T) {
	if _, err := newApp(t.Context(), config{BaseURL: "http://x", DataDir: t.TempDir(), Secret: []byte("short")}); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := newApp(t.Context(), config{BaseURL: "http://x", DataDir: t.TempDir(), StorageDSN: "nope://x", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}); err == nil {
		t.Error("unknown scheme accepted")
	}
}
