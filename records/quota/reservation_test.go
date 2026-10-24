package quota_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/internal/testdb"
	"github.com/ubgo/filesystem/records/quota"
)

// usageSink is a billing consumer installed beside quota: it keeps the
// quota.usage_changed events.
type usageSink struct {
	mu     sync.Mutex
	events []records.Event
}

func (k *usageSink) Name() string { return "billing" }

func (k *usageSink) OnEvent(_ context.Context, _ *records.Store, e records.Event) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e.Kind == quota.EventUsageChanged {
		k.events = append(k.events, e)
	}
	return nil
}

func newEnvWith(t *testing.T, limits quota.LimitFunc, more []records.Plugin, opts ...quota.Option) *env {
	t.Helper()
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	p, err := quota.New(limits, opts...)
	if err != nil {
		t.Fatal(err)
	}
	db := testdb.Open(t)
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(append([]records.Plugin{p}, more...)...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &env{store: s, plugin: p, disk: disk, db: db}
}

func TestReservationHoldsRoomUntilCommitted(t *testing.T) {
	e := newEnvWith(t, fixed(quota.Limit{Bytes: 100}), nil)
	ctx := t.Context()

	r, err := e.plugin.Reserve(ctx, e.store, "a", 60, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// The reservation counts against the limit: 50 more does not fit.
	if _, err := e.plugin.Reserve(ctx, e.store, "a", 50, time.Hour); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("second reserve = %v, want quota exceeded", err)
	}
	if _, err := e.add(t, "a", "other", 50); filesystem.CodeOf(err) != filesystem.CodeQuotaExceeded {
		t.Fatalf("an unreserved upload past the held room = %v", err)
	}

	// The reserved upload arrives smaller than announced: its real size is
	// charged and the rest of the reservation is freed.
	if _, err := e.disk.WriteBytes(ctx, "big", []byte(strings.Repeat("x", 55)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.CreateFile(quota.WithReservation(ctx, r.ID), records.NewFile{Scope: "a", Disk: "media", Key: "big"}); err != nil {
		t.Fatalf("committing the reserved upload: %v", err)
	}
	u, _ := e.plugin.Usage(ctx, e.store, "a")
	if u != (quota.Usage{Bytes: 55, Files: 1}) {
		t.Fatalf("usage = %+v, want the file charged once and nothing reserved", u)
	}
	// A reservation is used up once.
	if ok, err := e.plugin.Release(ctx, e.store, "a", r.ID); err != nil || ok {
		t.Fatalf("release after commit = %v, %v", ok, err)
	}
}

func TestReservationReleaseAndSweep(t *testing.T) {
	e := newEnvWith(t, fixed(quota.Limit{Bytes: 100}), nil)
	ctx := t.Context()
	keep, err := e.plugin.Reserve(ctx, e.store, "a", 30, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := e.plugin.Reserve(ctx, e.store, "a", 40, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.plugin.Sweep(ctx, e.store, gone.ExpiresAt); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want the expired one only", n, err)
	}
	if u, _ := e.plugin.Usage(ctx, e.store, "a"); u.Reserved != 30 {
		t.Fatalf("reserved after sweep = %d, want 30", u.Reserved)
	}
	if ok, err := e.plugin.Release(ctx, e.store, "a", keep.ID); err != nil || !ok {
		t.Fatalf("release = %v, %v", ok, err)
	}
	if u, _ := e.plugin.Usage(ctx, e.store, "a"); u.Reserved != 0 {
		t.Fatalf("reserved after release = %d", u.Reserved)
	}
	// A reservation of another scope cannot be released, or spent, here.
	other, _ := e.plugin.Reserve(ctx, e.store, "b", 10, time.Hour)
	if ok, _ := e.plugin.Release(ctx, e.store, "a", other.ID); ok {
		t.Fatal("released another scope's reservation")
	}
	// Recount keeps open reservations.
	if u, err := e.plugin.Recount(ctx, e.store, "b"); err != nil || u.Reserved != 10 {
		t.Fatalf("recount = %+v, %v", u, err)
	}
}

// Concurrent reservations can never promise more than the limit.
func TestConcurrentReservationsStayWithinLimit(t *testing.T) {
	e := newEnvWith(t, fixed(quota.Limit{Bytes: 100}), nil)
	if _, err := e.plugin.Reserve(t.Context(), e.store, "a", 1, time.Hour); err != nil { // the row exists
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := int64(1)
	for range 20 {
		wg.Go(func() {
			if _, err := e.plugin.Reserve(context.Background(), e.store, "a", 10, time.Hour); err == nil {
				mu.Lock()
				granted += 10
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	u, _ := e.plugin.Usage(t.Context(), e.store, "a")
	if granted > 100 || u.Reserved != granted {
		t.Fatalf("granted %d, reserved %d; limit 100", granted, u.Reserved)
	}
}

func TestUsageEvents(t *testing.T) {
	sink := &usageSink{}
	e := newEnvWith(t, fixed(quota.Limit{}), []records.Plugin{sink}, quota.WithUsageEvents())
	f, err := e.add(t, "a", "k1", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Purge(t.Context(), "a", f.ID); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 2 {
		t.Fatalf("events = %+v", sink.events)
	}
	add, del := sink.events[0], sink.events[1]
	if add.Detail[quota.DetailBytes] != "7" || add.Detail[quota.DetailDeltaFiles] != "1" ||
		del.Detail[quota.DetailBytes] != "0" || del.Detail[quota.DetailDeltaBytes] != "-7" || add.Scope != "a" {
		t.Fatalf("events = %+v", sink.events)
	}
	if add.ID == "" || add.ID == del.ID {
		t.Fatalf("ids %q, %q: each change needs its own id", add.ID, del.ID)
	}

	// Off by default.
	quiet := &usageSink{}
	q := newEnvWith(t, fixed(quota.Limit{}), []records.Plugin{quiet})
	if _, err := q.add(t, "a", "k1", 7); err != nil {
		t.Fatal(err)
	}
	if len(quiet.events) != 0 {
		t.Fatalf("events without WithUsageEvents: %+v", quiet.events)
	}
}

// TestReleaseEventsOnlyWhenUsageMoves: purging a charged file releases it
// and emits quota.usage_changed with the real change; purging a file that
// was never charged (recorded before quota was installed) leaves usage at
// zero and emits nothing, on every engine (MySQL's row count says nothing
// about it).
func TestReleaseEventsOnlyWhenUsageMoves(t *testing.T) {
	ctx := t.Context()
	db := testdb.Open(t)
	disk, _ := filesystem.NewDisk("media", memory.New())
	mgr, _ := filesystem.NewManager(disk)
	plain, err := records.New(db, testdb.Dialect(), mgr)
	if err != nil {
		t.Fatal(err)
	}
	sink := &usageSink{}
	p, _ := quota.New(fixed(quota.Limit{}), quota.WithUsageEvents())
	s, err := records.New(db, testdb.Dialect(), mgr, records.WithPlugins(p, sink))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	put := func(st *records.Store, key string, size int) records.File {
		t.Helper()
		if _, err := disk.WriteBytes(ctx, key, []byte(strings.Repeat("x", size)), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
		f, err := st.CreateFile(ctx, records.NewFile{Scope: "a", Disk: "media", Key: key})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	uncharged := put(plain, "old", 3) // before quota: never charged
	charged := put(s, "new", 5)
	if err := s.Purge(ctx, "a", charged.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(sink.events); n != 2 || sink.events[1].Detail[quota.DetailDeltaBytes] != "-5" || sink.events[1].Detail[quota.DetailBytes] != "0" {
		t.Fatalf("events after a real release = %+v", sink.events)
	}
	if err := s.Purge(ctx, "a", uncharged.ID); err != nil {
		t.Fatal(err)
	}
	if u, _ := p.Usage(ctx, s, "a"); u != (quota.Usage{}) || len(sink.events) != 2 {
		t.Fatalf("usage %+v, events %+v: releasing an uncharged file moved nothing and must say nothing", u, sink.events)
	}
}
