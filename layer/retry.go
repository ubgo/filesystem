package layer

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/ubgo/filesystem"
)

// Defaults for RetryOptions. Three attempts with a 100ms base delay ride out
// a throttling burst or a dropped connection without holding a request for
// more than about a second.
const (
	DefaultAttempts  = 3
	DefaultBaseDelay = 100 * time.Millisecond
	DefaultMaxDelay  = 2 * time.Second
)

// RetryOptions configure Retry. The zero value uses the defaults.
type RetryOptions struct {
	// Attempts is the total number of tries, including the first. 0 means
	// DefaultAttempts; 1 disables retrying.
	Attempts int
	// BaseDelay is the backoff before the second attempt; it doubles per
	// attempt up to MaxDelay, and each wait is a random duration up to that
	// bound ("full jitter"), so many clients do not retry in lockstep.
	BaseDelay time.Duration
	// MaxDelay caps the backoff, and the wait Retry will accept from a
	// backend's Retry-After (filesystem.Error.RetryAfter): a backend that
	// asks for a longer wait gets its error returned at once rather than a
	// request held past the cap or a retry it has said it will refuse.
	MaxDelay time.Duration
	// Retryable decides which errors are worth another attempt. nil means
	// errors whose Code is filesystem.CodeUnavailable, the code drivers use
	// for transient failures and the code a context error classifies as (so
	// an attempt cut short by Timeout is retried). Retryable is never asked
	// once the caller's context has ended.
	Retryable func(error) bool
	// Sleep waits for d or until ctx is done. nil means a real timer. It is
	// the escape hatch for tests that must not wait.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Retry retries failed attempts of replayable operations (see
// filesystem.Op.Replayable). A write whose body is a one-shot stream, a move
// and a multipart completion are never retried, because repeating them is
// unsafe or impossible.
//
// When the failed attempt's error carries a Retry-After from the backend
// (filesystem.RetryAfterOf), the wait before the next attempt is at least
// that long; a Retry-After over MaxDelay ends the retries, and the caller
// gets the error with its RetryAfter still set.
//
// Retry never retries once the context it was given has ended: a caller
// that cancelled, or whose own deadline passed, gets the attempt's error
// back at once. Only a deadline set inside Retry (layer.Timeout placed
// after it, so each attempt gets its own budget) leaves that context alive,
// and that is the timeout Retry retries.
func Retry(opts RetryOptions) filesystem.Interceptor {
	if opts.Attempts <= 0 {
		opts.Attempts = DefaultAttempts
	}
	if opts.BaseDelay <= 0 {
		opts.BaseDelay = DefaultBaseDelay
	}
	if opts.MaxDelay <= 0 {
		opts.MaxDelay = DefaultMaxDelay
	}
	if opts.Retryable == nil {
		opts.Retryable = func(err error) bool {
			return filesystem.CodeOf(err) == filesystem.CodeUnavailable
		}
	}
	if opts.Sleep == nil {
		opts.Sleep = sleep
	}
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		var err error
		for attempt := 1; ; attempt++ {
			err = call(ctx)
			if err == nil || attempt >= opts.Attempts || !op.Replayable || ctx.Err() != nil || !opts.Retryable(err) {
				return err
			}
			wait := backoff(opts, attempt)
			if after, ok := filesystem.RetryAfterOf(err); ok {
				if after > opts.MaxDelay {
					return err
				}
				wait = max(wait, after)
			}
			if serr := opts.Sleep(ctx, wait); serr != nil {
				return err
			}
		}
	}
}

// backoff returns a random wait in [0, min(MaxDelay, BaseDelay·2^(n-1))].
func backoff(opts RetryOptions, attempt int) time.Duration {
	bound := opts.BaseDelay << (attempt - 1)
	if bound <= 0 || bound > opts.MaxDelay {
		bound = opts.MaxDelay
	}
	// Jitter needs no cryptographic randomness.
	return time.Duration(rand.Int64N(int64(bound) + 1))
}

// sleep waits for d unless ctx ends first.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
