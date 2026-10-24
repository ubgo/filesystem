package layer

import (
	"testing"
	"time"
)

// TestBucketCostDoesNotOverflow pins the cost of a whole burst at rates
// where n·10⁹ no longer fits an int64 (a burst of 10 GB at 100 GB/s).
func TestBucketCostDoesNotOverflow(t *testing.T) {
	for _, rate := range []int64{1, 1 << 20, 100e9, 1 << 40} {
		b := &bucket{rate: rate, burst: max(rate/bandwidthBurst, 1)}
		want := time.Second / bandwidthBurst
		if rate < bandwidthBurst {
			want = time.Second / time.Duration(rate)
		}
		if got := b.cost(b.burst); got < want*99/100 || got > want*101/100 {
			t.Errorf("rate %d: a burst costs %s, want %s", rate, got, want)
		}
	}
}
