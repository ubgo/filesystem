package google_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/vision"
	"github.com/ubgo/filesystem/contrib/vision/google"
)

const apiKey = "AIza-test-key-0123456789"

// fakeVision serves images:annotate as Google documents it: it checks
// the key header, the method and the request, and answers with reply
// (status and body) or, when reply is nil, labels for the image.
type fakeVision struct {
	t      *testing.T
	calls  atomic.Int32
	status int
	body   string
}

func (f *fakeVision) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	if r.Method != http.MethodPost || r.Header.Get("X-Goog-Api-Key") != apiKey || r.URL.Query().Has("key") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":403,"message":"The request is missing a valid API key."}}`)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = fmt.Fprint(w, f.body)
		return
	}
	var req struct {
		Requests []struct {
			Image struct {
				Content string `json:"content"`
			} `json:"image"`
			Features []struct {
				Type       string `json:"type"`
				MaxResults int    `json:"maxResults"`
			} `json:"features"`
		} `json:"requests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Requests) != 1 || len(req.Requests[0].Features) != 1 || req.Requests[0].Features[0].Type != "LABEL_DETECTION" {
		f.t.Errorf("bad request: %+v %v", req, err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	img, err := base64.StdEncoding.DecodeString(req.Requests[0].Image.Content)
	if err != nil || string(img) != "an image" {
		_, _ = fmt.Fprint(w, `{"responses":[{"error":{"code":3,"message":"Bad image data."}}]}`)
		return
	}
	_, _ = fmt.Fprintf(w, `{"responses":[{"labelAnnotations":[{"mid":"/m/0bt9lr","description":"Dog","score":0.97,"topicality":0.97},{"mid":"/m/01lrl","description":"Carnivore","score":%g,"topicality":0.8}]}]}`, 0.8)
}

func newClient(t *testing.T, f *fakeVision, cfg google.Config) *google.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg.Endpoint = srv.URL + "/v1/images:annotate"
	if cfg.APIKey.IsZero() {
		cfg.APIKey = vision.NewSecret(apiKey)
	}
	c, err := google.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLabels(t *testing.T) {
	f := &fakeVision{t: t}
	c := newClient(t, f, google.Config{})
	got, err := c.Labels(t.Context(), strings.NewReader("an image"))
	want := []vision.Label{{Name: "Dog", Score: 0.97}, {Name: "Carnivore", Score: 0.8}}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("labels = %+v, %v", got, err)
	}
	// An image Google cannot decode is the client's file.
	if _, err := c.Labels(t.Context(), strings.NewReader("not an image")); filesystem.CodeOf(err) != filesystem.CodeRejected || !strings.Contains(err.Error(), "Bad image data") {
		t.Fatalf("bad image: %v", err)
	}
}

func TestLabels_Failures(t *testing.T) {
	ctx := t.Context()
	// Too large an image is never sent, so it costs nothing.
	f := &fakeVision{t: t}
	c := newClient(t, f, google.Config{MaxImage: 4})
	if _, err := c.Labels(ctx, strings.NewReader("an image")); filesystem.CodeOf(err) != filesystem.CodeTooLarge || f.calls.Load() != 0 {
		t.Fatalf("large image: %v, %d calls", err, f.calls.Load())
	}
	// A refused key, an invalid key (Google answers 400), a quota and an
	// outage are the deployment's problem; none names the key.
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusForbidden, `{"error":{"code":403,"message":"Cloud Vision API has not been used in project 1 before or it is disabled."}}`},
		{http.StatusBadRequest, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`},
		{http.StatusTooManyRequests, `{"error":{"code":429,"message":"Quota exceeded"}}`},
		{http.StatusBadGateway, `<html>bad gateway</html>`},
		{http.StatusOK, `not json`},
		{http.StatusOK, `{"responses":[]}`},
	} {
		c := newClient(t, &fakeVision{t: t, status: tc.status, body: tc.body}, google.Config{})
		_, err := c.Labels(ctx, strings.NewReader("an image"))
		if filesystem.CodeOf(err) != filesystem.CodeUnavailable || strings.Contains(err.Error(), apiKey) {
			t.Errorf("HTTP %d %s: %v", tc.status, tc.body, err)
		}
	}
	// A wrong key is refused by the fake as by Google.
	c = newClient(t, &fakeVision{t: t}, google.Config{APIKey: vision.NewSecret("wrong")})
	if _, err := c.Labels(ctx, strings.NewReader("an image")); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("wrong key: %v", err)
	}
	// A long error message is cut.
	c = newClient(t, &fakeVision{t: t, status: http.StatusServiceUnavailable, body: `{"error":{"message":"` + strings.Repeat("busy ", 500) + `"}}`}, google.Config{})
	if _, err := c.Labels(ctx, strings.NewReader("an image")); err == nil || len(err.Error()) > 300 {
		t.Fatalf("long message: %d bytes", len(err.Error()))
	}
	// An unreachable endpoint.
	c, err := google.New(google.Config{APIKey: vision.NewSecret(apiKey), Endpoint: "http://127.0.0.1:1/v1/images:annotate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Labels(ctx, strings.NewReader("an image")); filesystem.CodeOf(err) != filesystem.CodeUnavailable || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("unreachable: %v", err)
	}
	if _, err := google.New(google.Config{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("no key: %v", err)
	}
}

// TestSecret_NeverPrints pins that a Config or Client printed or logged
// in any way does not show the key.
func TestSecret_NeverPrints(t *testing.T) {
	cfg := google.Config{APIKey: vision.NewSecret(apiKey)}
	c, err := google.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		fmt.Fprintf(&out, verb+"\n", cfg)
		fmt.Fprintf(&out, verb+"\n", c)
		fmt.Fprintf(&out, verb+"\n", cfg.APIKey)
	}
	slog.New(slog.NewJSONHandler(&out, nil)).Info("config", "cfg", cfg, "key", cfg.APIKey)
	if strings.Contains(out.String(), apiKey) || strings.Contains(out.String(), fmt.Sprintf("%x", apiKey)) {
		t.Fatalf("the key printed:\n%s", out.String())
	}
	if got := fmt.Sprint(cfg.APIKey) + fmt.Sprintf("%#v", cfg.APIKey); got != "[redacted][redacted]" {
		t.Fatalf("a Secret prints as %q", got)
	}
	if !strings.Contains(out.String(), `"key":"[redacted]"`) {
		t.Fatalf("a logged Secret:\n%s", out.String())
	}
	if cfg.APIKey.Reveal() != apiKey || !vision.NewSecret("").IsZero() || vision.NewSecret("").Reveal() != "" {
		t.Fatal("Reveal")
	}
}

// TestLabels_RefusesRedirects pins that a redirect, even to the same
// path on another host, is not followed, so the key header never leaves
// for a host the response names.
func TestLabels_RefusesRedirects(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "" {
			leaked.Store(true)
		}
		_, _ = fmt.Fprint(w, `{"responses":[{}]}`)
	}))
	defer other.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	mine := &http.Client{}
	c, err := google.New(google.Config{APIKey: vision.NewSecret(apiKey), Endpoint: redirecting.URL + "/v1/images:annotate", HTTP: mine})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Labels(t.Context(), strings.NewReader("an image")); filesystem.CodeOf(err) != filesystem.CodeUnavailable || leaked.Load() {
		t.Fatalf("redirect: %v, key sent to the other host: %v", err, leaked.Load())
	}
	if mine.CheckRedirect != nil {
		t.Fatal("the caller's client was changed")
	}
}
