package share_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/share"
)

type env struct {
	db     *sql.DB
	faults *testdb.Faults
	store  *records.Store
	plugin *share.Plugin
	events *recorder
	now    time.Time
	file   records.File
}

// recorder logs the plugin's events of committed changes and fails the
// kind in refuse.
type recorder struct {
	log    []string
	refuse records.EventKind
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	if e.Kind == r.refuse {
		return errors.New("refused")
	}
	if !strings.HasPrefix(string(e.Kind), share.PluginName+".") {
		return nil
	}
	line := string(e.Kind) + " " + e.FileID + " " + e.Subject
	for _, k := range []string{share.DetailExpiresAt, share.DetailMaxDownloads, share.DetailDownloads} {
		if v, ok := e.Detail[k]; ok {
			line += " " + k + "=" + v
		}
	}
	s.AfterCommit(ctx, func(context.Context) { r.log = append(r.log, line) })
	return nil
}

type signer struct{}

func (signer) SignURL(_ context.Context, disk, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: o.Method, URL: "https://app.test/" + disk + "/" + key}, nil
}

func newEnv(t *testing.T, signing bool) *env {
	t.Helper()
	e := &env{events: &recorder{}, now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	e.db, e.faults = testdb.OpenFaulty(t)
	var opts []filesystem.Option
	if signing {
		opts = append(opts, filesystem.WithURLSigner(signer{}))
	}
	disk, _ := filesystem.NewDisk("media", memory.New(), opts...)
	mgr, _ := filesystem.NewManager(disk)
	e.plugin = share.New(share.WithClock(func() time.Time { return e.now }))
	var err error
	e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithPlugins(e.plugin, e.events))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _ = disk.WriteBytes(t.Context(), "report.pdf", []byte("%PDF"), filesystem.WriteOptions{})
	e.file, err = e.store.CreateFile(t.Context(), records.NewFile{Scope: "t", Disk: "media", Key: "report.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func gone(t *testing.T, err error) {
	t.Helper()
	if filesystem.CodeOf(err) != filesystem.CodeNotFound || !strings.Contains(err.Error(), "no longer valid") {
		t.Fatalf("want the uniform not-found, got %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, MaxDownloads: 2})
	if err != nil || len(sh.Token) < 40 || !strings.HasPrefix(sh.ID, share.IDPrefix) {
		t.Fatalf("Create = %+v, %v", sh, err)
	}
	for range 2 {
		f, err := e.plugin.Resolve(ctx, e.store, sh.Token)
		if err != nil || f.ID != e.file.ID {
			t.Fatalf("Resolve = %v", err)
		}
	}
	_, err = e.plugin.Resolve(ctx, e.store, sh.Token)
	gone(t, err)
	list, err := e.plugin.Shares(ctx, e.store, "t", e.file.ID)
	if err != nil || len(list) != 1 || list[0].Downloads != 2 || list[0].Token != "" {
		t.Errorf("Shares = %+v, %v", list, err)
	}
	// The token is never stored.
	var stored string
	_ = e.db.QueryRow("SELECT " + share.ColTokenHash + " FROM " + share.TableNameShares).Scan(&stored)
	if stored == sh.Token || len(stored) != 64 {
		t.Errorf("stored token column = %q", stored)
	}

	unlimited, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	for range 5 {
		if _, err := e.plugin.Resolve(ctx, e.store, unlimited.Token); err != nil {
			t.Fatal(err)
		}
	}
	e.now = e.now.Add(2 * time.Hour)
	_, err = e.plugin.Resolve(ctx, e.store, unlimited.Token)
	gone(t, err)

	e.now = e.now.Add(-2 * time.Hour)
	r, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	if err := e.plugin.Revoke(ctx, e.store, "t", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.plugin.Revoke(ctx, e.store, "t", r.ID); err != nil {
		t.Errorf("second revoke = %v", err)
	}
	_, err = e.plugin.Resolve(ctx, e.store, r.Token)
	gone(t, err)
	_, err = e.plugin.Resolve(ctx, e.store, "unknown-token")
	gone(t, err)

	// A trashed file's links stop working; purging deletes them.
	live, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	_ = e.store.Trash(ctx, "t", e.file.ID)
	_, err = e.plugin.Resolve(ctx, e.store, live.Token)
	gone(t, err)
	_, err = e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	if filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("share of a trashed file = %v", err)
	}
	if err := e.store.Purge(ctx, "t", e.file.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.plugin.Shares(ctx, e.store, "t", e.file.ID); len(list) != 0 {
		t.Errorf("links survived the purge: %d", len(list))
	}
}

func TestValidation(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	for _, o := range []share.Options{{}, {TTL: share.MaxLifetime + time.Second}, {TTL: time.Hour, MaxDownloads: -1}} {
		if _, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, o); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("Create(%+v) = %v", o, err)
		}
	}
	if _, err := e.plugin.Create(ctx, e.store, "other", e.file.ID, share.Options{TTL: time.Hour}); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("file of another scope = %v", err)
	}
	if share.New().Name() != share.PluginName {
		t.Error("name")
	}
}

func TestHandler(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, MaxDownloads: 1})
	srv := httptest.NewServer(http.StripPrefix("/s/", share.Handler(e.store, e.plugin)))
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(method, path string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp
	}
	resp := get(http.MethodGet, "/s/"+sh.Token)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "https://app.test/media/report.pdf") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("redirect = %d %v", resp.StatusCode, resp.Header)
	}
	for path, want := range map[string]int{"/s/" + sh.Token: 404, "/s/": 404, "/s/a/b": 404} {
		if r := get(http.MethodGet, path); r.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, r.StatusCode, want)
		}
	}
	if r := get(http.MethodPost, "/s/x"); r.StatusCode != 404 {
		t.Errorf("POST to an unknown link = %d", r.StatusCode)
	}
	if r := get(http.MethodPut, "/s/x"); r.StatusCode != 405 {
		t.Errorf("PUT = %d", r.StatusCode)
	}
	// A disk that cannot sign makes the redirect fail.
	plain := newEnv(t, false)
	sh2, _ := plain.plugin.Create(ctx, plain.store, "t", plain.file.ID, share.Options{TTL: time.Hour})
	srv2 := httptest.NewServer(share.Handler(plain.store, plain.plugin))
	defer srv2.Close()
	if r, _ := http.Get(srv2.URL + "/" + sh2.Token); r.StatusCode != 500 {
		t.Errorf("unsignable disk = %d", r.StatusCode)
	}
	// A database failure is a 500, not a not-found.
	testdb.OnlySQLite(t)
	testdb.Exec(t, plain.db, "DROP TABLE "+share.TableNameShares)
	if r, _ := http.Get(srv2.URL + "/" + sh2.Token); r.StatusCode != 500 {
		t.Errorf("database failure = %d", r.StatusCode)
	}
}

func TestDatabaseFailures(t *testing.T) {
	testdb.OnlySQLite(t)
	ctx := t.Context()
	fail := func(db *sql.DB, op string) {
		testdb.Exec(t, db, "CREATE TRIGGER f_"+op+" BEFORE "+op+" ON "+share.TableNameShares+" BEGIN SELECT RAISE(FAIL, 'injected'); END")
	}
	e := newEnv(t, true)
	sh, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	fail(e.db, "UPDATE")
	if _, err := e.plugin.Resolve(ctx, e.store, sh.Token); err == nil {
		t.Error("count failure ignored")
	}
	if err := e.plugin.Revoke(ctx, e.store, "t", sh.ID); err == nil {
		t.Error("revoke failure ignored")
	}
	fail(e.db, "INSERT")
	if _, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour}); err == nil {
		t.Error("insert failure ignored")
	}
	fail(e.db, "DELETE")
	if err := e.store.Purge(ctx, "t", e.file.ID); err == nil {
		t.Error("purge ignored the link delete failure")
	}
	if _, err := e.plugin.Prune(ctx, e.store, e.now.Add(2*time.Hour)); err == nil {
		t.Error("prune failure ignored")
	}
	testdb.Exec(t, e.db, "DROP TABLE "+share.TableNameShares)
	if _, err := e.plugin.Shares(ctx, e.store, "t", e.file.ID); err == nil {
		t.Error("listing failure ignored")
	}
	if err := e.plugin.Revoke(ctx, e.store, "t", sh.ID); err == nil {
		t.Error("revoke ignored the lookup failure")
	}
	// The file's row cannot be read: a failure, not a not-found.
	f := newEnv(t, true)
	sh, _ = f.plugin.Create(ctx, f.store, "t", f.file.ID, share.Options{TTL: time.Hour})
	testdb.Exec(t, f.db, "DROP TABLE "+records.TableNameFiles)
	if _, err := f.plugin.Resolve(ctx, f.store, sh.Token); err == nil || filesystem.CodeOf(err) == filesystem.CodeNotFound {
		t.Errorf("file read failure = %v", err)
	}
}

// downloads is how often the link with id was spent.
func (e *env) downloads(t *testing.T, id string) int64 {
	t.Helper()
	list, err := e.plugin.Shares(t.Context(), e.store, "t", e.file.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, sh := range list {
		if sh.ID == id {
			return sh.Downloads
		}
	}
	t.Fatalf("no link %s", id)
	return 0
}

// TestTrashedFileSpendsNothing: a link to a trashed file fails without
// spending a download, so it still works once the file is restored.
func TestTrashedFileSpendsNothing(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, MaxDownloads: 1})
	_ = e.store.Trash(ctx, "t", e.file.ID)
	_, err := e.plugin.Resolve(ctx, e.store, sh.Token)
	gone(t, err)
	if n := e.downloads(t, sh.ID); n != 0 {
		t.Fatalf("a refused download was spent: %d", n)
	}
	_ = e.store.Restore(ctx, "t", e.file.ID)
	if _, err := e.plugin.Resolve(ctx, e.store, sh.Token); err != nil {
		t.Fatalf("after restoring = %v", err)
	}
}

// TestCheck: Check answers as Resolve would, and spends nothing.
func TestCheck(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	once, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, MaxDownloads: 1})
	for range 3 {
		if f, err := e.plugin.Check(ctx, e.store, once.Token); err != nil || f.ID != e.file.ID {
			t.Fatalf("Check = %+v, %v", f, err)
		}
	}
	if n := e.downloads(t, once.ID); n != 0 {
		t.Fatalf("Check spent %d", n)
	}
	_, _ = e.plugin.Resolve(ctx, e.store, once.Token)
	_, err := e.plugin.Check(ctx, e.store, once.Token)
	gone(t, err) // exhausted
	revoked, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	_ = e.plugin.Revoke(ctx, e.store, "t", revoked.ID)
	_, err = e.plugin.Check(ctx, e.store, revoked.Token)
	gone(t, err)
	expiring, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	e.now = e.now.Add(time.Hour)
	_, err = e.plugin.Check(ctx, e.store, expiring.Token)
	gone(t, err)
	_, err = e.plugin.Check(ctx, e.store, "unknown")
	gone(t, err)
}

// TestHeadSpendsNothing: HEAD answers 204 without a Location for a usable
// link and 404 otherwise, and never spends a download.
func TestHeadSpendsNothing(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, MaxDownloads: 1})
	h := share.Handler(e.store, e.plugin)
	head := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodHead, "/"+sh.Token, nil))
		return rec
	}
	for range 3 {
		if rec := head(); rec.Code != http.StatusNoContent || rec.Header().Get("Location") != "" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("HEAD = %d %v", rec.Code, rec.Header())
		}
	}
	if n := e.downloads(t, sh.ID); n != 0 {
		t.Fatalf("HEAD spent %d", n)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+sh.Token, nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET after the HEADs = %d", rec.Code)
	}
	if rec := head(); rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD of a spent link = %d", rec.Code)
	}
}

// TestPrune deletes links expired or revoked before the cutoff, in every
// scope, and keeps the rest.
func TestPrune(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	opts := share.Options{TTL: time.Hour}
	_, _ = e.plugin.Create(ctx, e.store, "t", e.file.ID, opts) // expires before the cutoff
	revoked, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: 24 * time.Hour})
	_ = e.plugin.Revoke(ctx, e.store, "t", revoked.ID)
	e.now = e.now.Add(90 * time.Minute)
	live, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, opts)
	recentlyRevoked, _ := e.plugin.Create(ctx, e.store, "t", e.file.ID, opts)
	_ = e.plugin.Revoke(ctx, e.store, "t", recentlyRevoked.ID)
	n, err := e.plugin.Prune(ctx, e.store, e.now.Add(-time.Minute))
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	list, _ := e.plugin.Shares(ctx, e.store, "t", e.file.ID)
	var ids []string
	for _, sh := range list {
		ids = append(ids, sh.ID)
	}
	if got, want := strings.Join(ids, ","), strings.Join([]string{recentlyRevoked.ID, live.ID}, ","); got != want && got != live.ID+","+recentlyRevoked.ID {
		t.Fatalf("left %s, want %s and %s", got, live.ID, recentlyRevoked.ID)
	}
}

func TestMigrationsPerEngine(t *testing.T) {
	for _, engine := range []string{records.EngineSQLite, records.EnginePostgres, records.EngineMySQL} {
		ms, err := share.New().Migrations(engine)
		if err != nil || len(ms) != 2 {
			t.Fatalf("%s: %v", engine, err)
		}
		if inline := strings.Contains(ms[0].Statements[0], "CONSTRAINT `fs_shares_token` UNIQUE"); inline != (engine == records.EngineMySQL) {
			t.Errorf("%s: index placement", engine)
		}
	}
}

func TestEvents(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour, MaxDownloads: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err1 := e.plugin.Resolve(ctx, e.store, sh.Token)
	_, err2 := e.plugin.Resolve(ctx, e.store, sh.Token)
	_, err3 := e.plugin.Resolve(ctx, e.store, sh.Token) // exhausted: nothing spent, no event
	_, err4 := e.plugin.Check(ctx, e.store, sh.Token)   // spends nothing, no event
	err5 := e.plugin.Revoke(ctx, e.store, "t", sh.ID)
	err6 := e.plugin.Revoke(ctx, e.store, "t", sh.ID)      // revoked already: no event
	err7 := e.plugin.Revoke(ctx, e.store, "t", "shr_nope") // unknown: no event
	if err := errors.Join(err1, err2, err5, err6, err7); err != nil {
		t.Fatal(err)
	}
	gone(t, err3)
	gone(t, err4)
	if n, err := e.plugin.Prune(ctx, e.store, e.now.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	got := strings.NewReplacer(e.file.ID, "F", sh.ID, "S").Replace(strings.Join(e.events.log, "; "))
	want := "share.created F S expires_at=2026-10-03T13:00:00Z max_downloads=2; " +
		"share.downloaded F S downloads=1; share.downloaded F S downloads=2; share.revoked F S"
	if got != want {
		t.Fatalf("events:\n%s\nwant:\n%s", got, want)
	}
	if len(share.EventKindValues) != 3 {
		t.Fatalf("EventKindValues = %v", share.EventKindValues)
	}
}

// TestFaultSweep fails every database step of Create, Resolve and Revoke
// in turn, and a hook refusing each event: every failure must leave the
// links exactly as they were, so a download is never counted without its
// event, nor a link created or revoked without one.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	sh, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		list, err := e.plugin.Shares(ctx, e.store, "t", e.file.ID)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%+v", list)
	}
	ops := []struct {
		kind records.EventKind
		run  func() error
	}{
		{share.EventCreated, func() error {
			_, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour})
			return err
		}},
		{share.EventDownloaded, func() error { _, err := e.plugin.Resolve(ctx, e.store, sh.Token); return err }},
		{share.EventRevoked, func() error { return e.plugin.Revoke(ctx, e.store, "t", sh.ID) }},
	}
	for _, op := range ops {
		t.Run(string(op.kind), func(t *testing.T) {
			e.events.refuse = op.kind
			before := state()
			if err := op.run(); err == nil {
				t.Fatal("a refused event did not fail the change")
			}
			e.events.refuse = ""
			if after := state(); after != before {
				t.Fatalf("the refused change was kept:\n%s\n%s", before, after)
			}
			for n := 1; ; n++ {
				before := state()
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: err = %v, want the injected fault", n, err)
				}
				if after := state(); after != before {
					t.Fatalf("step %d failed but changed the links:\n%s\n%s", n, before, after)
				}
			}
		})
	}
}

func TestPurgeScope(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, true)
	if _, err := e.plugin.Create(ctx, e.store, "t", e.file.ID, share.Options{TTL: time.Hour}); err != nil {
		t.Fatal(err)
	}
	// The plugin's own scope purge removes links of the scope.
	if err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return e.plugin.PurgeScope(ctx, e.store, "t") }); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.plugin.Shares(ctx, e.store, "t", e.file.ID); len(list) != 0 {
		t.Fatalf("links left: %v", list)
	}
	if n, err := e.store.PurgeScope(ctx, "t"); err != nil || n != 1 {
		t.Fatalf("PurgeScope = %d, %v", n, err)
	}
}
