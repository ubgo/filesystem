package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngHead is enough of a PNG for the upload's sniffing.
var pngHead = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

// TestLabels runs the example against a stand-in for Cloud Vision.
func TestLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "test-key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, `{"responses":[{"labelAnnotations":[{"description":"Dog","score":0.97},{"description":"Grass","score":0.85},{"description":"Font","score":0.5}]}]}`)
	}))
	defer srv.Close()
	img := filepath.Join(t.TempDir(), "dog.png")
	if err := os.WriteFile(img, pngHead, 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := run(t.Context(), &out, "test-key", srv.URL, []string{img}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "dog.png  [auto:dog auto:grass]\n" {
		t.Fatalf("output = %q", got)
	}
}
