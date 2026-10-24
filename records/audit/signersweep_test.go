package audit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
)

// sweepSigner is a Signer over chainKey whose MAC fails on the failOn-th
// call since arm, so a test can fail each MAC a write or a prune makes in
// turn; 0 never fails.
type sweepSigner struct {
	mu            sync.Mutex
	inner         audit.Signer
	calls, failOn int
}

func (s *sweepSigner) arm(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls, s.failOn = 0, n
}

func (s *sweepSigner) CurrentID(ctx context.Context) (string, error) { return s.inner.CurrentID(ctx) }

func (s *sweepSigner) MAC(ctx context.Context, id string, msg []byte) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	fail := s.calls == s.failOn
	s.mu.Unlock()
	if fail {
		return nil, errKMS
	}
	return s.inner.MAC(ctx, id, msg)
}

// sweepMACs fails each MAC call of op in turn: every run until op
// succeeds must fail with errKMS. It returns how many runs failed.
func sweepMACs(t *testing.T, sig *sweepSigner, op func() error) int {
	t.Helper()
	for n := 1; ; n++ {
		sig.arm(n)
		err := op()
		sig.arm(0)
		if err == nil {
			return n - 1
		}
		if !errors.Is(err, errKMS) {
			t.Fatalf("MAC call %d failing: %v", n, err)
		}
	}
}

// TestSignerFailureAtEveryMAC: a failure of any MAC a write makes (a
// scope's first entry: its hash and the chain row's seal; a later one:
// the head check, its hash and the new seal) or a chained prune makes (the
// head check and the moved anchor's seal) fails the change with the
// signer's error and commits nothing of it; the chain still verifies.
func TestSignerFailureAtEveryMAC(t *testing.T) {
	ctx := t.Context()
	sig := &sweepSigner{inner: audit.KeyringMAC(audit.StaticKeyring{Keys: map[string][]byte{"": chainKey}})}
	e := newEnv(t, audit.WithHashChainMAC(sig))
	create := func(key string) func() error {
		return func() error {
			_, err := e.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: "media", Key: key})
			return err
		}
	}
	for _, key := range []string{"a", "b"} {
		if _, err := e.disk.WriteBytes(ctx, key, []byte(key), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if n := sweepMACs(t, sig, create("a")); n != 2 {
		t.Errorf("a scope's first entry failed at %d MAC calls, want its hash and the seal", n)
	}
	cutoff := e.now.Add(time.Second / 2)
	if n := sweepMACs(t, sig, create("b")); n != 3 {
		t.Errorf("a later entry failed at %d MAC calls, want the head check, its hash and the seal", n)
	}
	if e.count(t, "fs_audit") != 2 || e.count(t, "fs_files") != 2 {
		t.Fatal("a failed MAC left a change committed")
	}
	mustVerify(t, e, 2)
	var pruned int64
	if n := sweepMACs(t, sig, func() error {
		var err error
		pruned, err = e.p.PruneScope(ctx, e.store, scope, cutoff)
		return err
	}); n != 2 || pruned != 1 {
		t.Errorf("a chained prune failed at %d MAC calls and then pruned %d, want 2 and 1", n, pruned)
	}
	if _, err := e.p.Verify(ctx, e.store, scope); err != nil {
		t.Fatalf("Verify after the prune: %v", err)
	}
}

// TestHashChainMACNil: a nil signer is WithHashChain(nil): a log either
// writes verifies under the other.
func TestHashChainMACNil(t *testing.T) {
	e := newEnv(t, audit.WithHashChainMAC(nil))
	e.file(t.Context(), t, scope, "a")
	mustVerify(t, e, 1)
	if n, err := audit.New(audit.WithHashChain(nil)).Verify(t.Context(), e.store, scope); err != nil || n != 1 {
		t.Fatalf("WithHashChain(nil) verifying = %d, %v", n, err)
	}
	if n, err := audit.New(audit.WithHashChain(chainKey)).Verify(t.Context(), e.store, scope); err == nil {
		t.Fatalf("a keyed chain verified the nil signer's log (%d)", n)
	}
}
