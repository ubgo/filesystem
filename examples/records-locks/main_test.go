package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/locks"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"alice checked out until 09:30", "bob check out: locked (conflict)", "alice saves: ok",
		"bob saves: locked (conflict)", "bob trashes: locked (conflict)", "purge with no actor: locked (conflict)",
		"alice asks for 3h: invalid_argument", "held true by user:alice", "31 minutes later, bob saves: ok",
		"alice checks in bob's lock: ok, still held true", "admin breaks it: held false", "sweep removed 1 expired lock(s)",
		"token lock: alice's laptop got a token starting lkt_", "token lock: laptop saves with its token: ok",
		"token lock: tablet saves as alice without it: locked (conflict)", "token lock: extending keeps the token: true",
		"shared lock: alice and bob hold design.txt, own tokens: true", "shared lock: alice saves with her token: ok",
		"shared lock: bob saves with his token: ok", "shared lock: bob saves with alice's token: locked (conflict)",
		"shared lock: carol saves: locked (conflict)", "shared lock: carol checks out alone: locked (conflict)",
		"shared lock: after alice unlocks, held by user:bob", "shared lock: without WithSharedLocks: unsupported",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestTokenHolderIsWhatRefuses pins that the refusal comes from
// TokenHolder: the same token-less save as the lock's owner passes under
// the default holder, while under TokenHolder a wrong token is refused
// like a missing one.
func TestTokenHolderIsWhatRefuses(t *testing.T) {
	ctx := t.Context()
	open := func(t *testing.T) *sql.DB {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	for name, build := range map[string]func(*sql.DB) (*records.Store, *locks.Plugin, error){
		"default": func(db *sql.DB) (*records.Store, *locks.Plugin, error) { return newStore(ctx, db, time.Now) },
		"token":   func(db *sql.DB) (*records.Store, *locks.Plugin, error) { return newTokenStore(ctx, db, time.Now) },
	} {
		t.Run(name, func(t *testing.T) {
			s, lp, err := build(open(t))
			if err != nil {
				t.Fatal(err)
			}
			f, err := s.Upload(records.WithActor(ctx, alice), tenant, diskName, strings.NewReader("a"), "a.txt", docs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := checkOut(ctx, s, lp, f.ID, alice); err != nil {
				t.Fatal(err)
			}
			for _, token := range []string{"", locks.TokenPrefix + "not-the-token"} {
				err := saveWithToken(ctx, s, f.ID, alice, token, "b")
				if refused := errors.Is(err, locks.ErrLocked); refused != (name == "token") {
					t.Errorf("save with token %q = %v", token, err)
				}
			}
		})
	}
}

// TestSharedLockIsWhatRefuses pins that Carol is refused because of the
// shared locks: on the same Store with no lock held she saves, and once
// every holder has unlocked she saves again.
func TestSharedLockIsWhatRefuses(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s, lp, err := newSharedStore(ctx, db, time.Now, locks.WithSharedLocks())
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Upload(records.WithActor(ctx, alice), tenant, diskName, strings.NewReader("a"), "a.txt", docs)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWithToken(ctx, s, f.ID, carol, "", "b"); err != nil {
		t.Fatalf("save with no lock held = %v", err)
	}
	if _, _, err := coEdit(ctx, s, lp, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := saveWithToken(ctx, s, f.ID, carol, "", "c"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("save under shared locks = %v, want ErrLocked", err)
	}
	for _, u := range []string{alice, bob} {
		if err := lp.UnlockShared(ctx, s, tenant, f.ID, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := saveWithToken(ctx, s, f.ID, carol, "", "d"); err != nil {
		t.Fatalf("save after every holder unlocked = %v", err)
	}
}
