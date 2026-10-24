package locks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/sqlb"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/locks"
)

// TestLockTokens: a lock gets a fresh token, its owner keeps it while
// extending the lock, and a new taking after expiry gets a new one.
func TestLockTokens(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	l, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour)
	if err != nil || !strings.HasPrefix(l.Token, locks.TokenPrefix) || len(l.Token) < len(locks.TokenPrefix)+20 {
		t.Fatalf("Lock = %+v, %v", l, err)
	}
	again, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", 2*time.Hour)
	if err != nil || again.Token != l.Token {
		t.Fatalf("extending changed the token: %q -> %q, %v", l.Token, again.Token, err)
	}
	if got, held, _ := e.locks.LockOf(ctx, e.store, "t", f.ID); !held || got.Token != l.Token {
		t.Fatalf("LockOf = %+v", got)
	}
	e.now = e.now.Add(3 * time.Hour) // expired
	next, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour)
	if err != nil || next.Token == l.Token || next.Token == "" {
		t.Fatalf("a new taking kept the old token: %q, %v", next.Token, err)
	}
}

// TestTokenHolder: with TokenHolder the owner must present the token, so
// one user's second client is refused; the right token passes.
func TestTokenHolder(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithHolderFunc(locks.TokenHolder))
	f := e.file(t)
	l, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	alice := records.WithActor(ctx, "alice")
	if err := e.replace(alice, f, "no token"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("owner without the token = %v", err)
	}
	if err := e.replace(locks.WithLockToken(alice, "lkt_wrong"), f, "wrong token"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("owner with a wrong token = %v", err)
	}
	if err := e.replace(locks.WithLockToken(records.WithActor(ctx, "bob"), l.Token), f, "stolen token"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("another owner with the token = %v", err)
	}
	if err := e.replace(locks.WithLockToken(alice, l.Token), f, "v2"); err != nil {
		t.Fatalf("owner with the token = %v", err)
	}
	// A lock taken before tokens existed is held by its owner alone.
	legacy := locks.Lock{Scope: "t", FileID: f.ID, Owner: "alice"}
	if ok, err := locks.TokenHolder(ctx, locks.HolderRequest{Lock: legacy, Owner: "alice"}); !ok || err != nil {
		t.Fatalf("owner of a tokenless lock = %v, %v", ok, err)
	}
	if ok, _ := locks.TokenHolder(ctx, locks.HolderRequest{Lock: legacy, Owner: "bob"}); ok {
		t.Fatal("another owner holds a tokenless lock")
	}
}

var errPolicy = errors.New("policy engine down")

// TestHolderFuncCustom: a HolderFunc of the application's allows what it
// says, refuses what it says, and an error refuses the change with it.
func TestHolderFuncCustom(t *testing.T) {
	ctx := t.Context()
	var answer func(locks.HolderRequest) (bool, error)
	var asked locks.HolderRequest
	e := newEnv(t, locks.WithHolderFunc(func(_ context.Context, req locks.HolderRequest) (bool, error) {
		asked = req
		return answer(req)
	}))
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	lead := records.WithActor(ctx, "lead")

	answer = func(r locks.HolderRequest) (bool, error) { return r.Owner == "lead", nil } // the team lead passes
	if err := e.replace(lead, f, "by lead"); err != nil {
		t.Fatalf("allowed = %v", err)
	}
	if asked.Owner != "lead" || asked.Lock.Owner != "alice" || asked.Lock.FileID != f.ID {
		t.Fatalf("asked %+v", asked)
	}
	answer = func(locks.HolderRequest) (bool, error) { return false, nil }
	if err := e.replace(records.WithActor(ctx, "alice"), f, "by owner"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("denied = %v", err)
	}
	answer = func(locks.HolderRequest) (bool, error) { return true, errPolicy }
	if err := e.replace(lead, f, "on error"); !errors.Is(err, errPolicy) {
		t.Fatalf("error = %v", err)
	}
	if err := e.store.Trash(lead, "t", f.ID); !errors.Is(err, errPolicy) {
		t.Fatalf("trash on error = %v", err)
	}
}

// TestDefaultHolder: without WithHolderFunc the owner passes with no
// token and anyone else is refused, as before.
func TestDefaultHolder(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.replace(records.WithActor(ctx, "bob"), f, "x"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("other owner = %v", err)
	}
	if err := e.replace(records.WithActor(ctx, "alice"), f, "y"); err != nil {
		t.Fatalf("owner = %v", err)
	}
	if ok, _ := locks.DefaultHolder(ctx, locks.HolderRequest{Owner: "a", Lock: locks.Lock{Owner: "a"}}); !ok {
		t.Fatal("DefaultHolder refused the owner")
	}
}

// TestTokenlessLockGetsATokenOnExtend: a lock stored without a token
// (taken before migration 0002_lock_tokens, or imported from another
// system with no token) gets a fresh one the next time its owner takes it,
// so TokenHolder's owner-only fallback lasts at most one lock term.
func TestTokenlessLockGetsATokenOnExtend(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Exec(ctx, sqlb.NewUpdate(e.store.Dialect()).Table(locks.Table).Set(sqlb.Set(locks.Token, "")).
		Where(sqlb.Eq(locks.FileID, f.ID))); err != nil {
		t.Fatal(err)
	}
	if got, held, _ := e.locks.LockOf(ctx, e.store, "t", f.ID); !held || got.Token != "" {
		t.Fatalf("setup: LockOf = %+v", got)
	}
	l, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour)
	if err != nil || !strings.HasPrefix(l.Token, locks.TokenPrefix) {
		t.Fatalf("extending a tokenless lock = %+v, %v", l, err)
	}
	if got, _, _ := e.locks.LockOf(ctx, e.store, "t", f.ID); got.Token != l.Token {
		t.Fatalf("stored token %q, handed out %q", got.Token, l.Token)
	}
}
