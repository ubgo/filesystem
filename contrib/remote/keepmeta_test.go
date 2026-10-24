package remote_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ubgo/filesystem/contrib/remote"
)

// keepMetaPage has, in its head, tags a preview does not read itself:
// an author, a price, links, two JSON-LD blocks, a script that is not
// JSON-LD, and meta tags spoofing the names KeepMeta files from other
// tags; and in its body, a tag that must not be read.
const keepMetaPage = `<html><head><title>Real title</title>
<meta name="&lt;title&gt;" content="spoofed title">
<meta name="link:canonical" content="https://evil.example/">
<meta name="jsonld:0" content="spoofed">
<meta property="og:title" content="OG title">
<meta name="Author" content="Aman Bansal">
<meta property="product:price:amount" content="9.99">
<meta name="generator" content="secret-cms 1.0">
<link rel="icon shortcut" href="/favicon.ico">
<link rel="canonical" href="https://shop.example/p/1">
<script>var notJSONLD = 1;</script>
<script type="text/javascript">var notJSONLD2 = 1;</script>
<script type="application/ld+json">{"@type":"Product","name":"Lamp"}</script>
<script type="Application/LD+JSON">{"@type":"Offer"}</script>
</head><body><meta name="author" content="body author"></body></html>`

// TestKeepMeta pins R-I20: names KeepMeta accepts reach Extract (other
// meta tags lower-cased, link rels as link:<rel>, JSON-LD blocks as
// jsonld:<n>), names it refuses do not, a page cannot spoof the names
// filed from other tags, the built-in fields are unchanged, and without
// KeepMeta the map holds what it always did.
func TestKeepMeta(t *testing.T) {
	ctx := context.Background()
	srv := serve(t, "127.0.0.1:0", body("text/html; charset=utf-8", []byte(keepMetaPage)))
	var got map[string]string
	extract := func(_ context.Context, _ string, meta map[string]string) (map[string]string, error) {
		got = meta
		return nil, nil
	}
	accept := map[string]bool{"author": true, "product:price:amount": true, "link:icon": true, "link:canonical": true, "jsonld:0": true, "jsonld:1": true, "<title>": true}
	c, _ := newCache(t, func(o *remote.Options) {
		o.Extract = extract
		o.KeepMeta = func(name string) bool { return accept[name] }
	})
	p, err := c.LinkPreview(ctx, srv.URL+"/p")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"og:title": "OG title", "author": "Aman Bansal", "product:price:amount": "9.99",
		"link:icon": "/favicon.ico", "link:canonical": "https://shop.example/p/1",
		"jsonld:0": `{"@type":"Product","name":"Lamp"}`, "jsonld:1": `{"@type":"Offer"}`,
		"<title>": "Real title",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("meta[%q] = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"generator", "link:shortcut"} {
		if _, ok := got[k]; ok {
			t.Errorf("a refused name reached Extract: %q", k)
		}
	}
	if p.Title != "OG title" {
		t.Errorf("title %q", p.Title)
	}

	plain, _ := newCache(t, func(o *remote.Options) { o.Extract = extract })
	if _, err := plain.LinkPreview(ctx, srv.URL+"/p"); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(got))
	if !slices.Equal(keys, []string{"<title>", "og:title"}) {
		t.Errorf("without KeepMeta Extract got %v", keys)
	}
	if got["<title>"] != "Real title" {
		t.Errorf("without KeepMeta the title is %q", got["<title>"])
	}

	all, _ := newCache(t, func(o *remote.Options) {
		o.Extract = extract
		o.KeepMeta = func(string) bool { return true }
	})
	if _, err := all.LinkPreview(ctx, srv.URL+"/p"); err != nil {
		t.Fatal(err)
	}
	if got["<title>"] != "Real title" || got["link:canonical"] != "https://shop.example/p/1" || got["jsonld:0"] != `{"@type":"Product","name":"Lamp"}` || got["author"] != "Aman Bansal" {
		t.Errorf("a page spoofed a filed name: %v", got)
	}
	for k := range got {
		if strings.Contains(got[k], "body author") || strings.Contains(got[k], "notJSONLD") {
			t.Errorf("%q came from outside the head or a plain script: %q", k, got[k])
		}
	}
}
