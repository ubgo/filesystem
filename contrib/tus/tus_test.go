package tus_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	fstus "github.com/ubgo/filesystem/contrib/tus"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

const pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-a-resumable-avatar"

var images = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}

type env struct {
	srv  *httptest.Server
	disk *filesystem.Disk
	tmp  string
	// always are headers sent with every request (credentials).
	always map[string]string
}

func setup(t *testing.T, cfg fstus.Config) *env {
	t.Helper()
	disk, _ := filesystem.NewDisk("media", memory.New())
	cfg.Disk, cfg.BasePath = disk, "/uploads/"
	if cfg.TempDir == "" {
		cfg.TempDir = t.TempDir()
	}
	if cfg.Policy.MaxSize == 0 {
		cfg.Policy = images
	}
	h, err := fstus.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", h))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{srv: srv, disk: disk, tmp: cfg.TempDir}
}

func meta(kv map[string]string) string {
	var parts []string
	for k, v := range kv {
		parts = append(parts, k+" "+base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return strings.Join(parts, ",")
}

func (e *env) do(t *testing.T, method, url string, body io.Reader, h map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, url, body)
	req.Header.Set("Tus-Resumable", "1.0.0")
	for k, v := range e.always {
		req.Header.Set(k, v)
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// create starts an upload and returns its URL (or the failed response).
func (e *env) create(t *testing.T, size int, m map[string]string, extra map[string]string) (string, *http.Response) {
	t.Helper()
	h := map[string]string{"Upload-Length": strconv.Itoa(size), "Upload-Metadata": meta(m)}
	for k, v := range extra {
		h[k] = v
	}
	resp := e.do(t, http.MethodPost, e.srv.URL+"/uploads/", nil, h)
	return resp.Header.Get("Location"), resp
}

// send uploads data in two chunks and returns the final response.
func (e *env) send(t *testing.T, loc, data string) *http.Response {
	t.Helper()
	half := len(data) / 2
	h := map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}
	if r := e.do(t, http.MethodPatch, loc, strings.NewReader(data[:half]), h); r.StatusCode != http.StatusNoContent {
		t.Fatalf("first chunk = %d", r.StatusCode)
	}
	h["Upload-Offset"] = strconv.Itoa(half)
	return e.do(t, http.MethodPatch, loc, strings.NewReader(data[half:]), h)
}

func sessions(t *testing.T, dir string) int {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	return len(entries)
}

func TestResumableUpload(t *testing.T) {
	var stored []upload.Result
	e := setup(t, fstus.Config{OnStored: func(_ context.Context, r upload.Result, m map[string]string) error {
		if m[fstus.MetaFilename] != "me.png" {
			t.Errorf("metadata = %v", m)
		}
		stored = append(stored, r)
		return nil
	}})
	loc, resp := e.create(t, len(pngData), map[string]string{fstus.MetaFilename: "me.png", fstus.MetaFiletype: "image/png"}, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	final := e.send(t, loc, pngData)
	key := final.Header.Get(fstus.HeaderKey)
	if final.StatusCode != http.StatusNoContent || key == "" || len(stored) != 1 || stored[0].Key != key {
		t.Fatalf("final = %d key %q stored %v", final.StatusCode, key, stored)
	}
	if b, _ := e.disk.ReadAll(t.Context(), key); string(b) != pngData {
		t.Error("stored bytes differ")
	}
	if n := sessions(t, e.tmp); n != 0 {
		t.Errorf("%d session files left", n)
	}
}

func TestRefusals(t *testing.T) {
	e := setup(t, fstus.Config{Authorize: func(_ context.Context, h http.Header) error {
		if h.Get("Authorization") != "Bearer ok" {
			return errors.New("no")
		}
		return nil
	}})
	auth := map[string]string{"Authorization": "Bearer ok"}
	if _, r := e.create(t, 10, nil, nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("unauthorised = %d", r.StatusCode)
	}
	if _, r := e.create(t, 2<<20, nil, auth); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("declared too large = %d", r.StatusCode)
	}
	if _, r := e.create(t, 10, map[string]string{fstus.MetaFiletype: "text/html"}, auth); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("declared HTML = %d", r.StatusCode)
	}
	deferred := e.do(t, http.MethodPost, e.srv.URL+"/uploads/", nil, map[string]string{"Upload-Defer-Length": "1", "Authorization": "Bearer ok"})
	if deferred.StatusCode != http.StatusBadRequest {
		t.Errorf("deferred length = %d", deferred.StatusCode)
	}
	// Declared an image, sent HTML: refused at the end, nothing kept.
	e.always = auth
	html := "<!DOCTYPE html><script>alert(1)</script>"
	loc, _ := e.create(t, len(html), map[string]string{fstus.MetaFiletype: "image/png"}, nil)
	if r := e.send(t, loc, html); r.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("disguised HTML = %d", r.StatusCode)
	}
	if n := sessions(t, e.tmp); n != 0 {
		t.Errorf("%d session files left after a refusal", n)
	}
}

// TestAuthorizeEveryRequest proves Authorize guards the requests that
// continue, resume and cancel an upload, not only its creation, and that
// a refusal still carries the CORS headers a browser needs to read it.
func TestAuthorizeEveryRequest(t *testing.T) {
	var calls []string
	e := setup(t, fstus.Config{Authorize: func(ctx context.Context, h http.Header) error {
		if ctx == nil {
			t.Error("Authorize got no context")
		}
		calls = append(calls, h.Get("Authorization"))
		if h.Get("Authorization") != "Bearer ok" {
			return errors.New("no")
		}
		return nil
	}})
	auth := map[string]string{"Authorization": "Bearer ok"}
	loc, r := e.create(t, len(pngData), nil, auth)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("authorised create = %d", r.StatusCode)
	}
	patch := map[string]string{"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}
	for _, c := range []struct {
		method string
		body   io.Reader
		h      map[string]string
	}{
		{http.MethodPatch, strings.NewReader(pngData), patch},
		{http.MethodHead, nil, nil},
		{http.MethodDelete, nil, nil},
		{http.MethodPatch, strings.NewReader(pngData), map[string]string{"Authorization": "Bearer stolen-url", "Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}},
	} {
		if r := e.do(t, c.method, loc, c.body, c.h); r.StatusCode != http.StatusForbidden {
			t.Errorf("unauthorised %s = %d", c.method, r.StatusCode)
		}
	}
	// A browser's refused request still gets CORS headers and the tus
	// version, so the client sees a 403 rather than a network error.
	r = e.do(t, http.MethodHead, loc, nil, map[string]string{"Origin": "https://app.test"})
	if r.StatusCode != http.StatusForbidden || r.Header.Get("Access-Control-Allow-Origin") != "https://app.test" || r.Header.Get("Tus-Resumable") == "" {
		t.Errorf("refused browser HEAD = %d, headers %v", r.StatusCode, r.Header)
	}
	// Preflights carry no credentials and are not checked.
	before := len(calls)
	if r := e.do(t, http.MethodOptions, loc, nil, map[string]string{"Origin": "https://app.test", "Access-Control-Request-Method": "PATCH"}); r.StatusCode != http.StatusOK || len(calls) != before {
		t.Errorf("preflight = %d, Authorize calls %d → %d", r.StatusCode, before, len(calls))
	}
	// Authorised, the upload resumes and finishes.
	if r := e.do(t, http.MethodHead, loc, nil, auth); r.StatusCode != http.StatusOK || r.Header.Get("Upload-Offset") != "0" {
		t.Fatalf("authorised HEAD = %d offset %q", r.StatusCode, r.Header.Get("Upload-Offset"))
	}
	final := e.do(t, http.MethodPatch, loc, strings.NewReader(pngData), map[string]string{"Authorization": "Bearer ok", "Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"})
	if final.StatusCode != http.StatusNoContent || final.Header.Get(fstus.HeaderKey) == "" {
		t.Fatalf("authorised PATCH = %d", final.StatusCode)
	}
}

// TestOnStoredKeepsCode proves an OnStored error reaches the client with
// the status of its code, as upload.Handler answers it.
func TestOnStoredKeepsCode(t *testing.T) {
	e := setup(t, fstus.Config{OnStored: func(context.Context, upload.Result, map[string]string) error {
		return filesystem.Errorf(filesystem.CodeQuotaExceeded, "storage quota reached")
	}})
	loc, _ := e.create(t, len(pngData), nil, nil)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPatch, loc, strings.NewReader(pngData))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", "0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage || !strings.Contains(string(body), "ERR_QUOTA_EXCEEDED") || !strings.Contains(string(body), "storage quota reached") {
		t.Fatalf("quota refusal = %d %q", resp.StatusCode, body)
	}
}

// TestLogger proves tusd's request logs go to Config.Logger.
func TestLogger(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil))
	e := setup(t, fstus.Config{Logger: logger})
	loc, _ := e.create(t, len(pngData), nil, nil)
	e.send(t, loc, pngData)
	mu.Lock()
	defer mu.Unlock()
	if out := buf.String(); !strings.Contains(out, "ResponseOutgoing") || !strings.Contains(out, "method=PATCH") {
		t.Fatalf("log = %q", out)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestForwardedHeaders proves upload URLs follow the proxy's headers only
// when RespectForwardedHeaders is set.
func TestForwardedHeaders(t *testing.T) {
	fwd := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "files.example.com"}
	for _, respect := range []bool{true, false} {
		e := setup(t, fstus.Config{RespectForwardedHeaders: respect})
		loc, r := e.create(t, len(pngData), nil, fwd)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create = %d", r.StatusCode)
		}
		if got := strings.HasPrefix(loc, "https://files.example.com/uploads/"); got != respect {
			t.Errorf("respect %v: Location %q", respect, loc)
		}
	}
}

func TestOnStoredFailure(t *testing.T) {
	e := setup(t, fstus.Config{OnStored: func(context.Context, upload.Result, map[string]string) error {
		return errors.New("database down: secret detail")
	}})
	loc, _ := e.create(t, len(pngData), nil, nil)
	r := e.send(t, loc, pngData)
	if r.StatusCode != http.StatusInternalServerError {
		t.Fatalf("OnStored failure = %d", r.StatusCode)
	}
	n := 0
	for range e.disk.List(t.Context(), "", filesystem.ListOptions{Recursive: true}) {
		n++
	}
	if n != 0 {
		t.Errorf("%d objects kept after OnStored failed", n)
	}
}

func TestConfigValidation(t *testing.T) {
	disk, _ := filesystem.NewDisk("media", memory.New())
	for _, cfg := range []fstus.Config{
		{TempDir: t.TempDir(), BasePath: "/u/"},
		{Disk: disk, BasePath: "/u/"},
		{Disk: disk, TempDir: t.TempDir()},
	} {
		if _, err := fstus.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("New(%+v) = %v", cfg, err)
		}
	}
	file := t.TempDir() + "/f"
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := fstus.New(fstus.Config{Disk: disk, TempDir: file + "/sub", BasePath: "/u/", Policy: images}); err == nil {
		t.Error("temp dir under a file accepted")
	}
	if _, err := fstus.New(fstus.Config{Disk: disk, TempDir: t.TempDir(), BasePath: "/u/", Policy: upload.Policy{MaxSize: -1}}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("invalid policy = %v", err)
	}
}
