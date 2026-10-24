package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/scan"
	"github.com/ubgo/filesystem/upload"
)

// Secrets of test signing keys, each signedurl.MinKeyBytes or longer.
const (
	oldSecret = "old-secret-old-secret-old-secret-0"
	newSecret = "new-secret-new-secret-new-secret-0"
)

// env is a test environment: a map in place of os.LookupEnv.
func env(m map[string]string) filesystem.LookupFunc {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// baseEnv is a working configuration on local and memory disks.
func baseEnv(t *testing.T) map[string]string {
	dir := t.TempDir()
	return map[string]string{
		"STORAGE_DISKS":        "media,backups",
		"STORAGE_MEDIA_DSN":    "mem://",
		"STORAGE_BACKUPS_DSN":  "file://" + filepath.Join(dir, "backups"),
		"STORAGE_DEFAULT_DISK": "media",
		envFilesURL:            "https://files.example-usercontent.com/files",
		envSigningKeys:         "k1:" + oldSecret,
		envDatabase:            filepath.Join(dir, "app.db"),
	}
}

// quiet is a logger that writes nowhere.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestWireFromEnv wires everything from a map, and checks a missing
// variable is named in the error.
func TestWireFromEnv(t *testing.T) {
	m := baseEnv(t)
	w, err := wire(t.Context(), env(m), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if names := w.disks.Names(); len(names) != 2 {
		t.Errorf("disks = %v", names)
	}
	_ = w.Close()

	for _, unset := range []string{"STORAGE_BACKUPS_DSN", envSigningKeys, envFilesURL, envDatabase} {
		m := baseEnv(t)
		delete(m, unset)
		if _, err := wire(t.Context(), env(m), quiet()); err == nil || !strings.Contains(err.Error(), unset) {
			t.Errorf("without %s: %v", unset, err)
		}
	}
}

// TestKeyRotation signs with the old key, rotates in a new one, and checks
// old links still verify until the old key is removed.
// ds:def id=prod-keyrotation-test-dnhx53je
func TestKeyRotation(t *testing.T) {
	before, err := newSigner(env(map[string]string{envFilesURL: "https://f.example/files", envSigningKeys: "k1:" + oldSecret}))
	if err != nil {
		t.Fatal(err)
	}
	link, err := before.SignURL(t.Context(), "media", "a.png", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	during, err := newSigner(env(map[string]string{envFilesURL: "https://f.example/files", envSigningKeys: "k2:" + newSecret + ",k1:" + oldSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := during.Verify("media", "a.png", filesystem.MethodGet, u.Query()); err != nil {
		t.Errorf("old link during rotation: %v", err)
	}
	after, err := newSigner(env(map[string]string{envFilesURL: "https://f.example/files", envSigningKeys: "k2:" + newSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.Verify("media", "a.png", filesystem.MethodGet, u.Query()); !errors.Is(err, filesystem.ErrPermission) {
		t.Errorf("old link after rotation: %v", err)
	}
	if _, err := parseSigningKeys("no-separator"); err == nil {
		t.Error("malformed key list accepted")
	}
}

// TestInterceptorChain checks every operation is observed and logged.
func TestInterceptorChain(t *testing.T) {
	var logs bytes.Buffer
	var events []layer.Event
	chain := interceptors(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), func(_ context.Context, e layer.Event) { events = append(events, e) })
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(chain...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Op.Kind != filesystem.OpWrite || !strings.Contains(logs.String(), "op=write") {
		t.Errorf("events %+v, logs %s", events, logs.String())
	}
}

// TestMigrateCommands runs migrate twice and prints the SQL.
func TestMigrateCommands(t *testing.T) {
	m := env(baseEnv(t))
	var out strings.Builder
	for range 2 {
		if err := run(t.Context(), "migrate", m, quiet(), &out); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "applied records/") || !strings.HasSuffix(out.String(), "schema is up to date\n") {
		t.Errorf("migrate printed %q", out.String())
	}
	out.Reset()
	if err := run(t.Context(), "migrate-sql", m, quiet(), &out); err != nil || !strings.Contains(out.String(), "CREATE TABLE") {
		t.Errorf("migrate-sql: %v %q", err, out.String())
	}
}

// TestMaintain checks each step of the scheduled clean-up.
func TestMaintain(t *testing.T) {
	w, err := wire(t.Context(), env(baseEnv(t)), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	f, err := w.store.Upload(t.Context(), "acme", mediaDisk, strings.NewReader("%PDF-1.7\n"), "a.pdf", uploadPolicy(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.Trash(t.Context(), "acme", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.media.WriteBytes(t.Context(), incomingDir+"/x.bin", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	later := time.Now().Add(auditRetention + time.Hour)
	if err := maintain(t.Context(), w.store, w.audit, w.media, later, &out); err != nil {
		t.Fatal(err)
	}
	// Three audit entries, all older than a year at "later": the upload,
	// the trash, and the purge the sweep itself just logged.
	if out.String() != "purged 1 trashed files, swept 1 abandoned uploads, pruned 3 audit entries\n" {
		t.Errorf("maintain printed %q", out.String())
	}
}

// TestScanning plugs a fake scanner into the policy: an infected upload
// is refused and deleted, a scanner that is down refuses too.
func TestScanning(t *testing.T) {
	infected := scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) {
		return scan.Verdict{Signature: "Win.Test.EICAR_HDB-1"}, nil
	})
	down := scan.ScannerFunc(func(context.Context, io.Reader) (scan.Verdict, error) {
		return scan.Verdict{}, errors.New("connection refused")
	})
	for name, s := range map[string]scan.Scanner{"infected": infected, "down": down} {
		disk, err := filesystem.NewDisk("media", memory.New())
		if err != nil {
			t.Fatal(err)
		}
		_, err = upload.Receive(t.Context(), disk, strings.NewReader("%PDF-1.7\n"), "a.pdf", uploadPolicy(s))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if page, _ := disk.ListPage(t.Context(), "", filesystem.ListOptions{Recursive: true}); len(page.Entries) != 0 {
			t.Errorf("%s: left %v", name, page.Entries)
		}
	}
	if scannerFrom(env(map[string]string{envClamd: "127.0.0.1:3310"})) == nil || scannerFrom(env(nil)) != nil {
		t.Error("scannerFrom")
	}
}

// TestHealth answers 200 on working disks and 503 on a read-only one,
// which Check cannot prove.
func TestHealth(t *testing.T) {
	for dsn, want := range map[string]int{"mem://": http.StatusOK, "mem://?readonly=true": http.StatusServiceUnavailable} {
		mux, err := newMux()
		if err != nil {
			t.Fatal(err)
		}
		disk, err := mux.Open(t.Context(), "media", dsn)
		if err != nil {
			t.Fatal(err)
		}
		disks, err := filesystem.NewManager(disk)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		health(disks, quiet()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != want {
			t.Errorf("%s: %d", dsn, rec.Code)
		}
	}
}

// TestMirror writes through the mirror and finds the bytes on both disks.
func TestMirror(t *testing.T) {
	w, err := wire(t.Context(), env(baseEnv(t)), quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	m, err := mirrored(w.disks, "media-mirrored", "media", "backups")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.WriteBytes(t.Context(), "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"media", "backups"} {
		d, _ := w.disks.Disk(name)
		if ok, err := d.Exists(t.Context(), "a.txt"); !ok || err != nil {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
	if _, err := mirrored(w.disks, "x", "media", "nope"); err == nil {
		t.Error("unknown backup disk accepted")
	}
}

// TestLimits checks the plan lookup.
func TestLimits(t *testing.T) {
	limits := limitsFor(func(_ context.Context, scope string) (string, error) { return scope, nil })
	if l, err := limits(t.Context(), planPro); err != nil || l.Bytes != planProBytes || l.Files != 0 {
		t.Errorf("pro = %+v, %v", l, err)
	}
	if _, err := limits(t.Context(), "gold"); err == nil {
		t.Error("unknown plan accepted")
	}
}
