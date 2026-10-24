package layer

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
)

// Timeout bounds each attempt of an operation to d (placed inside Retry,
// each try gets its own budget). An attempt that runs out fails with the
// context's deadline error, which classifies as CodeUnavailable, so Retry
// retries it while the caller's own context is still alive. Streaming operations (Read) are left
// alone: their result is a reader that outlives the call, and cancelling the
// context when the call returned would break it; bound those with the
// caller's context instead. d must be positive.
func Timeout(d time.Duration) filesystem.Interceptor {
	if d <= 0 {
		panic("layer.Timeout: duration must be positive")
	}
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Streaming {
			return call(ctx)
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return call(ctx)
	}
}

// Limit allows at most n operations to run at once and makes the rest wait
// (or give up when their context ends). For a Read the slot is held while
// the stream is opened, not while it is consumed. n must be positive;
// Limit panics otherwise, for the same reason as Timeout.
func Limit(n int) filesystem.Interceptor {
	if n <= 0 {
		panic("layer.Limit: n must be positive")
	}
	slots := make(chan struct{}, n)
	return func(ctx context.Context, _ *filesystem.Op, call filesystem.Call) error {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return filesystem.NewError(filesystem.CodeUnavailable, ctx.Err())
		}
		defer func() { <-slots }()
		return call(ctx)
	}
}
