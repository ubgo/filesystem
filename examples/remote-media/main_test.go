package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestRun checks the output the remote media guide shows.
func TestRun(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`preview   "Release notes" / "What shipped this week."`,
		"image     image/png 33 bytes, stored under the URL's hash: true",
		`card      "Release notes", image through the proxy: true`,
		"signed    200 image/png 33 bytes, nosniff, public, max-age=",
		"forged    403",
		"svg-png   415",
		"sweep     kept 1 entries, 33 bytes; deleted 0",
		`oembed    video "Launch clip" by "Example Studio" from "Tube", 640x360, thumbnail https://img.example/1.jpg`,
		"oembed    javascript: provider URL dropped: true, html is an iframe for a sandbox: true",
		`off-list  discovery link followed: false, oEmbed false, preview stands: "A blog post"`,
		"endpoint  asked 1 time(s) in all",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestOffListEndpointAnswers shows the off-list check above is not
// vacuous: the endpoint the post links to answers a direct request with
// the same oEmbed JSON, so only the allowlist keeps the cache away.
func TestOffListEndpointAnswers(t *testing.T) {
	prov := newProvider()
	defer prov.Close()
	resp, err := http.Get(prov.URL + "/evil/oembed?url=x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"type":"video"`) || prov.evilHits.Load() != 1 {
		t.Fatalf("off-list endpoint: %d %s, %d hits", resp.StatusCode, body, prov.evilHits.Load())
	}
}
