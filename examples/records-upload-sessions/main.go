// Command records-upload-sessions shows upload sessions kept in the
// database by the records/uploadsessions plugin: the app's API issues a
// token for one key prefix with limits no wider than the endpoint's
// policy, a client that holds only the token uploads with it, uploads
// over the session's size or outside its types are refused, a second
// Sessions over the same database (another instance, or the app after a
// restart) redeems the same token, and revoked or expired tokens get the
// one 403 answer. SQLite and an in-memory disk; a fixed clock moves the
// demo past the expiry.
//
//	go run ./records-upload-sessions
//
// It prints what each step returned; main_test.go runs the same steps.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"mime/multipart"
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
	"github.com/ubgo/filesystem/records/uploadsessions"
	"github.com/ubgo/filesystem/upload"
	"github.com/ubgo/sqlb"
)

// Decisions of this demo app, not of the library.
const (
	diskName = "uploads"
	// sessionTTL is how long an issued token works.
	sessionTTL = 15 * time.Minute
	// sessionMax is the size cap of one session, below the endpoint's.
	sessionMax = 64
)

// endpoint is the policy of the upload endpoint tokens are redeemed at.
// A session can only narrow it.
// ds:def id=endpointpolicy-m3w9t5kc
var endpoint = upload.Policy{
	MaxSize: 1 << 20,
	Allowed: []string{"text/plain", "image/png", "image/jpeg"},
	Key:     upload.RandomKey(""),
}

// clock is the Sessions clock, moved by the demo past the expiry.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

// newStore installs the uploadsessions plugin; Migrate creates
// fs_upload_sessions.
// ds:def id=newsessionstore-k8c2v6nw
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *uploadsessions.Plugin, error) {
	p := uploadsessions.New()
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(p))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, p, nil
}

// newSessions is upload.Sessions over the database: every instance that
// builds it over the same Store redeems the same tokens. The clock judges
// expiry; the store's word is never trusted for it.
// ds:def id=newsessions-r5t8q2hx
func newSessions(s *records.Store, p *uploadsessions.Plugin, now func() time.Time) (*upload.Sessions, error) {
	return upload.NewSessions(p.Store(s), upload.SessionsOptions{Now: now})
}

// newUploadHandler is the endpoint a token is redeemed at: every request
// must carry a live session in the Upload-Session header, and is stored
// under the session's narrowed policy.
// ds:def id=newuploadhandler-w2n7c4tq
func newUploadHandler(disk *filesystem.Disk, sessions *upload.Sessions) http.Handler {
	return upload.Handler(disk, endpoint, upload.HandlerOptions{Resolve: sessions.Resolver()})
}

// issueFor is what the app's authenticated API does: a token for one
// user's prefix, at most sessionMax bytes of plain text, for sessionTTL.
// Show the token to the client once; only its SHA-256 is stored.
// ds:def id=issuefor-q6h3m9zp
func issueFor(ctx context.Context, sessions *upload.Sessions, userID string) (upload.IssuedSession, error) {
	return sessions.Issue(ctx, endpoint, upload.SessionRequest{
		Prefix:  "users/" + userID,
		MaxSize: sessionMax,
		Allowed: []string{"text/plain"},
		TTL:     sessionTTL,
	})
}

// send posts one file to the handler with the token, as the client does.
// ds:def id=send-c7w4k9rm
func send(h http.Handler, token, name, body string) (int, string, error) {
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, err := mw.CreateFormFile(upload.DefaultField, name)
	if err != nil {
		return 0, "", err
	}
	if _, err := io.WriteString(part, body); err != nil {
		return 0, "", err
	}
	if err := mw.Close(); err != nil {
		return 0, "", err
	}
	req := httptest.NewRequest(http.MethodPost, "/upload", &form)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set(upload.HeaderUploadSession, token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), nil
}

// run performs every step and prints what it got.
func run(ctx context.Context, w io.Writer, dir string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "records.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return err
	}
	s, p, err := newStore(ctx, db, disks)
	if err != nil {
		return err
	}
	c := &clock{now: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}
	sessions, err := newSessions(s, p, c.Now)
	if err != nil {
		return err
	}
	is, err := issueFor(ctx, sessions, "alice")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "issued %s… for prefix %s until %s\n", is.ID[:4], is.Prefix, is.ExpiresAt.Format("15:04"))
	h := newUploadHandler(disk, sessions)
	steps := []struct{ label, token, name, body string }{
		{"with the token", is.Token, "note.txt", "hello"},
		{"no token", "", "note.txt", "hello"},
		{"forged secret", is.ID + ".AAAA", "note.txt", "hello"},
		{"over the session's 64 bytes", is.Token, "big.txt", strings.Repeat("x", 100)},
		{"a PNG the endpoint allows", is.Token, "pic.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"},
	}
	for _, st := range steps {
		code, body, err := send(h, st.token, st.name, st.body)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%-30s %d%s\n", st.label+":", code, keyOf(body))
	}
	other, err := newSessions(s, p, c.Now)
	if err != nil {
		return err
	}
	code, body, err := send(newUploadHandler(disk, other), is.Token, "again.txt", "hi")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%-30s %d%s\n", "another instance:", code, keyOf(body))
	c.Add(sessionTTL)
	code, _, err = send(h, is.Token, "late.txt", "hi")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%-30s %d\n", "after the expiry:", code)
	n, err := sessions.Sweep(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sweep deleted %d expired session(s)\n", n)
	second, err := issueFor(ctx, sessions, "bob")
	if err != nil {
		return err
	}
	if err := sessions.Revoke(ctx, second.ID); err != nil {
		return err
	}
	code, _, err = send(h, second.Token, "note.txt", "hi")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%-30s %d\n", "revoked:", code)
	_, err = sessions.Issue(ctx, endpoint, upload.SessionRequest{Prefix: "users/eve", MaxSize: 2 << 20, TTL: sessionTTL})
	fmt.Fprintf(w, "issue wider than the endpoint: %s\n", filesystem.CodeOf(err))
	return nil
}

// keyOf picks the stored key's prefix out of a handler's JSON answer, for
// the printout: " users/alice/…" or "".
func keyOf(body string) string {
	i := strings.Index(body, `"key":"`)
	if i < 0 {
		return ""
	}
	key := body[i+len(`"key":"`):]
	key = key[:strings.IndexByte(key, '"')]
	if j := strings.LastIndexByte(key, '/'); j >= 0 {
		return " " + key[:j+1] + "…"
	}
	return " " + key
}

func main() {
	dir, err := os.MkdirTemp("", "records-upload-sessions")
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := run(context.Background(), os.Stdout, dir); err != nil {
		log.Fatal(err)
	}
}
