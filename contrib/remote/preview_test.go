package remote_test

import (
	"context"
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
)

// page returns an HTML page whose head carries extra.
func page(extra string) []byte {
	return []byte(`<!DOCTYPE html><html><head><title> Plain title </title>` + extra + `</head><body><meta property="og:title" content="from the body"></body></html>`)
}

func TestLinkPreview(t *testing.T) {
	ctx := context.Background()
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/posts/1/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/posts/1/", body("text/html; charset=utf-8", page(`
		<meta property="og:title" content="A &amp; B">
		<meta property="og:description" content="Desc">
		<meta name="description" content="plain desc">
		<meta property="og:site_name" content="Site">
		<meta property="og:image" content="cover.png">`)))
	mux.HandleFunc("/posts/1/cover.png", body("image/png", pngBytes))
	srv := serve(t, "127.0.0.1:0", mux.ServeHTTP)
	clk := newClock()
	c, disk := newCache(t, func(o *remote.Options) { o.Now = clk.Now; o.PreviewTTL = time.Hour })

	p, err := c.LinkPreview(ctx, srv.URL+"/start")
	if err != nil {
		t.Fatal(err)
	}
	// The relative image resolves against the page's final URL, not the
	// one asked for.
	if p.Title != "A & B" || p.Description != "Desc" || p.SiteName != "Site" || p.ImageURL != srv.URL+"/posts/1/cover.png" || p.ImageType != "image/png" || p.ImageError != "" {
		t.Fatalf("preview %+v", p)
	}
	if ok, _ := disk.Exists(ctx, p.ImageKey); !ok {
		t.Fatal("image not cached")
	}
	hits := srv.hits.Load() // redirect, page, image

	// Fresh: served from the disk.
	clk.add(59 * time.Minute)
	if again, err := c.LinkPreview(ctx, srv.URL+"/start"); err != nil || again.Title != p.Title || srv.hits.Load() != hits {
		t.Fatalf("fresh preview refetched: %v, hits %d -> %d", err, hits, srv.hits.Load())
	}
	// Stale: the page is fetched again, the image is still cached.
	clk.add(2 * time.Minute)
	if _, err := c.LinkPreview(ctx, srv.URL+"/start"); err != nil || srv.hits.Load() != hits+2 {
		t.Fatalf("stale preview: %v, hits %d -> %d; want redirect and page only", err, hits, srv.hits.Load())
	}
}

func TestLinkPreviewFallbacks(t *testing.T) {
	srv := serve(t, "127.0.0.1:0", body("text/html", page(`<meta name="Description" content="plain desc"><meta name="twitter:image" content="/i.png">`)))
	c, _ := newCache(t)
	p, err := c.LinkPreview(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Plain title" || p.Description != "plain desc" || p.ImageURL != srv.URL+"/i.png" || p.ImageError != filesystem.CodeRejected {
		t.Fatalf("preview %+v", p)
	}
}

func TestLinkPreviewImageGoesThroughTheGuard(t *testing.T) {
	internal := serve(t, "[::1]:0", body("image/png", pngBytes))
	srv := serve(t, "127.0.0.1:0", body("text/html", page(`<meta property="og:image" content="`+internal.URL+`/secret.png">`)))
	c, _ := newCache(t)
	p, err := c.LinkPreview(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if p.ImageError != filesystem.CodeInvalidArgument || p.ImageKey != "" || internal.hits.Load() != 0 {
		t.Fatalf("og:image on an internal address: %+v, internal hits %d", p, internal.hits.Load())
	}
}

func TestLinkPreviewRefusesWhatIsNotHTML(t *testing.T) {
	srv := serve(t, "127.0.0.1:0", body("text/html", pngBytes))
	c, _ := newCache(t)
	_, err := c.LinkPreview(context.Background(), srv.URL)
	wantCode(t, err, filesystem.CodeRejected)
}

func TestLinkPreviewReadsAtMostMaxPageBytes(t *testing.T) {
	padding := "<!--" + strings.Repeat("x", 4096) + "-->"
	srv := serve(t, "127.0.0.1:0", body("text/html", page(padding+`<meta property="og:title" content="late">`)))
	for limit, want := range map[int64]string{1024: "", 1 << 20: "late"} {
		c, _ := newCache(t, func(o *remote.Options) { o.MaxPageBytes = limit })
		p, err := c.LinkPreview(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Title; (want == "" && got != "Plain title") || (want != "" && got != want) {
			t.Fatalf("MaxPageBytes %d: title %q", limit, got)
		}
	}
}

func TestLinkPreviewReadsAtMostMaxTags(t *testing.T) {
	srv := serve(t, "127.0.0.1:0", body("text/html", page(strings.Repeat("<link>", 50)+`<meta property="og:title" content="late">`)))
	for limit, want := range map[int]string{20: "Plain title", 100: "late"} {
		c, _ := newCache(t, func(o *remote.Options) { o.MaxTags = limit })
		p, err := c.LinkPreview(context.Background(), srv.URL)
		if err != nil || p.Title != want {
			t.Fatalf("MaxTags %d: title %q, %v; want %q", limit, p.Title, err, want)
		}
	}
}

func TestLinkPreviewPageGoesThroughTheGuard(t *testing.T) {
	ctx := context.Background()
	internal := serve(t, "[::1]:0", body("text/html", page("")))
	redirect := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	})
	c, _ := newCache(t)
	for _, u := range []string{internal.URL, redirect.URL} {
		_, err := c.LinkPreview(ctx, u)
		wantCode(t, err, filesystem.CodeInvalidArgument)
	}
	if internal.hits.Load() != 0 {
		t.Fatal("a page on an internal address was fetched")
	}
}

func TestPurgeRemovesThePreview(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("text/html", page("")))
	c, _ := newCache(t)
	for range 2 {
		if _, err := c.LinkPreview(ctx, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Purge(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LinkPreview(ctx, srv.URL); err != nil || srv.hits.Load() != 2 {
		t.Fatalf("after Purge: %v, %d page fetches; want 2", err, srv.hits.Load())
	}
}

// A page may name any URL as its image; only http(s) is ever echoed.
func TestLinkPreviewEchoesOnlyHTTPImageURLs(t *testing.T) {
	for _, ref := range []string{"javascript:alert(document.domain)", "JaVaScRiPt:alert(1)", "data:image/svg+xml,<svg onload=alert(1)>", "file:///etc/passwd", "https://user:pw@example.com/a.png"} {
		srv := serve(t, "127.0.0.1:0", body("text/html", page(`<meta property="og:image" content="`+html.EscapeString(ref)+`">`)))
		c, _ := newCache(t)
		p, err := c.LinkPreview(context.Background(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if p.ImageURL != "" || p.ImageError != filesystem.CodeInvalidArgument {
			t.Errorf("og:image %q: ImageURL %q, ImageError %q", ref, p.ImageURL, p.ImageError)
		}
	}
}

// A page that drips its body after the headers cannot hold the fetch past
// the fetch timeout.
func TestLinkPreviewBoundsASlowPage(t *testing.T) {
	stop := make(chan struct{})
	srv := serve(t, "127.0.0.1:0", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><head>"))
		w.(http.Flusher).Flush()
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				_, _ = w.Write([]byte(" "))
				w.(http.Flusher).Flush()
			}
		}
	})
	t.Cleanup(func() { close(stop) }) // after serve's, so it runs before Close
	c, _ := newCache(t, func(o *remote.Options) { o.Fetch.Timeout = 200 * time.Millisecond })
	start := time.Now()
	_, err := c.LinkPreview(context.Background(), srv.URL)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("a dripping page held the fetch for %s (err %v)", took, err)
	}
}
