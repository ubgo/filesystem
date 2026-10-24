package audit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

const scope = "t"

type env struct {
	db     *sql.DB
	faults *testdb.Faults
	store  *records.Store
	disk   *filesystem.Disk
	p      *audit.Plugin
	now    time.Time
}

func newEnv(t *testing.T, opts ...audit.Option) *env {
	t.Helper()
	return newEnvStep(t, time.Second, opts...)
}

// newEnvStep is newEnv with a clock that advances step per reading; a
// zero step freezes it, so every change shares one microsecond.
func newEnvStep(t *testing.T, step time.Duration, opts ...audit.Option) *env {
	t.Helper()
	return newEnvWith(t, step, nil, opts...)
}

// newEnvWith is newEnvStep with more plugins installed after audit.
func newEnvWith(t *testing.T, step time.Duration, more []records.Plugin, opts ...audit.Option) *env {
	t.Helper()
	e := &env{p: audit.New(opts...), now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	e.db, e.faults = testdb.OpenFaulty(t)
	e.disk, _ = filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(e.disk)
	var err error
	e.store, err = records.New(e.db, testdb.Dialect(), mgr, records.WithPlugins(append([]records.Plugin{e.p}, more...)...), records.WithOwnerTypes("post"),
		records.WithClock(func() time.Time { e.now = e.now.Add(step); return e.now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) file(ctx context.Context, t *testing.T, sc, name string) records.File {
	t.Helper()
	if _, err := e.disk.WriteBytes(ctx, sc+"/"+name, []byte(name), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := e.store.CreateFile(ctx, records.NewFile{Scope: sc, Disk: "media", Key: sc + "/" + name})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// render shows entries as "actor kind" newest first.
func render(page audit.Page) string {
	var out []string
	for _, en := range page.Entries {
		out = append(out, en.Actor+" "+string(en.Kind))
	}
	return strings.Join(out, "; ")
}

func (e *env) entries(t *testing.T, q audit.Query) audit.Page {
	t.Helper()
	page, err := e.p.Entries(t.Context(), e.store, q)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func TestLog(t *testing.T) {
	alice := audit.WithActor(t.Context(), "user:alice")
	bob := audit.WithActor(t.Context(), "user:bob")
	e := newEnv(t)
	post := records.Owner{Type: "post", ID: "p1"}
	a := e.file(alice, t, scope, "a.png")
	b := e.file(bob, t, scope, "b.png")
	e.file(alice, t, "other", "o.png")
	att, _ := e.store.Attach(alice, scope, a.ID, post, "gallery")
	_ = e.store.Trash(bob, scope, a.ID)
	_ = e.store.Purge(t.Context(), scope, b.ID) // no actor: a background job

	all := e.entries(t, audit.Query{Scope: scope})
	if got := render(all); got != " file.purged; user:bob file.trashed; user:alice attachment.created; user:bob file.created; user:alice file.created" {
		t.Fatalf("log = %s", got)
	}
	created := all.Entries[2]
	if !strings.HasPrefix(created.ID, audit.IDPrefix) || created.Scope != scope || created.FileID != a.ID || created.AttachmentID != att.ID ||
		created.Owner != post || created.Role != "gallery" || created.At.Location() != time.UTC || created.At.IsZero() {
		t.Fatalf("attachment entry = %+v", created)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope, FileID: a.ID})); got != "user:bob file.trashed; user:alice attachment.created; user:alice file.created" {
		t.Fatalf("history of a = %s", got)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope, Actor: "user:alice"})); got != "user:alice attachment.created; user:alice file.created" {
		t.Fatalf("activity of alice = %s", got)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope, FileID: b.ID})); got != " file.purged; user:bob file.created" {
		t.Fatalf("a purged file keeps its history: %s", got)
	}

	var pages []string
	q := audit.Query{Scope: scope, Limit: 2}
	for {
		page := e.entries(t, q)
		pages = append(pages, render(page))
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if strings.Join(pages, "; ") != render(all) || len(pages) != 3 {
		t.Fatalf("paged = %q", pages)
	}

	for _, bad := range []audit.Query{{Scope: scope, Limit: -1}, {Scope: scope, Limit: audit.MaxPageSize + 1}, {Scope: scope, Cursor: "!"}} {
		if _, err := e.p.Entries(t.Context(), e.store, bad); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if audit.ActorFrom(t.Context()) != "" {
		t.Fatal("an actor without WithActor")
	}
}

func TestActorFuncAndLimit(t *testing.T) {
	type userKey struct{}
	e := newEnv(t, audit.WithActorFunc(func(ctx context.Context) string { u, _ := ctx.Value(userKey{}).(string); return u }))
	ctx := context.WithValue(t.Context(), userKey{}, "u42")
	f := e.file(ctx, t, scope, "a.png")
	if got := render(e.entries(t, audit.Query{Scope: scope})); got != "u42 file.created" {
		t.Fatalf("log = %s", got)
	}

	long := context.WithValue(t.Context(), userKey{}, strings.Repeat("x", audit.MaxActorBytes+1))
	if err := e.store.Trash(long, scope, f.ID); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an over-long actor = %v", err)
	}
	if got, _ := e.store.File(t.Context(), scope, f.ID); got.DeletedAt != nil {
		t.Fatal("the change went through without its entry")
	}
}

// TestRollbackLeavesNoEntry proves entries share the change's transaction.
func TestRollbackLeavesNoEntry(t *testing.T) {
	e := newEnv(t)
	f := e.file(t.Context(), t, scope, "a.png")
	boom := errors.New("later step failed")
	err := e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if err := e.store.Trash(ctx, scope, f.ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope})); got != " file.created" {
		t.Fatalf("log = %s", got)
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	f := e.file(t.Context(), t, scope, "a.png") // logged at 12:00:01
	cutoff := e.now.Add(time.Second)            // between the two entries
	_ = e.store.Trash(t.Context(), scope, f.ID) // logged at 12:00:02 or later
	n, err := e.p.Prune(t.Context(), e.store, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope})); got != " file.trashed" {
		t.Fatalf("log after pruning = %s", got)
	}
}

// TestSameMicrosecondOrder: entries stamped with the same time list in
// the order they were written, newest first, on every page. With random
// IDs as the tie-breaker the order of these 30 entries would be random.
func TestSameMicrosecondOrder(t *testing.T) {
	e := newEnvStep(t, 0)
	const n = 30
	var want []string
	for i := range n {
		actor := fmt.Sprintf("user:%02d", i)
		e.file(audit.WithActor(t.Context(), actor), t, scope, fmt.Sprintf("f%02d", i))
		want = append([]string{actor + " file.created"}, want...)
	}
	all := e.entries(t, audit.Query{Scope: scope, Limit: n})
	if got := render(all); got != strings.Join(want, "; ") {
		t.Fatalf("same-microsecond entries out of write order:\n got %s\nwant %s", got, strings.Join(want, "; "))
	}
	if !all.Entries[0].At.Equal(all.Entries[n-1].At) {
		t.Fatal("the clock was not frozen")
	}
	var pages []string
	q := audit.Query{Scope: scope, Limit: 7}
	for {
		page := e.entries(t, q)
		pages = append(pages, render(page))
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	if got := strings.Join(pages, "; "); got != render(all) {
		t.Fatalf("paged = %s", got)
	}
}

// TestPruneScope: retention applied to one scope leaves every other
// scope's entries, older ones included.
func TestPruneScope(t *testing.T) {
	e := newEnv(t)
	other := e.file(t.Context(), t, "other", "o.png") // 12:00:01
	f := e.file(t.Context(), t, scope, "a.png")       // 12:00:02
	cutoff := e.now.Add(time.Second)                  // after both
	_ = e.store.Trash(t.Context(), scope, f.ID)       // 12:00:03 or later
	n, err := e.p.PruneScope(t.Context(), e.store, scope, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("PruneScope = %d, %v", n, err)
	}
	if got := render(e.entries(t, audit.Query{Scope: scope})); got != " file.trashed" {
		t.Fatalf("log of the pruned scope = %s", got)
	}
	if got := e.entries(t, audit.Query{Scope: "other"}); render(got) != " file.created" || got.Entries[0].FileID != other.ID {
		t.Fatalf("another scope's log was touched: %s", render(got))
	}
	// A cutoff after the last entry empties the scope: an offboarding.
	if n, err := e.p.PruneScope(t.Context(), e.store, scope, e.now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("PruneScope all = %d, %v", n, err)
	}
	if e.count(t, audit.TableNameAudit) != 1 {
		t.Fatal("offboarding one scope deleted another's entries")
	}
}

func (e *env) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestFaultSweep fails every database step of a logged change in turn:
// the change and its entry stand or fall together.
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	var files []records.File
	for _, name := range []string{"a", "b", "c"} {
		files = append(files, e.file(ctx, t, scope, name))
	}
	ops := []struct {
		name string
		run  func() error
	}{
		{"trash", func() error { return e.store.Trash(ctx, scope, files[0].ID) }},
		{"purge", func() error { return e.store.Purge(ctx, scope, files[1].ID) }},
		{"entries", func() error { _, err := e.p.Entries(ctx, e.store, audit.Query{Scope: scope}); return err }},
		{"prune", func() error { _, err := e.p.Prune(ctx, e.store, time.Time{}); return err }},
		{"prune scope", func() error { _, err := e.p.PruneScope(ctx, e.store, scope, time.Time{}); return err }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := [2]int{e.count(t, audit.TableNameAudit), e.count(t, records.TableNameFiles)}
				trashed, _ := e.store.File(ctx, scope, files[0].ID)
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: err = %v", n, err)
				}
				after := [2]int{e.count(t, audit.TableNameAudit), e.count(t, records.TableNameFiles)}
				still, _ := e.store.File(ctx, scope, files[0].ID)
				if after != before || (trashed.DeletedAt == nil) != (still.DeletedAt == nil) {
					t.Fatalf("step %d failed but changed something: %v → %v", n, before, after)
				}
			}
		})
	}
}

// TestLogsFileMoved: a file moved to another disk is logged with the old
// and new disk in its detail.
func TestLogsFileMoved(t *testing.T) {
	ctx := audit.WithActor(t.Context(), "job:promote")
	p := audit.New()
	db, _ := testdb.OpenFaulty(t)
	fast, _ := filesystem.NewDisk("fast", memory.New())
	kept, _ := filesystem.NewDisk("kept", memory.New())
	mgr, _ := filesystem.NewManager(fast, kept)
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fast.WriteBytes(ctx, "a.txt", []byte("a"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := s.CreateFile(ctx, records.NewFile{Scope: "t", Disk: "fast", Key: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveFile(ctx, "t", f.ID, "kept"); err != nil {
		t.Fatal(err)
	}
	page, err := p.Entries(ctx, s, audit.Query{Scope: "t", FileID: f.ID})
	if err != nil {
		t.Fatal(err)
	}
	moved := page.Entries[0]
	if moved.Kind != records.EventFileMoved || moved.Actor != "job:promote" ||
		moved.Detail[records.DetailFrom] != "fast" || moved.Detail[records.DetailTo] != "kept" {
		t.Fatalf("entry = %+v", moved)
	}
}
