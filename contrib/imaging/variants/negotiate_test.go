package variants_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/imaging"
	"github.com/ubgo/filesystem/contrib/imaging/variants"
	"github.com/ubgo/filesystem/memory"
)

// negotiated serves the handler with a WebP-capable processor (no AVIF)
// and returns a function that fetches thumb with an Accept header and
// reports the redirect target and the Vary header.
func negotiated(t *testing.T, proc imaging.Processor, opts variants.HandlerOptions) (*env, string, func(accept string) (string, string)) {
	t.Helper()
	p, err := variants.New(transforms, imaging.Options{}, variants.WithProcessor(proc))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWithPlugin(t, memory.New(), p, linked...)
	f := e.image(t, "a.png", pngBytes(t, 60, 60), filesystem.VisibilityPublic)
	srv := httptest.NewServer(variants.Handler(e.store, p, func(*http.Request) (string, error) { return "t", nil }, opts))
	t.Cleanup(srv.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e, f.ID, func(accept string) (string, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/"+f.ID+"/thumb", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("status %d", resp.StatusCode)
		}
		return resp.Header.Get("Location"), resp.Header.Get("Vary")
	}
}

// TestNegotiate pins UR-17: an explicitly accepted format the processor
// writes is served as a sibling, with Vary: Accept; everything else gets
// the declared format.
func TestNegotiate(t *testing.T) {
	calls := 0
	proc := imaging.NewPipeline(imaging.WithEncoder(webpStandIn{calls: &calls}))
	e, _, get := negotiated(t, proc, variants.HandlerOptions{Negotiate: []imaging.Format{imaging.FormatAVIF, imaging.FormatWebP}})
	for accept, ext := range map[string]string{
		"image/avif,image/webp,image/*,*/*;q=0.8": ".webp", // no AVIF encoder: WebP
		"image/webp":                  ".webp",
		"IMAGE/WEBP;q=0.5":            ".webp",
		"image/*,*/*":                 ".jpg", // wildcards do not count
		"image/webp;q=0":              ".jpg",
		"image/webp;q=abc, text/html": ".jpg",
		"garbage;;, image/webp":       ".webp",
		"":                            ".jpg",
	} {
		loc, vary := get(accept)
		if !strings.HasSuffix(loc, ext) || vary != "Accept" {
			t.Errorf("Accept %q: %s (Vary %q), want %s", accept, loc, vary, ext)
		}
	}
	if calls != 1 {
		t.Errorf("the WebP sibling was encoded %d times, want once (then found)", calls)
	}
	// Siblings survive Prune: they are the declared transform.
	if n, err := e.plugin.Prune(t.Context(), e.store); err != nil || n != 0 {
		t.Errorf("Prune removed %d siblings, %v", n, err)
	}
	if e.rows(t) != 2 {
		t.Errorf("rows = %d, want the declared thumb and its WebP sibling", e.rows(t))
	}
}

func TestNegotiateOff(t *testing.T) {
	_, _, get := negotiated(t, imaging.Default(), variants.HandlerOptions{})
	if loc, vary := get("image/webp"); !strings.HasSuffix(loc, ".jpg") || vary != "" {
		t.Errorf("without Negotiate: %s, Vary %q", loc, vary)
	}
	// A processor that cannot say what it encodes is never asked for a
	// negotiated format: it might not write it, and the declared one
	// always works.
	calls := 0
	bare := bareProcessor{imaging.NewPipeline(imaging.WithEncoder(webpStandIn{calls: &calls}))}
	_, _, get = negotiated(t, bare, variants.HandlerOptions{Negotiate: []imaging.Format{imaging.FormatWebP}})
	if loc, _ := get("image/webp"); !strings.HasSuffix(loc, ".jpg") || calls != 0 {
		t.Errorf("bare processor: %s, WebP encodes %d", loc, calls)
	}
}
