package share_test

import (
	"context"
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

// TestHandlerEphemeral: a share link to a view-once file serves it once
// (GET counts the view, HEAD spends none), and a link to an expired file
// answers 404, as GET /files/{id} does.
func TestHandlerEphemeral(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	h := share.Handler(e.store, e.plugin)
	serve := func(method, token string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, method, "/"+token, nil))
		return rec.Code
	}
	record := func(key string, in records.NewFile) records.File {
		if _, err := mustDisk(t, e.store).WriteBytes(ctx, key, []byte("%PDF"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		in.Scope, in.Disk, in.Key = "t", "media", key
		f, err := e.store.CreateFile(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	link := func(f records.File) string {
		sh, err := e.plugin.Create(ctx, e.store, "t", f.ID, share.Options{TTL: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return sh.Token
	}
	once := link(record("once.pdf", records.NewFile{ViewOnce: true}))
	if code := serve(http.MethodHead, once); code != http.StatusNoContent {
		t.Fatalf("HEAD of an unviewed view-once file = %d", code)
	}
	if code := serve(http.MethodGet, once); code != http.StatusFound {
		t.Fatalf("first GET = %d", code)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if code := serve(m, once); code != http.StatusNotFound {
			t.Fatalf("%s after the one view = %d", m, code)
		}
	}
	past := time.Now().Add(-time.Minute)
	expired := link(record("old.pdf", records.NewFile{ExpiresAt: &past}))
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if code := serve(m, expired); code != http.StatusNotFound {
			t.Fatalf("%s of an expired file = %d", m, code)
		}
	}
}

func mustDisk(t *testing.T, s *records.Store) *filesystem.Disk {
	t.Helper()
	d, err := s.Disks().Disk("media")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// interleaver runs then once, after the first download it hears of has
// committed: a second request landing between the first one's Resolve and
// its Viewed.
type interleaver struct{ then func() }

func (*interleaver) Name() string { return "interleaver" }
func (i *interleaver) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == share.EventDownloaded && i.then != nil {
		run := i.then
		i.then = nil
		s.AfterCommit(ctx, func(context.Context) { run() })
	}
	return nil
}

// TestHandlerViewOnceRace: two first GETs of a view-once file through its
// share link, the second landing after the first loaded the file and
// before it counted the view: exactly one is served.
func TestHandlerViewOnceRace(t *testing.T) {
	ctx := t.Context()
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithURLSigner(signer{}))
	mgr, _ := filesystem.NewManager(disk)
	plugin := share.New()
	race := &interleaver{}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(plugin, race))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := disk.WriteBytes(ctx, "once.pdf", []byte("%PDF"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "media", Key: "once.pdf", ViewOnce: true})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := plugin.Create(ctx, s, "t", f.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := share.Handler(s, plugin)
	get := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+sh.Token, nil))
		return rec.Code
	}
	var second int
	race.then = func() { second = get() }
	first := get()
	if served := btoi(first == http.StatusFound) + btoi(second == http.StatusFound); served != 1 {
		t.Fatalf("two first GETs served %d times (%d, %d)", served, first, second)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestResolveChecksBeforeSpending: an expired file, and a view-once file
// already seen, are refused before the link spends anything, so they
// cannot burn its MaxDownloads.
func TestResolveChecksBeforeSpending(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	record := func(key string, in records.NewFile) records.File {
		if _, err := mustDisk(t, e.store).WriteBytes(ctx, key, []byte("%PDF"), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		in.Scope, in.Disk, in.Key = "t", "media", key
		f, err := e.store.CreateFile(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	downloads := func(f records.File) int64 {
		links, err := e.plugin.Shares(ctx, e.store, "t", f.ID)
		if err != nil || len(links) != 1 {
			t.Fatalf("Shares = %v, %v", links, err)
		}
		return links[0].Downloads
	}
	past := time.Now().Add(-time.Minute)
	old := record("old.pdf", records.NewFile{ExpiresAt: &past})
	sh, err := e.plugin.Create(ctx, e.store, "t", old.ID, share.Options{TTL: time.Hour, MaxDownloads: 3})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, err := e.plugin.Resolve(ctx, e.store, sh.Token)
		gone(t, err)
	}
	if n := downloads(old); n != 0 {
		t.Fatalf("an expired file spent %d downloads", n)
	}
	once := record("once.pdf", records.NewFile{ViewOnce: true})
	sh, err = e.plugin.Create(ctx, e.store, "t", once.ID, share.Options{TTL: time.Hour, MaxDownloads: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.plugin.Resolve(ctx, e.store, sh.Token); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err := e.plugin.Resolve(ctx, e.store, sh.Token)
		gone(t, err)
	}
	if n := downloads(once); n != 1 {
		t.Fatalf("a view-once file spent %d downloads, want its one view", n)
	}
}
