package locks_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/locks"
)

// TestSharedLocksHolders: two subjects hold a shared lock together; each
// may change the file and a third may not; one leaving keeps the other's.
func TestSharedLocksHolders(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithSharedLocks())
	f := e.file(t)
	a, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ben", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != locks.Shared || a.Token == "" || a.Token == b.Token {
		t.Fatalf("shared locks %+v %+v: each holder needs its own token", a, b)
	}
	held, err := e.locks.SharedLocksOf(ctx, e.store, "t", f.ID)
	if err != nil || len(held) != 2 || held[0].Owner != "user:ann" || held[1].Token != b.Token {
		t.Fatalf("SharedLocksOf = %+v, %v", held, err)
	}
	if err := e.replace(records.WithActor(ctx, "user:ann"), f, "ann's"); err != nil {
		t.Fatalf("a shared holder replacing = %v", err)
	}
	if err := e.replace(records.WithActor(ctx, "user:ben"), f, "ben's"); err != nil {
		t.Fatalf("the other shared holder replacing = %v", err)
	}
	if err := e.replace(records.WithActor(ctx, "user:cat"), f, "cat's"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("a subject without a shared lock replacing = %v", err)
	}
	if err := e.locks.UnlockShared(ctx, e.store, "t", f.ID, "user:ann"); err != nil {
		t.Fatal(err)
	}
	if err := e.replace(records.WithActor(ctx, "user:ann"), f, "again"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("ann after leaving = %v", err)
	}
	if err := e.replace(records.WithActor(ctx, "user:ben"), f, "still"); err != nil {
		t.Fatalf("ben kept his shared lock: %v", err)
	}
	// Extending keeps the token.
	again, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ben", 2*time.Hour)
	if err != nil || again.Token != b.Token {
		t.Fatalf("extending a shared lock = %+v, %v", again, err)
	}
}

// TestSharedAndExclusiveExclude: an exclusive lock refuses shared ones and
// the other way round; a subject's own shared lock is given up for its
// exclusive one; Break clears both.
func TestSharedAndExclusiveExclude(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithSharedLocks())
	f := e.file(t)
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:ann", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ben", time.Hour); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("a shared lock under an exclusive one = %v", err)
	}
	if _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Hour); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("the exclusive holder taking a shared lock = %v", err)
	}
	must(t, e.locks.Unlock(ctx, e.store, "t", f.ID, "user:ann"))
	if _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ben", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:ann", time.Hour); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("an exclusive lock over another's shared one = %v", err)
	}
	if _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "user:ben", time.Hour); err != nil {
		t.Fatalf("the only shared holder upgrading = %v", err)
	}
	if held, _ := e.locks.SharedLocksOf(ctx, e.store, "t", f.ID); len(held) != 0 {
		t.Fatalf("the upgrade kept the shared lock: %+v", held)
	}
	must(t, e.locks.Unlock(ctx, e.store, "t", f.ID, "user:ben"))
	for _, o := range []string{"user:x", "user:y"} {
		if _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, o, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	must(t, e.locks.Break(ctx, e.store, "t", f.ID))
	if held, _ := e.locks.SharedLocksOf(ctx, e.store, "t", f.ID); len(held) != 0 {
		t.Fatalf("Break left shared locks: %+v", held)
	}
	if err := e.replace(records.WithActor(ctx, "user:z"), f, "free"); err != nil {
		t.Fatalf("after Break = %v", err)
	}
}

// TestSharedTokensPerHolder: under TokenHolder each shared holder must
// present its own token; another holder's does not pass.
func TestSharedTokensPerHolder(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithSharedLocks(), locks.WithHolderFunc(locks.TokenHolder))
	f := e.file(t)
	a, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Hour)
	must(t, err)
	b, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ben", time.Hour)
	must(t, err)
	ann := records.WithActor(ctx, "user:ann")
	if err := e.replace(ann, f, "no token"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("no token = %v", err)
	}
	if err := e.replace(locks.WithLockToken(ann, b.Token), f, "ben's token"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("another holder's token = %v", err)
	}
	if err := e.replace(locks.WithLockToken(ann, a.Token), f, "own token"); err != nil {
		t.Fatalf("own token = %v", err)
	}
}

// TestSharedExpiryAndSweep: an expired shared lock guards nothing, is
// taken afresh with a new token, and goes with Sweep.
func TestSharedExpiryAndSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithSharedLocks())
	f := e.file(t)
	a, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Minute)
	must(t, err)
	e.now = e.now.Add(2 * time.Minute)
	if err := e.replace(records.WithActor(ctx, "user:cat"), f, "after expiry"); err != nil {
		t.Fatalf("an expired shared lock still guarded: %v", err)
	}
	again, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Minute)
	if err != nil || again.Token == a.Token {
		t.Fatalf("retaking an expired shared lock = %+v, %v", again, err)
	}
	e.now = e.now.Add(2 * time.Minute)
	n, err := e.locks.Sweep(ctx, e.store, e.now)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v", n, err)
	}
}

// TestSharedNeedsOption: without WithSharedLocks LockShared is
// CodeUnsupported and exclusive locks behave as always; shared locks
// stored by a Plugin with it still guard the file.
func TestSharedNeedsOption(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	f := e.file(t)
	if _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Hour); filesystem.CodeOf(err) != filesystem.CodeUnsupported {
		t.Fatalf("LockShared without the option = %v", err)
	}
	other := locks.New(locks.WithSharedLocks(), locks.WithClock(func() time.Time { return e.now }))
	if _, err := other.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := e.replace(records.WithActor(ctx, "user:cat"), f, "x"); !errors.Is(err, locks.ErrLocked) {
		t.Fatalf("a stored shared lock was ignored without the option: %v", err)
	}
}

// TestSharedHolderFuncFailsClosed: a HolderFunc error refuses the change
// under a shared lock too.
func TestSharedHolderFuncFailsClosed(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("directory down")
	e := newEnv(t, locks.WithSharedLocks(), locks.WithHolderFunc(func(context.Context, locks.HolderRequest) (bool, error) { return true, boom }))
	f := e.file(t)
	_, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "user:ann", time.Hour)
	must(t, err)
	if err := e.replace(records.WithActor(ctx, "user:ann"), f, "x"); !errors.Is(err, boom) {
		t.Fatalf("a failing HolderFunc = %v, want its error", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestSharedFaultSweep fails every database step of the shared-lock
// operations, and of the purge, scope purge and sweep that also clear the
// shared table, in turn: each failure is the injected fault, returned,
// and the run after the sweep has its normal outcome (want).
func TestSharedFaultSweep(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t, locks.WithSharedLocks())
	f, g := e.file(t), e.file(t)
	ops := []struct {
		name string
		want error
		run  func() error
	}{
		{"first shared lock", nil, func() error { _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "alice", time.Minute); return err }},
		{"extend shared", nil, func() error { _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "alice", time.Hour); return err }},
		{"second holder", nil, func() error { _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "bob", time.Hour); return err }},
		{"shared locks of", nil, func() error { _, err := e.locks.SharedLocksOf(ctx, e.store, "t", f.ID); return err }},
		{"exclusive over others", locks.ErrLocked, func() error { _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); return err }},
		{"unlock shared", nil, func() error { return e.locks.UnlockShared(ctx, e.store, "t", f.ID, "bob") }},
		{"exclusive over own shared", nil, func() error { _, err := e.locks.Lock(ctx, e.store, "t", f.ID, "alice", time.Hour); return err }},
		{"shared under exclusive", locks.ErrLocked, func() error { _, err := e.locks.LockShared(ctx, e.store, "t", f.ID, "bob", time.Hour); return err }},
		{"shared on g", nil, func() error { _, err := e.locks.LockShared(ctx, e.store, "t", g.ID, "carol", time.Hour); return err }},
		{"break", nil, func() error { return e.locks.Break(ctx, e.store, "t", g.ID) }},
		{"shared on g again", nil, func() error { _, err := e.locks.LockShared(ctx, e.store, "t", g.ID, "carol", time.Hour); return err }},
		{"sweep", nil, func() error { _, err := e.locks.Sweep(ctx, e.store, e.now.Add(2*time.Hour)); return err }},
		{"purge", nil, func() error { return e.store.Purge(ctx, "t", g.ID) }},
		{"purge scope", nil, func() error { _, err := e.store.PurgeScope(ctx, "t"); return err }},
		// A scope with no files: the plugin's own statements are the only
		// steps after the Store's, at the same place on every run.
		{"purge an empty scope", nil, func() error { _, err := e.store.PurgeScope(ctx, "empty"); return err }},
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

// TestSharedRefusesBadArguments: a shared lock needs an owner and a ttl
// within WithMaxTTL, and a live file, as an exclusive one does.
func TestSharedRefusesBadArguments(t *testing.T) {
	e := newEnv(t, locks.WithSharedLocks())
	f := e.file(t)
	for _, c := range []struct {
		owner string
		ttl   time.Duration
	}{{"", time.Minute}, {"alice", 0}, {strings.Repeat("a", locks.MaxOwnerBytes+1), time.Minute}} {
		if _, err := e.locks.LockShared(t.Context(), e.store, "t", f.ID, c.owner, c.ttl); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("owner %d bytes, ttl %s = %v", len(c.owner), c.ttl, err)
		}
	}
	if _, err := e.locks.LockShared(t.Context(), e.store, "t", "no-such-file", "alice", time.Minute); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a shared lock on an unknown file = %v", err)
	}
	if err := e.store.Trash(t.Context(), "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.locks.LockShared(t.Context(), e.store, "t", f.ID, "alice", time.Minute); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("a shared lock on a trashed file = %v", err)
	}
}
