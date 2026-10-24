package mux_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/contrib/videoplatform"
	"github.com/ubgo/filesystem/contrib/videoplatform/mux"
)

// fakeMux is an in-process fake of the parts of Mux's Video API the
// adapter uses (https://www.mux.com/docs/api-reference/video/assets):
// basic auth, POST /video/v1/assets with inputs, playback_policies and
// passthrough, GET and DELETE /video/v1/assets/{id}, the {"data": …}
// envelope, 404 for an unknown asset.
type fakeMux struct {
	mu      sync.Mutex
	assets  map[string]*fakeAsset
	paths   []string
	created []createBody
	n       int
}

// fakeAsset is Mux's asset object as the fake serves it.
type fakeAsset struct {
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	Duration    float64      `json:"duration,omitempty"`
	Passthrough string       `json:"passthrough,omitempty"`
	PlaybackIDs []playbackID `json:"playback_ids"`
	Errors      *assetErrors `json:"errors,omitempty"`
}

type playbackID struct {
	ID     string `json:"id"`
	Policy string `json:"policy"`
}

type assetErrors struct {
	Type     string   `json:"type"`
	Messages []string `json:"messages"`
}

// createBody is the request body of POST /video/v1/assets.
type createBody struct {
	Inputs []struct {
		URL string `json:"url"`
	} `json:"inputs"`
	PlaybackPolicies []string `json:"playback_policies"`
	Passthrough      string   `json:"passthrough"`
}

// data is Mux's response envelope.
type data struct {
	Data *fakeAsset `json:"data"`
}

const tokenID, tokenSecret = "tok", "sec"

func (f *fakeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.EscapedPath())
	if id, secret, ok := r.BasicAuth(); !ok || id != tokenID || secret != tokenSecret {
		http.Error(w, `{"error":{"type":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	id, isAsset := strings.CutPrefix(r.URL.Path, "/video/v1/assets/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/video/v1/assets":
		var in createBody
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Inputs) != 1 || len(in.PlaybackPolicies) != 1 {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.created = append(f.created, in)
		f.n++
		id := fmt.Sprintf("a%d", f.n)
		f.assets[id] = &fakeAsset{ID: id, Status: "preparing", Passthrough: in.Passthrough, PlaybackIDs: []playbackID{{ID: "p" + id, Policy: in.PlaybackPolicies[0]}}}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(data{f.assets[id]})
	case isAsset && r.Method == http.MethodGet:
		a, ok := f.assets[id]
		if !ok {
			http.Error(w, `{"error":{"type":"not_found"}}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(data{a})
	case isAsset && r.Method == http.MethodDelete:
		if _, ok := f.assets[id]; !ok {
			http.Error(w, `{"error":{"type":"not_found"}}`, http.StatusNotFound)
			return
		}
		delete(f.assets, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "no route", http.StatusNotFound)
	}
}

func newClient(t *testing.T, srv *httptest.Server, cfg mux.Config) *mux.Client {
	t.Helper()
	if cfg.TokenID == "" {
		cfg.TokenID, cfg.TokenSecret = tokenID, videoplatform.NewSecret(tokenSecret)
	}
	cfg.BaseURL = srv.URL + "/"
	c, err := mux.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAssetLifecycle(t *testing.T) {
	fake := &fakeMux{assets: map[string]*fakeAsset{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := newClient(t, srv, mux.Config{Policy: mux.PolicySigned})
	a, err := c.Ingest(t.Context(), videoplatform.Source{URL: "https://files.example/v.mp4?sig=x", Passthrough: "acme/f1"})
	if err != nil || a.ID != "a1" || a.Status != videoplatform.StatusPreparing || a.PlaybackID != "pa1" || a.HLS != "https://stream.mux.com/pa1.m3u8" || a.Thumbnail != "https://image.mux.com/pa1/thumbnail.jpg" || a.Passthrough != "acme/f1" {
		t.Fatalf("Ingest = %+v, %v", a, err)
	}
	if in := fake.created[0]; in.Inputs[0].URL != "https://files.example/v.mp4?sig=x" || in.PlaybackPolicies[0] != mux.PolicySigned || in.Passthrough != "acme/f1" {
		t.Fatalf("request = %+v", in)
	}
	fake.assets["a1"].Status, fake.assets["a1"].Duration = "ready", 12.5
	if a, err := c.Asset(t.Context(), "a1"); err != nil || a.Status != videoplatform.StatusReady || a.Duration != 12500*time.Millisecond {
		t.Fatalf("Asset = %+v, %v", a, err)
	}
	fake.assets["a1"].Status, fake.assets["a1"].Errors = "errored", &assetErrors{Type: "invalid_input", Messages: []string{"no video track"}}
	if a, err := c.Asset(t.Context(), "a1"); err != nil || a.Status != videoplatform.StatusErrored || a.Detail != "invalid_input: no video track" {
		t.Fatalf("errored = %+v, %v", a, err)
	}
	if err := c.Delete(t.Context(), "a1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), "a1"); err != nil {
		t.Fatalf("deleting a deleted asset: %v", err)
	}
	if _, err := c.Asset(t.Context(), "a1"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a deleted asset: %v", err)
	}
	// An ID cannot leave the asset path.
	_, _ = c.Asset(t.Context(), "../../v1/x")
	if last := fake.paths[len(fake.paths)-1]; last != "GET /video/v1/assets/..%2F..%2Fv1%2Fx" {
		t.Fatalf("path = %s", last)
	}
}

func TestErrors(t *testing.T) {
	fake := &fakeMux{assets: map[string]*fakeAsset{}}
	srv := httptest.NewServer(fake)
	wrong := newClient(t, srv, mux.Config{TokenID: "tok", TokenSecret: videoplatform.NewSecret("wrong")})
	if _, err := wrong.Ingest(t.Context(), videoplatform.Source{URL: "u"}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("bad credentials: %v", err)
	}
	c := newClient(t, srv, mux.Config{})
	if _, err := c.Ingest(t.Context(), videoplatform.Source{URL: "u", Passthrough: strings.Repeat("x", mux.MaxPassthroughBytes+1)}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("long passthrough: %v", err)
	}
	srv.Close()
	if _, err := c.Asset(t.Context(), "a1"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("API down: %v", err)
	}
	if err := c.Delete(t.Context(), "a1"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("delete with the API down: %v", err)
	}
	for name, cfg := range map[string]mux.Config{"no token": {TokenID: "x"}, "bad policy": {TokenID: "x", TokenSecret: videoplatform.NewSecret("y"), Policy: "private"}} {
		if _, err := mux.New(cfg); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if c, err := mux.New(mux.Config{TokenID: "x", TokenSecret: videoplatform.NewSecret("y")}); err != nil || c.Name() != mux.Name {
		t.Fatalf("defaults: %v", err)
	}
}

// webhook builds a request signed the way Mux signs.
func webhook(t *testing.T, secret []byte, at time.Time, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(at.Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/hooks/mux", strings.NewReader(body))
	r.Header.Set("Mux-Signature", "t="+ts+",v1="+hex.EncodeToString(videoplatform.Sign(secret, ts, []byte(body))))
	return r
}

func TestWebhook(t *testing.T) {
	secret, now := []byte("whsec"), time.Unix(1_800_000_000, 0)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := newClient(t, srv, mux.Config{WebhookSecret: videoplatform.NewSecret(string(secret)), Now: func() time.Time { return now }})
	check := func(body string) (videoplatform.Asset, error) {
		return c.Webhook(webhook(t, secret, now, body), []byte(body))
	}
	a, err := check(`{"type":"video.asset.ready","created_at":"2026-10-07T10:00:00.123456Z","data":{"id":"a1","status":"ready","duration":3,"passthrough":"acme/f1","playback_ids":[{"id":"p1"}]}}`)
	if err != nil || a.ID != "a1" || a.Status != videoplatform.StatusReady || a.Duration != 3*time.Second || a.Passthrough != "acme/f1" || !a.At.Equal(time.Date(2026, 10, 7, 10, 0, 0, 123456000, time.UTC)) {
		t.Fatalf("ready = %+v, %v", a, err)
	}
	// The deletion is dated like any event, so the plugin orders a
	// replayed ready before it.
	if a, err := check(`{"type":"video.asset.deleted","created_at":"2026-10-07T10:05:00Z","data":{"id":"a1","status":"ready"}}`); err != nil || a.Status != videoplatform.StatusDeleted || a.Detail != "deleted on Mux" || !a.At.Equal(time.Date(2026, 10, 7, 10, 5, 0, 0, time.UTC)) {
		t.Fatalf("deleted = %+v, %v", a, err)
	}
	if _, err := check(`{"type":"video.upload.created","data":{"id":"u1"}}`); err != videoplatform.ErrIgnoredEvent {
		t.Fatalf("an upload event: %v", err)
	}
	if _, err := check(`not json`); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("malformed: %v", err)
	}
	forged := webhook(t, []byte("guess"), now, `{"type":"video.asset.ready","data":{"id":"a1"}}`)
	if _, err := c.Webhook(forged, []byte(`{"type":"video.asset.ready","data":{"id":"a1"}}`)); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("forged: %v", err)
	}
	old := webhook(t, secret, now.Add(-time.Hour), `{}`)
	if _, err := c.Webhook(old, []byte(`{}`)); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("replayed: %v", err)
	}
}
