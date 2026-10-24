package layer

import (
	"context"
	"errors"
	"time"

	"github.com/ubgo/filesystem"
)

// Hedge cuts the tail latency of Stat and Read (the operations marked
// filesystem.Op.Hedgeable): when the first attempt has not answered after
// after, it starts a second one and returns whichever answers first,
// success or error. When both succeed, the winner is the attempt whose
// result the Disk kept (the other returns filesystem.ErrSuperseded). The
// other attempt's context is cancelled and Hedge waits for it to return,
// so nothing it opened leaks: the Disk closes a reader the losing attempt
// opened. The winning Read's context is released when its stream is
// closed. Every other operation runs once, unchanged, because running it
// twice is unsafe or pointless.
//
// Pick after near the operation's 95th-percentile latency: lower doubles
// the load on the backend, higher rarely hedges. Place Hedge after
// Observe, Log and Retry, so they see one operation and each retry may be
// hedged; interceptors placed after Hedge run once per attempt, possibly
// at the same time, and must not read Op.Attempt or call Op.WrapBody (so
// Bandwidth goes before Hedge). after must be positive.
func Hedge(after time.Duration) filesystem.Interceptor {
	if after <= 0 {
		panic("layer.Hedge: after must be positive")
	}
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if !op.Hedgeable {
			return call(ctx)
		}
		// Attempts may overlap from here on: the Disk keeps the first
		// success and supersedes the other.
		op.Concurrent = true
		type answer struct {
			attempt int
			err     error
		}
		answers := make(chan answer, 2)
		cancels := make([]context.CancelFunc, 0, 2)
		start := func(attempt int) {
			actx, cancel := context.WithCancel(ctx)
			cancels = append(cancels, cancel)
			go func() { answers <- answer{attempt: attempt, err: call(actx)} }()
		}
		// settle releases every attempt's context except the winner's
		// on a stream it returned, which is released when the caller
		// closes the stream.
		settle := func(first answer) error {
			for i, cancel := range cancels {
				if i == first.attempt && first.err == nil && op.Streaming {
					op.OnClose(cancel)
					continue
				}
				cancel()
			}
			return first.err
		}
		start(0)
		timer := time.NewTimer(after)
		defer timer.Stop()
		select {
		case first := <-answers:
			return settle(first)
		case <-timer.C:
		}
		start(1)
		first := <-answers
		if errors.Is(first.err, filesystem.ErrSuperseded) {
			// The other attempt delivered the result, so it is the
			// winner, whichever answered first.
			return settle(<-answers)
		}
		cancels[1-first.attempt]()
		<-answers
		return settle(first)
	}
}
