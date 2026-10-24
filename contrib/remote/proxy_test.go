package remote_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/signedurl"
)

const proxyBase = "https://app.example.com/media/proxy"

func newProxy(t *testing.T, c *remote.Cache, clk *clock) *remote.Proxy {
	t.Helper()
	return newProxyWithMaxAge(t, c, clk, time.Hour)
}

// newProxyWithMaxAge is newProxy with ProxyOptions.MaxAge set to maxAge.
func newProxyWithMaxAge(t *testing.T, c *remote.Cache, clk *clock, maxAge time.Duration) *remote.Proxy {
	t.Helper()
	s, err := signedurl.New(proxyBase, []signedurl.Key{{ID: "k1", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}}, signedurl.WithClock(clk.Now))
	if err != nil {
		t.Fatal(err)
	}
	p, err := remote.NewProxy(c, remote.ProxyOptions{Base: proxyBase, Signer: s, MaxAge: maxAge})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// get serves rawURL (a proxy URL) through p.
func get(p http.Handler, method, rawURL string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, rawURL, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestProxyServesSignedURLs(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("text/plain", pngBytes))
	clk := newClock()
	c, _ := newCache(t, func(o *remote.Options) { o.Now = clk.Now })
	p := newProxy(t, c, clk)
	u, err := p.URL(ctx, srv.URL+"/a.png", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(u, proxyBase+"?") {
		t.Fatalf("proxy URL %q", u)
	}
	w := get(p, http.MethodGet, u)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), pngBytes) {
		t.Fatalf("GET: %d %q", w.Code, w.Body.String())
	}
	h := w.Header()
	// The type comes from the bytes, not from the remote's text/plain;
	// browsers may keep it no longer than the URL lives.
	for name, want := range map[string]string{
		"Content-Type":            "image/png",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; sandbox",
		"Cache-Control":           "public, max-age=600",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if w := get(p, http.MethodGet, u, "If-None-Match", h.Get("ETag")); w.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", w.Code)
	}
	if w := get(p, http.MethodHead, u); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d with %d bytes", w.Code, w.Body.Len())
	}
	if w := get(p, http.MethodPost, u); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("remote hit %d times, want 1", n)
	}
}

func TestProxyIsNotAnOpenProxy(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	clk := newClock()
	c, _ := newCache(t, func(o *remote.Options) { o.Now = clk.Now })
	p := newProxy(t, c, clk)
	signed, err := p.URL(ctx, srv.URL+"/a.png", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tampered := func(param, value string) string {
		u, _ := url.Parse(signed)
		q := u.Query()
		if value == "" {
			q.Del(param)
		} else {
			q.Set(param, value)
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	for name, u := range map[string]string{
		"unsigned":       proxyBase + "?" + url.Values{remote.ParamURL: {srv.URL + "/a.png"}}.Encode(),
		"other URL":      tampered(remote.ParamURL, srv.URL+"/b.png"),
		"no signature":   tampered(signedurl.ParamSignature, ""),
		"forged":         tampered(signedurl.ParamSignature, strings.Repeat("0", 64)),
		"longer expiry":  tampered(signedurl.ParamExpires, "9999999999"),
		"unknown key ID": tampered(signedurl.ParamKeyID, "k2"),
	} {
		if w := get(p, http.MethodGet, u); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
	}
	clk.add(time.Minute)
	if w := get(p, http.MethodGet, signed); w.Code != http.StatusForbidden {
		t.Errorf("expired: %d, want 403", w.Code)
	}
	if n := srv.hits.Load(); n != 0 {
		t.Fatalf("refused requests reached the remote %d times", n)
	}
}

func TestProxyRechecksTheBytesItServes(t *testing.T) {
	ctx := context.Background()
	clk := newClock()
	c, disk := newCache(t, func(o *remote.Options) { o.Now = clk.Now })
	p := newProxy(t, c, clk)
	raw := "https://images.example.net/a.png"
	// A cache entry that did not come through the policy (written by
	// something else on the disk) is not served as an image.
	key, _ := c.MediaKey(raw)
	if _, err := disk.WriteBytes(ctx, key, svgBytes, filesystem.WriteOptions{ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	u, _ := p.URL(ctx, raw, time.Minute)
	w := get(p, http.MethodGet, u)
	if w.Code != http.StatusUnsupportedMediaType || strings.Contains(w.Body.String(), "<svg") {
		t.Fatalf("poisoned entry: %d %q", w.Code, w.Body.String())
	}
}

func TestProxyAnswersFetchErrorsWithoutDetail(t *testing.T) {
	ctx := context.Background()
	internal := serve(t, "[::1]:0", body("image/png", pngBytes))
	clk := newClock()
	c, _ := newCache(t, func(o *remote.Options) { o.Now = clk.Now })
	p := newProxy(t, c, clk)
	u, _ := p.URL(ctx, internal.URL+"/a.png", time.Minute)
	w := get(p, http.MethodGet, u)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "::1") || internal.hits.Load() != 0 {
		t.Fatalf("internal target: %d %q, hits %d", w.Code, w.Body.String(), internal.hits.Load())
	}
}

func TestNewProxyRefusesBadOptions(t *testing.T) {
	c, _ := newCache(t)
	s, _ := signedurl.New(proxyBase, []signedurl.Key{{ID: "k", Secret: bytes.Repeat([]byte("s"), signedurl.MinKeyBytes)}})
	for name, o := range map[string]remote.ProxyOptions{
		"no signer":     {Base: proxyBase},
		"relative base": {Base: "/proxy", Signer: s},
		"query in base": {Base: proxyBase + "?a=1", Signer: s},
	} {
		if _, err := remote.NewProxy(c, o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}
