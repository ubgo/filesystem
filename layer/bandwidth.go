package layer

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// bandwidthBurst is how much traffic, as a share of one second's worth,
// the bucket lets through at once after an idle spell: a tenth of a second,
// so short reads are not delayed while the average stays at the rate.
const bandwidthBurst = 10

// BandwidthOptions configure Bandwidth. The zero value is the real clock
// and a real timer.
type BandwidthOptions struct {
	// Now is the clock the token bucket is kept by; nil means time.Now.
	Now func() time.Time
	// Sleep waits for d or until ctx is done; nil means a real timer. With
	// Now it makes throttling testable without waiting: a fake Sleep can
	// record the waits the bucket asks for and advance the fake clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Bandwidth caps the bytes per second moving through the disk it is
// installed on: the bodies of writes, appends and uploaded parts as the
// driver reads them, and the readers reads and version reads return as
// the caller reads them. Every operation of the disk draws from one token bucket, so the
// cap is the disk's total, not each transfer's; install it on the disks
// (or scoped disks) whose traffic should share a cap. It throttles bytes,
// not calls: combine it with Limit for concurrency. A transfer waiting for
// tokens gives up when the operation's context ends. Place it before
// Hedge. bytesPerSec must be positive. At most one BandwidthOptions may be
// given (a fake clock in tests); more panics, like a non-positive rate,
// because both are programming errors.
func Bandwidth(bytesPerSec int64, opts ...BandwidthOptions) filesystem.Interceptor {
	if bytesPerSec <= 0 {
		panic("layer.Bandwidth: bytesPerSec must be positive")
	}
	if len(opts) > 1 {
		panic("layer.Bandwidth: at most one BandwidthOptions")
	}
	var o BandwidthOptions
	if len(opts) == 1 {
		o = opts[0]
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	b := &bucket{rate: bytesPerSec, burst: max(bytesPerSec/bandwidthBurst, 1), now: o.Now, sleep: o.Sleep}
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		switch op.Kind {
		case filesystem.OpRead, filesystem.OpReadVersion, filesystem.OpWrite, filesystem.OpAppend, filesystem.OpMultipartUpload:
			op.WrapBody(func(r io.Reader) io.Reader {
				// Placed inside Retry, this runs once per attempt; a body
				// already throttled by this bucket is not charged twice.
				if t, ok := r.(*throttled); ok && t.b == b {
					return r
				}
				return &throttled{ctx: ctx, r: r, b: b}
			})
		}
		return call(ctx)
	}
}

// bucket is a token bucket kept as the time its debt is paid off: taking n
// bytes pushes that time n/rate further, and a taker waits until it.
type bucket struct {
	mu    sync.Mutex
	rate  int64 // bytes per second
	burst int64 // bytes a transfer may take at once
	free  time.Time
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// take charges n bytes and waits until the bucket has paid for them.
func (b *bucket) take(ctx context.Context, n int) error {
	b.mu.Lock()
	now := b.now()
	// Unused capacity does not accumulate beyond one burst.
	if floor := now.Add(-b.cost(b.burst)); b.free.Before(floor) {
		b.free = floor
	}
	b.free = b.free.Add(b.cost(int64(n)))
	wait := b.free.Sub(now)
	b.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	return b.sleep(ctx, wait)
}

// cost is how long n bytes take at the rate. It is computed in floating
// point: n·10⁹ overflows an int64 once a burst passes about 9 GB, which
// a rate near 90 GB/s reaches.
func (b *bucket) cost(n int64) time.Duration {
	return time.Duration(float64(n) * float64(time.Second) / float64(b.rate))
}

// throttled is a reader that pays for every chunk it delivers.
type throttled struct {
	ctx context.Context
	r   io.Reader
	b   *bucket
}

// Read reads at most one burst and waits for its tokens before returning.
func (t *throttled) Read(p []byte) (int, error) {
	if int64(len(p)) > t.b.burst {
		p = p[:t.b.burst]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if werr := t.b.take(t.ctx, n); werr != nil {
			return n, filesystem.NewError(filesystem.CodeUnavailable, werr)
		}
	}
	return n, err
}
