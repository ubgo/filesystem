package webhooks_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/webhooks"
)

// seen is one request a test receiver accepted.
type seen struct {
	path, contentType, ceType, signature, body string
}

// TestEndpointPayload: each endpoint gets its own body and headers (a
// Slack message, a CloudEvents binary-mode request), the delivery is still
// signed, and a retry sends the same stored bytes and headers.
func TestEndpointPayload(t *testing.T) {
	ctx := t.Context()
	var (
		mu   sync.Mutex
		got  []seen
		fail = true
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		got = append(got, seen{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Ce-Type"), r.Header.Get(webhooks.HeaderSignature), string(b)})
		if fail && r.URL.Path == "/ce" {
			http.Error(w, "down", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	shape := func(_ context.Context, ev records.Event, ep webhooks.Endpoint) (webhooks.Body, error) {
		if strings.HasSuffix(ep.URL, "/slack") {
			return webhooks.Body{Data: []byte(`{"text":"` + string(ev.Kind) + `"}`)}, nil
		}
		return webhooks.Body{
			Data: []byte(`{"file":"` + ev.FileID + `"}`), ContentType: "application/json",
			Header: http.Header{"Ce-Specversion": {"1.0"}, "Ce-Type": {string(ev.Kind)}, "Ce-Id": {ev.ID}},
		}, nil
	}
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithEndpointPayload(shape)}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	for _, path := range []string{"/slack", "/ce"} {
		if _, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL+path); err != nil {
			t.Fatal(err)
		}
	}
	f := e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	e.advance(webhooks.DefaultBackoff(1))
	if n, err := e.p.Deliver(ctx, e.store, e.clock()); err != nil || n != 1 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	byPath := map[string][]seen{}
	for _, s := range got {
		byPath[s.path] = append(byPath[s.path], s)
	}
	slack, ce := byPath["/slack"], byPath["/ce"]
	if len(slack) != 1 || slack[0].body != `{"text":"file.created"}` || slack[0].contentType != webhooks.DefaultContentType || slack[0].signature == "" {
		t.Fatalf("slack got %+v", slack)
	}
	if len(ce) != 2 || ce[1].body != ce[0].body || ce[1].ceType != ce[0].ceType || ce[1].contentType != ce[0].contentType {
		t.Fatalf("the retry differs: %+v", ce)
	}
	if ce[1].body != `{"file":"`+f.ID+`"}` || ce[1].ceType != string(records.EventFileCreated) || ce[1].signature == "" {
		t.Fatalf("cloudevents got %+v", ce)
	}
}

// TestEndpointPayloadContentType: a non-JSON body goes out with its own
// Content-Type.
func TestEndpointPayloadContentType(t *testing.T) {
	ctx := t.Context()
	var ct, body string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ct, body = r.Header.Get("Content-Type"), string(b)
	}))
	t.Cleanup(srv.Close)
	form := func(_ context.Context, ev records.Event, _ webhooks.Endpoint) (webhooks.Body, error) {
		return webhooks.Body{Data: []byte("kind=" + string(ev.Kind)), ContentType: "application/x-www-form-urlencoded"}, nil
	}
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithEndpointPayload(form)}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	if _, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL); err != nil {
		t.Fatal(err)
	}
	e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	if ct != "application/x-www-form-urlencoded" || body != "kind=file.created" {
		t.Fatalf("got %q %q", ct, body)
	}
}

// TestEndpointPayloadFailsClosed: an error, a binary body or a reserved
// header fails the change, so nothing is queued or created.
func TestEndpointPayloadFailsClosed(t *testing.T) {
	ctx := t.Context()
	var answer func() (webhooks.Body, error)
	fn := func(context.Context, records.Event, webhooks.Endpoint) (webhooks.Body, error) { return answer() }
	e := newEnvWith(t, []webhooks.Option{webhooks.WithEndpointPayload(fn)}, nil)
	cases := map[string]struct {
		body webhooks.Body
		err  error
	}{
		"error":     {err: errSigner},
		"binary":    {body: webhooks.Body{Data: []byte{0xff, 0xfe}}},
		"signature": {body: webhooks.Body{Data: []byte("{}"), Header: http.Header{"Webhook-Signature": {"v1,forged"}}}},
		"host":      {body: webhooks.Body{Data: []byte("{}"), Header: http.Header{"host": {"evil"}}}},
		"length":    {body: webhooks.Body{Data: []byte("{}"), Header: http.Header{"Content-Length": {"1"}}}},
	}
	for name, c := range cases {
		answer = func() (webhooks.Body, error) { return c.body, c.err }
		if _, err := e.disk.WriteBytes(ctx, name, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		_, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: name})
		if c.err != nil && !errors.Is(err, c.err) || c.err == nil && filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: CreateFile = %v", name, err)
		}
	}
	if d := e.pending(t); len(d) != 0 {
		t.Fatalf("queued %+v", d)
	}
}

// TestDefaultBodyUnchanged: without WithEndpointPayload every endpoint
// gets DefaultPayload as application/json.
func TestDefaultBodyUnchanged(t *testing.T) {
	ctx := t.Context()
	var ct, body string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ct, body = r.Header.Get("Content-Type"), string(b)
	}))
	t.Cleanup(srv.Close)
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client())}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	if _, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL); err != nil {
		t.Fatal(err)
	}
	f := e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	if ct != "application/json" || !strings.Contains(body, `"file_id":"`+f.ID+`"`) {
		t.Fatalf("got %q %s", ct, body)
	}
}
