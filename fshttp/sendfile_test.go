package fshttp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/fshttp"
	"github.com/ubgo/filesystem/memory"
)

// accel maps keys under an nginx internal location and records every key
// it was asked for, so a test can see exactly what reached the mapping.
type accel struct{ asked []string }

func (a *accel) sendfile() *fshttp.Sendfile {
	return &fshttp.Sendfile{Path: func(disk, key string) (string, bool) {
		a.asked = append(a.asked, disk+":"+key)
		if strings.HasPrefix(key, "remote/") {
			return "", false
		}
		if strings.HasPrefix(key, "bad/") {
			return "/protected/x\r\nSet-Cookie: a=b", true
		}
		return "/protected/" + disk + "/" + filesystem.EscapeKey(key), true
	}}
}

func TestPublicSendfile(t *testing.T) {
	ctx := t.Context()
	d, err := filesystem.NewDisk("assets", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	for key, ct := range map[string]string{"icons/a b.png": "image/png", "page.html": "text/html", "remote/r.png": "image/png", "bad/x.png": "image/png"} {
		if _, err := d.Write(ctx, key, strings.NewReader("body of "+key), filesystem.WriteOptions{ContentType: ct}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Write(ctx, "secret.png", strings.NewReader("x"), filesystem.WriteOptions{Visibility: filesystem.VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	a := &accel{}
	h := fshttp.Public(d, fshttp.PublicOptions{Sendfile: a.sendfile()})
	get := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, method, "http://app.test"+path, nil))
		return rec
	}

	// The request path is cleaned before the mapping sees it.
	rec := get(http.MethodGet, "/./icons//a%20b.png")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get(fshttp.HeaderAccelRedirect) != "/protected/assets/icons/a%20b.png" {
		t.Fatalf("offload: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get(fshttp.HeaderContentTypeOptions) != "nosniff" || rec.Header().Get(fshttp.HeaderCSP) == "" || rec.Header().Get("ETag") == "" {
		t.Fatalf("offload lost headers: %v", rec.Header())
	}
	if rec = get(http.MethodHead, "/icons/a%20b.png"); rec.Header().Get(fshttp.HeaderAccelRedirect) == "" {
		t.Fatalf("HEAD not offloaded: %v", rec.Header())
	}

	// Active content keeps the forced attachment.
	rec = get(http.MethodGet, "/page.html")
	if rec.Header().Get(fshttp.HeaderAccelRedirect) == "" || rec.Header().Get("Content-Disposition") != "attachment" {
		t.Fatalf("HTML offload: %v", rec.Header())
	}

	// Unmapped and unsafe values are streamed by Go instead.
	if rec = get(http.MethodGet, "/remote/r.png"); rec.Body.String() != "body of remote/r.png" || rec.Header().Get(fshttp.HeaderAccelRedirect) != "" {
		t.Fatalf("unmapped key: %q %v", rec.Body.String(), rec.Header())
	}
	if rec = get(http.MethodGet, "/bad/x.png"); rec.Body.String() != "body of bad/x.png" || rec.Header().Get(fshttp.HeaderAccelRedirect) != "" || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("control characters: %q %v", rec.Body.String(), rec.Header())
	}

	// Refused requests never reach the mapping.
	before := len(a.asked)
	for path, code := range map[string]int{"/secret.png": 404, "/missing.png": 404, "/a/../icons/a%20b.png": 400} {
		if rec = get(http.MethodGet, path); rec.Code != code || rec.Header().Get(fshttp.HeaderAccelRedirect) != "" {
			t.Errorf("%s: %d %v, want %d without offload", path, rec.Code, rec.Header(), code)
		}
	}
	if len(a.asked) != before {
		t.Fatalf("refused requests reached the mapping: %v", a.asked[before:])
	}
	if want := "assets:icons/a b.png"; a.asked[0] != want {
		t.Fatalf("mapping got %q, want %q", a.asked[0], want)
	}
}

func TestSignedSendfile(t *testing.T) {
	a := &accel{}
	sf := a.sendfile()
	sf.Header = fshttp.HeaderSendfile
	e := setup(t, fshttp.SignedOptions{Sendfile: sf})
	if _, err := e.disk.Write(t.Context(), "invoices/42.pdf", strings.NewReader("%PDF-1.7"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	link := e.sign(t, e.disk, "invoices/42.pdf", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	resp, body := do(t, http.MethodGet, link, nil, nil)
	if resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get(fshttp.HeaderSendfile) != "/protected/uploads/invoices/42.pdf" {
		t.Fatalf("signed offload: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp.Header.Get("Cache-Control") != fshttp.DefaultSignedCacheControl || resp.Header.Get(fshttp.HeaderContentTypeOptions) != "nosniff" {
		t.Fatalf("signed offload lost headers: %v", resp.Header)
	}
	asked := len(a.asked)
	resp, _ = do(t, http.MethodGet, strings.Replace(link, "42.pdf", "43.pdf", 1), nil, nil)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(fshttp.HeaderSendfile) != "" || len(a.asked) != asked {
		t.Fatalf("tampered link: %d %v, mapping asked %v", resp.StatusCode, resp.Header, a.asked)
	}
}

// TestSendfileScopedDisk maps a tenant's scoped disk: the proxy must be
// sent the tenant's file, never the root's file with the same short key.
func TestSendfileScopedDisk(t *testing.T) {
	ctx := t.Context()
	root, err := filesystem.NewDisk("root", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a.png", "tenants/t1/a.png"} {
		if _, err := root.Write(ctx, key, strings.NewReader(key), filesystem.WriteOptions{ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
	}
	t1, err := root.Scope("t1", "tenants/t1")
	if err != nil {
		t.Fatal(err)
	}
	a := &accel{}
	rec := httptest.NewRecorder()
	fshttp.Public(t1, fshttp.PublicOptions{Sendfile: a.sendfile()}).ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.test/a.png", nil))
	if got, want := rec.Header().Get(fshttp.HeaderAccelRedirect), "/protected/t1/tenants/t1/a.png"; got != want {
		t.Fatalf("%s = %q, want %q (mapping got %v)", fshttp.HeaderAccelRedirect, got, want, a.asked)
	}
}

// TestSendfileConditional answers validators from the object's own ETag
// and modification time before offloading, as streaming does.
func TestSendfileConditional(t *testing.T) {
	ctx := t.Context()
	d, err := filesystem.NewDisk("assets", memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write(ctx, "a.png", strings.NewReader("png"), filesystem.WriteOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	h := fshttp.Public(d, fshttp.PublicOptions{Sendfile: (&accel{}).sendfile()})
	get := func(header, value string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "http://app.test/a.png", nil)
		if header != "" {
			req.Header.Set(header, value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := get("", "")
	etag, lastMod := first.Header().Get("ETag"), first.Header().Get("Last-Modified")
	if etag == "" || lastMod == "" || first.Code != http.StatusOK {
		t.Fatalf("first response: %d %v", first.Code, first.Header())
	}
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	past := time.Unix(0, 0).UTC().Format(http.TimeFormat)
	for _, c := range []struct {
		header, value string
		code          int
	}{
		{"If-None-Match", etag, http.StatusNotModified},
		{"If-None-Match", "W/" + etag, http.StatusNotModified},
		{"If-None-Match", `"other", ` + etag, http.StatusNotModified},
		{"If-None-Match", "*", http.StatusNotModified},
		{"If-None-Match", `"other"`, http.StatusOK},
		{"If-Modified-Since", lastMod, http.StatusNotModified},
		{"If-Modified-Since", future, http.StatusNotModified},
		{"If-Modified-Since", past, http.StatusOK},
	} {
		rec := get(c.header, c.value)
		offloaded := rec.Header().Get(fshttp.HeaderAccelRedirect) != ""
		if rec.Code != c.code || offloaded != (c.code == http.StatusOK) || rec.Body.Len() != 0 {
			t.Errorf("%s: %s -> %d (offloaded %t), want %d", c.header, c.value, rec.Code, offloaded, c.code)
		}
	}
}
