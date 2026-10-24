package uploadsessions_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/uploadsessions"
	"github.com/ubgo/filesystem/upload"
)

const pngData = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-png-body"

var images = upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}}

type env struct {
	db     *sql.DB
	store  *records.Store
	faults *testdb.Faults
	ss     uploadsessions.Store
	disk   *filesystem.Disk
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	db, faults := testdb.OpenFaulty(t)
	e.db, e.faults = db, faults
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	p := uploadsessions.New()
	var err error
	if e.store, err = records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.ss = p.Store(e.store)
	return e
}

// TestStoreRoundTrip: a session reads back as saved, an ID is unique,
// delete is idempotent, and Sweep removes exactly the expired sessions.
func TestStoreRoundTrip(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	in := upload.Session{ID: "ups_a", SecretHash: strings.Repeat("ab", 32), Prefix: "users/1", MaxSize: 99, Allowed: []string{"image/png", "image/gif"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := e.ss.SaveSession(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := e.ss.LoadSession(ctx, "ups_a")
	if err != nil || got.ID != in.ID || got.SecretHash != in.SecretHash || got.Prefix != in.Prefix || got.Key != "" || got.MaxSize != 99 ||
		strings.Join(got.Allowed, ",") != "image/png,image/gif" || !got.CreatedAt.Equal(now) || !got.ExpiresAt.Equal(in.ExpiresAt) || got.ExpiresAt.Location() != time.UTC {
		t.Fatalf("LoadSession = %+v, %v", got, err)
	}
	if err := e.ss.SaveSession(ctx, in); filesystem.CodeOf(err) != filesystem.CodeExists {
		t.Fatalf("a duplicate ID = %v", err)
	}
	if _, err := e.ss.LoadSession(ctx, "ups_none"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("an unknown ID = %v", err)
	}
	keyed := upload.Session{ID: "ups_b", Key: "a.png", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	if err := e.ss.SaveSession(ctx, keyed); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ss.LoadSession(ctx, "ups_b"); got.Key != "a.png" || got.Allowed != nil {
		t.Fatalf("keyed session = %+v", got)
	}
	if n, err := e.ss.SweepSessions(ctx, now.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if _, err := e.ss.LoadSession(ctx, "ups_b"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("the expired session was kept: %v", err)
	}
	for range 2 {
		if err := e.ss.DeleteSession(ctx, "ups_a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.ss.LoadSession(ctx, "ups_a"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("a deleted session: %v", err)
	}
}

// TestCorruptRow: a row whose types cannot be read is an error, never a
// session without type limits.
func TestCorruptRow(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	now := time.Now()
	if err := e.ss.SaveSession(ctx, upload.Session{ID: "ups_c", Prefix: "p", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	testdb.Exec(t, e.db, "UPDATE "+uploadsessions.TableNameSessions+" SET "+uploadsessions.ColAllowed+" = 'not json' WHERE "+uploadsessions.ColID+" = 'ups_c'")
	if _, err := e.ss.LoadSession(ctx, "ups_c"); filesystem.CodeOf(err) != filesystem.CodeUnknown {
		t.Fatalf("a corrupt row = %v", err)
	}
}

// TestWithSessions runs upload.Sessions over the store end to end: an
// issued token uploads through a handler, a revoked one is refused, and
// Sweep clears the expired ones.
func TestWithSessions(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	m, err := upload.NewSessions(e.ss, upload.SessionsOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	is, err := m.Issue(ctx, images, upload.SessionRequest{Prefix: "inbox", MaxSize: 1 << 10, Allowed: []string{"image/png"}, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := upload.Handler(e.disk, images, upload.HandlerOptions{Resolve: m.Resolver()})
	send := func(token string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		w, _ := mw.CreateFormFile(upload.DefaultField, "a.png")
		_, _ = io.WriteString(w, pngData)
		_ = mw.Close()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set(upload.HeaderUploadSession, token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := send(is.Token)
	var resp upload.Response
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || !strings.HasPrefix(resp.Files[0].Key, "inbox/") {
		t.Fatalf("upload with a session: %d %s", rec.Code, rec.Body)
	}
	now = now.Add(time.Hour)
	if rec := send(is.Token); rec.Code != http.StatusForbidden {
		t.Fatalf("an expired session (unswept): %d", rec.Code)
	}
	if n, err := m.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	is2, err := m.Issue(ctx, images, upload.SessionRequest{Key: "one.png", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(ctx, is2.ID); err != nil {
		t.Fatal(err)
	}
	if rec := send(is2.Token); rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked session: %d", rec.Code)
	}
}

// TestFaults: every database failure of the store is returned.
func TestFaults(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	now := time.Now()
	ops := map[string]func() error{
		"save": func() error {
			return e.ss.SaveSession(ctx, upload.Session{ID: "ups_" + now.Format("150405.000000000"), Prefix: "p", CreatedAt: now, ExpiresAt: now})
		},
		"load":   func() error { _, err := e.ss.LoadSession(ctx, "ups_x"); return err },
		"delete": func() error { return e.ss.DeleteSession(ctx, "ups_x") },
		"sweep":  func() error { _, err := e.ss.SweepSessions(ctx, now); return err },
	}
	for name, op := range ops {
		e.faults.Arm(1)
		err := op()
		e.faults.Arm(0)
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("%s with a failing database = %v", name, err)
		}
	}
}
