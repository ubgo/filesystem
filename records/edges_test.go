package records_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/upload"
)

// hookedConn runs a statement of the test's on the same connection,
// right before the first query the hook matches: a write that lands
// between two reads of one transaction, the way a concurrent writer's
// committed change would on an engine with row locks.
type hookedConn struct {
	driver.Conn
	h *queryHook
}

// queryHook holds the statement to inject and the query text that
// triggers it; it fires once.
type queryHook struct {
	mu    sync.Mutex
	match func(query string) bool
	stmt  string
}

func (c *hookedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.h.mu.Lock()
	match, stmt := c.h.match, c.h.stmt
	if match != nil && match(query) {
		c.h.match = nil
	} else {
		stmt = ""
	}
	c.h.mu.Unlock()
	if stmt != "" {
		if _, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, stmt, nil); err != nil {
			return nil, err
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *hookedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *hookedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

// hookedConnector opens SQLite connections wrapped in hookedConn.
type hookedConnector struct {
	dsn   string
	inner driver.Driver
	h     *queryHook
}

func (c hookedConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.inner.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &hookedConn{Conn: conn, h: c.h}, nil
}

func (c hookedConnector) Driver() driver.Driver { return c.inner }

// openHooked opens a fresh SQLite database whose connection runs h's
// statement before the query h matches.
func openHooked(t *testing.T, h *queryHook) *sql.DB {
	t.Helper()
	onlySQLite(t)
	dsn := "file:" + filepath.Join(t.TempDir(), "hooked.db") + "?_pragma=busy_timeout(5000)"
	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	inner := probe.Driver()
	_ = probe.Close()
	db := sql.OpenDB(hookedConnector{dsn: dsn, inner: inner, h: h})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestPurgeRefusesAFileMovedWhileLocking: a replacement or move that
// lands between the purge's first read and its read under the lock is
// refused with CodeUnavailable, purging neither the row nor the object
// the purge planned on.
func TestPurgeRefusesAFileMovedWhileLocking(t *testing.T) {
	ctx := t.Context()
	h := &queryHook{}
	e := newEnvOn(t, openHooked(t, h), nil)
	f := e.put(t, "t", "a.png")
	h.mu.Lock()
	h.match = func(q string) bool { return strings.Contains(q, "ORDER BY") && strings.Contains(q, records.ColFileKey) }
	h.stmt = "UPDATE " + records.TableNameFiles + " SET " + records.ColFileKey + " = 'moved.png'"
	h.mu.Unlock()
	err := e.store.Purge(ctx, "t", f.ID)
	wantCode(t, err, filesystem.CodeUnavailable)
	if _, err := e.store.File(ctx, "t", f.ID); err != nil {
		t.Fatalf("the refused purge removed the row: %v", err)
	}
	if ok, _ := e.disk.Exists(ctx, "a.png"); !ok {
		t.Fatal("the refused purge deleted the object")
	}
}

// TestRecheckUnderLock: a guarded change still holding when its row is
// locked goes ahead; one whose file was trashed meanwhile (same content,
// so IfUnchanged alone passes) is refused with CodeUnavailable and its
// object change undone.
func TestRecheckUnderLock(t *testing.T) {
	ctx := t.Context()
	e := newRaceEnv(t)
	f := e.public(t)
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate, records.IfUnchanged(f.SHA256)); err != nil {
		t.Fatalf("an unraced guarded change = %v", err)
	}
	if cur, _ := e.store.File(ctx, "t", f.ID); cur.Visibility != filesystem.VisibilityPrivate {
		t.Fatalf("visibility = %q", cur.Visibility)
	}

	e = newRaceEnv(t)
	f = e.public(t)
	e.race = func(ctx context.Context) {
		if err := e.store.Trash(ctx, "t", f.ID); err != nil {
			t.Error(err)
		}
	}
	err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPrivate, records.IfUnchanged(f.SHA256))
	wantCode(t, err, filesystem.CodeUnavailable)
	if cur, _ := e.store.File(ctx, "t", f.ID); cur.Visibility != filesystem.VisibilityPublic || cur.DeletedAt == nil {
		t.Fatalf("the refused change left %+v", cur)
	}
}

// TestIfTrashedOnlyForPurge: Trash, Restore and SetVisibility refuse
// IfTrashed, which only Purge takes, and IfUnchanged naming stale
// content, before touching anything.
func TestIfTrashedOnlyForPurge(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil)
	f := e.put(t, "t", "a.png")
	wantCode(t, e.store.Trash(ctx, "t", f.ID, records.IfTrashed(false)), filesystem.CodeInvalidArgument)
	wantCode(t, e.store.Restore(ctx, "t", f.ID, records.IfTrashed(true)), filesystem.CodeInvalidArgument)
	wantCode(t, e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic, records.IfTrashed(false)), filesystem.CodeInvalidArgument)
	stale := records.IfUnchanged(strings.Repeat("0", 64))
	if err := e.store.Trash(ctx, "t", f.ID, stale); !errors.Is(err, records.ErrChanged) {
		t.Fatalf("Trash of stale content = %v", err)
	}
	if err := e.store.SetVisibility(ctx, "t", f.ID, filesystem.VisibilityPublic, stale); !errors.Is(err, records.ErrChanged) {
		t.Fatalf("SetVisibility of stale content = %v", err)
	}
	if cur, _ := e.store.File(ctx, "t", f.ID); cur.DeletedAt != nil || cur.Visibility != f.Visibility {
		t.Fatalf("a refused change changed the file: %+v", cur)
	}
}

// TestStoreNow: Now is the Store's clock, the one plugins stamp rows with.
func TestStoreNow(t *testing.T) {
	e := newEnv(t, nil)
	if got := e.store.Now(); !got.Equal(e.now) {
		t.Fatalf("Now = %v, want the clock's %v", got, e.now)
	}
}

// purgeWatcher records, for every purge it sees, whether PurgingScope
// marked it and with which scope.
type purgeWatcher struct{ seen []string }

func (*purgeWatcher) Name() string { return "purgewatch" }
func (*purgeWatcher) BeforeCreate(context.Context, *records.Store, *records.File) error {
	return nil
}
func (*purgeWatcher) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (w *purgeWatcher) BeforePurge(ctx context.Context, _ *records.Store, _ records.File) error {
	scope, ok := records.PurgingScope(ctx)
	if !ok {
		scope = "-"
	}
	w.seen = append(w.seen, scope)
	return nil
}

// TestPurgingScopeMarksOffboarding: a hook sees PurgingScope with the
// scope during PurgeScope and nothing during an ordinary Purge.
func TestPurgingScopeMarksOffboarding(t *testing.T) {
	ctx := t.Context()
	w := &purgeWatcher{}
	e := newEnv(t, nil, records.WithPlugins(w))
	if err := e.store.Purge(ctx, "t", e.put(t, "t", "a.png").ID); err != nil {
		t.Fatal(err)
	}
	e.put(t, "gone", "b.png")
	if _, err := e.store.PurgeScope(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(w.seen, ",") != "-,gone" {
		t.Fatalf("PurgingScope seen by the hooks = %v, want [- gone]", w.seen)
	}
}

// serveLog is a ServeHooks plugin that records every read and fails with
// err when set.
type serveLog struct {
	name string
	got  []records.Served
	err  error
}

func (l *serveLog) Name() string { return l.name }
func (l *serveLog) OnServed(_ context.Context, _ *records.Store, sv records.Served) error {
	l.got = append(l.got, sv)
	return l.err
}

// TestServedReports: URL reports ServedAs's kind and subject; an invalid
// read is refused before any hook; a failing hook does not stop the
// next one, its error is returned and URL then issues no link.
func TestServedReports(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("meter down")
	first, second := &serveLog{name: "first"}, &serveLog{name: "second"}
	e := newEnv(t, nil, records.WithPlugins(first, second))
	f := e.put(t, "t", "a.png")
	link, err := e.store.URL(ctx, f, time.Minute, records.ServedAs(records.ServeShare, "lnk_1"))
	if err != nil || link == "" {
		t.Fatalf("URL = %q, %v", link, err)
	}
	if len(second.got) != 1 || second.got[0].Kind != records.ServeShare || second.got[0].Subject != "lnk_1" || second.got[0].At.IsZero() {
		t.Fatalf("reported %+v", second.got)
	}
	wantCode(t, e.store.Served(ctx, records.Served{Kind: "access.bogus"}), filesystem.CodeInvalidArgument)
	wantCode(t, e.store.Served(ctx, records.Served{Kind: records.ServeStream, Bytes: -1}), filesystem.CodeInvalidArgument)
	if len(first.got) != 1 {
		t.Fatalf("an invalid read reached a hook: %+v", first.got)
	}
	first.err = boom
	link, err = e.store.URL(ctx, f, time.Minute)
	if !errors.Is(err, boom) || link != "" {
		t.Fatalf("URL with a failing hook = %q, %v", link, err)
	}
	if len(second.got) != 2 {
		t.Fatalf("a failing hook hid the read from the next one: %d reads", len(second.got))
	}
}

// TestReadFailuresSurface: FindTrash and a limited role's slot lookup
// return a failed read rather than an empty result.
func TestReadFailuresSurface(t *testing.T) {
	onlySQLite(t)
	ctx := t.Context()
	e := newEnv(t, nil, records.WithRoleLimit("gallery", 2))
	f := e.put(t, "t", "a.png")
	exec(t, e.db, "DROP TABLE "+records.TableNameAttachments)
	_, err := e.store.Attach(ctx, "t", f.ID, records.Owner{Type: "post", ID: "p"}, "gallery")
	injected(t, err)
	exec(t, e.db, "DROP TABLE "+records.TableNameFiles)
	n, err := e.store.FindTrash(ctx, time.Now(), func(records.File) { t.Error("found a file") })
	injected(t, err)
	if n != 0 {
		t.Fatalf("FindTrash counted %d", n)
	}
}

// ownerOf is an ObjectOwner whose lookup fails, and a FileHooks plugin
// that refuses every new file, so an upload fails after storing its
// object.
type ownerOf struct{}

func (ownerOf) Name() string { return "failingowner" }
func (ownerOf) OwnedObjects(context.Context, *records.Store, string, []string) (map[string]bool, error) {
	return nil, errors.New("owner lookup down")
}
func (ownerOf) BeforeCreate(context.Context, *records.Store, *records.File) error {
	return filesystem.ErrPermission
}
func (ownerOf) AfterCreate(context.Context, *records.Store, records.File) error { return nil }
func (ownerOf) BeforePurge(context.Context, *records.Store, records.File) error { return nil }

// TestDiscardKeepsObjectOnDoubt: a failed upload whose cleanup cannot ask
// the plugins whether they hold the object keeps it (an orphan for
// SweepOrphans) instead of deleting bytes a plugin may need.
func TestDiscardKeepsObjectOnDoubt(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, nil, records.WithPlugins(ownerOf{}))
	_, err := e.store.Upload(ctx, "t", "media", reader(pngData), "a.png", images)
	wantCode(t, err, filesystem.CodePermission)
	if n := countObjects(t, e.disk); n != 1 {
		t.Fatalf("objects after the failed upload = %d, want the kept one", n)
	}
}

// noModTime is a memory driver that does not declare SetModTime, so a
// renewal rewrites the object (copy aside, move back).
type noModTime struct{ *memory.Driver }

func (d noModTime) Capabilities() filesystem.Capabilities {
	c := d.Driver.Capabilities()
	c.SetModTime = false
	return c
}

// TestRenewalFailuresFailTheUpload: a dedup hit whose renewal cannot
// stat, copy or move the reused object fails the upload, records no row
// and leaves no aside copy, so SweepOrphans is never left to take an
// object a committed row names. Each case fails the n-th operation of its
// kind in turn, until the upload gets past every one of them.
func TestRenewalFailuresFailTheUpload(t *testing.T) {
	boom := errors.New("backend down")
	for _, kind := range []filesystem.OpKind{filesystem.OpStat, filesystem.OpCopy, filesystem.OpMove} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := t.Context()
			failAt, seen := 0, 0
			fail := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
				if op.Kind == kind {
					if seen++; seen == failAt {
						return boom
					}
				}
				return call(ctx)
			}
			disk, err := filesystem.NewDisk("media", noModTime{memory.New()}, filesystem.WithInterceptors(fail))
			if err != nil {
				t.Fatal(err)
			}
			mgr, _ := filesystem.NewManager(disk)
			s, err := records.New(openDB(t), testDialect(), mgr, records.WithClock(func() time.Time { return time.Now().Add(time.Hour) }))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			key, settle := upload.ContentAddressed("blobs", upload.ContentAddressing{Shared: true})
			p := images
			p.Key, p.Settle = key, settle
			if _, err := s.Upload(ctx, "t", "media", reader(pngData), "a.png", p); err != nil {
				t.Fatal(err)
			}
			for n := 1; ; n++ {
				failAt, seen = n, 0
				_, err := s.Upload(ctx, "t", "media", reader(pngData), "b.png", p)
				failAt = 0
				if err == nil {
					break
				}
				if !errors.Is(err, boom) {
					t.Fatalf("%s %d failing: %v", kind, n, err)
				}
				page, err := s.Files(ctx, records.FileQuery{Scope: "t"})
				if err != nil || len(page.Files) != 1 {
					t.Fatalf("rows after the failed upload: %d, %v", len(page.Files), err)
				}
				if c := countObjects(t, disk); c != 1 {
					t.Fatalf("objects after %s %d failed = %d, want the reused one only", kind, n, c)
				}
			}
		})
	}
}

// TestGuardedChangesFaultSweep fails every database step of a guarded
// change and of an attach to a limited role in turn: each failure is the
// injected fault, returned, and the run after the sweep succeeds.
func TestGuardedChangesFaultSweep(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db, nil, records.WithRoleLimit("gallery", 2))
	f := e.put(t, "t", "a.png")
	g := e.put(t, "t", "b.png")
	ops := []struct {
		name string
		run  func() error
	}{
		{"guarded visibility", func() error {
			cur, _ := e.store.File(ctx, "t", f.ID)
			next := filesystem.VisibilityPublic
			if cur.Visibility == next {
				next = filesystem.VisibilityPrivate
			}
			return e.store.SetVisibility(ctx, "t", f.ID, next, records.IfUnchanged(f.SHA256))
		}},
		{"guarded trash", func() error { return e.store.Trash(ctx, "t", f.ID, records.IfUnchanged(f.SHA256)) }},
		{"limited attach", func() error {
			_, err := e.store.Attach(ctx, "t", g.ID, records.Owner{Type: "post", ID: "p"}, "gallery")
			return err
		}},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				faults.Arm(n)
				err := op.run()
				faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
			}
		})
	}
}
