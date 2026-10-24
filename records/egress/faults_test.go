package egress_test

import (
	"errors"
	"testing"

	"github.com/ubgo/filesystem/records/internal/testdb"
)

// TestReportFaultSweep fails every database step of a Report over two
// due buckets in turn: each failure is the injected fault, returned with
// what was sent before it, and the run after the sweep reports every
// bucket not reported yet, so a failure never loses or repeats a total.
func TestReportFaultSweep(t *testing.T) {
	ctx := t.Context()
	db, faults := testdb.OpenFaulty(t)
	e := newEnvOn(t, db)
	e.at(day1)
	e.serve(t, "a", 10)
	e.serve(t, "b", 20)
	e.at(day2)
	sent := 0
	for n := 1; ; n++ {
		faults.Arm(n)
		got, err := e.p.Report(ctx, e.store, day2)
		faults.Arm(0)
		sent += got
		if err == nil {
			break
		}
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("step %d: %v", n, err)
		}
	}
	if sent != 2 {
		t.Fatalf("reported %d buckets, want each of the 2 once", sent)
	}
	if again, err := e.p.Report(ctx, e.store, day2); err != nil || again != 0 {
		t.Fatalf("a rerun reported %d, %v; want nothing left", again, err)
	}
}
