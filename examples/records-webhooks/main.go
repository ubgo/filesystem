// Command records-webhooks shows the webhooks plugin of the records layer
// on SQLite and an in-memory disk: an endpoint registered for two event
// kinds, deliveries written in the change's own transaction (an outbox),
// a receiver that checks the Standard Webhooks signature and fails the
// first attempt, the retry after the backoff, and the delivered state. A
// receiver in the same process listens on 127.0.0.1, which the plugin's
// default client refuses, so the demo passes its own client through
// WithClient, the documented escape hatch.
//
//	go run ./records-webhooks
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/webhooks"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "media"
	tenant   = "acme"
	// retryAfter is the backoff between attempts: short and fixed for
	// the demo; the default doubles from 30 seconds to 6 hours.
	retryAfter = time.Minute
)

// The three delivery rounds run makes, named as it prints them: the first
// attempt fails, a second before the backoff has passed finds nothing due,
// and a third after it delivers.
const (
	stepFirst        = "first"
	stepTooEarly     = "too early"
	stepAfterBackoff = "after backoff"
)

// maxEventBody caps the event body the receiver reads; a real event is a
// few hundred bytes, and a receiver must never buffer an unbounded body.
const maxEventBody = 1 << 20

// clock is the Store's clock, moved by the demo past the backoff.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

// newStore installs the webhooks plugin with the demo's client and
// backoff. In production, leave WithClient out: the default client
// connects only to public internet addresses, checked after DNS on every
// connection, and follows no redirect.
// ds:def id=newwebhookstore-x6t3n9qc
func newStore(ctx context.Context, db *sql.DB, now func() time.Time, client *http.Client) (*records.Store, *webhooks.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	wh, err := webhooks.New(
		webhooks.WithClient(client),
		webhooks.WithBackoff(func(int) time.Duration { return retryAfter }),
		webhooks.WithMaxAttempts(5),
		webhooks.WithLogger(slog.New(slog.DiscardHandler)), // the demo prints the state instead
	)
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(now), records.WithPlugins(wh))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, wh, nil
}

// register adds an endpoint for files created and purged in the tenant.
// The secret is shown to the receiver's owner once; it prints, logs and
// marshals as [redacted], and only Reveal returns it.
// ds:def id=register-m4w8k2tv
func register(ctx context.Context, s *records.Store, wh *webhooks.Plugin, url string) (webhooks.Endpoint, error) {
	return wh.AddEndpoint(ctx, s, tenant, url, records.EventFileCreated, records.EventFilePurged)
}

// receiver is the other side: it checks every delivery's signature with
// the endpoint's secret, drops IDs it has handled (deliveries are at
// least once), and answers 503 to the first attempt to show a retry.
// ds:def id=receiver-q7n2c5hw
type receiver struct {
	mu     sync.Mutex
	secret webhooks.Secret
	seen   map[string]bool
	calls  int
	got    []string
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if err := webhooks.VerifySignature(r.Header, rc.secret, body, time.Now(), 0); err != nil {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	rc.calls++
	if rc.calls == 1 {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	var p webhooks.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	if !rc.seen[p.ID] {
		rc.seen[p.ID] = true
		rc.got = append(rc.got, fmt.Sprintf("%s %s", p.Kind, p.File.Filename))
	}
	w.WriteHeader(http.StatusNoContent)
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	rc := &receiver{seen: map[string]bool{}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	c := &clock{now: time.Now().UTC()}
	s, wh, err := newStore(ctx, db, c.Now, srv.Client())
	if err != nil {
		return err
	}
	ep, err := register(ctx, s, wh, srv.URL+"/hooks/files")
	if err != nil {
		return err
	}
	rc.mu.Lock()
	rc.secret = ep.Secret
	rc.mu.Unlock()
	fmt.Fprintf(w, "endpoint %s… secret prints as %v, starts whsec_ %t\n", ep.ID[:4], ep.Secret, strings.HasPrefix(ep.Secret.Reveal(), "whsec_"))
	if _, err := record(ctx, s, "report.txt", "q3 numbers"); err != nil {
		return err
	}
	pending, err := wh.Deliveries(ctx, s, tenant, webhooks.StatePending)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "pending after the upload: %d (%s)\n", len(pending), pending[0].Kind)
	for _, step := range []string{stepFirst, stepTooEarly, stepAfterBackoff} {
		if step == stepAfterBackoff {
			c.Add(retryAfter)
		}
		n, err := wh.Deliver(ctx, s, c.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "deliver (%s): %d delivered\n", step, n)
		if step == stepFirst {
			retry, err := wh.Deliveries(ctx, s, tenant, webhooks.StatePending)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "  still pending, attempts %d, last error %q\n", retry[0].Attempts, strings.ReplaceAll(retry[0].LastError, "\n", " "))
		}
	}
	done, err := wh.Deliveries(ctx, s, tenant, webhooks.StateDelivered)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "delivered: %d after %d attempt(s); receiver got %s\n", len(done), done[0].Attempts, strings.Join(rc.got, ", "))
	return models(ctx, w, dir)
}

// record stores body and records it as a file of the tenant.
func record(ctx context.Context, s *records.Store, key, body string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: tenant, Disk: diskName, Key: key})
}

func main() {
	dir, err := os.MkdirTemp("", "records-webhooks")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
