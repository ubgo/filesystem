package share_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
)

// pinned is a collection holding a fixed list of files.
type pinned struct{ files []records.File }

func (pinned) Kind() string { return "pinned" }
func (c pinned) Files(context.Context, *records.Store, string, string, int) ([]records.File, error) {
	return c.files, nil
}
func (c pinned) Contains(_ context.Context, _ *records.Store, _, _, id string) (bool, error) {
	for _, f := range c.files {
		if f.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// TestHandlerUsesStoreClock: the share handler and a plugin built without
// WithClock read the Store's clock (records.WithClock). The Store runs a
// year ahead; a file expiring before then is expired for the file route
// and left out of a collection listing, and a link made "now" by the
// Store's clock lasts its TTL from there.
func TestHandlerUsesStoreClock(t *testing.T) {
	ctx := t.Context()
	ahead := time.Now().AddDate(1, 0, 0).UTC().Truncate(time.Microsecond)
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	coll := &pinned{}
	plugin := share.New(share.WithCollections(coll))
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(plugin), records.WithClock(func() time.Time { return ahead }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	record := func(key string, exp time.Time) records.File {
		if _, err := disk.WriteBytes(ctx, key, []byte("%PDF"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: key, ExpiresAt: &exp})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	soon := record("soon.pdf", time.Now().Add(time.Hour))
	later := record("later.pdf", ahead.Add(time.Hour))
	coll.files = []records.File{soon, later}

	sh, err := plugin.Create(ctx, s, "t", soon.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !sh.CreatedAt.Equal(ahead) || !sh.ExpiresAt.Equal(ahead.Add(time.Hour)) {
		t.Fatalf("link created %v, expires %v; want the Store's clock %v", sh.CreatedAt, sh.ExpiresAt, ahead)
	}
	h := share.Handler(s, plugin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+sh.Token, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET of a file expired by the Store's clock = %d, want 404", rec.Code)
	}

	all, err := plugin.CreateFor(ctx, s, "t", share.Target{Kind: "pinned", Ref: "x"}, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+all.Token, nil))
	var listed []share.ListedFile
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list %d %q: %v", rec.Code, rec.Body.String(), err)
	}
	if len(listed) != 1 || listed[0].ID != later.ID {
		t.Fatalf("listed = %+v, want only %s", listed, later.ID)
	}
}

// TestAttemptLimiterClock: NewAttemptLimiterClock's windows follow the
// clock it is given, not the wall clock.
func TestAttemptLimiterClock(t *testing.T) {
	now := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	l := share.NewAttemptLimiterClock(1, time.Minute, func() time.Time { return now })
	first, second := l.Allow("k"), l.Allow("k")
	if !first || second {
		t.Fatalf("allowance of 1: %v %v", first, second)
	}
	now = now.Add(2 * time.Minute)
	if !l.Allow("k") {
		t.Fatal("the window did not reset when the given clock passed it")
	}
	// A nil clock is the wall clock: the allowance still holds.
	wall := share.NewAttemptLimiterClock(1, time.Minute, nil)
	if !wall.Allow("k") || wall.Allow("k") {
		t.Fatal("a nil clock does not limit like NewAttemptLimiter")
	}
}

// TestHandlerLimiterUsesStoreClock: the Handler's own password limiter
// counts its window by the Store's clock, so moving the Store's clock past
// DefaultAttemptWindow lets the right password through again.
func TestHandlerLimiterUsesStoreClock(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	plugin := share.New()
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(plugin), records.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(ctx, "a.pdf", []byte("%PDF"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "a.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := plugin.Create(ctx, s, "t", f.ID, share.Options{TTL: 24 * time.Hour, Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(s, plugin)
	try := func(pw string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+sh.Token, nil)
		req.Header.Set("Share-Password", pw)
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for range share.DefaultAttempts {
		if code := try("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("a wrong password = %d", code)
		}
	}
	if code := try("pw"); code != http.StatusTooManyRequests {
		t.Fatalf("past the allowance = %d", code)
	}
	now = now.Add(share.DefaultAttemptWindow + time.Minute)
	if code := try("pw"); code != http.StatusFound {
		t.Fatalf("after the window by the Store's clock = %d, want 302", code)
	}
}
