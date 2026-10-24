package quota_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
)

// TestReserveRefusals: Reserve refuses non-positive sizes and lifetimes,
// a reservation larger than the whole limit, and returns a failed limit
// lookup, holding nothing in each case.
func TestReserveRefusals(t *testing.T) {
	ctx := t.Context()
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 100}))
	for _, c := range []struct {
		n   int64
		ttl time.Duration
	}{{0, time.Minute}, {10, 0}, {-1, time.Minute}} {
		if _, err := e.plugin.Reserve(ctx, e.store, "t", c.n, c.ttl); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
			t.Fatalf("Reserve(%d, %s) = %v", c.n, c.ttl, err)
		}
	}
	if _, err := e.plugin.Reserve(ctx, e.store, "t", 101, time.Minute); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("a reservation over the whole limit = %v", err)
	}
	if u := usage(t, e, "t"); u.Reserved != 0 {
		t.Fatalf("a refused reservation held %d bytes", u.Reserved)
	}
	boom := errors.New("plan service down")
	down, _ := newEnv(t, func(context.Context, string) (quota.Limit, error) { return quota.Limit{}, boom })
	if _, err := down.plugin.Reserve(ctx, down.store, "t", 1, time.Minute); !errors.Is(err, boom) {
		t.Fatalf("Reserve with the limit lookup down = %v", err)
	}
}

// TestRacingFirstReservationsRetry: a first charge of the scope landing
// between Reserve's update (no row yet) and its insert makes the insert
// a unique violation, reported as CodeUnavailable (retry). A SQLite
// trigger plays the other charge.
func TestRacingFirstReservationsRetry(t *testing.T) {
	testdb.OnlySQLite(t)
	e, exec := newEnv(t, fixed(quota.Limit{Bytes: 100}))
	exec("CREATE TRIGGER racer BEFORE INSERT ON " + quota.TableNameUsage + " BEGIN " +
		"INSERT INTO " + quota.TableNameUsage + " (" + quota.ColScope + ", " + quota.ColBytes + ", " + quota.ColFiles + ", " + quota.ColReserved + ", " + quota.ColUpdatedAt + ") " +
		"VALUES (NEW." + quota.ColScope + ", 0, 0, 0, NEW." + quota.ColUpdatedAt + "); END")
	if _, err := e.plugin.Reserve(t.Context(), e.store, "t", 10, time.Minute); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Fatalf("a racing first reservation = %v, want CodeUnavailable", err)
	}
}

// TestReleaseOfAnUncountedFile: with usage events on, purging a file
// whose scope has no usage row (recorded before quota counted it)
// releases nothing and emits nothing.
func TestReleaseOfAnUncountedFile(t *testing.T) {
	ctx := t.Context()
	sink := &usageSink{}
	e := newEnvWith(t, fixed(quota.Limit{}), []records.Plugin{sink}, quota.WithUsageEvents())
	f, err := e.add(t, "t", "a", 5)
	if err != nil {
		t.Fatal(err)
	}
	db, ok := e.db.(*sql.DB)
	if !ok {
		t.Fatalf("env database is %T", e.db)
	}
	testdb.Exec(t, db, "DELETE FROM "+quota.TableNameUsage)
	sink.events = nil
	if err := e.store.Purge(ctx, "t", f.ID); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 0 {
		t.Fatalf("releasing an uncounted file emitted %+v", sink.events)
	}
	if u := usage(t, e, "t"); u != (quota.Usage{}) {
		t.Fatalf("usage after releasing an uncounted file = %+v", u)
	}
}

// TestReservationFaultSweep fails every database step of the reservation
// lifecycle and of charges and releases with usage events on, in turn:
// each failure is the injected fault, returned, changing no usage, and
// the run after the sweep succeeds.
func TestReservationFaultSweep(t *testing.T) {
	ctx := t.Context()
	e, _ := newEnv(t, fixed(quota.Limit{Bytes: 1000}), quota.WithUsageEvents())
	var held, open quota.Reservation
	var file records.File
	ops := []struct {
		name string
		run  func() error
	}{
		{"first reserve", func() (err error) { held, err = e.plugin.Reserve(ctx, e.store, "t", 10, time.Minute); return err }},
		{"reserve", func() error { _, err := e.plugin.Reserve(ctx, e.store, "t", 10, time.Second); return err }},
		{"record with the reservation", func() error {
			if _, err := e.disk.WriteBytes(ctx, "r", []byte("12345"), filesystem.WriteOptions{}); err != nil {
				t.Fatal(err)
			}
			f, err := e.store.CreateFile(quota.WithReservation(ctx, held.ID), records.NewFile{Scope: "t", Disk: "media", Key: "r"})
			file = f
			return err
		}},
		{"reserve to release", func() (err error) { open, err = e.plugin.Reserve(ctx, e.store, "t", 10, time.Minute); return err }},
		{"release", func() error {
			ok, err := e.plugin.Release(ctx, e.store, "t", open.ID)
			if err == nil && !ok {
				t.Fatal("the open reservation was not released")
			}
			return err
		}},
		{"sweep", func() error { _, err := e.plugin.Sweep(ctx, e.store, time.Now().Add(time.Hour)); return err }},
		{"purge", func() error { return e.store.Purge(ctx, "t", file.ID) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			for n := 1; ; n++ {
				before := usage(t, e, "t")
				e.faults.Arm(n)
				err := op.run()
				e.faults.Arm(0)
				if err == nil {
					return
				}
				if !errors.Is(err, testdb.ErrInjected) {
					t.Fatalf("step %d: %v", n, err)
				}
				if after := usage(t, e, "t"); after != before {
					t.Fatalf("step %d failed but changed the usage: %+v -> %+v", n, before, after)
				}
			}
		})
	}
	if u := usage(t, e, "t"); u.Reserved != 0 || u.Bytes != 0 || u.Files != 0 {
		t.Fatalf("usage after the lifecycle = %+v, want nothing held", u)
	}
}
