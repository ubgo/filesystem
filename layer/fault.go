package layer

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
)

// FaultOptions configure Fault. The zero value injects nothing.
type FaultOptions struct {
	// Rate is the probability, from 0 to 1, that an operation fails
	// instead of reaching the driver.
	Rate float64
	// Codes are the codes an injected failure is drawn from, evenly. nil
	// means CodeUnavailable, the transient failure Retry retries.
	Codes []filesystem.Code
	// Latency is added before every operation Fault applies to, failed or
	// not, to simulate a slow backend. It ends early when the context does.
	Latency time.Duration
	// Ops limits Fault to these operations; nil means every operation.
	Ops []filesystem.OpKind
	// Seed makes the sequence of failures repeatable: the same seed and
	// the same order of operations fail the same ones. 0 seeds from the
	// clock.
	Seed uint64
}

// Fault makes a disk misbehave on purpose, for tests and chaos runs: it
// fails a share of operations with chosen codes before they reach the
// driver, and can slow every operation down. Placed inside Retry it proves
// that retries cover the failures it injects; placed outside, that callers
// handle them. An injected failure is an *filesystem.Error whose message
// says it was injected, so it is easy to tell from a real one. Never
// install it in production by accident: it does exactly what it says.
func Fault(opts FaultOptions) filesystem.Interceptor {
	if opts.Rate < 0 || opts.Rate > 1 {
		panic("layer.Fault: Rate must be between 0 and 1")
	}
	if len(opts.Codes) == 0 {
		opts.Codes = []filesystem.Code{filesystem.CodeUnavailable}
	}
	seed := opts.Seed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(seed, seed))
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if opts.Ops != nil && !slices.Contains(opts.Ops, op.Kind) {
			return call(ctx)
		}
		if opts.Latency > 0 {
			if err := sleep(ctx, opts.Latency); err != nil {
				return filesystem.NewError(filesystem.CodeUnavailable, err)
			}
		}
		mu.Lock()
		fail := rng.Float64() < opts.Rate
		code := opts.Codes[rng.IntN(len(opts.Codes))]
		mu.Unlock()
		if fail {
			return filesystem.Errorf(code, "fault injected by layer.Fault")
		}
		return call(ctx)
	}
}
