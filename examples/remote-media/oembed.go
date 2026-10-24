package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
	"github.com/ubgo/filesystem/upload"
)

// The fake provider's pages and answer. The answer carries a field the
// spec does not define and a provider URL that would run script; neither
// may reach the preview.
const (
	watchPage = `<!DOCTYPE html><html><head><meta property="og:title" content="Launch clip"></head><body>…</body></html>`
	// postPage links to an endpoint on the same host that nobody put on
	// the allowlist; a page must not be able to send the cache there.
	postPage = `<!DOCTYPE html><html><head><meta property="og:title" content="A blog post">
<link rel="alternate" type="application/json+oembed" href="/evil/oembed?url=x"></head><body>…</body></html>`
	oembedAnswer = `{"type":"video","version":"1.0","title":"Launch clip","author_name":"Example Studio",
"provider_name":"Tube","provider_url":"javascript:alert(1)","html":"<iframe src=\"https://tube.example/embed/1\"></iframe>",
"width":640,"height":360,"thumbnail_url":"https://img.example/1.jpg","secret_token":"s3cret"}`
)

// provider is the fake oEmbed provider: a watch page, a blog post, the
// allowlisted endpoint /oembed and an endpoint off the list, counting the
// requests each endpoint got.
type provider struct {
	*httptest.Server
	endpointHits, evilHits atomic.Int64
}

func newProvider() *provider {
	p := &provider{}
	mux := http.NewServeMux()
	html := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, body)
		}
	}
	mux.HandleFunc("/watch", html(watchPage))
	mux.HandleFunc("/post", html(postPage))
	mux.HandleFunc("/oembed", func(w http.ResponseWriter, _ *http.Request) {
		p.endpointHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, oembedAnswer)
	})
	mux.HandleFunc("/evil/oembed", func(w http.ResponseWriter, _ *http.Request) {
		p.evilHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, oembedAnswer)
	})
	p.Server = httptest.NewServer(mux)
	return p
}

// newOEmbedCache is a cache whose link previews ask one oEmbed provider:
// pages matching its scheme go to its endpoint, and a page's discovery
// link is followed only when it names that endpoint. In production the
// provider is real (https://www.youtube.com/oembed) and allow is nil.
// ds:def id=newoembedcache-s8w3k6pn
func newOEmbedCache(disk *filesystem.Disk, allow func(netip.Addr) bool, providerBase string) (*remote.Cache, error) {
	return remote.New(disk, remote.Options{
		Dir:        "oembed",
		Policy:     upload.Policy{MaxSize: 5 << 20, Allowed: []string{"image/*"}},
		Fetch:      upload.FetchOptions{Timeout: 10 * time.Second, AllowAddr: allow},
		PreviewTTL: 24 * time.Hour,
		OEmbed: []remote.OEmbedProvider{
			{Schemes: []string{providerBase + "/watch*"}, Endpoint: providerBase + "/oembed"},
		},
	})
}

// oembedPreviews previews a watch page the provider describes, then a
// post whose discovery link points off the allowlist, and prints what the
// cache asked and kept.
// ds:def id=oembedpreviews-f2q7v9hc
func oembedPreviews(ctx context.Context, out io.Writer, disk *filesystem.Disk, allow func(netip.Addr) bool) error {
	prov := newProvider()
	defer prov.Close()
	cache, err := newOEmbedCache(disk, allow, prov.URL)
	if err != nil {
		return err
	}

	p, err := cache.LinkPreview(ctx, prov.URL+"/watch?v=1")
	if err != nil {
		return err
	}
	o := p.OEmbed
	if o == nil {
		return fmt.Errorf("no oEmbed for the watch page: %s", p.OEmbedError)
	}
	fmt.Fprintf(out, "oembed    %s %q by %q from %q, %dx%d, thumbnail %s\n", o.Type, o.Title, o.AuthorName, o.ProviderName, o.Width, o.Height, o.ThumbnailURL)
	fmt.Fprintf(out, "oembed    javascript: provider URL dropped: %t, html is an iframe for a sandbox: %t\n", o.ProviderURL == "", strings.HasPrefix(o.HTML, "<iframe "))

	p, err = cache.LinkPreview(ctx, prov.URL+"/post")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "off-list  discovery link followed: %t, oEmbed %t, preview stands: %q\n", prov.evilHits.Load() > 0, p.OEmbed != nil, p.Title)
	fmt.Fprintf(out, "endpoint  asked %d time(s) in all\n", prov.endpointHits.Load())
	return nil
}
