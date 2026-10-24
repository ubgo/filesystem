package egress_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// newSubjectEnv is newEnv with egress built from opts.
func newSubjectEnv(t *testing.T, opts ...egress.Option) *env {
	t.Helper()
	return newSubjectEnvOn(t, testdb.Open(t), opts...)
}

// newSubjectEnvOn is newSubjectEnv on db.
func newSubjectEnvOn(t *testing.T, db *sql.DB, opts ...egress.Option) *env {
	t.Helper()
	e := &env{p: egress.New(opts...), sink: &sink{}, now: day1.Add(time.Hour)}
	mgr, _ := filesystem.NewManager()
	var err error
	e.store, err = records.New(db, testdb.Dialect(), mgr, records.WithPlugins(e.p, e.sink),
		records.WithClock(func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) serveAs(t *testing.T, kind records.ServeKind, subject string, n int64) {
	t.Helper()
	if err := e.store.Served(t.Context(), records.Served{Kind: kind, Scope: "a", FileID: "fil_1", Subject: subject, Bytes: n}); err != nil {
		t.Fatal(err)
	}
}

// TestPerShareLink: WithPerSubject(ServeShare) meters each share link per
// day beside its scope; other kinds, and a share read without a link ID,
// count for the scope only.
func TestPerShareLink(t *testing.T) {
	e := newSubjectEnv(t, egress.WithPerSubject(records.ServeShare))
	e.serveAs(t, records.ServeShare, "shr_1", 100)
	e.serveAs(t, records.ServeShare, "shr_1", 20)
	e.serveAs(t, records.ServeShare, "shr_2", 5)
	e.serveAs(t, records.ServeShare, "", 9)
	e.serveAs(t, records.ServeVariant, "thumb", 7)
	e.at(day2.Add(time.Hour))
	e.serveAs(t, records.ServeShare, "shr_1", 1)
	ctx := t.Context()
	one, err := e.p.SubjectDays(ctx, e.store, "a", records.ServeShare, "shr_1", day1, day3)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(one); got != "[{a access.share shr_1 2026-10-03 120 2} {a access.share shr_1 2026-10-04 1 1}]" {
		t.Fatalf("shr_1 = %s", got)
	}
	if two, _ := e.p.SubjectDays(ctx, e.store, "a", records.ServeShare, "shr_2", day1, day3); len(two) != 1 || two[0].Bytes != 5 {
		t.Fatalf("shr_2 = %+v", two)
	}
	if v, _ := e.p.SubjectDays(ctx, e.store, "a", records.ServeVariant, "thumb", day1, day3); len(v) != 0 {
		t.Fatalf("a kind not named was broken down: %+v", v)
	}
	total, err := e.p.Total(ctx, e.store, "a", day1, day3)
	if err != nil || total.Bytes != 142 || total.Requests != 6 {
		t.Fatalf("the scope's total = %+v, %v", total, err)
	}
	n, err := e.p.Prune(ctx, e.store, day2)
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want the two day-1 subject buckets (the scope's is unreported)", n, err)
	}
}

// TestPerSubjectConcurrent: racing first reads of a link's day lose no
// count.
func TestPerSubjectConcurrent(t *testing.T) {
	e := newSubjectEnv(t, egress.WithPerSubject(records.ServeShare))
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := e.store.Served(context.Background(), records.Served{Kind: records.ServeShare, Scope: "a", Subject: "shr_1", Bytes: 3}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, err := e.p.SubjectDays(t.Context(), e.store, "a", records.ServeShare, "shr_1", day1, day3)
	if err != nil || len(got) != 1 || got[0].Bytes != 3*n || got[0].Requests != n {
		t.Fatalf("shr_1 = %+v, %v", got, err)
	}
}

// TestPerSubjectRefusals: an invalid kind fails every read's metering,
// and a subject past MaxSubjectBytes fails its read; without the option
// nothing is broken down.
func TestPerSubjectRefusals(t *testing.T) {
	bad := newSubjectEnv(t, egress.WithPerSubject("nope"))
	if err := bad.store.Served(t.Context(), records.Served{Kind: records.ServeLink, Scope: "a", Bytes: 1}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("an invalid kind = %v", err)
	}
	e := newSubjectEnv(t, egress.WithPerSubject(records.ServeShare))
	long := string(make([]byte, egress.MaxSubjectBytes+1))
	if err := e.store.Served(t.Context(), records.Served{Kind: records.ServeShare, Scope: "a", Subject: long, Bytes: 1}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Fatalf("a long subject = %v", err)
	}
	plain := newSubjectEnv(t)
	plain.serveAs(t, records.ServeShare, "shr_1", 10)
	if got, _ := plain.p.SubjectDays(t.Context(), plain.store, "a", records.ServeShare, "shr_1", day1, day3); len(got) != 0 {
		t.Fatalf("a breakdown without the option: %+v", got)
	}
}

// dayOrNothing buckets by UTC day, and names no bucket for the zero time,
// so a caller can make the bucket function fail on demand.
func dayOrNothing(scope string, at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return egress.UTCDay(scope, at)
}

// TestPerSubjectWithBuckets: the breakdown follows WithBucket, Prune
// removes each scope's old subject buckets by that scope's own key, and a
// bucket function that names no bucket fails SubjectDays and Prune with
// CodeInvalidArgument.
func TestPerSubjectWithBuckets(t *testing.T) {
	ctx := t.Context()
	e := newSubjectEnv(t, egress.WithPerSubject(records.ServeShare), egress.WithBucket(dayOrNothing))
	e.serveAs(t, records.ServeShare, "shr_1", 10)
	e.at(day2.Add(time.Hour))
	e.serveAs(t, records.ServeShare, "shr_1", 5)
	if got, err := e.p.SubjectDays(ctx, e.store, "a", records.ServeShare, "shr_1", day1, day3); err != nil || len(got) != 2 {
		t.Fatalf("SubjectDays = %+v, %v", got, err)
	}
	if n, err := e.p.Prune(ctx, e.store, day2); err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want the day-1 subject bucket", n, err)
	}
	for name, err := range map[string]error{
		"from": func() error {
			_, err := e.p.SubjectDays(ctx, e.store, "a", records.ServeShare, "shr_1", time.Time{}, day3)
			return err
		}(),
		"to": func() error {
			_, err := e.p.SubjectDays(ctx, e.store, "a", records.ServeShare, "shr_1", day1, time.Time{})
			return err
		}(),
		"prune": func() error { _, err := e.p.Prune(ctx, e.store, time.Time{}); return err }(),
	} {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s with no bucket = %v", name, err)
		}
	}
}

// TestPerSubjectFaultSweep fails every database step of a metered share
// read and of Prune, with and without WithBucket, in turn: each failure
// is the injected fault, returned, and the run after the sweep succeeds.
func TestPerSubjectFaultSweep(t *testing.T) {
	for name, opts := range map[string][]egress.Option{
		"utc days":    {egress.WithPerSubject(records.ServeShare)},
		"own buckets": {egress.WithPerSubject(records.ServeShare), egress.WithBucket(dayOrNothing)},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			db, faults := testdb.OpenFaulty(t)
			e := newSubjectEnvOn(t, db, opts...)
			for _, op := range []struct {
				name string
				run  func() error
			}{
				{"first read", func() error {
					return e.store.Served(ctx, records.Served{Kind: records.ServeShare, Scope: "a", Subject: "shr_1", Bytes: 1})
				}},
				{"next read", func() error {
					return e.store.Served(ctx, records.Served{Kind: records.ServeShare, Scope: "a", Subject: "shr_1", Bytes: 1})
				}},
				{"prune", func() error { _, err := e.p.Prune(ctx, e.store, day3); return err }},
			} {
				for n := 1; ; n++ {
					faults.Arm(n)
					err := op.run()
					faults.Arm(0)
					if err == nil {
						break
					}
					if !errors.Is(err, testdb.ErrInjected) {
						t.Fatalf("%s step %d: %v", op.name, n, err)
					}
				}
			}
		})
	}
}
