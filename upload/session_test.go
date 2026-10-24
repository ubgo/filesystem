package upload_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/upload"
)

// sessionEnv is Sessions over a MemorySessions with a clock the test
// moves, and a form handler that redeems them.
type sessionEnv struct {
	now      time.Time
	store    *upload.MemorySessions
	sessions *upload.Sessions
	disk     *filesystem.Disk
	handler  http.Handler
}

func newSessionEnv(t *testing.T, base upload.Policy) *sessionEnv {
	t.Helper()
	e := &sessionEnv{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), store: &upload.MemorySessions{}}
	var err error
	e.sessions, err = upload.NewSessions(e.store, upload.SessionsOptions{Now: func() time.Time { return e.now }})
	if err != nil {
		t.Fatal(err)
	}
	e.disk = newDisk(t, memory.New())
	e.handler = upload.Handler(e.disk, base, upload.HandlerOptions{Resolve: e.sessions.Resolver()})
	return e
}

func (e *sessionEnv) issue(t *testing.T, base upload.Policy, req upload.SessionRequest) upload.IssuedSession {
	t.Helper()
	is, err := e.sessions.Issue(t.Context(), base, req)
	if err != nil {
		t.Fatal(err)
	}
	return is
}

// send posts one file with token (none when "").
func (e *sessionEnv) send(t *testing.T, token, name, data string) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, []formFile{{upload.DefaultField, name, data}}, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/upload", body)
	req.Header.Set("Content-Type", ct)
	if token != "" {
		req.Header.Set(upload.HeaderUploadSession, token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func storedKey(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp upload.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Files) != 1 {
		t.Fatalf("response %s: %v", rec.Body, err)
	}
	return resp.Files[0].Key
}

// TestSessionLifecycle: a session's token uploads under its prefix until
// it expires or is revoked; a request without one, or with a malformed,
// unknown or forged one, is the same 403; Sweep removes expired
// sessions only.
func TestSessionLifecycle(t *testing.T) {
	ctx := t.Context()
	e := newSessionEnv(t, images)
	is := e.issue(t, images, upload.SessionRequest{Prefix: "users/42", TTL: time.Hour})
	if !strings.HasPrefix(is.ID, upload.SessionIDPrefix) || !strings.HasPrefix(is.Token, is.ID+".") || len(is.SecretHash) != 64 || strings.Contains(is.Token, is.SecretHash) {
		t.Fatalf("issued %+v", is)
	}
	if stored, _ := e.store.LoadSession(ctx, is.ID); strings.Contains(is.Token, stored.SecretHash) || !stored.ExpiresAt.Equal(e.now.Add(time.Hour)) {
		t.Fatalf("stored %+v", stored)
	}
	for range 2 {
		if key := storedKey(t, e.send(t, is.Token, "a.png", pngData)); !strings.HasPrefix(key, "users/42/") || !strings.HasSuffix(key, ".png") {
			t.Fatalf("key %q is outside the session's prefix", key)
		}
	}
	forged := is.ID + ".AAAA" + is.Token[len(is.ID)+5:]
	for name, token := range map[string]string{
		"none": "", "no separator": is.ID, "no secret": is.ID + ".", "wrong prefix": "x_" + is.Token,
		"unknown": upload.SessionIDPrefix + "nope.secret", "forged secret": forged, "too long": is.Token + strings.Repeat("a", 300),
	} {
		rec := e.send(t, token, "a.png", pngData)
		if rec.Code != http.StatusForbidden || errorBody(t, rec).Code != filesystem.CodePermission {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		// One message for every refusal, so tokens cannot be probed.
		if _, _, err := e.sessions.Redeem(ctx, token, images); err == nil || !strings.Contains(err.Error(), "missing, unknown or expired") {
			t.Fatalf("%s: Redeem = %v", name, err)
		}
	}

	short := e.issue(t, images, upload.SessionRequest{Prefix: "short", TTL: time.Minute})
	e.now = e.now.Add(time.Minute) // the expiry instant itself is refused
	if rec := e.send(t, short.Token, "a.png", pngData); rec.Code != http.StatusForbidden {
		t.Fatalf("expired session: %d", rec.Code)
	}
	if n, err := e.sessions.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
	if _, err := e.store.LoadSession(ctx, short.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("swept session: %v", err)
	}
	storedKey(t, e.send(t, is.Token, "a.png", pngData))

	if err := e.sessions.Revoke(ctx, is.ID); err != nil {
		t.Fatal(err)
	}
	if rec := e.send(t, is.Token, "a.png", pngData); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked session: %d", rec.Code)
	}
	if err := e.sessions.Revoke(ctx, is.ID); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
}

// TestSessionLimitsOnBytes: the session's size and types are enforced on
// the bytes received, below the endpoint's own limits, and an exact-key
// session stores at its key only.
func TestSessionLimitsOnBytes(t *testing.T) {
	e := newSessionEnv(t, anyFile)
	is := e.issue(t, anyFile, upload.SessionRequest{Key: "avatars/u1.png", MaxSize: int64(len(pngData)), Allowed: []string{" Image/PNG "}, TTL: time.Hour})
	if is.Allowed[0] != "image/png" {
		t.Fatalf("allowed stored as %q", is.Allowed)
	}
	if key := storedKey(t, e.send(t, is.Token, "evil/../name.png", pngData)); key != "avatars/u1.png" {
		t.Fatalf("exact-key session stored at %q", key)
	}
	// A client-declared name or type changes nothing: the bytes are HTML.
	if rec := e.send(t, is.Token, "a.png", htmlData); rec.Code != http.StatusUnsupportedMediaType && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a type outside the session: %d %s", rec.Code, rec.Body)
	}
	if rec := e.send(t, is.Token, "a.png", pngData+"x"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file over the session's MaxSize: %d %s", rec.Code, rec.Body)
	}
	// Without a session the same endpoint refuses before reading.
	if rec := e.send(t, "", "a.txt", "plain"); rec.Code != http.StatusForbidden {
		t.Fatalf("no session: %d", rec.Code)
	}
}

// TestSessionPrefixWithPolicyKey: the endpoint's KeyFunc chooses the key
// inside the session's prefix, and its errors stay errors.
func TestSessionPrefixWithPolicyKey(t *testing.T) {
	ctx := t.Context()
	base := images
	base.Key = upload.NameKey("photos")
	e := newSessionEnv(t, base)
	is := e.issue(t, base, upload.SessionRequest{Prefix: "tenant-a", TTL: time.Hour})
	if key := storedKey(t, e.send(t, is.Token, "cat.png", pngData)); key != "tenant-a/photos/cat.png" {
		t.Fatalf("key %q", key)
	}
	boom := errors.New("no keys today")
	failing := images
	failing.Key = func(upload.Info) (string, error) { return "", boom }
	p, s, err := e.sessions.Redeem(ctx, is.Token, failing)
	if err != nil || s.ID != is.ID {
		t.Fatalf("Redeem = %v", err)
	}
	if _, err := upload.Receive(ctx, e.disk, strings.NewReader(pngData), "a.png", p); !errors.Is(err, boom) {
		t.Fatalf("a failing KeyFunc = %v", err)
	}
}

// TestSessionIssueNarrowsOnly: a session can only narrow the endpoint's
// policy; every widening or malformed request is CodeInvalidArgument.
func TestSessionIssueNarrowsOnly(t *testing.T) {
	ctx := t.Context()
	e := newSessionEnv(t, images)
	base := images
	base.MinSize = 10
	ok := upload.SessionRequest{Prefix: "p", TTL: time.Hour}
	for name, req := range map[string]upload.SessionRequest{
		"no TTL":            {Prefix: "p"},
		"too long":          {Prefix: "p", TTL: upload.MaxSessionLifetime + time.Second},
		"neither":           {TTL: time.Hour},
		"both":              {Key: "k", Prefix: "p", TTL: time.Hour},
		"bigger":            {Prefix: "p", TTL: time.Hour, MaxSize: base.MaxSize + 1},
		"under MinSize":     {Prefix: "p", TTL: time.Hour, MaxSize: 9},
		"negative":          {Prefix: "p", TTL: time.Hour, MaxSize: -1},
		"wider wildcard":    {Prefix: "p", TTL: time.Hour, Allowed: []string{"*/*"}},
		"other family":      {Prefix: "p", TTL: time.Hour, Allowed: []string{"text/*"}},
		"other type":        {Prefix: "p", TTL: time.Hour, Allowed: []string{"text/plain"}},
		"no slash":          {Prefix: "p", TTL: time.Hour, Allowed: []string{"image"}},
		"empty half":        {Prefix: "p", TTL: time.Hour, Allowed: []string{"image/"}},
		"two slashes":       {Prefix: "p", TTL: time.Hour, Allowed: []string{"image/png/x"}},
		"star type":         {Prefix: "p", TTL: time.Hour, Allowed: []string{"*/png"}},
		"parameters":        {Prefix: "p", TTL: time.Hour, Allowed: []string{"image/png;q=1"}},
		"control character": {Prefix: "p", TTL: time.Hour, Allowed: []string{"image/p\x01g"}},
		"long pattern":      {Prefix: "p", TTL: time.Hour, Allowed: []string{"image/" + strings.Repeat("x", 200)}},
		"too many types":    {Prefix: "p", TTL: time.Hour, Allowed: make([]string, upload.MaxSessionTypes+1)},
	} {
		if _, err := e.sessions.Issue(ctx, base, req); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err := e.sessions.Issue(ctx, base, upload.SessionRequest{Key: "../etc/passwd", TTL: time.Hour})
	wantCode(t, err, filesystem.CodeInvalidKey)
	_, err = e.sessions.Issue(ctx, upload.Policy{}, ok)
	wantCode(t, err, filesystem.CodeInvalidArgument)
	all := upload.Policy{MaxSize: 10, Allowed: []string{"*/*"}}
	for _, narrower := range [][]string{{"*/*"}, {"image/*"}, {"image/png", "text/plain"}} {
		if _, err := e.sessions.Issue(ctx, all, upload.SessionRequest{Prefix: "p", TTL: time.Hour, Allowed: narrower}); err != nil {
			t.Fatalf("%v within */*: %v", narrower, err)
		}
	}
	if _, err := e.sessions.Issue(ctx, images, upload.SessionRequest{Prefix: "p", TTL: time.Hour, Allowed: []string{"image/*", "image/gif"}}); err != nil {
		t.Fatalf("narrower types within image/*: %v", err)
	}
}

// TestSessionRecheckedAgainstPolicy: a session's types are judged
// against the endpoint's policy at each redemption, so narrowing the
// policy narrows sessions already issued.
func TestSessionRecheckedAgainstPolicy(t *testing.T) {
	ctx := t.Context()
	e := newSessionEnv(t, anyFile)
	is := e.issue(t, anyFile, upload.SessionRequest{Prefix: "p", TTL: time.Hour, Allowed: []string{"image/png", "text/plain"}})
	p, _, err := e.sessions.Redeem(ctx, is.Token, upload.Policy{MaxSize: 10, Allowed: []string{"text/*"}})
	if err != nil || len(p.Allowed) != 1 || p.Allowed[0] != "text/plain" || p.MaxSize != 10 {
		t.Fatalf("Redeem under a narrower policy = %+v, %v", p, err)
	}
	_, _, err = e.sessions.Redeem(ctx, is.Token, upload.Policy{MaxSize: 10, Allowed: []string{"video/*"}})
	wantCode(t, err, filesystem.CodeRejected)
}

// brokenStore fails or misbehaves on demand.
type brokenStore struct {
	upload.MemorySessions
	saveErr, loadErr error
	wrongID          bool
}

func (b *brokenStore) SaveSession(ctx context.Context, s upload.Session) error {
	if b.saveErr != nil {
		return b.saveErr
	}
	return b.MemorySessions.SaveSession(ctx, s)
}

func (b *brokenStore) LoadSession(ctx context.Context, id string) (upload.Session, error) {
	if b.loadErr != nil {
		return upload.Session{}, b.loadErr
	}
	s, err := b.MemorySessions.LoadSession(ctx, id)
	if b.wrongID {
		s.ID = upload.SessionIDPrefix + "other"
	}
	return s, err
}

// TestSessionStoreFailures: a store's failure is returned, never taken
// for a refusal or a pass, and a store answering with another session is
// refused.
func TestSessionStoreFailures(t *testing.T) {
	ctx := t.Context()
	if _, err := upload.NewSessions(nil, upload.SessionsOptions{}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("nil store = %v", err)
	}
	b := &brokenStore{}
	m, err := upload.NewSessions(b, upload.SessionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	down := filesystem.Errorf(filesystem.CodeUnavailable, "store down")
	b.saveErr = down
	if _, err := m.Issue(ctx, images, upload.SessionRequest{Prefix: "p", TTL: time.Hour}); !errors.Is(err, down) {
		t.Fatalf("Issue with a failing store = %v", err)
	}
	b.saveErr = nil
	is, err := m.Issue(ctx, images, upload.SessionRequest{Prefix: "p", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	b.loadErr = down
	if _, _, err := m.Redeem(ctx, is.Token, images); !errors.Is(err, down) {
		t.Fatalf("Redeem with a failing store = %v", err)
	}
	b.loadErr, b.wrongID = nil, true
	_, _, err = m.Redeem(ctx, is.Token, images)
	wantCode(t, err, filesystem.CodePermission)
	b.wrongID = false
	if _, _, err := m.Redeem(ctx, is.Token, images); err != nil {
		t.Fatalf("Redeem with the real clock: %v", err)
	}
}

// TestMemorySessions: IDs are unique, stored values are copies, and only
// expired sessions are swept.
func TestMemorySessions(t *testing.T) {
	ctx := t.Context()
	var m upload.MemorySessions
	if _, err := m.LoadSession(ctx, "ups_x"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("load from empty = %v", err)
	}
	if n, err := m.SweepSessions(ctx, time.Now()); n != 0 || err != nil {
		t.Fatalf("sweep of empty = %d, %v", n, err)
	}
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	s := upload.Session{ID: "ups_a", Allowed: []string{"image/png"}, ExpiresAt: now.Add(time.Hour)}
	if err := m.SaveSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	wantCode(t, m.SaveSession(ctx, s), filesystem.CodeExists)
	s.Allowed[0] = "text/html"
	got, _ := m.LoadSession(ctx, "ups_a")
	got.Allowed = append(got.Allowed[:0], "video/mp4")
	if again, _ := m.LoadSession(ctx, "ups_a"); again.Allowed[0] != "image/png" {
		t.Fatalf("stored session changed through a copy: %v", again.Allowed)
	}
	if err := m.SaveSession(ctx, upload.Session{ID: "ups_b", ExpiresAt: now}); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.SweepSessions(ctx, now); n != 1 {
		t.Fatalf("swept %d, want the one expired at now", n)
	}
	if _, err := m.LoadSession(ctx, "ups_a"); err != nil {
		t.Fatalf("a live session was swept: %v", err)
	}
	if !(upload.Session{ExpiresAt: now}).Expired(now) || (upload.Session{ExpiresAt: now}).Expired(now.Add(-time.Nanosecond)) {
		t.Fatal("Expired is wrong at the boundary")
	}
}

// TestSessionLimitsHandler: the limits endpoint resolved through a
// session shows the client the session's limits.
func TestSessionLimitsHandler(t *testing.T) {
	e := newSessionEnv(t, images)
	is := e.issue(t, images, upload.SessionRequest{Prefix: "p", MaxSize: 100, Allowed: []string{"image/png"}, TTL: time.Hour})
	h := upload.LimitsHandler(images, upload.LimitsOptions{Resolve: e.sessions.Resolver()})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/limits", nil)
	req.Header.Set(upload.HeaderUploadSession, is.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var l upload.Limits
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || l.MaxBytes != 100 || len(l.Allowed) != 1 || l.Allowed[0] != "image/png" {
		t.Fatalf("limits %d %s", rec.Code, rec.Body)
	}
}
