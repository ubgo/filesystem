package webhooks_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/webhooks"
)

const scope = "t"

// receiver is an endpoint that checks every signature and keeps what it
// accepted; fail makes it answer 500.
type receiver struct {
	mu     sync.Mutex
	secret webhooks.Secret
	ids    []string
	bodies []webhooks.Payload
	fail   bool
	now    func() time.Time
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	if err := webhooks.VerifySignature(r.Header, rc.secret, body, rc.now(), 0); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	rc.ids = append(rc.ids, r.Header.Get(webhooks.HeaderID))
	if rc.fail {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	var pl webhooks.Payload
	_ = json.Unmarshal(body, &pl)
	rc.bodies = append(rc.bodies, pl)
}

type env struct {
	mu    sync.Mutex // guards now, read by the Store and the test server
	db    *sql.DB
	store *records.Store
	disk  *filesystem.Disk
	p     *webhooks.Plugin
	now   time.Time
	rc    *receiver
	ep    webhooks.Endpoint
}

// newEnv installs the plugin with a client for a local test server (the
// documented escape hatch) and registers that server for kinds.
func newEnv(t *testing.T, more []records.Plugin, kinds ...records.EventKind) *env {
	t.Helper()
	return newEnvWith(t, nil, more, kinds...)
}

// newEnvWith is newEnv with extra plugin options, applied after the
// defaults so they win.
func newEnvWith(t *testing.T, opts []webhooks.Option, more []records.Plugin, kinds ...records.EventKind) *env {
	t.Helper()
	return newEnvOn(t, testdb.Open(t), opts, more, kinds...)
}

// newEnvOn is newEnvWith on a given database, such as one from
// testdb.OpenFaulty.
func newEnvOn(t *testing.T, db *sql.DB, opts []webhooks.Option, more []records.Plugin, kinds ...records.EventKind) *env {
	t.Helper()
	e := &env{db: db, now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	e.rc = &receiver{now: e.clock}
	srv := httptest.NewServer(e.rc)
	t.Cleanup(srv.Close)
	var err error
	all := append([]webhooks.Option{webhooks.WithClient(srv.Client()), webhooks.WithMaxAttempts(3), webhooks.WithLogger(slog.New(slog.DiscardHandler))}, opts...)
	if e.p, err = webhooks.New(all...); err != nil {
		t.Fatal(err)
	}
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	e.store, err = records.New(db, testdb.Dialect(), mgr, records.WithPlugins(append([]records.Plugin{e.p}, more...)...),
		records.WithClock(e.clock))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.ep, err = e.p.AddEndpoint(t.Context(), e.store, scope, srv.URL+"/hook", kinds...); err != nil {
		t.Fatal(err)
	}
	e.rc.secret = e.ep.Secret
	return e
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func (e *env) file(t *testing.T, sc, key string) records.File {
	t.Helper()
	if _, err := e.disk.WriteBytes(t.Context(), key, []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: sc, Disk: "media", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *env) pending(t *testing.T) []webhooks.Delivery {
	t.Helper()
	d, err := e.p.Deliveries(t.Context(), e.store, scope, webhooks.StatePending)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDeliversSignedEventsOfItsScopeAndKinds(t *testing.T) {
	e := newEnv(t, nil, records.EventFileTrashed)
	f := e.file(t, scope, "a")
	e.file(t, "other", "b")
	if len(e.pending(t)) != 0 {
		t.Fatal("a file.created was queued for an endpoint that wants file.trashed only")
	}
	if err := e.store.Trash(t.Context(), scope, f.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := e.p.Deliver(t.Context(), e.store, e.now); err != nil || n != 1 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	if len(e.rc.bodies) != 1 {
		t.Fatalf("received %d", len(e.rc.bodies))
	}
	pl := e.rc.bodies[0]
	if pl.Kind != string(records.EventFileTrashed) || pl.FileID != f.ID || pl.File == nil || pl.File.Size != 1 || pl.ID == "" || pl.ID != e.rc.ids[0] {
		t.Fatalf("payload = %+v, id header %q", pl, e.rc.ids[0])
	}
	if n, _ := e.p.Deliver(t.Context(), e.store, e.now.Add(time.Hour)); n != 0 {
		t.Fatal("a delivered event was sent again")
	}
}

// failAll refuses every change after the webhooks plugin queued it.
type failAll struct{}

func (failAll) Name() string { return "fail_all" }
func (failAll) OnEvent(context.Context, *records.Store, records.Event) error {
	return errors.New("refused")
}

func TestRolledBackChangeSendsNothing(t *testing.T) {
	e := newEnv(t, []records.Plugin{failAll{}})
	if _, err := e.store.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "a"}); err == nil {
		t.Fatal("the change committed")
	}
	if d := e.pending(t); len(d) != 0 {
		t.Fatalf("a rolled-back change queued %+v", d)
	}
}

func TestRetriesWithBackoffThenFails(t *testing.T) {
	e := newEnv(t, nil)
	e.rc.fail = true
	e.file(t, scope, "a")
	if n, err := e.p.Deliver(t.Context(), e.store, e.now); err != nil || n != 0 {
		t.Fatalf("Deliver = %d, %v", n, err)
	}
	d := e.pending(t)
	if len(d) != 1 || d[0].Attempts != 1 || !d[0].NextAt.Equal(e.now.Add(webhooks.DefaultBackoff(1))) || !strings.Contains(d[0].LastError, "500") {
		t.Fatalf("after one failure = %+v", d)
	}
	// Not due again before its backoff.
	e.p.Deliver(t.Context(), e.store, e.now.Add(time.Second))
	if len(e.rc.ids) != 1 {
		t.Fatalf("retried before the backoff: %d attempts", len(e.rc.ids))
	}
	at := e.now
	for range 2 {
		at = at.Add(webhooks.DefaultBackoff(10))
		e.now = at // the receiver's clock, which checks the timestamp
		if _, err := e.p.Deliver(t.Context(), e.store, at); err != nil {
			t.Fatal(err)
		}
	}
	failed, _ := e.p.Deliveries(t.Context(), e.store, scope, webhooks.StateFailed)
	if len(failed) != 1 || failed[0].Attempts != 3 || len(e.pending(t)) != 0 {
		t.Fatalf("after max attempts: failed %+v", failed)
	}
	// Every attempt carried the same event ID, for the receiver to dedupe.
	if len(e.rc.ids) != 3 || e.rc.ids[0] != e.rc.ids[2] {
		t.Fatalf("ids = %v", e.rc.ids)
	}
	e.file(t, scope, "b") // pending, and as old as the cutoff: kept
	if n, err := e.p.Prune(t.Context(), e.store, at.Add(time.Second)); err != nil || n != 1 || len(e.pending(t)) != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
}

// TestWithBackoff: a failed attempt waits what the custom backoff returns
// for that attempt, not DefaultBackoff.
func TestWithBackoff(t *testing.T) {
	const wait = 7 * time.Second
	var asked []int
	e := newEnvWith(t, []webhooks.Option{webhooks.WithBackoff(func(n int) time.Duration { asked = append(asked, n); return wait })}, nil)
	e.rc.fail = true
	e.file(t, scope, "a")
	if _, err := e.p.Deliver(t.Context(), e.store, e.now); err != nil {
		t.Fatal(err)
	}
	if d := e.pending(t); len(d) != 1 || !d[0].NextAt.Equal(e.now.Add(wait)) || len(asked) != 1 || asked[0] != 1 {
		t.Fatalf("after one failure = %+v, backoff asked for %v", d, asked)
	}
	if _, err := webhooks.New(webhooks.WithBackoff(nil)); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("New with a nil backoff = %v", err)
	}
}

func TestDefaultBackoff(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 4: 4 * time.Minute, 30: 6 * time.Hour} {
		if got := webhooks.DefaultBackoff(attempt); got != want {
			t.Errorf("DefaultBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestRemovedEndpointFailsItsDeliveries(t *testing.T) {
	e := newEnv(t, nil)
	e.file(t, scope, "a")
	if ok, err := e.p.RemoveEndpoint(t.Context(), e.store, scope, e.ep.ID); err != nil || !ok {
		t.Fatalf("RemoveEndpoint = %v, %v", ok, err)
	}
	e.p.Deliver(t.Context(), e.store, e.now)
	failed, _ := e.p.Deliveries(t.Context(), e.store, scope, webhooks.StateFailed)
	if len(failed) != 1 || failed[0].LastError != "endpoint removed" || len(e.rc.ids) != 0 {
		t.Fatalf("failed = %+v, sent %d", failed, len(e.rc.ids))
	}
}

// The default client never reaches an internal address: a literal one is
// refused at registration, and a name resolving to one at every attempt.
func TestSSRFGuard(t *testing.T) {
	p, err := webhooks.New(webhooks.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	s, _ := records.New(testdb.Open(t), testdb.Dialect(), mgr, records.WithPlugins(p))
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://127.0.0.1/x", "http://[::1]/x", "http://169.254.169.254/latest", "http://10.0.0.1/", "ftp://example.com/", "http://user:pw@example.com/", "/relative"} {
		if _, err := p.AddEndpoint(t.Context(), s, scope, u); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("AddEndpoint(%q) = %v", u, err)
		}
	}
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer srv.Close()
	if _, err := p.AddEndpoint(t.Context(), s, scope, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(t.Context(), "a", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(t.Context(), records.NewFile{Scope: scope, Disk: "media", Key: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deliver(t.Context(), s, s.Now()); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Deliveries(t.Context(), s, scope, webhooks.StatePending)
	if hit || len(d) != 1 || !strings.Contains(d[0].LastError, "public internet") {
		t.Fatalf("reached the local server: %v; deliveries %+v", hit, d)
	}
}

func TestSecretIsRedacted(t *testing.T) {
	e := newEnv(t, nil)
	value := e.ep.Secret.Reveal()
	if !strings.HasPrefix(value, "whsec_") || len(value) < 30 {
		t.Fatalf("secret %q", value)
	}
	listed, err := e.p.Endpoints(t.Context(), e.store, scope)
	if err != nil || len(listed) != 1 || listed[0].Secret.Reveal() != value {
		t.Fatalf("Endpoints = %v, %v", listed, err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("endpoint", "secret", listed[0].Secret, "endpoint", listed[0])
	asJSON, _ := json.Marshal(listed[0])
	for _, out := range []string{fmt.Sprint(listed[0]), fmt.Sprintf("%+v %#v %s %v", listed[0], listed[0], listed[0].Secret, listed[0].Secret), string(asJSON), logged.String()} {
		if strings.Contains(out, value) || strings.Contains(out, strings.TrimPrefix(value, "whsec_")) {
			t.Fatalf("the secret leaked: %s", out)
		}
	}
}

func TestVerifySignatureRefuses(t *testing.T) {
	secret := webhooks.Secret("whsec_" + "c2VjcmV0LWtleS1mb3ItdGVzdHM=")
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"id":"evt_1"}`)
	h := http.Header{}
	if err := webhooks.Sign(h, secret, "evt_1", now, body); err != nil {
		t.Fatal(err)
	}
	if err := webhooks.VerifySignature(h, secret, body, now, 0); err != nil {
		t.Fatalf("a valid signature: %v", err)
	}
	for name, check := range map[string]func() error{
		"body changed": func() error { return webhooks.VerifySignature(h, secret, []byte(`{"id":"evt_2"}`), now, 0) },
		"replayed":     func() error { return webhooks.VerifySignature(h, secret, body, now.Add(time.Hour), 0) },
		"other secret": func() error {
			return webhooks.VerifySignature(h, webhooks.Secret("whsec_b3RoZXI="), body, now, 0)
		},
	} {
		if err := check(); filesystem.CodeOf(err) != filesystem.CodePermission {
			t.Errorf("%s: %v", name, err)
		}
	}
}
