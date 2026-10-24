package egress_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// sink is a second records.EventHooks plugin: a billing consumer that
// keeps the events egress emits.
type sink struct {
	mu     sync.Mutex
	events []records.Event
}

func (k *sink) Name() string { return "billing" }

func (k *sink) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.events = append(k.events, e)
	return nil
}

type env struct {
	store *records.Store
	p     *egress.Plugin
	sink  *sink
	now   time.Time
	mu    sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvOn(t, testdb.Open(t))
}

// newEnvOn is newEnv on a given database, such as one from
// testdb.OpenFaulty.
func newEnvOn(t *testing.T, db *sql.DB) *env {
	t.Helper()
	e := &env{p: egress.New(), sink: &sink{}, now: time.Date(2026, 10, 3, 23, 59, 0, 0, time.UTC)}
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
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

func (e *env) at(t time.Time) { e.mu.Lock(); e.now = t; e.mu.Unlock() }

func (e *env) serve(t *testing.T, scope string, n int64) {
	t.Helper()
	if err := e.store.Served(t.Context(), records.Served{Kind: records.ServeLink, Scope: scope, FileID: "fil_1", Bytes: n}); err != nil {
		t.Fatal(err)
	}
}

var (
	day1 = time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	day2 = day1.AddDate(0, 0, 1)
	day3 = day1.AddDate(0, 0, 2)
)

func TestBucketsPerScopeAndDay(t *testing.T) {
	e := newEnv(t)
	e.serve(t, "a", 100)
	e.serve(t, "a", 50)
	e.serve(t, "b", 7)
	e.at(day2.Add(time.Hour))
	e.serve(t, "a", 1)

	days, err := e.p.Days(t.Context(), e.store, "a", day1, day3)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(days); got != "[{a 2026-10-03 150 2} {a 2026-10-04 1 1}]" {
		t.Fatalf("days = %s", got)
	}
	total, err := e.p.Total(t.Context(), e.store, "a", day1, day2)
	if err != nil || total.Bytes != 150 || total.Requests != 2 {
		t.Fatalf("total of day 1 = %+v, %v", total, err)
	}
	if b, _ := e.p.Total(t.Context(), e.store, "b", day1, day3); b.Bytes != 7 {
		t.Fatalf("scope b = %+v; scopes must not mix", b)
	}
}

// Concurrent first reads of a day race on the INSERT; none may be lost.
func TestConcurrentReadsAllCounted(t *testing.T) {
	e := newEnv(t)
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := e.store.Served(context.Background(), records.Served{Kind: records.ServeShare, Scope: "a", Bytes: 3}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	total, err := e.p.Total(t.Context(), e.store, "a", day1, day3)
	if err != nil || total.Bytes != 3*n || total.Requests != n {
		t.Fatalf("total = %+v, %v; want %d bytes in %d requests", total, err, 3*n, n)
	}
}

func TestReportEmitsGrowthOnceWithStableIDs(t *testing.T) {
	e := newEnv(t)
	e.serve(t, "a", 100)
	e.at(day2.Add(time.Minute))

	// Today's bucket is still filling: nothing to report before midnight.
	if n, err := e.p.Report(t.Context(), e.store, day1.Add(12*time.Hour)); err != nil || n != 0 {
		t.Fatalf("report during the day = %d, %v", n, err)
	}
	if n, err := e.p.Report(t.Context(), e.store, day2); err != nil || n != 1 {
		t.Fatalf("report = %d, %v", n, err)
	}
	// Run again: nothing grew, nothing is sent twice.
	if n, err := e.p.Report(t.Context(), e.store, day2); err != nil || n != 0 {
		t.Fatalf("second report = %d, %v", n, err)
	}
	// A slow read lands on the reported day: the next run sends only it.
	e.at(day1.Add(23 * time.Hour))
	e.serve(t, "a", 5)
	if n, err := e.p.Report(t.Context(), e.store, day2); err != nil || n != 1 {
		t.Fatalf("late report = %d, %v", n, err)
	}
	ev := e.sink.events
	if len(ev) != 2 {
		t.Fatalf("events = %d", len(ev))
	}
	if ev[0].Kind != egress.EventDailyTotal || ev[0].Detail[egress.DetailBytes] != "100" || ev[1].Detail[egress.DetailBytes] != "5" ||
		ev[1].Detail[egress.DetailTotalBytes] != "105" || ev[1].Detail[egress.DetailDay] != "2026-10-03" {
		t.Fatalf("events = %+v", ev)
	}
	if ev[0].ID == ev[1].ID || ev[0].ID[:4] != "egr_" {
		t.Fatalf("ids %q, %q: each growth needs its own deterministic id", ev[0].ID, ev[1].ID)
	}

	// The same totals give the same id in a fresh deployment: a report
	// repeated after a crash is recognisable downstream.
	f := newEnv(t)
	f.serve(t, "a", 100)
	if _, err := f.p.Report(t.Context(), f.store, day2); err != nil {
		t.Fatal(err)
	}
	if f.sink.events[0].ID != ev[0].ID {
		t.Fatalf("id %q != %q for the same bucket", f.sink.events[0].ID, ev[0].ID)
	}
}

func TestPruneKeepsUnreported(t *testing.T) {
	e := newEnv(t)
	e.serve(t, "a", 1)
	e.serve(t, "b", 1)
	if _, err := e.p.Report(t.Context(), e.store, day2); err != nil {
		t.Fatal(err)
	}
	e.at(day1.Add(time.Hour))
	e.serve(t, "b", 1) // b grows after the report
	n, err := e.p.Prune(t.Context(), e.store, day3)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want only a's reported bucket", n, err)
	}
	if b, _ := e.p.Total(t.Context(), e.store, "b", day1, day3); b.Requests != 2 {
		t.Fatalf("b = %+v", b)
	}
}

func TestServedRefusesInvalidReads(t *testing.T) {
	e := newEnv(t)
	for _, sv := range []records.Served{{Kind: "access.other", Scope: "a"}, {Kind: records.ServeLink, Scope: "a", Bytes: -1}} {
		if err := e.store.Served(t.Context(), sv); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%+v: %v", sv, err)
		}
	}
}
