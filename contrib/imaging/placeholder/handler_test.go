package placeholder_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging/placeholder"
)

func serve(t *testing.T, h http.Handler, method, target string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res, body
}

func TestHandlerServesCachedPNG(t *testing.T) {
	h := placeholder.Handler(placeholder.HandlerOptions{})
	res, body := serve(t, h, http.MethodGet, "/usr_1.png?name=Ada+Lovelace&size=64", nil)
	want := render(t, placeholder.Request{Seed: "usr_1", Name: "Ada Lovelace", Size: 64})
	tag, _ := placeholder.ETag(placeholder.Request{Seed: "usr_1", Name: "Ada Lovelace", Size: 64})
	if res.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
		t.Fatalf("GET = %d, %d bytes", res.StatusCode, len(body))
	}
	for k, v := range map[string]string{
		"Content-Type": "image/png", "ETag": tag, "Cache-Control": placeholder.CacheControlImmutable, "X-Content-Type-Options": "nosniff",
	} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Without the suffix, the same image.
	if _, b := serve(t, h, http.MethodGet, "/usr_1?name=Ada+Lovelace&size=64", nil); !bytes.Equal(b, want) {
		t.Error("/usr_1 and /usr_1.png differ")
	}
	for _, inm := range []string{tag, "W/" + tag, `"other", ` + tag, "*"} {
		if res, b := serve(t, h, http.MethodGet, "/usr_1.png?name=Ada+Lovelace&size=64", map[string]string{"If-None-Match": inm}); res.StatusCode != http.StatusNotModified || len(b) != 0 || res.Header.Get("ETag") != tag {
			t.Errorf("If-None-Match %s = %d", inm, res.StatusCode)
		}
	}
	if res, _ := serve(t, h, http.MethodGet, "/usr_1.png?name=Ada+Lovelace&size=64", map[string]string{"If-None-Match": `"stale"`}); res.StatusCode != http.StatusOK {
		t.Errorf("a stale If-None-Match = %d", res.StatusCode)
	}
	if res, b := serve(t, h, http.MethodHead, "/usr_1.png?size=64", nil); res.StatusCode != http.StatusOK || len(b) != 0 || res.Header.Get("Content-Length") == "" {
		t.Errorf("HEAD = %d, %d bytes", res.StatusCode, len(b))
	}
}

func TestHandlerRefusals(t *testing.T) {
	h := placeholder.Handler(placeholder.HandlerOptions{})
	for _, target := range []string{"/", "/a/b", "/.png", "/s?size=big", "/s?size=4096", "/s?style=robot"} {
		if res, _ := serve(t, h, http.MethodGet, target, nil); res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d", target, res.StatusCode)
		}
	}
	if res, _ := serve(t, h, http.MethodPost, "/s", nil); res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST = %d", res.StatusCode)
	}
}

// TestHandlerNameLookup pins that a Name lookup replaces the query's name
// (a client cannot write on someone else's fallback), shortens the cache,
// and maps its errors.
func TestHandlerNameLookup(t *testing.T) {
	h := placeholder.Handler(placeholder.HandlerOptions{
		Style: placeholder.StyleInitials,
		Name: func(_ context.Context, seed string) (string, error) {
			if seed == "ghost" {
				return "", filesystem.Errorf(filesystem.CodeNotFound, "no such user")
			}
			return "Grace Hopper", nil
		},
	})
	res, body := serve(t, h, http.MethodGet, "/usr_2?name=Evil+Text&size=64", nil)
	if want := render(t, placeholder.Request{Seed: "usr_2", Name: "Grace Hopper", Size: 64}); !bytes.Equal(body, want) {
		t.Error("the query name was used despite the lookup")
	}
	if res.Header.Get("Cache-Control") != placeholder.CacheControlLookup {
		t.Errorf("Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	if res, _ := serve(t, h, http.MethodGet, "/ghost", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("lookup error = %d", res.StatusCode)
	}
	icon := placeholder.Handler(placeholder.HandlerOptions{Style: placeholder.StyleIdenticon, CacheControl: "no-cache"})
	res, body = serve(t, icon, http.MethodGet, "/usr_2?name=Grace", nil)
	if !bytes.Equal(body, render(t, placeholder.Request{Seed: "usr_2", Style: placeholder.StyleIdenticon})) || res.Header.Get("Cache-Control") != "no-cache" {
		t.Error("the default style or the Cache-Control override was ignored")
	}
}
