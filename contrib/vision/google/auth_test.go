package google_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision/google"
)

// bearer is an HTTP transport that authenticates itself, as an OAuth2
// transport does.
type bearer struct{ seen *http.Header }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer service-account-token")
	*b.seen = r.Header.Clone()
	return http.DefaultTransport.RoundTrip(r)
}

// TestNew_OAuthWithoutAPIKey pins the auth seam: with an HTTP client
// that authenticates, no API key is needed and none is sent; without
// either, New refuses.
func TestNew_OAuthWithoutAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"responses":[{"labelAnnotations":[{"description":"Dog","score":0.9}]}]}`))
	}))
	defer srv.Close()
	var seen http.Header
	c, err := google.New(google.Config{Endpoint: srv.URL, HTTP: &http.Client{Transport: bearer{&seen}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Labels(t.Context(), strings.NewReader("an image"))
	if err != nil || len(got) != 1 || len(seen.Values("X-Goog-Api-Key")) != 0 || seen.Get("Authorization") == "" {
		t.Fatalf("labels %v, %v; headers %v", got, err, seen)
	}
	if _, err := google.New(google.Config{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("no key and no client: %v", err)
	}
}
