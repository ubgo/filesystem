package webhooks_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records/webhooks"
)

var errNoEntropy = errors.New("no entropy")

// hubSigner signs GitHub-style (X-Hub-Signature-256 over the body, HMAC
// under the raw secret) and owns its secret format: free-form text,
// refused only when it starts with "bad". next overrides NewSecret's
// result; fail fails it.
type hubSigner struct {
	next webhooks.Secret
	fail bool
}

func (h *hubSigner) Sign(_ context.Context, req webhooks.SignRequest) error {
	m := hmac.New(sha256.New, []byte(req.Endpoint.Secret.Reveal()))
	m.Write(req.Body)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	return nil
}

func (h *hubSigner) NewSecret() (webhooks.Secret, error) {
	if h.fail {
		return "", errNoEntropy
	}
	if h.next != "" {
		return h.next, nil
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return webhooks.Secret(hex.EncodeToString(b)), nil
}

func (h *hubSigner) CheckSecret(s webhooks.Secret) error {
	if strings.HasPrefix(s.Reveal(), "bad") {
		return filesystem.Errorf(filesystem.CodeInvalidArgument, "hub: refused")
	}
	return nil
}

// TestSecretFormat: a Signer with SecretFormat takes a free-form secret a
// receiver already holds and signs with it, generates secrets in its own
// format, and its refusal is honoured.
func TestSecretFormat(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	var sigs []string
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		sigs, bodies = append(sigs, r.Header.Get("X-Hub-Signature-256")), append(bodies, b)
	}))
	t.Cleanup(srv.Close)
	sg := &hubSigner{}
	e := newEnvWith(t, []webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithSigner(sg)}, nil)
	_, _ = e.p.RemoveEndpoint(ctx, e.store, scope, e.ep.ID)
	const held = "the-secret-our-receivers-already-hold"
	ep, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: srv.URL, Secret: held})
	if err != nil {
		t.Fatal(err)
	}
	e.file(t, scope, "a")
	if n, err := e.p.Deliver(ctx, e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	m := hmac.New(sha256.New, []byte(held))
	m.Write(bodies[0])
	if sigs[0] != "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Fatalf("signature %q not under the held secret", sigs[0])
	}
	if _, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: srv.URL, Secret: "bad-but-long-enough-secret-xyz"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a secret the signer refuses = %v", err)
	}
	gen, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: srv.URL})
	if err != nil || strings.HasPrefix(gen.Secret.Reveal(), "whsec_") || len(gen.Secret.Reveal()) != 64 {
		t.Fatalf("generated %d-byte secret, %v; want the signer's 64 hex characters", len(gen.Secret.Reveal()), err)
	}
	rotated, err := e.p.RotateSecret(ctx, e.store, webhooks.EndpointRef{Scope: scope, ID: ep.ID}, "another-free-form-secret-text")
	if err != nil || rotated.Secret.Reveal() != "another-free-form-secret-text" {
		t.Fatalf("rotate = %v, %v", rotated.Secret.Reveal(), err)
	}
}

// TestSecretFormatFloor: the floor holds whatever the format: a short
// secret, one with whitespace, or a short generated one is refused, and a
// failing generator fails the registration.
func TestSecretFormatFloor(t *testing.T) {
	ctx := t.Context()
	sg := &hubSigner{}
	e := newEnvWith(t, []webhooks.Option{webhooks.WithSigner(sg)}, nil)
	for name, sec := range map[string]webhooks.Secret{
		"short":      webhooks.Secret(strings.Repeat("k", webhooks.MinSecretBytes-1)),
		"whitespace": "a secret with spaces in it, long enough",
		"long":       webhooks.Secret(strings.Repeat("k", webhooks.MaxSecretTextBytes+1)),
	} {
		if _, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: "https://example.com/" + name, Secret: sec}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	sg.next = "tooshort"
	if _, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: "https://example.com/gen"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a short generated secret = %v", err)
	}
	sg.next, sg.fail = "", true
	if _, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: "https://example.com/gen"}); !errors.Is(err, errNoEntropy) {
		t.Fatalf("a failing generator = %v", err)
	}
	if _, err := e.p.RotateSecret(ctx, e.store, webhooks.EndpointRef{Scope: scope, ID: e.ep.ID}, ""); !errors.Is(err, errNoEntropy) {
		t.Fatalf("rotate with a failing generator = %v", err)
	}
}

// TestSecretFormatDefault: without SecretFormat a free-form secret is
// refused and generated secrets are "whsec_".
func TestSecretFormatDefault(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	if _, err := e.p.Register(ctx, e.store, webhooks.EndpointSpec{Scope: scope, URL: "https://example.com/a", Secret: "the-secret-our-receivers-already-hold"}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("free-form secret without SecretFormat = %v", err)
	}
	if !strings.HasPrefix(e.ep.Secret.Reveal(), "whsec_") {
		t.Fatal("the default secret is not whsec_")
	}
}
