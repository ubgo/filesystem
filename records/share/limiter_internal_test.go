package share

import (
	"testing"
	"time"
)

// TestLimiterCap: a full limiter drops its oldest window for a new key
// rather than growing, and drops expired windows as attempts are taken.
func TestLimiterCap(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	l := newWindowLimiter(1, time.Minute, 2, func() time.Time { return now })
	for _, k := range []string{"a", "b", "c"} {
		if !l.Allow(k) {
			t.Fatalf("first attempt of %s refused", k)
		}
		now = now.Add(time.Second)
	}
	if len(l.seen) != 2 || l.seen["a"] != nil || l.Allow("b") || l.Allow("c") {
		t.Fatalf("seen %d keys, a kept %v", len(l.seen), l.seen["a"] != nil)
	}
	now = now.Add(time.Minute)
	l.Allow("d")
	if len(l.seen) != 1 || l.oldest != l.seen["d"] || l.newest != l.seen["d"] {
		t.Fatalf("expired windows kept: %d keys", len(l.seen))
	}
}
