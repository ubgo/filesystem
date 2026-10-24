package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

func testStore(t *testing.T) (*records.Store, *sql.DB, *Clock) {
	t.Helper()
	db, err := openSQLite(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &Clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var failed []string
	s, err := newStore(t.Context(), db, clock, &failed)
	if err != nil {
		t.Fatal(err)
	}
	return s, db, clock
}

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(t.Context(), &out, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"after Trash: live=0 trashed=1",
		"after Restore: live=1 trashed=0",
		"rolled-back purge: err=request failed after the purge row kept=true object kept=true",
		"purged: row=not_found object kept=false",
		"PurgeOwner: purged=1 err=<nil> bob still has 1 file(s)",
		"SweepTrash: purged=1 err=<nil> still in trash=1",
		"Purge outside a Tx returns the object error: read_only",
		`Purge inside a Tx: err=<nil> handler got ["archive/archive/b.txt: read_only"]`,
		"retention dry run: detach 2, purge 1; real run: detached 2, purged 1",
		"old chat photo: not_found; logo still used by a user: true",
		"story now: 200; view-once first: 200, second: 404",
		"a day later, SweepExpired purged 2 (the story and the viewed note)",
		"takedown: 1 kept version(s) before, 0 after",
		"retention by plan, free-co after 60 days: detached 1, purged 1",
		"retention by plan, pro-co after 60 days: detached 0, purged 0",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestDeleteUserRollsBackTogether runs deleteUser inside an outer
// transaction that then fails: the user row and the user's file must both
// come back, and the object must never have been deleted.
func TestDeleteUserRollsBackTogether(t *testing.T) {
	s, db, _ := testStore(t)
	if _, err := db.ExecContext(t.Context(), "INSERT INTO users (id, email) VALUES ('usr_9', 'x@example.com')"); err != nil {
		t.Fatal(err)
	}
	f, err := store(t.Context(), s, diskName, "u.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Attach(t.Context(), tenant, f.ID, records.Owner{Type: ownerUser, ID: "usr_9"}, roleUpload); err != nil {
		t.Fatal(err)
	}
	err = s.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		if _, err := deleteUser(ctx, s, "usr_9"); err != nil {
			return err
		}
		return errAbort
	})
	if !errors.Is(err, errAbort) {
		t.Fatalf("Tx = %v", err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&n); err != nil || n != 1 {
		t.Fatalf("users = %d, %v; want the row back", n, err)
	}
	if _, err := s.File(t.Context(), tenant, f.ID); err != nil {
		t.Fatalf("file row after rollback: %v", err)
	}
	if ok, _ := objectExists(t.Context(), s, f); !ok {
		t.Fatal("object deleted by a rolled-back PurgeOwner")
	}
}

func TestDeleteUserDeletesRow(t *testing.T) {
	s, db, _ := testStore(t)
	if _, err := db.ExecContext(t.Context(), "INSERT INTO users (id, email) VALUES ('usr_9', 'x@example.com')"); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteUser(t.Context(), s, "usr_9"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&n); err != nil || n != 0 {
		t.Fatalf("users left = %d, %v", n, err)
	}
}

func TestRetrashKeepsTheClock(t *testing.T) {
	s, _, clock := testStore(t)
	f, err := store(t.Context(), s, diskName, "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trash(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := s.File(t.Context(), tenant, f.ID)
	clock.now = clock.now.Add(time.Hour)
	if err := s.Trash(t.Context(), tenant, f.ID); err != nil {
		t.Fatal(err)
	}
	again, _ := s.File(t.Context(), tenant, f.ID)
	if !first.DeletedAt.Equal(*again.DeletedAt) {
		t.Fatalf("re-trash moved DeletedAt from %v to %v", first.DeletedAt, again.DeletedAt)
	}
}

func TestPurgeThenFailKeepsEverything(t *testing.T) {
	s, _, _ := testStore(t)
	f, err := store(t.Context(), s, diskName, "y.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := purgeThenFail(t.Context(), s, f); !errors.Is(err, errAbort) {
		t.Fatalf("purgeThenFail = %v", err)
	}
	if ok, _ := objectExists(t.Context(), s, f); !ok {
		t.Fatal("object deleted by a rolled-back purge")
	}
}

// TestRetentionByPlanRefusesAnUnregisteredOwnerType pins that a per-scope
// rule naming an owner type the Store did not register is refused with
// invalid_argument and sweeps nothing.
func TestRetentionByPlanRefusesAnUnregisteredOwnerType(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &Clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s, err := newPlanStore(t.Context(), db, clock)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SweepRetentionIn(t.Context(), "free-co", []records.Retention{{OwnerType: "invoice", MaxAge: freeKeep}}, clock.Now(), false)
	if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("rule for an unregistered owner type = %v, want invalid_argument", err)
	}
}
