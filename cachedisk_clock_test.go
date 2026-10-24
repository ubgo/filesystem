package filesystem_test

import (
	"sync"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/memory"
)

// fakeClock is a clock tests move by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestCachedReadJudgesTTLByItsClock pins CacheOptions.Now: a copy is
// fresh, stale and re-marked fresh by the given clock, with no sleeping.
func TestCachedReadJudgesTTLByItsClock(t *testing.T) {
	ctx := t.Context()
	const ttl = time.Hour
	clock := &fakeClock{now: time.Now()}
	rd := &countingReads{Driver: memory.New()}
	remote := mustDisk(t, "remote", rd)
	cache := mustDisk(t, "cache", memory.New())
	c, err := filesystem.CachedRead("c", remote, cache, filesystem.CacheOptions{TTL: ttl, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if _, err := remote.WriteBytes(ctx, "k", []byte(body), filesystem.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		t.Helper()
		b, err := c.ReadAll(ctx, "k")
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	write("one")
	read()
	// Changed behind the cache's back, within TTL by the clock: the copy
	// is served.
	write("two")
	if got := read(); got != "one" {
		t.Errorf("fresh copy: read %q, want one", got)
	}
	// Past TTL by the clock: revalidated, the ETag differs, refetched.
	clock.Advance(2 * ttl)
	if got := read(); got != "two" {
		t.Errorf("expired copy: read %q, want two", got)
	}
	// Past TTL again but unchanged: revalidated by ETag and marked fresh
	// at the clock's time, so the next read within TTL asks remote nothing.
	clock.Advance(2 * ttl)
	reads := rd.reads.Load()
	read()
	if e, err := cache.Stat(ctx, "k"); err != nil || !e.ModTime.Equal(clock.Now()) {
		t.Errorf("revalidated copy marked fresh at %v (%v), want %v", e.ModTime, err, clock.Now())
	}
	if rd.reads.Load() != reads {
		t.Errorf("revalidation downloaded again: %d remote reads, want %d", rd.reads.Load(), reads)
	}
}
