package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/signedurl"
)

var (
	oldSecret = bytes.Repeat([]byte("o"), 32)
	newSecret = bytes.Repeat([]byte("n"), 32)
)

func setup(t *testing.T) (*App, string) {
	t.Helper()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	app, err := New(t.Context(), t.TempDir(), srv.URL, signingKeys(func(string) (string, bool) { return "", false }))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app.Handler
	return app, srv.URL
}

// get sends a request as customer (none when empty) without following
// redirects.
func get(t *testing.T, rawURL, customer string, header ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	if customer != "" {
		req.Header.Set(headerCustomer, customer)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPrivateDownloads(t *testing.T) {
	app, base := setup(t)
	f, err := app.AddDocument(t.Context(), "c1", []byte(samplePDF), "Rechnung März.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if resp := get(t, base+"/invoices/"+f.ID, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("signed out = %d", resp.StatusCode)
	}
	if resp := get(t, base+"/invoices/"+f.ID, "c2"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("other customer = %d", resp.StatusCode)
	}
	resp := get(t, base+"/invoices/"+f.ID, "c1")
	link := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("view = %d %v", resp.StatusCode, resp.Header)
	}
	file := get(t, link, "")
	body, _ := io.ReadAll(file.Body)
	if file.StatusCode != 200 || string(body) != samplePDF || file.Header.Get("Content-Type") != "application/pdf" {
		t.Errorf("signed GET = %d %q", file.StatusCode, body)
	}
	if r := get(t, link, "", "If-None-Match", file.Header.Get("ETag")); r.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d", r.StatusCode)
	}
	if r := get(t, link, "", "Range", "bytes=0-7"); r.StatusCode != http.StatusPartialContent {
		t.Errorf("range = %d", r.StatusCode)
	}
	tampered := strings.Replace(link, "exp=", "exp=9", 1)
	if r := get(t, tampered, ""); r.StatusCode != http.StatusForbidden {
		t.Errorf("tampered link = %d", r.StatusCode)
	}
	dl := get(t, get(t, base+"/invoices/"+f.ID+"/download", "c1").Header.Get("Location"), "")
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''Rechnung%20M%C3%A4rz.pdf") {
		t.Errorf("download disposition = %q", cd)
	}
}

func TestActiveContentDownloads(t *testing.T) {
	app, base := setup(t)
	f, err := app.ImportLegacy(t.Context(), "c1", "legacy/x.html", []byte(legacyHTML), "x.html")
	if err != nil {
		t.Fatal(err)
	}
	r := get(t, get(t, base+"/invoices/"+f.ID, "c1").Header.Get("Location"), "")
	if !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") || r.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("HTML served inline: %v", r.Header)
	}
}

// verify checks a signed URL with a key ring at a given time.
func verify(t *testing.T, link string, keys []signedurl.Key, at time.Time) error {
	t.Helper()
	v, err := signedurl.New("http://x/files", keys, signedurl.WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	_, err = v.Verify("documents", "a.pdf", filesystem.MethodGet, u.Query())
	return err
}

func TestKeyRotationAndExpiry(t *testing.T) {
	now := time.Now()
	old, _ := signedurl.New("http://x/files", []signedurl.Key{{ID: keyIDPrevious, Secret: oldSecret}})
	req, err := old.SignURL(t.Context(), "documents", "a.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: linkLifetime})
	if err != nil {
		t.Fatal(err)
	}
	rotated := signingKeys(func(name string) (string, bool) {
		return map[string]string{envKeyCurrent: string(newSecret), envKeyPrevious: string(oldSecret)}[name], true
	})
	if err := verify(t, req.URL, rotated, now); err != nil {
		t.Errorf("link from before the rotation refused: %v", err)
	}
	if err := verify(t, req.URL, rotated[:1], now); err == nil {
		t.Error("link verified after the previous key was removed")
	}
	if err := verify(t, req.URL, rotated, now.Add(linkLifetime+time.Minute)); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("expired link = %v", err)
	}
}
