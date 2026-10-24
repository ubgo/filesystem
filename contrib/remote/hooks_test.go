package remote_test

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/ubgo/filesystem/contrib/remote"
)

// dropTracking keys a URL without its utm_ parameters.
func dropTracking(canon string) string {
	u, err := url.Parse(canon)
	if err != nil {
		return canon
	}
	q := u.Query()
	for k := range q {
		if len(k) > 4 && k[:4] == "utm_" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// TestCacheKey pins the key seam: URLs that differ in tracking
// parameters share one entry and one fetch, and without the hook they
// do not.
func TestCacheKey(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("image/png", pngBytes))
	c, _ := newCache(t, func(o *remote.Options) { o.CacheKey = dropTracking })
	a, err := c.Get(ctx, srv.URL+"/a.png?utm_source=x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Get(ctx, srv.URL+"/a.png?utm_source=y")
	if err != nil || a.Key != b.Key || srv.hits.Load() != 1 {
		t.Fatalf("keys %q %q, %d fetches, %v", a.Key, b.Key, srv.hits.Load(), err)
	}
	plain, _ := newCache(t)
	ka, _ := plain.MediaKey(srv.URL + "/a.png?utm_source=x")
	kb, _ := plain.MediaKey(srv.URL + "/a.png?utm_source=y")
	if ka == kb {
		t.Fatal("the default key dropped a query parameter")
	}
}

const extractPage = `<html><head><title>T</title><meta property="og:video" content="https://v.example/1.mp4"><meta name="twitter:player" content="https://p.example"></head><body></body></html>`

// TestExtract pins the extractor seam: its fields reach Preview.Extra
// from the parsed meta tags, the built-in fields stay, and its error
// fails the preview.
func TestExtract(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("text/html; charset=utf-8", []byte(extractPage)))
	var fail error
	c, _ := newCache(t, func(o *remote.Options) {
		o.Extract = func(_ context.Context, pageURL string, meta map[string]string) (map[string]string, error) {
			if fail != nil {
				return nil, fail
			}
			return map[string]string{"video": meta["og:video"], "player": meta["twitter:player"], "page": pageURL}, nil
		}
	})
	p, err := c.LinkPreview(ctx, srv.URL+"/p")
	if err != nil || p.Title != "T" || p.Extra["video"] != "https://v.example/1.mp4" || p.Extra["player"] != "https://p.example" || p.Extra["page"] == "" {
		t.Fatalf("preview %+v, %v", p, err)
	}
	fail = errors.New("extractor down")
	if _, err := c.LinkPreview(ctx, srv.URL+"/q"); !errors.Is(err, fail) {
		t.Fatalf("a failing extractor: %v", err)
	}
	def, _ := newCache(t)
	if p, err := def.LinkPreview(ctx, srv.URL+"/p"); err != nil || p.Extra != nil {
		t.Fatalf("default preview %+v, %v", p, err)
	}
}
