package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/webhooks"
	"github.com/ubgo/sqlb"
)

// Decisions of the second half, not the library's.
const (
	// otherTenant is a second scope, to show a global endpoint takes the
	// events of every scope.
	otherTenant = "globex"
	// headerHub is the GitHub-style signature header githubSigner adds.
	headerHub = "X-Hub-Signature-256"
	// hubPrefix starts headerHub's value.
	hubPrefix = "sha256="
)

// hubKey keys the GitHub-style signature. A real app keeps it in a secret
// manager (or never sees it, with a KMS-backed Signer).
var hubKey = []byte("hub-signing-key-kept-in-a-vault")

// githubSigner signs every delivery twice: the Standard Webhooks headers,
// with every active secret, and a GitHub-style X-Hub-Signature-256 for
// receivers written against GitHub's format. An error from either fails
// the attempt, which is retried: nothing is sent unsigned.
// ds:def id=githubsigner-w8r3k6pz
type githubSigner struct{ key []byte }

// Sign implements webhooks.Signer.
func (g githubSigner) Sign(ctx context.Context, req webhooks.SignRequest) error {
	if err := (webhooks.StandardSigner{}).Sign(ctx, req); err != nil {
		return err
	}
	m := hmac.New(sha256.New, g.key)
	m.Write(req.Body)
	req.Header.Set(headerHub, hubPrefix+hex.EncodeToString(m.Sum(nil)))
	return nil
}

// cloudEvent wraps the default body in a CloudEvents 1.0 envelope, for
// receivers that route CloudEvents (an event bus, a Knative trigger).
// ds:def id=cloudevent-c4n9t2xa
func cloudEvent(_ context.Context, e records.Event) (any, error) {
	return map[string]any{
		"specversion": "1.0", "id": e.ID, "type": string(e.Kind), "source": "/scopes/" + e.Scope,
		"time": e.At, "data": webhooks.DefaultPayload(e),
	}, nil
}

// newModelStore installs webhooks with the signer and the payload of the
// app's own, beside the demo's client (loopback receivers).
// ds:def id=newmodelstore-h6q2v8tm
func newModelStore(ctx context.Context, db *sql.DB, client *http.Client) (*records.Store, *webhooks.Plugin, error) {
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
		webhooks.WithSigner(githubSigner{key: hubKey}),
		webhooks.WithPayload(cloudEvent),
		webhooks.WithLogger(slog.New(slog.DiscardHandler)),
	)
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(wh))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, wh, nil
}

// delivery is one request a recorder received.
type delivery struct {
	path   string
	header http.Header
	body   []byte
}

// recorder answers 204 to every delivery and keeps it.
type recorder struct {
	mu  sync.Mutex
	got []delivery
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	rc.mu.Lock()
	rc.got = append(rc.got, delivery{path: r.URL.Path, header: r.Header.Clone(), body: body})
	rc.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// take returns the deliveries received since the last take.
func (rc *recorder) take() []delivery {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	got := rc.got
	rc.got = nil
	return got
}

// hubValid checks a delivery's X-Hub-Signature-256 as a GitHub-style
// receiver would, in constant time.
func hubValid(d delivery) bool {
	m := hmac.New(sha256.New, hubKey)
	m.Write(d.body)
	return hmac.Equal([]byte(d.header.Get(headerHub)), []byte(hubPrefix+hex.EncodeToString(m.Sum(nil))))
}

// models runs a global endpoint, a rotation, the CloudEvents body and the
// second signature on a Store and database of their own.
func models(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "models.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	rc := &recorder{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	s, wh, err := newModelStore(ctx, db, srv.Client())
	if err != nil {
		return err
	}
	if _, err := wh.AddGlobalEndpoint(ctx, s, srv.URL+"/indexer", records.EventFileCreated); err != nil {
		return err
	}
	portal, err := wh.AddEndpoint(ctx, s, tenant, srv.URL+"/portal", records.EventFileCreated)
	if err != nil {
		return err
	}
	if err := globalDemo(ctx, w, s, wh, rc); err != nil {
		return err
	}
	return rotateDemo(ctx, w, s, wh, rc, portal)
}

// globalDemo records one file in each of two scopes: the global indexer
// gets both, the tenant's portal only its own.
func globalDemo(ctx context.Context, w io.Writer, s *records.Store, wh *webhooks.Plugin, rc *recorder) error {
	for _, scope := range []string{tenant, otherTenant} {
		if _, err := recordIn(ctx, s, scope, scope+"/a.txt", "hello"); err != nil {
			return err
		}
	}
	if _, err := wh.Deliver(ctx, s, time.Now()); err != nil {
		return err
	}
	var indexer, portal []string
	var first delivery
	for _, d := range rc.take() {
		var ev struct {
			SpecVersion string `json:"specversion"`
			Type        string `json:"type"`
			Source      string `json:"source"`
		}
		if err := json.Unmarshal(d.body, &ev); err != nil {
			return err
		}
		line := ev.Type + " " + ev.Source
		if d.path == "/indexer" {
			indexer = append(indexer, line)
		} else {
			portal = append(portal, line)
		}
		first = d
	}
	slices.Sort(indexer) // one Deliver run may send in any order
	listed, err := wh.Endpoints(ctx, s, tenant)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "global indexer got %d: %s\n", len(indexer), strings.Join(indexer, ", "))
	fmt.Fprintf(w, "acme's portal got %d: %s; Endpoints(acme) lists %d\n", len(portal), strings.Join(portal, ", "), len(listed))
	fmt.Fprintf(w, "body is a CloudEvent: %t; X-Hub-Signature-256 valid: %t\n", strings.Contains(string(first.body), `"specversion":"1.0"`), hubValid(first))
	return nil
}

// rotateDemo rotates the portal's secret: deliveries carry a signature
// for each active secret until RetireSecrets drops the old one.
// ds:def id=rotatedemo-b2m7x4kc
func rotateDemo(ctx context.Context, w io.Writer, s *records.Store, wh *webhooks.Plugin, rc *recorder, portal webhooks.Endpoint) error {
	old := portal.Secret
	ref := webhooks.EndpointRef{Scope: tenant, ID: portal.ID}
	rotated, err := wh.RotateSecret(ctx, s, ref, "")
	if err != nil {
		return err
	}
	check := func(step string) error {
		if _, err := recordIn(ctx, s, tenant, tenant+"/"+strings.ReplaceAll(step, " ", "-")+".txt", step); err != nil {
			return err
		}
		if _, err := wh.Deliver(ctx, s, time.Now()); err != nil {
			return err
		}
		for _, d := range rc.take() {
			if d.path != "/portal" {
				continue
			}
			sigs := len(strings.Fields(d.header.Get(webhooks.HeaderSignature)))
			oldOK := webhooks.VerifySignature(d.header, old, d.body, time.Now(), 0) == nil
			newOK := webhooks.VerifySignature(d.header, rotated.Secret, d.body, time.Now(), 0) == nil
			fmt.Fprintf(w, "%s: %d signature(s); old secret verifies %t, new secret verifies %t\n", step, sigs, oldOK, newOK)
		}
		return nil
	}
	if err := check("rotating"); err != nil {
		return err
	}
	if _, err := wh.RetireSecrets(ctx, s, ref); err != nil {
		return err
	}
	return check("retired")
}

// recordIn stores body and records it as a file of scope.
func recordIn(ctx context.Context, s *records.Store, scope, key, body string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, strings.NewReader(body), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskName, Key: key})
}
