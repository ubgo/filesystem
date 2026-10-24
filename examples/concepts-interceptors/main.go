// Command concepts-interceptors shows the interceptor chain: the order
// interceptors run in, Retry with a flaky backend (and the one-shot stream
// it refuses to replay), Timeout and what its deadline error looks like,
// Limit, ReadOnly, Guard, Log and Observe, then Bandwidth, Fault and
// Hedge.
//
//	go run ./concepts-interceptors          (from the examples directory)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// Timings of the demo, short so it runs in milliseconds.
const (
	// attemptBudget is the per-attempt timeout given to layer.Timeout.
	attemptBudget = 20 * time.Millisecond
	// waitForSlot is how long a caller waits for a Limit slot before
	// giving up.
	waitForSlot = 20 * time.Millisecond
	// transientFailures is how many times the flaky driver fails before
	// it succeeds.
	transientFailures = 2
)

// Settings of the last three steps, chosen so they finish in well under a
// second while still showing the effect.
const (
	// bandwidthCap is the disk's byte rate in the bandwidth demo.
	bandwidthCap = 20_000
	// bandwidthBody is how much the bandwidth demo writes: three tenths of
	// a second at bandwidthCap, less the one burst the bucket starts with.
	bandwidthBody = 6_000
	// bandwidthFloor is the least time that write can take at the cap.
	bandwidthFloor = 150 * time.Millisecond
	// faultRate is the share of operations the fault demo fails.
	faultRate = 0.5
	// faultSeed makes the injected failures the same on every run.
	faultSeed = 7
	// faultWrites is how many writes the fault demo makes.
	faultWrites = 20
	// hedgeAfter is how long a read may take before Hedge starts a second
	// attempt; in production, about the operation's p95 latency.
	hedgeAfter = 10 * time.Millisecond
)

// tenantPrefix is the only part of the disk the Guard demo allows.
const tenantPrefix = "tenants/acme/"

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run is main without the process, so the test can call it.
func run(ctx context.Context, out io.Writer) error {
	for _, step := range []func(context.Context, io.Writer) error{
		order, retry, oneShotStream, timeout, limit, readOnly, guard, logOps,
		bandwidth, faults, hedge,
	} {
		if err := step(ctx, out); err != nil {
			return err
		}
	}
	return nil
}

// trace is an interceptor that prints when it is entered and left, to
// show the order of the chain.
// ds:def id=trace-c9p637eb
func trace(name string, out io.Writer) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		fmt.Fprintf(out, "  enter %s (%s %s)\n", name, op.Kind, op.Key)
		err := call(ctx)
		fmt.Fprintf(out, "  leave %s\n", name)
		return err
	}
}

// order shows that the first interceptor given is the outermost one.
// ds:def id=order-t2nkyn8y
func order(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New(),
		filesystem.WithInterceptors(trace("outer", out), trace("inner", out)))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "order:")
	_, err = disk.Exists(ctx, "a.txt")
	return err
}

// flaky is a memory driver whose Stat and Write fail transientFailures
// times with CodeUnavailable before working, like a throttled backend.
// Embedding keeps every capability of the memory driver.
// ds:def id=flaky-d97z36uh
type flaky struct {
	*memory.Driver
	stats, writes int
}

func (f *flaky) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if f.stats++; f.stats <= transientFailures {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "throttled")
	}
	return f.Driver.Stat(ctx, key)
}

func (f *flaky) Write(ctx context.Context, key string, body io.Reader, opts filesystem.WriteOptions) (filesystem.Entry, error) {
	if f.writes++; f.writes <= transientFailures {
		_, _ = io.Copy(io.Discard, body) // a failed upload still consumes its body
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "connection reset")
	}
	return f.Driver.Write(ctx, key, body, opts)
}

// noWait replaces Retry's backoff timer so the demo does not sleep.
func noWait(context.Context, time.Duration) error { return nil }

// report is an Observe callback that prints each finished operation.
func report(out io.Writer) func(context.Context, layer.Event) {
	return func(_ context.Context, e layer.Event) {
		fmt.Fprintf(out, "  observed %s %s: attempts=%d code=%q\n", e.Op.Kind, e.Op.Key, e.Op.Attempt, e.Code)
	}
}

// retry puts Observe outside Retry, so the observer sees one operation
// with its attempt count. A seekable body (strings.Reader) is rewound and
// written again.
// ds:def id=retry-vgfqxqfc
func retry(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", &flaky{Driver: memory.New()}, filesystem.WithInterceptors(
		layer.Observe(report(out)),
		layer.Retry(layer.RetryOptions{Sleep: noWait}),
	))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "retry:")
	if _, err := disk.Write(ctx, "a.txt", strings.NewReader("seekable"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.Stat(ctx, "a.txt")
	return err
}

// oneShotStream writes a body that cannot be rewound (an HTTP request
// body behaves the same). The Disk marks the operation not replayable, so
// Retry returns the first failure instead of writing a truncated body.
// ds:def id=oneshotstream-heknvesg
func oneShotStream(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", &flaky{Driver: memory.New()}, filesystem.WithInterceptors(
		layer.Observe(report(out)),
		layer.Retry(layer.RetryOptions{Sleep: noWait}),
	))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "one-shot stream:")
	stream := io.MultiReader(strings.NewReader("streamed once"))
	_, err = disk.Write(ctx, "b.txt", stream, filesystem.WriteOptions{})
	fmt.Fprintf(out, "  write: %s\n", filesystem.CodeOf(err))
	return nil
}

// slow is a memory driver whose Stat waits until it is released or its
// context ends, like a backend that stopped answering.
// ds:def id=slow-3dqtn4yr
type slow struct {
	*memory.Driver
	release chan struct{}
}

func (s *slow) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	select {
	case <-s.release:
		return s.Driver.Stat(ctx, key)
	case <-ctx.Done():
		return filesystem.Entry{}, ctx.Err()
	}
}

// timeout bounds each attempt. A driver that returns the bare context
// error still gives CodeUnavailable (a context that ended classifies as
// unavailable), so the default Retryable retries a per-attempt deadline;
// Retry stops at once when the caller's own context has ended.
// ds:def id=timeout-e4rdgcdr
func timeout(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", &slow{Driver: memory.New(), release: make(chan struct{})},
		filesystem.WithInterceptors(
			layer.Observe(report(out)),
			layer.Retry(layer.RetryOptions{Sleep: noWait}),
			layer.Timeout(attemptBudget),
		))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "timeout:")
	_, err = disk.Stat(ctx, "a.txt")
	fmt.Fprintf(out, "  deadline exceeded: %t\n", errors.Is(err, context.DeadlineExceeded))
	return nil
}

// limit allows one operation at a time. While a slow Stat holds the only
// slot, a second caller whose context ends first gets CodeUnavailable.
// ds:def id=limit-4vspbm26
func limit(ctx context.Context, out io.Writer) error {
	drv := &slow{Driver: memory.New(), release: make(chan struct{})}
	disk, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(layer.Limit(1)))
	if err != nil {
		return err
	}
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		_, _ = disk.Stat(ctx, "a.txt")
	}()
	<-started
	time.Sleep(waitForSlot) // let the first Stat take the slot
	short, cancel := context.WithTimeout(ctx, waitForSlot)
	defer cancel()
	_, err = disk.Stat(short, "b.txt")
	fmt.Fprintf(out, "limit: second caller %s\n", filesystem.CodeOf(err))
	close(drv.release)
	<-done
	return nil
}

// readOnly refuses every mutation before it reaches the driver, signed
// PUT URLs included, while reads still work.
// ds:def id=readonly-vmd3t6en
func readOnly(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("archive", memory.New(), filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "read-only write: %s\n", filesystem.CodeOf(err))
	_, err = disk.Exists(ctx, "a.txt")
	fmt.Fprintf(out, "read-only exists: %v\n", err)
	return nil
}

// guard applies one rule to every operation: only keys under one tenant's
// prefix. A plain error from the check becomes CodePermission.
// ds:def id=guard-ru54uzvj
func guard(ctx context.Context, out io.Writer) error {
	onlyAcme := func(_ context.Context, op *filesystem.Op) error {
		if !strings.HasPrefix(op.Key, tenantPrefix) {
			return fmt.Errorf("key %q is outside %s", op.Key, tenantPrefix)
		}
		return nil
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(layer.Guard(onlyAcme)))
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, tenantPrefix+"logo.png", []byte("png"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "guard inside: %v\n", err)
	_, err = disk.WriteBytes(ctx, "tenants/globex/logo.png", []byte("png"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "guard outside: %s\n", filesystem.CodeOf(err))
	return nil
}

// dropVolatile removes the time and duration attributes, so the demo's
// log lines are the same on every run.
func dropVolatile(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey || a.Key == layer.AttrDuration {
		return slog.Attr{}
	}
	return a
}

// logOps writes one structured record per operation. Successes and
// not-found answers to reads log at Debug, failures at Warn.
// ds:def id=logops-rk8jecbk
func logOps(ctx context.Context, out io.Writer) error {
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropVolatile}))
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(layer.Log(logger)))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, _ = disk.Exists(ctx, "missing.txt")
	_ = disk.SetVisibility(ctx, "missing.txt", filesystem.VisibilityPublic)
	return nil
}

// bandwidth caps a disk's total byte rate. A write of bandwidthBody bytes
// at bandwidthCap bytes a second cannot finish faster than the cap allows,
// however fast the backend is; reads are throttled the same way.
// ds:def id=bandwidth-gx8pkgzr
func bandwidth(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(layer.Bandwidth(bandwidthCap)))
	if err != nil {
		return err
	}
	start := time.Now()
	if _, err := disk.Write(ctx, "big.bin", strings.NewReader(strings.Repeat("x", bandwidthBody)), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Fprintf(out, "bandwidth: %d bytes at %d B/s took at least %s: %t\n",
		bandwidthBody, bandwidthCap, bandwidthFloor, time.Since(start) >= bandwidthFloor)
	return nil
}

// faults injects failures, for tests and chaos runs. Outside Retry every
// injected failure reaches the caller; inside Retry, with a fixed seed,
// every write still succeeds and the attempts counter shows how many
// failures the retries absorbed, the same number on every run.
// ds:def id=faults-edeunkmc
func faults(ctx context.Context, out io.Writer) error {
	always := layer.Fault(layer.FaultOptions{Rate: 1, Seed: faultSeed})
	bare, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(always))
	if err != nil {
		return err
	}
	_, err = bare.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "fault without retry: %s\n", filesystem.CodeOf(err))
	attempts := 0
	count := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		attempts++
		return call(ctx)
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(
		layer.Retry(layer.RetryOptions{Attempts: 10, Sleep: noWait}),
		count,
		layer.Fault(layer.FaultOptions{Rate: faultRate, Seed: faultSeed, Ops: []filesystem.OpKind{filesystem.OpWrite}}),
	))
	if err != nil {
		return err
	}
	for i := range faultWrites {
		if _, err := disk.WriteBytes(ctx, fmt.Sprintf("f%d.txt", i), []byte("x"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "fault inside retry: %d writes stored in %d attempts\n", faultWrites, attempts)
	return nil
}

// stuck is a memory driver whose first Read never answers until its
// context ends, like one request lost on a slow backend node; every later
// Read answers at once.
// ds:def id=stuck-95ayc2cx
type stuck struct {
	*memory.Driver
	reads atomic.Int32
}

func (s *stuck) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if s.reads.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Driver.Read(ctx, key, rng)
}

// hedge starts a second attempt of a read that has not answered after
// hedgeAfter and returns whichever answers first, cancelling the other.
// The stuck first read would hang forever; the hedged one answers.
// ds:def id=hedge-v7agwz6w
func hedge(ctx context.Context, out io.Writer) error {
	drv := &stuck{Driver: memory.New()}
	disk, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(layer.Hedge(hedgeAfter)))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("answered"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	b, err := disk.ReadAll(ctx, "a.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "hedge: read %q after %d driver reads\n", b, drv.reads.Load())
	return nil
}
