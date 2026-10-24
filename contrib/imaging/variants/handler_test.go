package variants_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
)

func TestHandler(t *testing.T) {
	e := newEnv(t, nil, transforms)
	f := e.image(t, "a.png", pngBytes(t, 60, 40), filesystem.VisibilityPrivate)
	_, _ = e.disk.WriteBytes(t.Context(), "doc.txt", []byte("text"), filesystem.WriteOptions{})
	scopeOf := func(r *http.Request) (string, error) {
		if r.Header.Get("X-Tenant") == "" {
			return "", errors.New("signed out")
		}
		return r.Header.Get("X-Tenant"), nil
	}
	srv := httptest.NewServer(http.StripPrefix("/v/", variants.Handler(e.store, e.plugin, scopeOf, variants.HandlerOptions{})))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(method, path, tenant string) *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), method, srv.URL+path, nil)
		if tenant != "" {
			req.Header.Set("X-Tenant", tenant)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	resp := get(http.MethodGet, "/v/"+f.ID+"/thumb", "t")
	if resp.StatusCode != http.StatusFound || !strings.Contains(resp.Header.Get("Location"), "/variants/"+f.ID+"/thumb-") {
		t.Errorf("redirect = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	cases := []struct {
		method, path, tenant string
		want                 int
	}{
		{http.MethodGet, "/v/" + f.ID + "/thumb", "", 403},
		{http.MethodGet, "/v/" + f.ID + "/thumb", "other-tenant", 404},
		{http.MethodGet, "/v/" + f.ID + "/huge", "t", 404},
		{http.MethodGet, "/v/" + f.ID, "t", 404},
		{http.MethodGet, "/v/" + f.ID + "/thumb/extra", "t", 404},
		{http.MethodPost, "/v/" + f.ID + "/thumb", "t", 405},
	}
	for _, c := range cases {
		if r := get(c.method, c.path, c.tenant); r.StatusCode != c.want {
			t.Errorf("%s %s (%q) = %d, want %d", c.method, c.path, c.tenant, r.StatusCode, c.want)
		}
	}
	custom := httptest.NewServer(variants.Handler(e.store, e.plugin, scopeOf, variants.HandlerOptions{LinkLifetime: 30 * time.Second}))
	defer custom.Close()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, custom.URL+"/"+f.ID+"/square", nil)
	req.Header.Set("X-Tenant", "t")
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusFound {
		t.Errorf("custom lifetime = %d", r.StatusCode)
	}
}
