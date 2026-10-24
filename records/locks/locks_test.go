package locks_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/upload"
)

var anything = upload.Policy{MaxSize: 1 << 10, Allowed: []string{"*/*"}}

type env struct {
	store  *records.Store
	locks  *locks.Plugin
	now    time.Time
	db     *sql.DB
	faults *testdb.Faults
}

func newEnv(t *testing.T, opts ...locks.Option) *env {
	t.Helper()
	e := &env{now: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	db, faults := testdb.OpenFaulty(t)
	e.db, e.faults = db, faults
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	e.locks = locks.New(append([]locks.Option{locks.WithClock(func() time.Time { return e.now })}, opts...)...)
	var err error
	if e.store, err = records.New(db, testdb.Dialect(), mgr, records.WithPlugins(e.locks)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) file(t *testing.T) records.File {
	t.Helper()
	f, err := e.store.Upload(t.Context(), "t", "media", strings.NewReader("v1"), "doc.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *env) replace(ctx context.Context, f records.File, body string) error {
	_, err := e.store.Replace(ctx, "t", f.ID, strings.NewReader(body), "", anything)
	return err
}

// TestCheckOut: while alice holds the lock only she may replace the file;
// bob cannot lock it; once it expires, or she unlocks it, anyone may.
func TestCheckOut(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	alice, bob := records.WithActor(ctx, "user:alice"), records.WithActor(ctx, "user:bob")
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.replace(bob, f, "bob's"); !errors.Is(err, locks.ErrLocked) || filesystem.CodeOf(err) != filesystem.CodeConflict {
		t.Fatalf("bob replacing a file alice checked out = %v", err)
	}
	if err := e.replace(ctx, f, "anonymous"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("a replacement with no actor = %v", err)
	}
	if err := e.replace(alice, f, "alice's"); err != nil {
		t.Fatalf("alice replacing her checked-out file = %v", err)
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:bob", time.Hour); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("bob locking alice's file = %v", err)
	}
	if err := e.locks.Unlock(ctx, e.store, "t", f.ID, "user:bob"); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := e.locks.LockOf(ctx, e.store, "t", f.ID); !held {
		t.Fatal("bob's unlock removed alice's lock")
	}
	// Extending her own lock, then letting it expire.
	l, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:alice", time.Minute)
	if err != nil || !l.ExpiresAt.Equal(e.now.Add(time.Minute)) {
		t.Fatalf("extend = %+v, %v", l, err)
	}
	e.now = e.now.Add(2 * time.Minute)
	if err := e.replace(bob, f, "after expiry"); err != nil {
		t.Fatalf("bob after the lock expired = %v", err)
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:bob", time.Hour); err != nil {
		t.Fatalf("bob taking an expired lock = %v", err)
	}
	if err := e.locks.Unlock(ctx, e.store, "t", f.ID, "user:bob"); err != nil {
		t.Fatal(err)
	}
	if err := e.replace(alice, f, "free"); err != nil {
		t.Fatalf("a replacement once unlocked = %v", err)
	}
}

// TestBreakSweepPurge: Break removes anyone's lock; Sweep removes expired
// ones only; a purge by the lock's owner removes the file's lock.
func TestBreakSweepPurge(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	a, b, c := e.file(t), e.file(t), e.file(t)
	for i, f := range []records.File{a, b, c} {
		if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:x", time.Duration(i+1)*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.locks.Break(ctx, e.store, "t", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, held, _ := e.locks.LockOf(ctx, e.store, "t", a.ID); held {
		t.Fatal("Break kept the lock")
	}
	if n, err := e.locks.Sweep(ctx, e.store, e.now.Add(150*time.Minute)); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want b's", n, err)
	}
	if err := e.store.Purge(records.WithActor(ctx, "user:x"), "t", c.ID); err != nil {
		t.Fatal(err)
	}
	if l, held, err := e.locks.LockOf(ctx, e.store, "t", c.ID); err != nil || held || l.Owner != "" {
		t.Fatalf("a purged file's lock = %+v %v %v", l, held, err)
	}
	for _, bad := range []struct {
		owner string
		ttl   time.Duration
	}{{"", time.Hour}, {"u", 0}, {strings.Repeat("x", locks.MaxOwnerBytes+1), time.Hour}} {
		if _, err := e.locks.Lock(ctx, e.store, "t", a.ID, bad.owner, bad.ttl); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", "fil_missing", "u", time.Hour); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("locking an unknown file = %v", err)
	}
}

// TestOwnerFunc: the owner can come from the application's own context
// key instead of records.WithActor.
func TestOwnerFunc(t *testing.T) {
	type userKey struct{}
	ctx := t.Context()
	e := newEnv(t, locks.WithOwnerFunc(func(ctx context.Context) string { u, _ := ctx.Value(userKey{}).(string); return u }))
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "u42", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.replace(context.WithValue(ctx, userKey{}, "u42"), f, "mine"); err != nil {
		t.Fatalf("the owner by the application's key = %v", err)
	}
	if err := e.replace(records.WithActor(ctx, "u42"), f, "actor"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("WithActor ignored under WithOwnerFunc = %v", err)
	}
}

// TestTrashPurgeGuarded: while alice holds the lock, bob (and a purge with
// no actor) cannot trash or purge the file, and nothing changes; changing
// its visibility stays allowed; alice may trash and purge it, and after a
// Break so may bob.
func TestTrashPurgeGuarded(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	alice, bob := records.WithActor(ctx, "user:alice"), records.WithActor(ctx, "user:bob")
	f, g := e.file(t), e.file(t)
	for _, x := range []records.File{f, g} {
		if _, err := e.locks.Lock(ctx, e.store, "t", x.ID, "user:alice", time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.Trash(bob, "t", f.ID); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("bob's Trash = %v", err)
	}
	for _, c := range []context.Context{bob, ctx} {
		if err := e.store.Purge(c, "t", f.ID); !errors.Is(err, locks.ErrLocked) {
			t.Fatalf("a purge not by the owner = %v", err)
		}
	}
	if cur, err := e.store.File(ctx, "t", f.ID); err != nil || cur.DeletedAt != nil {
		t.Fatalf("after refused changes = %+v, %v", cur, err)
	}
	if _, held, _ := e.locks.LockOf(ctx, e.store, "t", f.ID); !held {
		t.Fatal("a refused purge removed the lock")
	}
	if err := e.store.SetVisibility(bob, "t", f.ID, filesystem.VisibilityPublic); err != nil {
		t.Fatalf("bob's SetVisibility = %v", err)
	}
	if err := e.store.Trash(alice, "t", f.ID); err != nil {
		t.Fatalf("alice's Trash = %v", err)
	}
	if err := e.store.Purge(alice, "t", f.ID); err != nil {
		t.Fatalf("alice's Purge = %v", err)
	}
	if err := e.locks.Break(ctx, e.store, "t", g.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Trash(bob, "t", g.ID); err != nil {
		t.Fatalf("bob's Trash after a Break = %v", err)
	}
}

// TestMaxTTL: a lock longer than the maximum is refused, the default
// maximum is DefaultMaxTTL, and WithMaxTTL changes it.
func TestMaxTTL(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "u", locks.DefaultMaxTTL+time.Second); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a lock past DefaultMaxTTL = %v", err)
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "u", locks.DefaultMaxTTL); err != nil {
		t.Fatalf("a lock of DefaultMaxTTL = %v", err)
	}
	short := newEnv(t, locks.WithMaxTTL(time.Minute))
	g := short.file(t)
	if _, err := short.locks.Lock(ctx, short.store, "t", g.ID, "u", 2*time.Minute); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a lock past WithMaxTTL = %v", err)
	}
}

// TestPurgeScopeIgnoresLocks: offboarding a tenant removes its locked
// files and their locks; a lock never blocks PurgeScope.
func TestPurgeScopeIgnoresLocks(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.PurgeScope(records.WithActor(ctx, "user:bob"), "t"); err != nil || n != 1 {
		t.Fatalf("PurgeScope = %d, %v", n, err)
	}
	if _, err := e.store.File(ctx, "t", f.ID); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("the locked file survived: %v", err)
	}
	if _, held, err := e.locks.LockOf(ctx, e.store, "t", f.ID); err != nil || held {
		t.Fatalf("its lock survived: %v %v", held, err)
	}
}

// purgeElsewhere purges another scope's file from inside a PurgeScope, with
// that PurgeScope's context, and keeps the result.
type purgeElsewhere struct {
	scope, id string
	err       error
	ran       bool
}

func (p *purgeElsewhere) Name() string { return "purge_elsewhere" }
func (p *purgeElsewhere) BeforeCreate(context.Context, *records.Store, *records.File) error {
	return nil
}
func (p *purgeElsewhere) AfterCreate(context.Context, *records.Store, records.File) error {
	return nil
}
func (p *purgeElsewhere) BeforePurge(ctx context.Context, s *records.Store, f records.File) error {
	if p.ran || f.Scope == p.scope {
		return nil
	}
	p.ran = true
	p.err = s.Purge(ctx, p.scope, p.id)
	return nil
}

// TestPurgeScopeBypassIsScoped: offboarding scope "t" lets "t"'s locked
// files go, but a purge of another scope's locked file made with that
// context is still refused.
func TestPurgeScopeBypassIsScoped(t *testing.T) {
	ctx := t.Context()
	db, _ := testdb.OpenFaulty(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	lk := locks.New()
	other := &purgeElsewhere{scope: "u"}
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(lk, other))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upload(ctx, "t", "media", strings.NewReader("v1"), "a.txt", anything); err != nil {
		t.Fatal(err)
	}
	locked, err := s.Upload(ctx, "u", "media", strings.NewReader("v1"), "b.txt", anything)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lk.Lock(ctx, s, "u", locked.ID, "user:alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	other.id = locked.ID
	if _, err := s.PurgeScope(records.WithActor(ctx, "user:bob"), "t"); err != nil {
		t.Fatal(err)
	}
	if !other.ran || !errors.Is(other.err, locks.ErrLocked) {
		t.Fatalf("cross-scope purge under PurgeScope(t): ran %v, %v; want ErrLocked", other.ran, other.err)
	}
	if _, err := s.File(ctx, "u", locked.ID); err != nil {
		t.Fatalf("the other scope's locked file went: %v", err)
	}
}
