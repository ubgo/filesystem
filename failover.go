package filesystem

import (
	"context"
	"io"
)

// FailoverEvent describes one read that moved from a disk that failed to
// the next one.
type FailoverEvent struct {
	// Op is OpStat or OpRead.
	Op OpKind
	// Key is the key read.
	Key string
	// From names the disk that failed, To the disk tried next.
	From, To string
	// Err is From's error.
	Err error
}

// FailoverOptions configure Failover. The zero value fails over on
// CodeUnavailable and CodeUnknown and reports nothing.
type FailoverOptions struct {
	// ShouldFailover decides which errors move a read to the next disk.
	// nil means CodeUnavailable or CodeUnknown: a backend that is down or
	// answered with something unclassifiable. An answer such as
	// CodeNotFound or CodePermission is the disk's answer, not an outage,
	// and is returned as it is.
	ShouldFailover func(error) bool
	// OnFailover, when set, is called each time a read moves on, to count
	// or log outages of the primary.
	OnFailover func(ctx context.Context, e FailoverEvent)
}

// Failover returns a Disk named name that reads (Stat and Read) from
// primary and, when primary fails as ShouldFailover decides, from each
// secondary in turn, so reads survive an outage of the primary backend.
// Everything else (writes, deletes, listings, URLs, every capability) goes
// to primary alone.
//
// Semantics, which callers must accept:
//   - Secondaries are only read. Keeping them in step is the
//     application's job, typically with Mirror (write to all) or a backfill;
//     a read that fails over returns whatever the secondary holds, which may
//     be older than the primary's copy or missing (CodeNotFound).
//   - Listings are not failed over: a cursor from one backend means nothing
//     to another.
//   - The disk has primary's capabilities and interceptors, except
//     ConditionalRead, so conditional reads run as a Stat and a Read that
//     both fail over, and SweepAbandoned and FindAbandoned (sweep the
//     primary directly). A
//     secondary runs its own interceptors.
//   - When every disk fails, the last secondary's error is returned.
func Failover(name string, primary *Disk, secondaries []*Disk, opts FailoverOptions) (*Disk, error) {
	if primary == nil || len(secondaries) == 0 {
		return nil, Errorf(CodeInvalidArgument, "failover needs a primary and at least one secondary")
	}
	for _, s := range secondaries {
		if s == nil || s == primary {
			return nil, Errorf(CodeInvalidArgument, "failover secondaries must be distinct from the primary and non-nil")
		}
	}
	if opts.ShouldFailover == nil {
		opts.ShouldFailover = func(err error) bool {
			c := CodeOf(err)
			return c == CodeUnavailable || c == CodeUnknown
		}
	}
	// A scoped driver with no prefix forwards every optional interface to
	// the primary unchanged; Failover overrides only the reads.
	drv := &failoverDriver{scopedDriver: newScopedDriver(primary.driver, ""), primary: primary.name, secondaries: secondaries, opts: opts}
	return primary.derive(name, drv, primary.publicBase)
}

// failoverDriver reads from the primary driver, then the secondaries.
type failoverDriver struct {
	*scopedDriver
	primary     string
	secondaries []*Disk
	opts        FailoverOptions
}

// Capabilities are the primary's without ConditionalRead (see Failover)
// and SweepAbandoned and FindAbandoned, which are backend-wide: sweep the
// primary directly.
func (f *failoverDriver) Capabilities() Capabilities {
	c := f.inner.Capabilities()
	c.ConditionalRead, c.SweepAbandoned, c.FindAbandoned = false, false, false
	return c
}

// next reports whether err moves the read on from the disk named from to
// secondary i, and tells OnFailover when it does.
func (f *failoverDriver) next(ctx context.Context, op OpKind, key, from string, i int, err error) bool {
	if i >= len(f.secondaries) || !f.opts.ShouldFailover(err) || ctx.Err() != nil {
		return false
	}
	if f.opts.OnFailover != nil {
		f.opts.OnFailover(ctx, FailoverEvent{Op: op, Key: key, From: from, To: f.secondaries[i].name, Err: err})
	}
	return true
}

func (f *failoverDriver) Stat(ctx context.Context, key string) (Entry, error) {
	e, err := f.inner.Stat(ctx, key)
	from := f.primary
	for i := 0; err != nil && f.next(ctx, OpStat, key, from, i, err); i++ {
		e, err = f.secondaries[i].Stat(ctx, key)
		from = f.secondaries[i].name
	}
	return e, err
}

func (f *failoverDriver) Read(ctx context.Context, key string, rng ByteRange) (io.ReadCloser, error) {
	rc, err := f.inner.Read(ctx, key, rng)
	from := f.primary
	for i := 0; err != nil && f.next(ctx, OpRead, key, from, i, err); i++ {
		if rc != nil {
			_ = rc.Close()
		}
		rc, err = f.secondaries[i].ReadRange(ctx, key, rng)
		from = f.secondaries[i].name
	}
	return rc, err
}
