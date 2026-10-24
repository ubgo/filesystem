package filesystem_test

import (
	"context"
	"io"
	"testing"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/filesystemtest"
	"github.com/ubgo/filesystem/memory"
)

// downDriver is a memory driver whose reads fail with err while down is set.
type downDriver struct {
	*memory.Driver
	down bool
	err  error
}

func (d *downDriver) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if d.down {
		return filesystem.Entry{}, d.err
	}
	return d.Driver.Stat(ctx, key)
}

func (d *downDriver) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if d.down {
		return nil, d.err
	}
	return d.Driver.Read(ctx, key, rng)
}

func TestFailoverConformance(t *testing.T) {
	filesystemtest.Run(t, func(t *testing.T) *filesystem.Disk {
		f, err := filesystem.Failover("failover", mustDisk(t, "primary", memory.New()), []*filesystem.Disk{mustDisk(t, "secondary", memory.New())}, filesystem.FailoverOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return f
	})
}

func TestFailoverReadsTheNextDisk(t *testing.T) {
	ctx := t.Context()
	pd := &downDriver{Driver: memory.New(), err: filesystem.ErrUnavailable}
	primary := mustDisk(t, "primary", pd)
	second := mustDisk(t, "second", &downDriver{Driver: memory.New(), down: true, err: filesystem.ErrUnavailable})
	third := mustDisk(t, "third", memory.New())
	var events []filesystem.FailoverEvent
	f, err := filesystem.Failover("f", primary, []*filesystem.Disk{second, third}, filesystem.FailoverOptions{
		OnFailover: func(_ context.Context, e filesystem.FailoverEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteBytes(ctx, "k", []byte("primary"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := third.WriteBytes(ctx, "k", []byte("third"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := third.Exists(ctx, "k"); !ok {
		t.Fatal("setup")
	}
	if b, _ := f.ReadAll(ctx, "k"); string(b) != "primary" {
		t.Errorf("healthy primary read %q", b)
	}
	if len(events) != 0 {
		t.Errorf("failover events while healthy: %v", events)
	}

	pd.down = true
	b, err := f.ReadAll(ctx, "k")
	if err != nil || string(b) != "third" {
		t.Fatalf("read during the outage: %q, %v", b, err)
	}
	if len(events) != 2 || events[0].From != "primary" || events[0].To != "second" || events[1].From != "second" || events[1].To != "third" || events[1].Op != filesystem.OpRead {
		t.Errorf("events %+v", events)
	}
	if e, err := f.Stat(ctx, "k"); err != nil || e.Size != int64(len("third")) {
		t.Errorf("stat during the outage: %+v, %v", e, err)
	}
	// Writes never fail over.
	pd.Driver = memory.New()
	if _, err := f.WriteBytes(ctx, "w", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := third.Exists(ctx, "w"); ok {
		t.Error("a write reached a secondary")
	}

	// A disk's own answer is not an outage.
	pd.err = filesystem.ErrPermission
	if _, err := f.Stat(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("permission error failed over: %v", err)
	}
	// Every disk down: the last error.
	pd.err = filesystem.ErrUnavailable
	bad := mustDisk(t, "bad", &downDriver{Driver: memory.New(), down: true, err: filesystem.ErrUnavailable})
	all, _ := filesystem.Failover("all", primary, []*filesystem.Disk{bad}, filesystem.FailoverOptions{})
	if _, err := all.Stat(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("all down: %v", err)
	}

	for _, s := range [][]*filesystem.Disk{nil, {nil}, {primary}} {
		if _, err := filesystem.Failover("x", primary, s, filesystem.FailoverOptions{}); err == nil {
			t.Errorf("secondaries %v accepted", s)
		}
	}
}
