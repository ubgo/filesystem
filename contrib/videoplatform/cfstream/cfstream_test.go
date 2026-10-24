package cfstream_test

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
	"github.com/ubgo/filesystem/contrib/videoplatform/cfstream"
)

// fakeStream is an in-process fake of the parts of Cloudflare Stream's
// API the adapter uses (https://developers.cloudflare.com/api/resources/stream/):
// bearer auth, POST /accounts/{id}/stream/copy with url, meta and
// requireSignedURLs, GET and DELETE /accounts/{id}/stream/{uid}, the
// {"success", "result"} envelope, 404 for an unknown video.
type fakeStream struct {
	mu     sync.Mutex
	videos map[string]*fakeVideo
	copies []copyBody
	n      int
	// fail makes the next call answer success:false.
	fail bool
}

const account, token = "acct", "tok"

// fakeVideo is Stream's video object as the fake serves it.
type fakeVideo struct {
	UID    string `json:"uid"`
	Status struct {
		State         string `json:"state"`
		ErrReasonCode string `json:"errReasonCode,omitempty"`
		ErrReasonText string `json:"errReasonText,omitempty"`
	} `json:"status"`
	Playback struct {
		HLS string `json:"hls"`
	} `json:"playback"`
	Thumbnail string            `json:"thumbnail"`
	Duration  float64           `json:"duration"`
	Meta      map[string]string `json:"meta"`
}

// copyBody is the request body of POST /stream/copy.
type copyBody struct {
	URL               string            `json:"url"`
	Meta              map[string]string `json:"meta"`
	RequireSignedURLs bool              `json:"requireSignedURLs"`
}

type envelope struct {
	Success bool       `json:"success"`
	Result  *fakeVideo `json:"result"`
}

func (f *fakeStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		http.Error(w, `{"success":false}`, http.StatusForbidden)
		return
	}
	uid, ok := strings.CutPrefix(r.URL.Path, "/accounts/"+account+"/stream/")
	switch {
	case !ok:
		http.Error(w, "no route", http.StatusNotFound)
	case r.Method == http.MethodPost && uid == "copy":
		var in copyBody
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		f.copies = append(f.copies, in)
		f.n++
		v := &fakeVideo{UID: fmt.Sprintf("v%d", f.n), Duration: -1, Meta: in.Meta}
		v.Status.State = "downloading"
		f.videos[v.UID] = v
		f.reply(w, v)
	case r.Method == http.MethodGet:
		v, ok := f.videos[uid]
		if !ok {
			http.Error(w, `{"success":false}`, http.StatusNotFound)
			return
		}
		f.reply(w, v)
	case r.Method == http.MethodDelete:
		if _, ok := f.videos[uid]; !ok {
			http.Error(w, `{"success":false}`, http.StatusNotFound)
			return
		}
		delete(f.videos, uid)
		w.WriteHeader(http.StatusOK)
	}
}

func (f *fakeStream) reply(w http.ResponseWriter, v *fakeVideo) {
	ok := !f.fail
	f.fail = false
	_ = json.NewEncoder(w).Encode(envelope{Success: ok, Result: v})
}

func newClient(t *testing.T, srv *httptest.Server, cfg cfstream.Config) *cfstream.Client {
	t.Helper()
	if cfg.APIToken.IsZero() {
		cfg.AccountID, cfg.APIToken = account, videoplatform.NewSecret(token)
	}
	cfg.BaseURL = srv.URL + "/"
	c, err := cfstream.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVideoLifecycle(t *testing.T) {
	fake := &fakeStream{videos: map[string]*fakeVideo{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := newClient(t, srv, cfstream.Config{RequireSignedURLs: true})
	a, err := c.Ingest(t.Context(), videoplatform.Source{URL: "https://files.example/v.mp4?sig=x", Passthrough: "acme/f1"})
	if err != nil || a.ID != "v1" || a.Status != videoplatform.StatusPreparing || a.Duration != 0 || a.Passthrough != "acme/f1" {
		t.Fatalf("Ingest = %+v, %v", a, err)
	}
	if in := fake.copies[0]; in.URL != "https://files.example/v.mp4?sig=x" || !in.RequireSignedURLs || in.Meta[cfstream.MetaPassthrough] != "acme/f1" {
		t.Fatalf("request = %+v", in)
	}
	v := fake.videos["v1"]
	v.Status.State, v.Duration, v.Playback.HLS, v.Thumbnail = "ready", 4.5, "https://customer-x.cloudflarestream.com/v1/manifest/video.m3u8", "https://customer-x.cloudflarestream.com/v1/thumbnails/thumbnail.jpg"
	if a, err := c.Asset(t.Context(), "v1"); err != nil || a.Status != videoplatform.StatusReady || a.Duration != 4500*time.Millisecond || a.HLS != v.Playback.HLS || a.Thumbnail != v.Thumbnail {
		t.Fatalf("Asset = %+v, %v", a, err)
	}
	v.Status.State, v.Status.ErrReasonCode, v.Status.ErrReasonText = "error", "ERR_NON_VIDEO", "The file was not recognized as a valid video file."
	if a, err := c.Asset(t.Context(), "v1"); err != nil || a.Status != videoplatform.StatusErrored || a.Detail != "ERR_NON_VIDEO: The file was not recognized as a valid video file." {
		t.Fatalf("errored = %+v, %v", a, err)
	}
	fake.fail = true
	if _, err := c.Asset(t.Context(), "v1"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("success:false: %v", err)
	}
	if err := c.Delete(t.Context(), "v1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), "v1"); err != nil {
		t.Fatalf("deleting a deleted video: %v", err)
	}
	if _, err := c.Asset(t.Context(), "v1"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a deleted video: %v", err)
	}
	// No passthrough, no meta.
	if _, err := c.Ingest(t.Context(), videoplatform.Source{URL: "u"}); err != nil || fake.copies[1].Meta != nil {
		t.Fatalf("meta without passthrough = %v, %v", fake.copies[1].Meta, err)
	}
}

func TestErrors(t *testing.T) {
	fake := &fakeStream{videos: map[string]*fakeVideo{}}
	srv := httptest.NewServer(fake)
	wrong := newClient(t, srv, cfstream.Config{AccountID: account, APIToken: videoplatform.NewSecret("wrong")})
	if _, err := wrong.Ingest(t.Context(), videoplatform.Source{URL: "u"}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("bad token: %v", err)
	}
	c := newClient(t, srv, cfstream.Config{})
	srv.Close()
	if err := c.Delete(t.Context(), "v1"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("API down: %v", err)
	}
	if _, err := cfstream.New(cfstream.Config{AccountID: "a"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("no token: %v", err)
	}
	if c, err := cfstream.New(cfstream.Config{AccountID: "a", APIToken: videoplatform.NewSecret("t")}); err != nil || c.Name() != cfstream.Name {
		t.Fatalf("defaults: %v", err)
	}
}

// webhook builds a request signed the way Stream signs.
func webhook(t *testing.T, secret []byte, at time.Time, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(at.Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/hooks/stream", strings.NewReader(body))
	r.Header.Set("Webhook-Signature", "time="+ts+",sig1="+hex.EncodeToString(videoplatform.Sign(secret, ts, []byte(body))))
	return r
}

func TestWebhook(t *testing.T) {
	secret, now := []byte("whsec"), time.Unix(1_800_000_000, 0)
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := newClient(t, srv, cfstream.Config{WebhookSecret: videoplatform.NewSecret(string(secret)), Now: func() time.Time { return now }})
	body := `{"uid":"v1","readyToStream":true,"status":{"state":"ready"},"meta":{"passthrough":"acme/f1"},"duration":2,"modified":"2026-10-07T10:00:00Z","playback":{"hls":"https://h/v1.m3u8"}}`
	a, err := c.Webhook(webhook(t, secret, now, body), []byte(body))
	if err != nil || a.ID != "v1" || a.Status != videoplatform.StatusReady || a.Passthrough != "acme/f1" || a.HLS != "https://h/v1.m3u8" || !a.At.Equal(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("ready = %+v, %v", a, err)
	}
	for _, bad := range []string{`not json`, `{"status":{"state":"ready"}}`} {
		if _, err := c.Webhook(webhook(t, secret, now, bad), []byte(bad)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := c.Webhook(webhook(t, []byte("guess"), now, body), []byte(body)); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("forged: %v", err)
	}
}
