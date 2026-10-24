package records_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestTxCleanupOutlivesTheRequest: rollback and after-commit work runs with
// a context the request's cancellation cannot reach, so a client that went
// away mid-request still gets its objects cleaned up.
func TestTxCleanupOutlivesTheRequest(t *testing.T) {
	e := newEnv(t, nil)
	errFail := errors.New("fail")

	ctx, cancel := context.WithCancel(t.Context())
	var rolledBack context.Context
	err := e.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		e.store.OnRollback(ctx, func(ctx context.Context) { rolledBack = ctx })
		cancel()
		return errFail
	})
	if !errors.Is(err, errFail) || rolledBack == nil {
		t.Fatalf("Tx = %v, rollback ran %v", err, rolledBack != nil)
	}
	if rolledBack.Err() != nil || rolledBack.Done() != nil {
		t.Errorf("rollback work got a cancellable context: %v", rolledBack.Err())
	}

	var committed context.Context
	if err := e.store.Tx(t.Context(), func(ctx context.Context, _ *sql.Tx) error {
		e.store.AfterCommit(ctx, func(ctx context.Context) { committed = ctx })
		return nil
	}); err != nil || committed == nil {
		t.Fatalf("Tx = %v, after-commit ran %v", err, committed != nil)
	}
	if committed.Done() != nil {
		t.Error("after-commit work got a cancellable context")
	}

	// Without a transaction AfterCommit runs at once, with the same
	// guarantee.
	var direct context.Context
	ctx2, cancel2 := context.WithCancel(t.Context())
	cancel2()
	e.store.AfterCommit(ctx2, func(ctx context.Context) { direct = ctx })
	if direct == nil || direct.Err() != nil {
		t.Errorf("AfterCommit without a transaction got a cancelled context")
	}
}
