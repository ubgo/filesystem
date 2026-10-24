package audit

import (
	"errors"
	"testing"

	"github.com/ubgo/filesystem"
)

// TestLockConflictIsRetryable: a deadlock the database broke by aborting
// the transaction reaches the caller as CodeUnavailable; other errors
// pass unchanged.
func TestLockConflictIsRetryable(t *testing.T) {
	deadlock := errors.New("Error 1213 (40001): Deadlock found when trying to get lock")
	if err := retryable(deadlock); filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, deadlock) {
		t.Fatalf("deadlock = %v", err)
	}
	other := errors.New("disk full")
	if err := retryable(other); err != other {
		t.Fatalf("other = %v", err)
	}
	if retryable(nil) != nil {
		t.Fatal("nil became an error")
	}
}
