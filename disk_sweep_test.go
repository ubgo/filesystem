package filesystem_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// sweepLiar declares SweepAbandoned without implementing it.
type sweepLiar struct{ *basicDriver }

func (sweepLiar) Capabilities() filesystem.Capabilities {
	return filesystem.Capabilities{SweepAbandoned: true}
}

// flakySweep fails its first sweep part-way, after removing one upload.
type flakySweep struct {
	*memory.Driver
	failed bool
}

func (f *flakySweep) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	if !f.failed {
		f.failed = true
		return 1, filesystem.ErrUnavailable
	}
	return f.Driver.SweepAbandoned(ctx, before)
}

func TestSweepAbandoned(t *testing.T) {
	ctx := t.Context()
	if _, err := filesystem.NewDisk("liar", sweepLiar{newBasic()}); filesystem.CodeOf(err) != filesystem.CodeInvalidArgument {
		t.Errorf("declared without the interface: %v", err)
	}
	_, err := mustDisk(t, "basic", newBasic()).SweepAbandoned(ctx, time.Now())
	if !errors.Is(err, filesystem.ErrUnsupported) {
		t.Errorf("without the capability: %v", err)
	}

	// The operation passes through interceptors as a mutation, so a
	// read-only disk refuses it.
	var seen []filesystem.Op
	rec := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		seen = append(seen, *op)
		return call(ctx)
	}
	d := mustDisk(t, "mem", memory.New(), filesystem.WithInterceptors(rec))
	if _, err := d.CreateMultipart(ctx, "a.bin", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if n, err := d.SweepAbandoned(ctx, time.Now().Add(time.Hour)); n != 1 || err != nil {
		t.Errorf("sweep = %d, %v", n, err)
	}
	last := seen[len(seen)-1]
	if last.Kind != filesystem.OpSweepAbandoned || !last.Mutating || !last.Replayable || !filesystem.OpSweepAbandoned.Mutates() {
		t.Errorf("op = %+v", last)
	}
	ro := mustDisk(t, "ro", memory.New(), filesystem.WithInterceptors(filesystem.ReadOnly()))
	if _, err := ro.SweepAbandoned(ctx, time.Now()); !errors.Is(err, filesystem.ErrReadOnly) {
		t.Errorf("read-only disk: %v", err)
	}

	// A retried attempt's count adds to the failed one's.
	retry := func(ctx context.Context, _ *filesystem.Op, call filesystem.Call) error {
		if err := call(ctx); err != nil {
			return call(ctx)
		}
		return nil
	}
	fd := mustDisk(t, "flaky", &flakySweep{Driver: memory.New()}, filesystem.WithInterceptors(retry))
	if _, err := fd.CreateMultipart(ctx, "b.bin", filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if n, err := fd.SweepAbandoned(ctx, time.Now().Add(time.Hour)); n != 2 || err != nil {
		t.Errorf("retried sweep = %d, %v; want 1 from each attempt", n, err)
	}
}

func TestWrappersDoNotForwardSweepAbandoned(t *testing.T) {
	base := mustDisk(t, "base", memory.New())
	if !base.Capabilities().SweepAbandoned || !strings.Contains(base.Capabilities().String(), "sweep-abandoned") {
		t.Fatalf("memory caps %v", base.Capabilities())
	}
	scoped, err := base.Scope("tenant", "t1")
	if err != nil {
		t.Fatal(err)
	}
	mirrored, err := filesystem.Mirror("mirrored", base, mustDisk(t, "copy", memory.New()))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*filesystem.Disk{scoped, mirrored} {
		if d.Capabilities().SweepAbandoned {
			t.Errorf("%s forwards SweepAbandoned", d.Name())
		}
		if _, err := d.SweepAbandoned(t.Context(), time.Now()); !errors.Is(err, filesystem.ErrUnsupported) {
			t.Errorf("%s: %v", d.Name(), err)
		}
	}
}
