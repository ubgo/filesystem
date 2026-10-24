package cfstream_test

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
	"github.com/ubgo/filesystem/contrib/videoplatform/cfstream"
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
	rec := &bodies{next: &fakeStream{videos: map[string]*fakeVideo{}}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := newClient(t, srv, cfstream.Config{CreateOptions: map[string]json.RawMessage{"maxDurationSeconds": json.RawMessage(`600`), "allowedOrigins": json.RawMessage(`["app.example.com"]`)}})
	if _, err := c.Ingest(t.Context(), videoplatform.Source{URL: "https://files.example/v.mp4", Passthrough: "acme/f1"}); err != nil {
		t.Fatal(err)
	}
	plain := newClient(t, srv, cfstream.Config{})
	if _, err := plain.Ingest(t.Context(), videoplatform.Source{URL: "https://files.example/v.mp4"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 2 || rec.got[0]["maxDurationSeconds"] == nil || rec.got[0]["url"] == nil || rec.got[1]["maxDurationSeconds"] != nil || rec.got[1]["url"] == nil {
		t.Fatalf("bodies %v", rec.got)
	}
	for name, opts := range map[string]map[string]json.RawMessage{
		"reserved": {"requireSignedURLs": json.RawMessage(`"x"`)},
		"not json": {"x": json.RawMessage(`{`)},
	} {
		cfg := cfstream.Config{CreateOptions: opts, BaseURL: srv.URL}
		cfg.AccountID, cfg.APIToken = account, videoplatform.NewSecret(token)
		if _, err := cfstream.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestSourceOptions pins the per-file seam (R-I21): a Source's Options
// reach the request over the Config's (the file's value winning a field
// both set), and a reserved field or a value that is not JSON fails
// Ingest before any request is sent.
func TestSourceOptions(t *testing.T) {
	rec := &bodies{next: &fakeStream{videos: map[string]*fakeVideo{}}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := newClient(t, srv, cfstream.Config{CreateOptions: map[string]json.RawMessage{"maxDurationSeconds": json.RawMessage(`600`), "allowedOrigins": json.RawMessage(`["app.example.com"]`)}})
	src := videoplatform.Source{URL: "https://files.example/v.mp4", Passthrough: "acme/f1", Options: map[string]json.RawMessage{"maxDurationSeconds": json.RawMessage(`60`), "creator": json.RawMessage(`"u1"`)}}
	if _, err := c.Ingest(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 || string(rec.got[0]["maxDurationSeconds"]) != `60` || string(rec.got[0]["creator"]) != `"u1"` || rec.got[0]["allowedOrigins"] == nil || rec.got[0]["url"] == nil {
		t.Fatalf("body %v", rec.got)
	}
	for name, opts := range map[string]map[string]json.RawMessage{
		"reserved": {"url": json.RawMessage(`"https://evil.example/x.mp4"`)},
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
