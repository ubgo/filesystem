package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const jpegData = "\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01-a-resumable-upload-in-two-chunks"

type env struct {
	t    *testing.T
	app  *App
	base string
}

func setup(t *testing.T) *env {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	app, err := New(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return &env{t: t, app: app, base: srv.URL}
}

// do sends one tus request as user and returns the response.
func (e *env) do(method, url, user string, body string, h map[string]string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Tus-Resumable", "1.0.0")
	if user != "" {
		req.Header.Set(headerUser, user)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func meta(name, typ string) string {
	enc := base64.StdEncoding.EncodeToString
	return "filename " + enc([]byte(name)) + ",filetype " + enc([]byte(typ))
}

// create starts an upload of size bytes and returns its URL.
func (e *env) create(user string, size int, name, typ string) (string, int) {
	resp := e.do(http.MethodPost, e.base+tusPath, user, "", map[string]string{
		"Upload-Length": strconv.Itoa(size), "Upload-Metadata": meta(name, typ),
	})
	return resp.Header.Get("Location"), resp.StatusCode
}

// patch sends a chunk at offset.
func (e *env) patch(url, user string, offset int, chunk string) *http.Response {
	return e.do(http.MethodPatch, url, user, chunk, map[string]string{
		"Upload-Offset": strconv.Itoa(offset), "Content-Type": "application/offset+octet-stream",
	})
}

func TestResumeAndRecord(t *testing.T) {
	e := setup(t)
	url, code := e.create("u1", len(jpegData), "holiday.jpg", "image/jpeg")
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	half := len(jpegData) / 2
	e.patch(url, "u1", 0, jpegData[:half])
	// The connection "drops"; the client asks how far it got.
	head := e.do(http.MethodHead, url, "u1", "", nil)
	if head.Header.Get("Upload-Offset") != strconv.Itoa(half) {
		t.Fatalf("offset after first chunk = %q", head.Header.Get("Upload-Offset"))
	}
	final := e.patch(url, "u1", half, jpegData[half:])
	if final.StatusCode != http.StatusNoContent || final.Header.Get("Filesystem-Key") == "" {
		t.Fatalf("final = %d %v", final.StatusCode, final.Header)
	}
	resp, err := http.Get(e.base + "/files?") // no user
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("anonymous list = %v %v", resp, err)
	}
	req, _ := http.NewRequest(http.MethodGet, e.base+"/files", nil)
	req.Header.Set(headerUser, "u1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var files []fileResponse
	_ = json.NewDecoder(resp.Body).Decode(&files)
	_ = resp.Body.Close()
	if len(files) != 1 || files[0].Filename != "holiday.jpg" || files[0].Key != final.Header.Get("Filesystem-Key") {
		t.Errorf("files = %+v", files)
	}
}

func TestRefusals(t *testing.T) {
	e := setup(t)
	if _, code := e.create("", 10, "a.jpg", "image/jpeg"); code != http.StatusForbidden {
		t.Errorf("anonymous create = %d", code)
	}
	if _, code := e.create("u1", 10, "a.html", "text/html"); code != http.StatusUnsupportedMediaType {
		t.Errorf("declared HTML = %d", code)
	}
	if _, code := e.create("u1", maxUploadBytes+1, "a.mp4", "video/mp4"); code != http.StatusRequestEntityTooLarge {
		t.Errorf("declared oversize = %d", code)
	}
	html := "<html><script>alert(1)</script></html>"
	url, _ := e.create("u1", len(html), "cat.jpg", "image/jpeg")
	if resp := e.patch(url, "u1", 0, html); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("HTML bytes declared as JPEG = %d", resp.StatusCode)
	}
	if left, _ := os.ReadDir(e.app.TempDir); len(left) != 0 {
		t.Errorf("refused session left %d temp files", len(left))
	}
}

func TestSweepSessions(t *testing.T) {
	e := setup(t)
	url, _ := e.create("u1", len(jpegData), "a.jpg", "image/jpeg")
	e.patch(url, "u1", 0, jpegData[:5])
	if n, err := sweepSessions(t.Context(), e.app.TempDir, time.Now().Add(-sessionMaxAge)); n != 0 || err != nil {
		t.Fatalf("fresh sweep = %d %v", n, err)
	}
	// A slow upload: the .info file is old, but bytes arrived recently. It
	// must survive a sweep whose cutoff falls between the two.
	entries, err := os.ReadDir(e.app.TempDir)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * sessionMaxAge)
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".info") {
			if err := os.Chtimes(filepath.Join(e.app.TempDir, en.Name()), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := sweepSessions(t.Context(), e.app.TempDir, time.Now().Add(-sessionMaxAge)); n != 0 || err != nil {
		t.Fatalf("sweep of a live slow upload = %d %v, want it kept", n, err)
	}
	if n, err := sweepSessions(t.Context(), e.app.TempDir, time.Now().Add(time.Minute)); n != 1 || err != nil {
		t.Fatalf("late sweep = %d %v, want the one session", n, err)
	}
	if left, _ := os.ReadDir(e.app.TempDir); len(left) != 0 {
		t.Errorf("late sweep left %d files", len(left))
	}
	if n, err := sweepSessions(t.Context(), filepath.Join(e.app.TempDir, "missing"), time.Now()); n != 0 || err != nil {
		t.Errorf("sweep of a missing directory = %d %v, want nothing to do", n, err)
	}
}

// TestSharedInstances resumes on instance b an upload created on
// instance a, which only a shared session store and locker allow.
func TestSharedInstances(t *testing.T) {
	var out strings.Builder
	if err := sharedInstances(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`instance a: create 201, first 22 bytes 204`,
		`instance b: HEAD sees offset 22, last 22 bytes 204`,
		`recorded for u1: image/jpeg, 44 bytes`,
		`shared store without a locker: invalid_argument`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
