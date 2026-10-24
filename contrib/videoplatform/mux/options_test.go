package mux_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/contrib/videoplatform/mux"
)

// bodies records every POST body before the fake serves it.
type bodies struct {
	mu   sync.Mutex
	got  []map[string]json.RawMessage
	next http.Handler
}

func (b *bodies) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(raw, &m)
		b.mu.Lock()
		b.got = append(b.got, m)
		b.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}
	b.next.ServeHTTP(w, r)
}

// TestCreateOptions pins the create-request seam: extra fields reach the
// platform beside the adapter's own, a field the adapter sets or a value
// that is not JSON is refused at New, and without options the request
// carries the adapter's fields only.
func TestCreateOptions(t *testing.T) {
	rec := &bodies{next: &fakeMux{assets: map[string]*fakeAsset{}}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := newClient(t, srv, mux.Config{CreateOptions: map[string]json.RawMessage{"video_quality": json.RawMessage(`"basic"`), "mp4_support": json.RawMessage(`"capped-1080p"`)}})
	if _, err := c.Ingest(t.Context(), videoplatform.Source{URL: "https://files.example/v.mp4", Passthrough: "acme/f1"}); err != nil {
		t.Fatal(err)
	}
	plain := newClient(t, srv, mux.Config{})
	if _, err := plain.Ingest(t.Context(), videoplatform.Source{URL: "https://files.example/v.mp4"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 2 || rec.got[0]["video_quality"] == nil || rec.got[0]["inputs"] == nil || rec.got[1]["video_quality"] != nil || rec.got[1]["inputs"] == nil {
		t.Fatalf("bodies %v", rec.got)
	}
	for name, opts := range map[string]map[string]json.RawMessage{
		"reserved": {"passthrough": json.RawMessage(`"x"`)},
		"not json": {"x": json.RawMessage(`{`)},
	} {
		cfg := mux.Config{CreateOptions: opts, BaseURL: srv.URL}
		cfg.TokenID, cfg.TokenSecret = tokenID, videoplatform.NewSecret(tokenSecret)
		if _, err := mux.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestSourceOptions pins the per-file seam (R-I21): a Source's Options
// reach the request over the Config's (the file's value winning a field
// both set), and a reserved field or a value that is not JSON fails
// Ingest before any request is sent.
func TestSourceOptions(t *testing.T) {
	rec := &bodies{next: &fakeMux{assets: map[string]*fakeAsset{}}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := newClient(t, srv, mux.Config{CreateOptions: map[string]json.RawMessage{"video_quality": json.RawMessage(`"basic"`), "mp4_support": json.RawMessage(`"capped-1080p"`)}})
	src := videoplatform.Source{URL: "https://files.example/v.mp4", Passthrough: "acme/f1", Options: map[string]json.RawMessage{"video_quality": json.RawMessage(`"plus"`), "max_resolution_tier": json.RawMessage(`"2160p"`)}}
	if _, err := c.Ingest(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 || string(rec.got[0]["video_quality"]) != `"plus"` || string(rec.got[0]["max_resolution_tier"]) != `"2160p"` || rec.got[0]["mp4_support"] == nil || rec.got[0]["inputs"] == nil {
		t.Fatalf("body %v", rec.got)
	}
	for name, opts := range map[string]map[string]json.RawMessage{
		"reserved": {"playback_policies": json.RawMessage(`["public"]`)},
		"not json": {"x": json.RawMessage(`{`)},
	} {
		src := videoplatform.Source{URL: "https://files.example/v.mp4", Options: opts}
		if _, err := c.Ingest(t.Context(), src); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(rec.got) != 1 {
		t.Fatalf("a refused Source reached the platform: %v", rec.got[1:])
	}
}
