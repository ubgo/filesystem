package remote_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/remote"
)

// oembedJSON is a provider's answer, with fields the spec does not define
// and a thumbnail URL that would run script.
const oembedJSON = `{"type":"video","version":"1.0","title":" Clip ","author_name":"Aman Bansal","author_url":"https://example.com/aman",
"provider_name":"Tube","provider_url":"javascript:alert(1)","html":"<iframe src=\"https://tube.example/embed/1\"></iframe>",
"width":"640","height":360,"thumbnail_url":"https://img.example/1.jpg","thumbnail_width":"auto","secret_token":"s3cret"}`

// oembedSite serves a watch page, a page with a discovery link to
// discovered, and an oEmbed endpoint at /oembed answering answer; it
// counts the endpoint's requests and records the last url parameter.
type oembedSite struct {
	*counted
	endpointHits atomic.Int64
	evilHits     atomic.Int64
	lastURL      atomic.Pointer[string]
}

func newOEmbedSite(t *testing.T, answer string, discovered string) *oembedSite {
	t.Helper()
	s := &oembedSite{}
	mux := http.NewServeMux()
	mux.HandleFunc("/watch", body("text/html", page(`<meta property="og:title" content="Watch">`)))
	mux.HandleFunc("/post", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(page(`<link rel="Alternate" type="application/json+oembed" href="` + discovered + `">`))
	})
	mux.HandleFunc("/oembed", func(w http.ResponseWriter, r *http.Request) {
		s.endpointHits.Add(1)
		u := r.URL.Query().Get("url")
		s.lastURL.Store(&u)
		w.Header().Set("Content-Type", "application/json")
		// Flushed first, so the answer is chunked and announces no
		// Content-Length: the read itself must enforce MaxOEmbedBytes.
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(answer))
	})
	mux.HandleFunc("/evil/oembed", func(w http.ResponseWriter, _ *http.Request) {
		s.evilHits.Add(1)
		_, _ = w.Write([]byte(oembedJSON))
	})
	s.counted = serve(t, "127.0.0.1:0", mux.ServeHTTP)
	return s
}

func TestOEmbed_OffByDefault(t *testing.T) {
	site := newOEmbedSite(t, oembedJSON, "/oembed?url=x")
	c, _ := newCache(t)
	p, err := c.LinkPreview(context.Background(), site.URL+"/post")
	if err != nil {
		t.Fatal(err)
	}
	if p.OEmbed != nil || p.OEmbedError != "" || site.endpointHits.Load() != 0 {
		t.Fatalf("oEmbed asked without Options.OEmbed: %+v, %d hits", p, site.endpointHits.Load())
	}
}

func TestOEmbed_SchemeMatchKeepsTypedFieldsOnly(t *testing.T) {
	site := newOEmbedSite(t, oembedJSON, "")
	c, _ := newCache(t, func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Schemes: []string{"https://other.example/*", site.URL + "/watch*"}, Endpoint: site.URL + "/oembed"}}
	})
	p, err := c.LinkPreview(context.Background(), site.URL+"/watch?v=1")
	if err != nil {
		t.Fatal(err)
	}
	o := p.OEmbed
	if o == nil {
		t.Fatalf("no oEmbed: %+v", p)
	}
	if got := *site.lastURL.Load(); got != site.URL+"/watch?v=1" {
		t.Errorf("url parameter = %q", got)
	}
	want := remote.OEmbed{
		Type: remote.OEmbedVideo, Title: "Clip", AuthorName: "Aman Bansal", AuthorURL: "https://example.com/aman",
		ProviderName: "Tube", HTML: `<iframe src="https://tube.example/embed/1"></iframe>`,
		Width: 640, Height: 360, ThumbnailURL: "https://img.example/1.jpg",
	}
	if *o != want {
		t.Errorf("oEmbed = %+v\nwant     %+v", *o, want)
	}
	if p.Title != "Watch" {
		t.Errorf("the page's own fields changed: %+v", p)
	}
}

func TestOEmbed_DiscoveryOnlyToAllowlistedEndpoints(t *testing.T) {
	ctx := context.Background()
	site := newOEmbedSite(t, oembedJSON, "/oembed?url=whatever&format=json")
	allow := func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Endpoint: site.URL + "/oembed?format=json"}}
	}
	c, _ := newCache(t, allow)
	p, err := c.LinkPreview(ctx, site.URL+"/post")
	if err != nil || p.OEmbed == nil || p.OEmbed.Type != remote.OEmbedVideo || site.endpointHits.Load() != 1 {
		t.Fatalf("discovered allowlisted endpoint: %+v, %v, %d hits", p, err, site.endpointHits.Load())
	}

	// A link to an endpoint nobody allowlisted, on the same host, is never
	// fetched.
	evil := newOEmbedSite(t, oembedJSON, "/evil/oembed?url=x")
	c, _ = newCache(t, func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Endpoint: evil.URL + "/oembed"}}
	})
	p, err = c.LinkPreview(ctx, evil.URL+"/post")
	if err != nil || p.OEmbed != nil || evil.evilHits.Load() != 0 {
		t.Fatalf("followed a link off the allowlist: %+v, %v, %d hits", p, err, evil.evilHits.Load())
	}
}

func TestOEmbed_EndpointGoesThroughTheSSRFGuard(t *testing.T) {
	internal := serve(t, "[::1]:0", body("application/json", []byte(oembedJSON)))
	site := newOEmbedSite(t, oembedJSON, "")
	c, _ := newCache(t, func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Schemes: []string{site.URL + "/watch*"}, Endpoint: internal.URL + "/oembed"}}
	})
	p, err := c.LinkPreview(context.Background(), site.URL+"/watch")
	if err != nil {
		t.Fatal(err)
	}
	if p.OEmbed != nil || p.OEmbedError == "" || internal.hits.Load() != 0 {
		t.Fatalf("an internal endpoint was reached: %+v, %d hits", p, internal.hits.Load())
	}
}

func TestOEmbed_Failures(t *testing.T) {
	for name, tc := range map[string]struct {
		answer string
		max    int64
		code   filesystem.Code
	}{
		"over MaxOEmbedBytes": {oembedJSON, 64, filesystem.CodeTooLarge},
		"not JSON":            {"<html>no</html>", 0, filesystem.CodeRejected},
		"unknown type":        {`{"type":"script","html":"<script>"}`, 0, filesystem.CodeRejected},
	} {
		site := newOEmbedSite(t, tc.answer, "")
		c, _ := newCache(t, func(o *remote.Options) {
			o.OEmbed = []remote.OEmbedProvider{{Schemes: []string{site.URL + "/watch*"}, Endpoint: site.URL + "/oembed"}}
			o.MaxOEmbedBytes = tc.max
		})
		p, err := c.LinkPreview(context.Background(), site.URL+"/watch")
		if err != nil {
			t.Fatalf("%s: the preview failed with the oEmbed: %v", name, err)
		}
		if p.OEmbed != nil || p.OEmbedError != tc.code || p.Title != "Watch" {
			t.Errorf("%s: %+v, want error %s", name, p, tc.code)
		}
	}

	// HTML over the cap is dropped, never cut.
	long := `{"type":"rich","html":"` + strings.Repeat("x", 20<<10) + `"}`
	site := newOEmbedSite(t, long, "")
	c, _ := newCache(t, func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Schemes: []string{site.URL + "/watch*"}, Endpoint: site.URL + "/oembed"}}
	})
	if p, err := c.LinkPreview(context.Background(), site.URL+"/watch"); err != nil || p.OEmbed == nil || p.OEmbed.HTML != "" {
		t.Errorf("long html: %+v, %v", p.OEmbed, err)
	}
}

func TestOEmbed_NewRefusesBadProviders(t *testing.T) {
	for name, p := range map[string]remote.OEmbedProvider{
		"no endpoint":         {},
		"script endpoint":     {Endpoint: "javascript:alert(1)"},
		"relative endpoint":   {Endpoint: "/oembed"},
		"credentials":         {Endpoint: "https://u:p@example.com/oembed"},
		"empty scheme":        {Schemes: []string{""}, Endpoint: "https://example.com/oembed"},
		"unparseable":         {Endpoint: "https://exa mple.com/%zz"},
		"ftp endpoint":        {Endpoint: "ftp://example.com/oembed"},
		"endpoint with space": {Endpoint: " "},
	} {
		_, err := remote.New(newDisk(t), remote.Options{Dir: "r", Policy: imagePolicy(), OEmbed: []remote.OEmbedProvider{p}})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := remote.New(newDisk(t), remote.Options{Dir: "r", Policy: imagePolicy(), MaxOEmbedBytes: -1}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("negative MaxOEmbedBytes: %v", err)
	}
}

// TestOEmbed_UnusableDiscoveryLinks: a page with no discovery link, one
// whose href does not parse, one naming a non-HTTP endpoint and one that
// is not a JSON oEmbed link all get a plain preview, and no endpoint is
// asked.
func TestOEmbed_UnusableDiscoveryLinks(t *testing.T) {
	var hits atomic.Int64
	mux := http.NewServeMux()
	links := map[string]string{
		"/none":   ``,
		"/escape": `<link rel="alternate" type="application/json+oembed" href="%zz">`,
		"/ftp":    `<link rel="alternate" type="application/json+oembed" href="ftp://files.example/oembed">`,
		"/xml":    `<link rel="alternate" type="text/xml+oembed" href="/oembed">`,
	}
	for path, link := range links {
		mux.HandleFunc(path, body("text/html", page(link)))
	}
	mux.HandleFunc("/oembed", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(oembedJSON))
	})
	site := serve(t, "127.0.0.1:0", mux.ServeHTTP)
	c, _ := newCache(t, func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Schemes: []string{"https://other.example/*"}, Endpoint: site.URL + "/oembed"}}
	})
	for path := range links {
		p, err := c.LinkPreview(context.Background(), site.URL+path)
		if err != nil || p.OEmbed != nil || p.OEmbedError != "" || p.Title != "Plain title" {
			t.Errorf("%s: %+v, %v", path, p, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("an unusable link reached the endpoint %d times", hits.Load())
	}
}

// TestOEmbed_CutResponse: an endpoint that drops the connection mid-body
// leaves the preview standing with OEmbedError unavailable.
func TestOEmbed_CutResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/watch", body("text/html", page("")))
	mux.HandleFunc("/oembed", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"video",`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drops the connection mid-body
	})
	site := serve(t, "127.0.0.1:0", mux.ServeHTTP)
	c, _ := newCache(t, func(o *remote.Options) {
		o.OEmbed = []remote.OEmbedProvider{{Schemes: []string{site.URL + "/watch*"}, Endpoint: site.URL + "/oembed"}}
	})
	p, err := c.LinkPreview(context.Background(), site.URL+"/watch")
	if err != nil || p.OEmbed != nil || p.OEmbedError != filesystem.CodeUnavailable || p.Title != "Plain title" {
		t.Fatalf("cut response: %+v, %v", p, err)
	}
}
