package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// pngData is the start of a PNG: enough for type detection.
const pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-site-image"

// noRedirect is a client that shows redirects instead of following them.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func post[T any](t *testing.T, url, contentType string, body io.Reader) (int, T) {
	t.Helper()
	var out T
	resp, err := http.Post(url, contentType, body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

func uploadFile(t *testing.T, base string) assetResponse {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "hero.png")
	_, _ = io.WriteString(fw, pngData)
	_ = mw.Close()
	code, out := post[assetResponse](t, base+"/assets", mw.FormDataContentType(), &body)
	if code != http.StatusOK {
		t.Fatalf("upload = %d", code)
	}
	return out
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// purges records the URLs the app asked the CDN to purge.
type purges struct {
	mu   sync.Mutex
	urls []string
}

func (p *purges) purge(_ context.Context, urls []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.urls = append(p.urls, urls...)
	return nil
}

func TestPublishCycleOffline(t *testing.T) {
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	disks, err := memoryDisks(srv.URL, bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	var p purges
	app, err := New(t.Context(), t.TempDir(), disks, p.purge)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler

	up := uploadFile(t, srv.URL)
	if up.Disk != diskVault || !strings.Contains(up.URL, "sig=") {
		t.Fatalf("upload = %+v, want a signed vault URL", up)
	}
	_, pub := post[assetResponse](t, srv.URL+"/assets/"+up.FileID+"/publish", "", nil)
	if pub.Disk != diskCDN || !strings.HasPrefix(pub.URL, srv.URL+cdnPrefix+"/") {
		t.Fatalf("publish = %+v", pub)
	}
	if resp, body := get(t, pub.URL); resp.StatusCode != 200 || body != pngData {
		t.Errorf("public read = %d", resp.StatusCode)
	}
	_, priv := post[assetResponse](t, srv.URL+"/assets/"+up.FileID+"/unpublish", "", nil)
	if priv.Disk != diskVault {
		t.Fatalf("unpublish = %+v", priv)
	}
	if resp, _ := get(t, pub.URL); resp.StatusCode != http.StatusNotFound {
		t.Errorf("old public URL = %d after unpublish", resp.StatusCode)
	}
	if len(p.urls) != 1 || p.urls[0] != pub.URL {
		t.Errorf("purged %v, want %s", p.urls, pub.URL)
	}
	if resp, body := get(t, priv.URL); resp.StatusCode != 200 || body != pngData {
		t.Errorf("signed read = %d", resp.StatusCode)
	}
	resp, err := noRedirect.Get(srv.URL + "/assets/" + up.FileID)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect = %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestSiteAssetsOffline(t *testing.T) {
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	disks, _ := memoryDisks(srv.URL, bytes.Repeat([]byte("k"), 32))
	app, err := New(t.Context(), t.TempDir(), disks, logPurge)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	css := "body{margin:0}"
	_, a1 := post[assetResponse](t, srv.URL+"/site-assets?ext=.css", "text/css", strings.NewReader(css))
	_, a2 := post[assetResponse](t, srv.URL+"/site-assets?ext=.css", "text/css", strings.NewReader(css))
	_, a3 := post[assetResponse](t, srv.URL+"/site-assets?ext=.css", "text/css", strings.NewReader(css+"p{}"))
	if a1.URL == "" || a1.URL != a2.URL || a1.URL == a3.URL {
		t.Errorf("content addressing: %s %s %s", a1.URL, a2.URL, a3.URL)
	}
	resp, body := get(t, a1.URL)
	if body != css || resp.Header.Get("Cache-Control") != immutable || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Errorf("asset = %q %v", body, resp.Header)
	}
	if code, _ := post[assetResponse](t, srv.URL+"/site-assets?ext=.html", "text/html", strings.NewReader("<p>")); code != http.StatusBadRequest {
		t.Errorf(".html = %d", code)
	}
}

// headerSpy records the Cache-Control of every PUT the S3 driver sends,
// because the fake does not store it.
type headerSpy struct {
	next http.Handler
	mu   sync.Mutex
	seen map[string]string
}

func (h *headerSpy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		h.mu.Lock()
		h.seen[r.URL.Path] = r.Header.Get("Cache-Control")
		h.mu.Unlock()
	}
	h.next.ServeHTTP(w, r)
}

// TestBucketsThroughDSN runs the real-bucket mode against an in-process S3:
// DSNs through the Mux, a publish copying between buckets, and the
// Cache-Control the driver sends with a build asset.
func TestBucketsThroughDSN(t *testing.T) {
	spy := &headerSpy{next: gofakes3.New(s3mem.New()).Server(), seen: map[string]string{}}
	fake := httptest.NewServer(spy)
	t.Cleanup(fake.Close)
	for _, b := range []string{"site-public", "site-private"} {
		req, _ := http.NewRequest(http.MethodPut, fake.URL+"/"+b, nil)
		if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
			t.Fatalf("create bucket %s: %v", b, err)
		}
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	dsn := func(b string) string { return "s3://" + b + "?region=us-east-1&path_style=true&endpoint=" + fake.URL }
	disks, err := bucketDisks(t.Context(), dsn("site-public"), dsn("site-private"), "https://cdn.example.com")
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(t.Context(), t.TempDir(), disks, logPurge)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.Handler)
	t.Cleanup(srv.Close)

	up := uploadFile(t, srv.URL)
	if !strings.Contains(up.URL, "X-Amz-Signature=") {
		t.Errorf("vault URL %s is not SigV4-signed", up.URL)
	}
	_, pub := post[assetResponse](t, srv.URL+"/assets/"+up.FileID+"/publish", "", nil)
	if !strings.HasPrefix(pub.URL, "https://cdn.example.com/uploads/") {
		t.Errorf("published URL = %s", pub.URL)
	}
	if ok, _ := disks.Vault.Exists(t.Context(), strings.TrimPrefix(pub.URL, "https://cdn.example.com/")); ok {
		t.Error("vault still holds the published object")
	}
	_, asset := post[assetResponse](t, srv.URL+"/site-assets?ext=.css", "text/css", strings.NewReader("a{}"))
	key := "/site-public/" + strings.TrimPrefix(asset.URL, "https://cdn.example.com/")
	if spy.seen[key] != immutable {
		t.Errorf("Cache-Control sent with %s = %q", key, spy.seen[key])
	}
	if !disks.CDN.Capabilities().Headers {
		t.Errorf("S3 disk without the Headers capability")
	}
}
