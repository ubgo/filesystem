package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// post uploads files (name → content) in one request.
func post(t *testing.T, url string, files ...[2]string) int {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		w, _ := mw.CreateFormFile("file", f[0])
		_, _ = io.WriteString(w, f[1])
	}
	_ = mw.Close()
	resp, err := http.Post(url+"/uploads", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func startServer(t *testing.T, s scan.Scanner) (string, *filesystem.Disk) {
	t.Helper()
	h, disk, err := New(t.TempDir(), s)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL, disk
}

// stored counts the objects on the disk.
func stored(t *testing.T, disk *filesystem.Disk) int {
	t.Helper()
	n := 0
	for _, err := range disk.List(t.Context(), "uploads", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

func TestScanning(t *testing.T) {
	url, disk := startServer(t, scan.ScannerFunc(eicarOnly))
	if got := post(t, url, [2]string{"notes.txt", "meeting notes"}); got != http.StatusCreated {
		t.Fatalf("clean upload = %d", got)
	}
	if got := post(t, url, [2]string{"eicar.txt", eicarSignature}); got != http.StatusUnsupportedMediaType {
		t.Errorf("infected upload = %d", got)
	}
	// One infected file sinks the request: the clean one is deleted too.
	if got := post(t, url, [2]string{"a.txt", "fine"}, [2]string{"b.txt", eicarSignature}); got != http.StatusUnsupportedMediaType {
		t.Errorf("mixed upload = %d", got)
	}
	if got := post(t, url, [2]string{"broken.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDRbroken"}); got != http.StatusUnsupportedMediaType {
		t.Errorf("broken image = %d", got)
	}
	if n := stored(t, disk); n != 1 {
		t.Errorf("%d objects stored, want only the clean upload", n)
	}
}

func TestFailsClosed(t *testing.T) {
	down := scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) {
		return scan.Verdict{}, errors.New("connection refused")
	})
	url, disk := startServer(t, down)
	if got := post(t, url, [2]string{"notes.txt", "meeting notes"}); got != http.StatusServiceUnavailable {
		t.Errorf("upload with the scanner down = %d", got)
	}
	if n := stored(t, disk); n != 0 {
		t.Errorf("%d objects left by a refused upload", n)
	}
}

// TestConfirmScans shows that a direct upload (presigned PUT) is scanned
// when the server confirms it: Confirm runs the same Inspect.
func TestConfirmScans(t *testing.T) {
	_, disk := startServer(t, scan.ScannerFunc(eicarOnly))
	ctx := t.Context()
	if _, err := disk.WriteBytes(ctx, "uploads/direct.txt", []byte(eicarSignature), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := upload.Confirm(ctx, disk, "uploads/direct.txt", "direct.txt", newPolicy(scan.ScannerFunc(eicarOnly)))
	if filesystem.CodeOf(err) != filesystem.CodeRejected {
		t.Errorf("Confirm = %v", err)
	}
	if ok, _ := disk.Exists(ctx, "uploads/direct.txt"); ok {
		t.Error("Confirm left the infected object")
	}
}

func TestReadiness(t *testing.T) {
	rec := httptest.NewRecorder()
	ready(scan.Clamd{Network: "tcp", Address: "127.0.0.1:1"})(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness with clamd down = %d", rec.Code)
	}
	if _, ok := newScanner(func(string) (string, bool) { return "127.0.0.1:3310", true }).(scan.Clamd); !ok {
		t.Error("CLAMD_ADDR did not select clamd")
	}
}
