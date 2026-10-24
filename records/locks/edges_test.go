package locks_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/locks"
)

// TestLockTrashedFile: a trashed file cannot be checked out.
func TestLockTrashedFile(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	if err := e.store.Trash(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Minute); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("Lock of a trashed file = %v", err)
	}
}

// TestRacingFirstLocksRetry: a first lock of the same file committing
// between Lock's look for a row and its insert makes the insert a unique
// violation, reported as CodeUnavailable (retry) rather than a failure. A
// SQLite trigger plays the other lock.
func TestRacingFirstLocksRetry(t *testing.T) {
	testdb.OnlySQLite(t)
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	testdb.Exec(t, e.db, "CREATE TRIGGER racer BEFORE INSERT ON "+locks.TableNameLocks+" WHEN NEW."+locks.ColOwner+" != 'bob' BEGIN "+
		"INSERT INTO "+locks.TableNameLocks+" ("+locks.ColScope+", "+locks.ColFileID+", "+locks.ColOwner+", "+locks.ColExpiresAt+", "+locks.ColLockedAt+") "+
		"VALUES (NEW."+locks.ColScope+", NEW."+locks.ColFileID+", 'bob', NEW."+locks.ColExpiresAt+", NEW."+locks.ColLockedAt+"); END")
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Minute); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("a racing first lock = %v, want CodeUnavailable", err)
	}
}

// TestFaultSweep fails every database step of the plugin's operations in
// turn: each failure is the injected fault, returned, never read as "no
// lock", and the run after the sweep has its normal outcome (want).
func TestFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	ops := []struct {
		name string
		want error
		run  func() error
	}{
		{"first lock", nil, func() error { _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Minute); return err }},
		{"extend", nil, func() error { _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); return err }},
		{"lock held by another", locks.ErrLocked, func() error {
			_, err := e.locks.Lock(ctx, e.store, "t", f.ID, "bob", time.Minute)
			return err
		}},
		{"lock of", nil, func() error {
			l, held, err := e.locks.LockOf(ctx, e.store, "t", f.ID)
			if err == nil && (!held || l.Owner != "alice") {
				t.Fatalf("LockOf = %+v, %v", l, held)
			}
			return err
		}},
		{"guarded replace", locks.ErrLocked, func() error { return e.replace(ctx, f, "v2") }},
		{"unlock", nil, func() error { return e.locks.Unlock(ctx, e.store, "t", f.ID, "alice") }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if errors.Is(err, testdb.ErrInjected) {
					continue
				}
				if !errors.Is(err, op.want) {
					t.Fatalf("step %d: %v, want %v", n, err, op.want)
				}
				return
			}
		})
	}
}
