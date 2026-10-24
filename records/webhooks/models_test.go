package webhooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/webhooks"
)

var errSigner = errors.New("kms unavailable")

// TestGlobalEndpoint: a global endpoint takes every scope's events, is
// listed apart from scoped ones, and is not reachable through scope "".
func TestGlobalEndpoint(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.EventFileCreated)
	// The test receiver checks one secret: give the global endpoint the
	// same (a caller's secret, through Register).
	g, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: "ignored", URL: e.ep.URL, Kinds: []records.EventKind{records.EventFileCreated}, Secret: e.ep.Secret, Global: true})
	if err != nil || !g.Global || g.Scope != "" {
		t.Fatalf("AddGlobalEndpoint = %+v, %v", g, err)
	}
	e.file(t, scope, "a")
	e.file(t, "other", "b")
	e.file(t, "", "c")
	// scope's endpoint gets "a"; the global one gets all three.
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 4 {
		t.Fatalf("Deliver = %d, %v; want 1 scoped + 3 global", n, err)
	}
	if eps, _ := e.p.Endpoints(ctx, e.store, ""); len(eps) != 0 {
		t.Fatalf("scope \"\" lists %+v", eps)
	}
	if eps, _ := e.p.Endpoints(ctx, e.store, scope); len(eps) != 1 || eps[0].Global {
		t.Fatalf("scope lists %+v", eps)
	}
	if gl, _ := e.p.GlobalEndpoints(ctx, e.store); len(gl) != 1 || gl[0].ID != g.ID || !gl[0].Global {
		t.Fatalf("GlobalEndpoints = %+v", gl)
	}
	if ok, err := e.p.RemoveEndpoint(ctx, e.store, "", g.ID); ok || err != nil {
		t.Fatalf("RemoveEndpoint(scope \"\") removed the global endpoint: %v, %v", ok, err)
	}
	if _, err := e.p.RotateSecret(ctx, e.store, webhooks.EndpointRef{ID: g.ID}, ""); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("scoped rotate of a global endpoint = %v", err)
	}
	if ok, err := e.p.RemoveGlobalEndpoint(ctx, e.store, g.ID); !ok || err != nil {
		t.Fatalf("RemoveGlobalEndpoint = %v, %v", ok, err)
	}
}

// verifies reports whether a delivery signed in h verifies with secret.
func verifies(h http.Header, secret webhooks.Secret, body []byte, now time.Time) bool {
	return webhooks.VerifySignature(h, secret, body, now, 0) == nil
}

// TestRotateSecret: during a rotation every delivery carries a signature
// per active secret, so receivers on the old and the new secret both
// accept it; retiring leaves only the new one.
func TestRotateSecret(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	ref := webhooks.EndpointRef{Scope: scope, ID: e.ep.ID}
	old := e.ep.Secret
	ep, err := e.p.RotateSecret(ctx, e.store, ref, "")
	if err != nil || len(ep.Secrets) != 2 || ep.Secrets[1] != old || ep.Secret == old {
		t.Fatalf("RotateSecret = %+v, %v", ep.Secrets, err)
	}
	next := ep.Secret

	h, body := http.Header{}, []byte(`{}`)
	if err := (webhooks.StandardSigner{}).Sign(ctx, webhooks.SignRequest{Header: h, Endpoint: ep, MessageID: "m", At: e.now, Body: body}); err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(h.Get(webhooks.HeaderSignature))) != 2 || !verifies(h, old, body, e.now) || !verifies(h, next, body, e.now) {
		t.Fatalf("signature %q does not carry both secrets", h.Get(webhooks.HeaderSignature))
	}
	// The real delivery path: the test receiver still holds the old one.
	e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver during rotation = %d, %v", n, err)
	}

	ep, err = e.p.RetireSecrets(ctx, e.store, ref)
	if err != nil || len(ep.Secrets) != 1 || ep.Secret != next {
		t.Fatalf("RetireSecrets = %+v, %v", ep.Secrets, err)
	}
	if _, err := e.p.RetireSecrets(ctx, e.store, ref); err != nil {
		t.Fatalf("retiring again = %v", err)
	}
	e.file(t, scope, "b")
	if n, _ := e.p.Deliver(ctx, e.store, e.now); n != 0 {
		t.Fatal("a receiver on the retired secret still accepted a delivery")
	}

	// A caller's secret, a cap on how many stay active, and refusals.
	mine := webhooks.NewSecret()
	if ep, err = e.p.RotateSecret(ctx, e.store, ref, mine); err != nil || ep.Secret != mine {
		t.Fatalf("RotateSecret(mine) = %v, %v", ep.Secret, err)
	}
	if _, err := e.p.RotateSecret(ctx, e.store, ref, mine); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("rotating to an active secret = %v", err)
	}
	for range webhooks.MaxSecrets + 2 {
		if ep, err = e.p.RotateSecret(ctx, e.store, ref, ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(ep.Secrets) != webhooks.MaxSecrets {
		t.Fatalf("%d active secrets, want the cap %d", len(ep.Secrets), webhooks.MaxSecrets)
	}
	if _, err := e.p.RotateSecret(ctx, e.store, webhooks.EndpointRef{Scope: "other", ID: e.ep.ID}, ""); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("rotating another scope's endpoint = %v", err)
	}
}

// TestRegisterCallerSecret: Register keeps a caller's well-formed secret
// and refuses a weak or malformed one.
func TestRegisterCallerSecret(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	mine := webhooks.NewSecret()
	ep, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: e.ep.URL, Secret: mine})
	if err != nil || ep.Secret != mine {
		t.Fatalf("Register = %v, %v", ep.Secret, err)
	}
	// Found by ID: both endpoints share the test's frozen CreatedAt, so
	// their listing order falls to the random IDs.
	listed, err := e.p.Endpoints(ctx, e.store, scope)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(listed, func(l webhooks.Endpoint) bool { return l.ID == ep.ID })
	if i < 0 || listed[i].Secret != mine {
		t.Fatal("the stored secret is not the caller's")
	}
	for _, bad := range []webhooks.Secret{"whsec_c2hvcnQ=", "nope_" + mine[len("whsec_"):], "whsec_!!!", webhooks.Secret("whsec_" + strings.Repeat("QUFB", 30))} {
		if _, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: e.ep.URL, Secret: bad}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Register(%q) = %v", bad, err)
		}
	}
}

// headerSigner is a Signer of the application's: a fixed header, or an
// error.
type headerSigner struct{ err error }

func (sg headerSigner) Sign(_ context.Context, req webhooks.SignRequest) error {
	if sg.err != nil {
		return sg.err
	}
	req.Header.Set("X-Custom-Signature", req.MessageID+"/"+req.Endpoint.ID)
	return nil
}

// TestCustomSigner: a Signer replaces the Standard Webhooks headers, and
// one that fails sends nothing and leaves the delivery to retry.
func TestCustomSigner(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Header.Clone())
	}))
	t.Cleanup(srv.Close)

	sg := &headerSigner{}
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithSigner(sg)}, nil)
	ep, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID) // only srv
	e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0].Get("X-Custom-Signature"), "/"+ep.ID) || got[0].Get(webhooks.HeaderSignature) != "" {
		t.Fatalf("headers = %v", got)
	}

	sg.err = errSigner
	e.file(t, scope, "b")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 0 {
		t.Fatalf("Deliver with a failing signer = %d, %v", n, err)
	}
	d := e.pending(t)
	if len(got) != 1 || len(d) != 1 || d[0].Attempts != 1 || !strings.Contains(d[0].LastError, errSigner.Error()) {
		t.Fatalf("sent %d, pending %+v", len(got), d)
	}
}

// TestDefaultSignerIsSign: by default a delivery carries exactly one v1
// signature, the one Sign computes.
func TestDefaultSignerIsSign(t *testing.T) {
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	sec := webhooks.NewSecret()
	a, b := http.Header{}, http.Header{}
	if err := webhooks.Sign(a, sec, "m", at, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := (webhooks.StandardSigner{}).Sign(t.Context(), webhooks.SignRequest{Header: b, Endpoint: webhooks.Endpoint{Secrets: []webhooks.Secret{sec}}, MessageID: "m", At: at, Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if a.Get(webhooks.HeaderSignature) != b.Get(webhooks.HeaderSignature) || strings.Count(b.Get(webhooks.HeaderSignature), "v1,") != 1 {
		t.Fatalf("%q != %q", a.Get(webhooks.HeaderSignature), b.Get(webhooks.HeaderSignature))
	}
	if err := (webhooks.StandardSigner{}).Sign(t.Context(), webhooks.SignRequest{Header: http.Header{}}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("signing with no secret = %v", err)
	}
}

// cloudEvent is a CloudEvents 1.0 envelope, the shape a custom
// PayloadFunc sends.
type cloudEvent struct {
	SpecVersion string            `json:"specversion"`
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Source      string            `json:"source"`
	Data        webhooks.Payload  `json:"data"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// TestPayloadFunc: a PayloadFunc's value is the body; one that fails
// fails the change, so nothing is queued.
func TestPayloadFunc(t *testing.T) {
	ctx := t.Context()
	var bodies []cloudEvent
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ce cloudEvent
		_ = json.Unmarshal(b, &ce)
		mu.Lock()
		bodies = append(bodies, ce)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	fail := false
	pf := func(_ context.Context, ev records.Event) (any, error) {
		if fail {
			return nil, errSigner
		}
		return cloudEvent{SpecVersion: "1.0", ID: ev.ID, Type: string(ev.Kind), Source: "/scopes/" + ev.Scope, Data: webhooks.DefaultPayload(ev)}, nil
	}
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithPayload(pf)}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	if _, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL); err != nil {
		t.Fatal(err)
	}
	f := e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	if len(bodies) != 1 || bodies[0].SpecVersion != "1.0" || bodies[0].Type != string(records.EventFileCreated) || bodies[0].Data.FileID != f.ID {
		t.Fatalf("bodies = %+v", bodies)
	}
	fail = true
	if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: "a"}); !errors.Is(err, errSigner) {
		t.Fatalf("CreateFile with a failing PayloadFunc = %v", err)
	}
	if d := e.pending(t); len(d) != 0 {
		t.Fatalf("queued %+v", d)
	}
}

// TestBatchAndConcurrency: WithBatch bounds a run; WithConcurrency sends
// that many at once, each still delivered exactly once.
func TestBatchAndConcurrency(t *testing.T) {
	ctx := t.Context()
	const workers = 3
	var (
		mu             sync.Mutex
		inFlight, peak int
		seen           = map[string]int{}
	)
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		seen[r.Header.Get(webhooks.HeaderID)]++
		if inFlight == workers {
			close(gate) // every worker is in: release them
		}
		mu.Unlock()
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
		mu.Lock()
		inFlight--
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithBatch(workers + 1), webhooks.WithConcurrency(workers)}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	if _, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		e.file(t, scope, k)
	}
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != workers+1 {
		t.Fatalf("first run = %d, %v; want the batch of %d", n, err, workers+1)
	}
	if peak != workers {
		t.Fatalf("peak %d concurrent sends, want %d", peak, workers)
	}
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 2 {
		t.Fatalf("second run = %d, %v", n, err)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("delivery %s sent %d times", id, n)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("%d deliveries sent, want 6", len(seen))
	}
	for _, o := range []webhooks.Option{webhooks.WithBatch(0), webhooks.WithConcurrency(0), webhooks.WithSigner(nil)} {
		if _, err := webhooks.New(o, webhooks.WithLogger(slog.New(slog.DiscardHandler))); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("New = %v", err)
		}
	}
}

// TestDefaultBatchIsSerial: by default one run takes DefaultBatch and
// sends one at a time.
func TestDefaultBatchIsSerial(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	var inFlight, peak, sent int
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		sent++
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client())}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	if _, err := e.p.AddEndpoint(ctx, e.store, scope, srv.URL); err != nil {
		t.Fatal(err)
	}
	for i := range webhooks.DefaultBatch + 3 {
		e.file(t, scope, strings.Repeat("k", i+1))
	}
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != webhooks.DefaultBatch || peak != 1 {
		t.Fatalf("Deliver = %d, %v, peak %d", n, err, peak)
	}
}
