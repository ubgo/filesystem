package egress_test

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/egress"
	"github.com/ubgo/filesystem/records/internal/testdb"
)

// newBucketEnv is newEnv with plugin options.
func newBucketEnv(t *testing.T, opts ...egress.Option) *env {
	t.Helper()
	return newBucketEnvOn(t, testdb.Open(t), opts...)
}

// newBucketEnvOn is newBucketEnv on a given database, such as one from
// testdb.OpenFaulty.
func newBucketEnvOn(t *testing.T, db *sql.DB, opts ...egress.Option) *env {
	t.Helper()
	e := &env{p: egress.New(opts...), sink: &sink{}, now: day1}
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

// hourly is a BucketFunc metering per UTC hour.
func hourly(_ string, at time.Time) string { return at.UTC().Format("2006-01-02T15") }

// tokyo is a fixed +9 zone, so the test needs no tzdata.
var tokyo = time.FixedZone("JST", 9*60*60)

// tenantDay meters scope "jp" in Tokyo days and every other scope in UTC
// days: billing days in the tenant's own time zone.
func tenantDay(scope string, at time.Time) string {
	if scope == "jp" {
		return at.In(tokyo).Format(egress.DayLayout)
	}
	return egress.UTCDay(scope, at)
}

// TestBucketHourly: a custom BucketFunc's keys are the buckets, and
// Report leaves the current one alone.
func TestBucketHourly(t *testing.T) {
	e := newBucketEnv(t, egress.WithBucket(hourly))
	e.at(day1.Add(10*time.Hour + 15*time.Minute))
	e.serve(t, "a", 10)
	e.at(day1.Add(11*time.Hour + 20*time.Minute))
	e.serve(t, "a", 5)

	days, err := e.p.Days(t.Context(), e.store, "a", day1.Add(10*time.Hour), day1.Add(12*time.Hour))
	if err != nil || fmt.Sprint(days) != "[{a 2026-10-03T10 10 1} {a 2026-10-03T11 5 1}]" {
		t.Fatalf("days = %v, %v", days, err)
	}
	if n, err := e.p.Report(t.Context(), e.store, day1.Add(11*time.Hour+30*time.Minute)); err != nil || n != 1 {
		t.Fatalf("report inside hour 11 = %d, %v; want only hour 10", n, err)
	}
	if got := e.sink.events[0].Detail[egress.DetailDay]; got != "2026-10-03T10" {
		t.Fatalf("reported bucket %q", got)
	}
	if n, err := e.p.Report(t.Context(), e.store, day1.Add(12*time.Hour)); err != nil || n != 1 {
		t.Fatalf("report after hour 11 = %d, %v", n, err)
	}
	n, err := e.p.Prune(t.Context(), e.store, day1.Add(11*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune before hour 11 = %d, %v", n, err)
	}
}

// TestBucketPerTenantZone: the current bucket is each scope's own, so a
// Tokyo day is reported once it ends in Tokyo, not in UTC.
func TestBucketPerTenantZone(t *testing.T) {
	e := newBucketEnv(t, egress.WithBucket(tenantDay))
	e.at(day1.Add(16 * time.Hour)) // 2026-10-03 16:00 UTC is 2026-10-04 01:00 in Tokyo
	e.serve(t, "jp", 7)
	e.serve(t, "us", 3)

	jp, _ := e.p.Days(t.Context(), e.store, "jp", day1, day3)
	us, _ := e.p.Days(t.Context(), e.store, "us", day1, day3)
	if fmt.Sprint(jp, us) != "[{jp 2026-10-04 7 1}] [{us 2026-10-03 3 1}]" {
		t.Fatalf("buckets %v %v", jp, us)
	}
	// 2026-10-04 12:00 UTC: the UTC day 10-03 is over, Tokyo's 10-04 is not.
	if n, err := e.p.Report(t.Context(), e.store, day2.Add(12*time.Hour)); err != nil || n != 1 || e.sink.events[0].Scope != "us" {
		t.Fatalf("report = %d, %v, %+v", n, err, e.sink.events)
	}
	// 2026-10-04 15:00 UTC is 10-05 00:00 in Tokyo: now its day is over.
	if n, err := e.p.Report(t.Context(), e.store, day2.Add(15*time.Hour)); err != nil || n != 1 || e.sink.events[1].Scope != "jp" {
		t.Fatalf("report = %d, %v, %+v", n, err, e.sink.events)
	}
	// Pruning before 2026-10-04 08:00 UTC: us's cutoff day is 10-04 (its
	// 10-03 goes), jp's is 10-04 17:00 Tokyo, so its 10-04 stays.
	if n, err := e.p.Prune(t.Context(), e.store, day2.Add(8*time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune = %d, %v", n, err)
	}
	if b, _ := e.p.Total(t.Context(), e.store, "jp", day1, day3); b.Bytes != 7 {
		t.Fatalf("jp = %+v", b)
	}
}

// TestBucketBadKeyFailsClosed: a key that is empty or too wide fails the
// metering instead of being cut short or stored under "".
func TestBucketBadKeyFailsClosed(t *testing.T) {
	for name, fn := range map[string]egress.BucketFunc{
		"empty": func(string, time.Time) string { return "" },
		"wide":  func(string, time.Time) string { return strings.Repeat("d", egress.MaxBucketBytes+1) },
	} {
		e := newBucketEnv(t, egress.WithBucket(fn))
		err := e.store.Served(t.Context(), records.Served{Kind: records.ServeLink, Scope: "a", FileID: "fil_1", Bytes: 1})
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: Served = %v", name, err)
		}
		if _, err := e.p.Days(t.Context(), e.store, "a", day1, day3); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("%s: Days = %v", name, err)
		}
	}
}

// TestBucketDefaultIsUTCDay: without WithBucket the buckets are UTC days,
// exactly UTCDay's keys.
func TestBucketDefaultIsUTCDay(t *testing.T) {
	e := newBucketEnv(t)
	at := time.Date(2026, 10, 3, 23, 30, 0, 0, tokyo) // 14:30 UTC
	e.at(at)
	e.serve(t, "a", 1)
	days, err := e.p.Days(t.Context(), e.store, "a", day1, day3)
	if err != nil || len(days) != 1 || days[0].Day != egress.UTCDay("a", at) || days[0].Day != "2026-10-03" {
		t.Fatalf("days = %v, %v", days, err)
	}
}

// TestBucketKeyFailuresEverywhere: a bucket key that fails for one time
// fails every call that needs that time's bucket (Total at either end of
// its period, Report's current bucket, Prune's cutoff) rather than being
// skipped; and Prune by bucket returns a database failure at each step.
func TestBucketKeyFailuresEverywhere(t *testing.T) {
	ctx := t.Context()
	poison := day3.Add(time.Hour)
	fn := func(scope string, at time.Time) string {
		if at.Equal(poison) {
			return ""
		}
		return hourly(scope, at)
	}
	db, faults := testdb.OpenFaulty(t)
	e := newBucketEnvOn(t, db, egress.WithBucket(fn))
	e.serve(t, "a", 5)
	e.serve(t, "b", 5)
	if _, err := e.p.Report(ctx, e.store, day3); err != nil { // reported rows are what Prune removes
		t.Fatal(err)
	}
	e.serve(t, "a", 1) // a's bucket is due again, b's stays reported
	for name, err := range map[string]error{
		"Total from": func() error { _, err := e.p.Total(ctx, e.store, "a", poison, day3.Add(2*time.Hour)); return err }(),
		"Total to":   func() error { _, err := e.p.Total(ctx, e.store, "a", day1, poison); return err }(),
		"Report":     func() error { _, err := e.p.Report(ctx, e.store, poison); return err }(),
		"Prune":      func() error { _, err := e.p.Prune(ctx, e.store, poison); return err }(),
	} {
		if filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Errorf("%s with a failing key = %v", name, err)
		}
	}
	for n := 1; ; n++ {
		faults.Arm(n)
		_, err := e.p.Prune(ctx, e.store, day3)
		faults.Arm(0)
		if err == nil {
			break
		}
		if !errors.Is(err, testdb.ErrInjected) {
			t.Fatalf("prune step %d: %v", n, err)
		}
	}
}
