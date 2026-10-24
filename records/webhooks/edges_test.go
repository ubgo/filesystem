package webhooks_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/webhooks"
)

// TestSignRefusesAMalformedSecret: signing or verifying with a secret
// that is not base64 key material is CodeInvalidArgument, and a request
// without a timestamp is refused before any signature is computed.
func TestSignRefusesAMalformedSecret(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, bad := range []webhooks.Secret{"whsec_!!!", "whsec_"} {
		if err := webhooks.Sign(http.Header{}, bad, "evt_1", now, nil); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Sign with %q = %v", bad.Reveal(), err)
		}
	}
	h := http.Header{}
	h.Set(webhooks.HeaderTimestamp, "1790000000")
	if err := webhooks.VerifySignature(h, "whsec_!!!", nil, time.Unix(1790000000, 0), 0); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("VerifySignature with a malformed secret = %v", err)
	}
	if err := webhooks.VerifySignature(http.Header{}, "whsec_c2VjcmV0", nil, now, 0); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Fatalf("VerifySignature without a timestamp = %v", err)
	}
}

// TestAddEndpointRefusesBadKinds: an empty kind, one holding the stored
// separator, or one longer than records allows is refused.
func TestAddEndpointRefusesBadKinds(t *testing.T) {
	e := newEnv(t, nil)
	long := make([]byte, records.MaxEventKindBytes+1)
	for i := range long {
		long[i] = 'k'
	}
	for _, k := range []records.EventKind{"", "a b", records.EventKind(long)} {
		if _, err := e.p.AddEndpoint(t.Context(), e.store, scope, "https://hooks.example.com/x", k); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("AddEndpoint(kind %q) = %v", k, err)
		}
	}
}

// brokenSealer is a KMS that is down: sealing or opening fails while
// down is set.
type brokenSealer struct{ down *bool }

var errKMS = errors.New("kms down")

func (b brokenSealer) Seal(ctx context.Context, secret string) (string, error) {
	if *b.down {
		return "", errKMS
	}
	return sealer{}.Seal(ctx, secret)
}

func (b brokenSealer) Open(ctx context.Context, sealed string) (string, error) {
	if *b.down {
		return "", errKMS
	}
	return sealer{}.Open(ctx, sealed)
}

// TestSealerFailures: with the sealer down an endpoint cannot be added,
// listed, fed events (the change fails rather than dropping its
// deliveries) or delivered to; once it is back, the queued delivery goes
// out.
func TestSealerFailures(t *testing.T) {
	ctx := t.Context()
	down := false
	e := newEnvWith(t, []webhooks.Option{webhooks.WithSealer(brokenSealer{&down})}, nil)
	e.rc.secret = e.ep.Secret
	down = true
	if _, err := e.p.AddEndpoint(ctx, e.store, scope, "https://hooks.example.com/x"); !errors.Is(err, errKMS) {
		t.Fatalf("AddEndpoint with the sealer down = %v", err)
	}
	if _, err := e.p.Endpoints(ctx, e.store, scope); !errors.Is(err, errKMS) {
		t.Fatalf("Endpoints with the sealer down = %v", err)
	}
	if _, err := e.disk.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: "a"}); !errors.Is(err, errKMS) {
		t.Fatalf("a change with the sealer down = %v", err)
	}
	down = false
	e.file(t, scope, "b")
	down = true
	if _, err := e.p.Deliver(ctx, e.store, e.clock()); !errors.Is(err, errKMS) {
		t.Fatalf("Deliver with the sealer down = %v", err)
	}
	down = false
	e.advance(time.Hour)
	if n, err := e.p.Deliver(ctx, e.store, e.clock()); err != nil || n != 1 {
		t.Fatalf("Deliver after the sealer recovered = %d, %v", n, err)
	}
}

// emitter is a plugin that reports its own changes, as folders or share
// do, so a test can emit an event that writes no row of its own.
type emitter struct{}

func (emitter) Name() string { return "emitter" }

// TestUnencodableEventFailsTheChange: an event whose time JSON cannot
// encode (past year 9999) fails the change instead of queueing a
// delivery with no body. The event comes from a plugin's Emit, which
// writes no row, so no engine refuses the time before the encoder sees
// it: the failure is the encoder's on every engine.
func TestUnencodableEventFailsTheChange(t *testing.T) {
	e := newEnv(t, []records.Plugin{emitter{}})
	err := e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		e.mu.Lock()
		e.now = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // the event's stamp only
		e.mu.Unlock()
		return e.store.Emit(ctx, emitter{}, records.Event{Kind: "emitter.ping", Scope: scope})
	})
	if err == nil {
		t.Fatal("a change with an unencodable event was kept")
	} else if !strings.Contains(err.Error(), "json:") || !strings.Contains(err.Error(), "year outside of range") {
		t.Fatalf("the change failed for another reason: %v", err)
	}
	e.mu.Lock()
	e.now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	e.mu.Unlock()
	if p := e.pending(t); len(p) != 0 {
		t.Fatalf("the failed change queued %+v", p)
	}
}

// TestUnsendableEndpointIsRecorded: an endpoint row whose URL or secret
// was corrupted behind the plugin cannot be sent to; each attempt records
// why on the delivery, and Deliver itself does not fail.
func TestUnsendableEndpointIsRecorded(t *testing.T) {
	for _, c := range []struct{ name, col, value string }{
		{"url", webhooks.ColEndpointURL, "http://%zz"},
		{"secret", webhooks.ColEndpointSecret, "whsec_!!!"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.file(t, scope, "a")
			testdb.Exec(t, e.db, "UPDATE "+webhooks.TableNameEndpoints+" SET "+c.col+" = '"+c.value+"'")
			if n, err := e.p.Deliver(t.Context(), e.store, e.clock()); err != nil || n != 0 {
				t.Fatalf("Deliver = %d, %v", n, err)
			}
			p := e.pending(t)
			if len(p) != 1 || p[0].Attempts != 1 || p[0].LastError == "" {
				t.Fatalf("pending after the failed attempt = %+v", p)
			}
		})
	}
}

// TestFaultSweep fails every database step of adding an endpoint, of a
// change that queues a delivery and of a delivery run in turn: each
// failure is the injected fault, returned, and the run after the sweep
// succeeds.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, nil, nil)
	key := 0
	ops := []struct {
		name string
		run  func() error
	}{
		{"add endpoint", func() error {
			_, err := e.p.AddEndpoint(ctx, e.store, "other", "https://hooks.example.com/x")
			return err
		}},
		{"queue", func() error {
			key++
			k := "k" + string(rune('a'+key))
			if _, err := e.disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			_, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: k})
			return err
		}},
		{"deliver", func() error {
			n, err := e.p.Deliver(ctx, e.store, e.clock().Add(time.Hour))
			if err == nil && n != 1 {
				t.Fatalf("delivered %d", n)
			}
			return err
		}},
		{"add global endpoint", func() error {
			_, err := e.p.AddGlobalEndpoint(ctx, e.store, "https://hooks.example.com/all")
			return err
		}},
		{"rotate secret", func() error {
			_, err := e.p.RotateSecret(ctx, e.store, webhooks.EndpointRef{Scope: scope, ID: e.ep.ID}, "")
			return err
		}},
		{"retire secrets", func() error {
			_, err := e.p.RetireSecrets(ctx, e.store, webhooks.EndpointRef{Scope: scope, ID: e.ep.ID})
			return err
		}},
		{"retire a single secret", func() error {
			_, err := e.p.RetireSecrets(ctx, e.store, webhooks.EndpointRef{Scope: scope, ID: e.ep.ID})
			return err
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				faults.Arm(n)
				err := op.run()
				faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
			}
		})
	}
}

// TestSecretEdges pins the rotation's refusals: a malformed next secret,
// an endpoint whose stored secrets are empty, and the StandardSigner's
// fallback to Endpoint.Secret for an endpoint built by hand.
func TestSecretEdges(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	ref := webhooks.EndpointRef{Scope: scope, ID: e.ep.ID}
	if _, err := e.p.RotateSecret(ctx, e.store, ref, "not-a-secret"); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("malformed next secret = %v", err)
	}
	testdb.Exec(t, e.db, "UPDATE "+webhooks.EndpointTable.Name+" SET "+webhooks.ColEndpointSecret+" = '' WHERE "+webhooks.ColEndpointID+" = '"+e.ep.ID+"'")
	if _, err := e.p.RotateSecret(ctx, e.store, ref, ""); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("rotating an endpoint without secrets = %v", err)
	}

	h := http.Header{}
	err := webhooks.StandardSigner{}.Sign(ctx, webhooks.SignRequest{Header: h, Endpoint: webhooks.Endpoint{Secret: webhooks.NewSecret()}, MessageID: "msg_1", At: time.Unix(1, 0), Body: []byte("{}")})
	if err != nil || !strings.HasPrefix(h.Get("Webhook-Signature"), "v1,") {
		t.Errorf("signing with Endpoint.Secret only: %v, %v", err, h)
	}
}

// TestConcurrentDeliverFaults: with WithConcurrency, a database failure in
// any attempt stops new attempts, lets those under way finish, and
// returns the failure; the sweep's last run delivers what is left, and
// nothing stays due.
func TestConcurrentDeliverFaults(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, []webhooks.Option{webhooks.WithConcurrency(2)}, nil)
	for i := range 5 {
		k := "c" + string(rune('a'+i))
		if _, err := e.disk.WriteBytes(ctx, k, []byte("x"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: k}); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for n := 1; ; n++ {
		faults.Arm(n)
		got, err := e.p.Deliver(ctx, e.store, e.clock().Add(time.Hour))
		faults.Arm(0)
		total += got
		if err == nil {
			break
		}
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("step %d: %v", n, err)
		}
	}
	// At least once: an attempt whose send succeeded but whose bookkeeping
	// failed is sent again, so the sweep may count one twice; none is lost.
	if total < 5 {
		t.Fatalf("delivered %d of 5 across the sweep", total)
	}
	if again, err := e.p.Deliver(ctx, e.store, e.clock().Add(2*time.Hour)); err != nil || again != 0 {
		t.Fatalf("left after the sweep: %d, %v", again, err)
	}
}
