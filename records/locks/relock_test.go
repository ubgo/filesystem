package locks_test

import (
	"testing"
	"time"
)

// TestRelockWithTheSameTimes: an owner taking its own lock again at the
// same instant writes the row's existing values. MySQL counts that UPDATE
// as no row, which must not read as "held by someone else".
func TestRelockWithTheSameTimes(t *testing.T) {
	e := newEnv(t) // a frozen clock
	f := e.file(t)
	for range 2 {
		if _, err := e.locks.Lock(t.Context(), e.store, "t", f.ID, "alice", time.Hour); err != nil {
			t.Fatalf("alice re-taking her own lock: %v", err)
		}
	}
}
