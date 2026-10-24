package layer_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// TestBandwidthFollowsItsClock pins BandwidthOptions: with a fake clock
// and a Sleep that advances it, moving 1000 bytes at 100 bytes a second
// asks for about ten seconds of waits (the first burst is free) and
// takes no real time.
func TestBandwidthFollowsItsClock(t *testing.T) {
	const rate, size = 100, 1000
	var (
		mu     sync.Mutex
		now    = time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
		waited time.Duration
		sleeps int
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
		waited += d
		sleeps++
		return nil
	}
	d := disk(t, memory.New(), layer.Bandwidth(rate, layer.BandwidthOptions{Now: clock, Sleep: sleep}))
	start := time.Now()
	if _, err := d.Write(t.Context(), "k", strings.NewReader(strings.Repeat("x", size)), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	burst := time.Second / 10 // bandwidthBurst: a tenth of a second's worth
	want := time.Duration(size)*time.Second/rate - burst
	if sleeps == 0 || waited < want-burst || waited > want+burst {
		t.Errorf("waited %s over %d sleeps, want about %s", waited, sleeps, want)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("throttling took %s of real time with a fake clock", elapsed)
	}
}

// TestBandwidthRefusesTwoOptions pins that a second BandwidthOptions is a
// programming error, not silently ignored.
func TestBandwidthRefusesTwoOptions(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("two BandwidthOptions did not panic")
		}
	}()
	layer.Bandwidth(1, layer.BandwidthOptions{}, layer.BandwidthOptions{})
}
