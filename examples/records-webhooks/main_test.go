package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/webhooks"
	"github.com/ubgo/sqlb"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"secret prints as [redacted], starts whsec_ true", "pending after the upload: 1 (file.created)",
		"deliver (first): 0 delivered", `still pending, attempts 1, last error "webhooks: endpoint answered 503 Service Unavailable"`,
		"deliver (too early): 0 delivered", "deliver (after backoff): 1 delivered",
		"delivered: 1 after 2 attempt(s); receiver got file.created report.txt",
		"global indexer got 2: file.created /scopes/acme, file.created /scopes/globex",
		"acme's portal got 1: file.created /scopes/acme; Endpoints(acme) lists 1",
		"body is a CloudEvent: true; X-Hub-Signature-256 valid: true",
		"rotating: 2 signature(s); old secret verifies true, new secret verifies true",
		"retired: 1 signature(s); old secret verifies false, new secret verifies true",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestDefaultClientRefusesLoopback pins the address guard: with the
// default client, an endpoint on a loopback address is refused when it is
// registered, so a tenant cannot aim deliveries at the server's own
// network.
func TestDefaultClientRefusesLoopback(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, _, err := newStore(t.Context(), db, time.Now, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	guarded, err := webhooks.New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.AddEndpoint(t.Context(), s, tenant, "http://127.0.0.1:9/hook"); err == nil {
		t.Fatal("a loopback endpoint was registered with the default client")
	}
}

// TestTamperedBodyRefused pins that the receiver's check refuses a body
// changed after signing.
func TestTamperedBodyRefused(t *testing.T) {
	secret := webhooks.Secret("whsec_" + strings.Repeat("A", 44))
	h := http.Header{}
	now := time.Now()
	if err := webhooks.Sign(h, secret, "evt_1", now, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := webhooks.VerifySignature(h, secret, []byte(`{"a":2}`), now, 0); err == nil {
		t.Fatal("a changed body passed the signature check")
	}
}

// failingSigner stands for a KMS that is down.
type failingSigner struct{}

func (failingSigner) Sign(context.Context, webhooks.SignRequest) error {
	return errors.New("kms unavailable")
}

// TestSignerFailureSendsNothing pins that the plugged-in Signer is the one
// used and that its failure fails the attempt: the receiver gets nothing
// unsigned, and the delivery stays pending with the error.
func TestSignerFailureSendsNothing(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	ctx := t.Context()
	wh, err := webhooks.New(webhooks.WithClient(srv.Client()), webhooks.WithSigner(failingSigner{}), webhooks.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, mustDisks(t), records.WithPlugins(wh))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := wh.AddEndpoint(ctx, s, tenant, srv.URL+"/hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := recordIn(ctx, s, tenant, "a.txt", "x"); err != nil {
		t.Fatal(err)
	}
	if n, err := wh.Deliver(ctx, s, time.Now()); err != nil || n != 0 {
		t.Fatalf("Deliver with a failing signer = %d, %v", n, err)
	}
	if got := rc.take(); len(got) != 0 {
		t.Fatalf("receiver got %d unsigned deliveries", len(got))
	}
	pending, err := wh.Deliveries(ctx, s, tenant, webhooks.StatePending)
	if err != nil || len(pending) != 1 || !strings.Contains(pending[0].LastError, "kms unavailable") {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

// mustDisks is a manager with one memory disk.
func mustDisks(t *testing.T) *filesystem.Manager {
	t.Helper()
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		t.Fatal(err)
	}
	m, err := filesystem.NewManager(disk)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
