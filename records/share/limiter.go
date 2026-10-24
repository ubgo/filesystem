package share

import (
	"sync"
	"time"
)

// AttemptLimiter bounds password guessing on a link. key is the link's ID
// (never its token), so only links that exist are ever tracked. The
// Handler takes a slot with Allow before it checks a password, so the
// check and the count are one step and parallel guesses cannot overrun
// the allowance, and gives the slot back with Release when the password
// was right: only wrong passwords stay counted, so the owner of a link is
// never limited by their own use of it. Once a key is refused the Handler
// checks no password for it, the right one included, until the limiter
// allows it again: answering a right password then would let guessing
// go on. Supply one backed by a shared store (Redis, the database) when
// several instances serve the same links; the default, NewAttemptLimiter,
// counts in this process only.
type AttemptLimiter interface {
	// Allow takes one attempt for key and reports whether a password may
	// be checked; a refused attempt takes nothing.
	Allow(key string) bool
	// Release gives back an attempt Allow took, for a right password.
	Release(key string)
}

// Default allowance of the Handler's own limiter.
const (
	DefaultAttempts      = 10
	DefaultAttemptWindow = 15 * time.Minute
)

// NewAttemptLimiter allows max attempts in flight or failed per key in
// each window, counted in this process from the first attempt of the
// window. Its state lives in the value it returns, never in a package
// variable, and holds at most MaxTrackedKeys keys: expired windows are
// dropped oldest first as attempts are taken, and past the cap the oldest
// window is dropped, both in amortised constant time.
func NewAttemptLimiter(max int, window time.Duration) AttemptLimiter {
	return NewAttemptLimiterClock(max, window, time.Now)
}

// NewAttemptLimiterClock is NewAttemptLimiter reading time from now, so a
// window follows the same clock as the rest of a deployment: the Handler
// builds its own limiter with the Store's clock (records.WithClock), and a
// test can age a window without sleeping. A nil now is time.Now.
func NewAttemptLimiterClock(max int, window time.Duration, now func() time.Time) AttemptLimiter {
	if now == nil {
		now = time.Now
	}
	return newWindowLimiter(max, window, MaxTrackedKeys, now)
}

// MaxTrackedKeys bounds the memory of NewAttemptLimiter's limiter.
const MaxTrackedKeys = 100_000

func newWindowLimiter(max int, window time.Duration, keys int, now func() time.Time) *windowLimiter {
	return &windowLimiter{max: max, window: window, keys: keys, now: now, seen: map[string]*failures{}}
}

// failures is one key's window: when it started and how many attempts it
// holds (taken and not released), linked into the limiter's queue.
type failures struct {
	key        string
	start      time.Time
	n          int
	prev, next *failures
}

type windowLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	keys   int
	now    func() time.Time
	seen   map[string]*failures
	// oldest and newest end the queue of seen's windows, oldest start
	// first: a window starts only when its key joins at the newest end,
	// so the oldest end is always the first to expire.
	oldest, newest *failures
}

// Allow implements AttemptLimiter, first dropping every expired window.
func (l *windowLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for l.oldest != nil && now.Sub(l.oldest.start) >= l.window {
		l.dropOldest()
	}
	if a := l.seen[key]; a != nil {
		if a.n >= l.max {
			return false
		}
		a.n++
		return true
	}
	if l.max <= 0 {
		return false
	}
	if len(l.seen) >= l.keys {
		l.dropOldest()
	}
	a := &failures{key: key, start: now, n: 1, prev: l.newest}
	if l.newest != nil {
		l.newest.next = a
	} else {
		l.oldest = a
	}
	l.newest = a
	l.seen[key] = a
	return true
}

// Release implements AttemptLimiter; a key it no longer tracks is a no-op.
func (l *windowLimiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.seen[key]; a != nil && a.n > 0 {
		a.n--
	}
}

// dropOldest forgets the oldest window, unlinking it from the front of
// the queue. Only the oldest is ever dropped (expired first, or evicted
// at the cap), so the queue needs no unlink from its middle.
func (l *windowLimiter) dropOldest() {
	a := l.oldest
	delete(l.seen, a.key)
	l.oldest = a.next
	if a.next != nil {
		a.next.prev = nil
	} else {
		l.newest = nil
	}
}
