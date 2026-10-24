package layer_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

func TestBandwidthCapsThroughput(t *testing.T) {
	ctx := t.Context()
	const rate = 1 << 20 // 1 MiB/s
	d := disk(t, memory.New(), layer.Bandwidth(rate))
	body := bytes.Repeat([]byte("x"), rate/4)

	// A quarter of a second's worth, less the one burst let through at
	// once, takes about 150ms each way.
	const want, tolerance = 150 * time.Millisecond, 60 * time.Millisecond
	start := time.Now()
	if _, err := d.WriteBytes(ctx, "k", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < want-tolerance || took > want+3*tolerance {
		t.Errorf("throttled write took %s, want about %s", took, want)
	}
	time.Sleep(200 * time.Millisecond) // let the bucket refill
	start = time.Now()
	got, err := d.ReadAll(ctx, "k")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("throttled read changed the bytes: %v", err)
	}
	if took := time.Since(start); took < want-tolerance || took > want+3*tolerance {
		t.Errorf("throttled read took %s, want about %s", took, want)
	}

	// Inside Retry, a retried write is charged once, not once per attempt
	// that registered the throttle.
	var attempts atomic.Int64
	failFirst := func(ctx context.Context, _ *filesystem.Op, call filesystem.Call) error {
		if attempts.Add(1) == 1 {
			return filesystem.ErrUnavailable
		}
		return call(ctx)
	}
	retried := disk(t, memory.New(), layer.Retry(layer.RetryOptions{Sleep: noSleep}), layer.Bandwidth(rate), failFirst)
	start = time.Now()
	if _, err := retried.WriteBytes(ctx, "k", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > want+3*tolerance {
		t.Errorf("retried throttled write took %s, want about %s", took, want)
	}

	// Version reads are throttled like reads.
	versioned := disk(t, memory.New(memory.WithVersions()), layer.Bandwidth(rate))
	if _, err := versioned.WriteBytes(ctx, "v", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	vs, err := versioned.Versions(ctx, "v")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start = time.Now()
	vr, err := versioned.ReadVersion(ctx, "v", vs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, vr); err != nil {
		t.Fatal(err)
	}
	_ = vr.Close()
	if took := time.Since(start); took < want-tolerance {
		t.Errorf("version read took %s, want about %s", took, want)
	}

	// A very high rate neither overflows nor stalls: 1 TiB/s moves a
	// quarter MiB at once.
	fast := disk(t, memory.New(), layer.Bandwidth(1<<40))
	start = time.Now()
	if _, err := fast.WriteBytes(ctx, "k", body, filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, err := fast.ReadAll(ctx, "k"); err != nil || len(b) != len(body) {
		t.Fatalf("fast read: %d bytes, %v", len(b), err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("1 TiB/s moved %d bytes in %s", len(body), took)
	}

	// A transfer gives up when its context ends while it waits.
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := d.WriteBytes(short, "k2", body, filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("write past its deadline: %v", err)
	}
}

func TestFaultIsDeterministicAndRetried(t *testing.T) {
	ctx := t.Context()
	outcomes := func(seed uint64) []bool {
		d := disk(t, memory.New(), layer.Fault(layer.FaultOptions{Rate: 0.5, Seed: seed}))
		var out []bool
		for range 32 {
			_, err := d.Stat(ctx, "missing")
			out = append(out, filesystem.CodeOf(err) == filesystem.CodeUnavailable)
		}
		return out
	}
	a, b := outcomes(7), outcomes(7)
	failed := 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("the same seed failed different operations")
		}
		if a[i] {
			failed++
		}
	}
	if failed == 0 || failed == len(a) {
		t.Errorf("rate 0.5 failed %d of %d", failed, len(a))
	}

	// Retry outside Fault absorbs every injected failure.
	d := disk(t, memory.New(),
		layer.Retry(layer.RetryOptions{Attempts: 10, Sleep: noSleep}),
		layer.Fault(layer.FaultOptions{Rate: 0.5, Seed: 7}))
	for i := range 32 {
		if _, err := d.WriteBytes(ctx, "k", []byte{byte(i)}, filesystem.WriteOptions{}); err != nil {
			t.Fatalf("write %d not retried past the fault: %v", i, err)
		}
	}

	// Ops and Codes narrow what fails and how.
	d = disk(t, memory.New(), layer.Fault(layer.FaultOptions{Rate: 1, Codes: []filesystem.Code{filesystem.CodePermission}, Ops: []filesystem.OpKind{filesystem.OpWrite}}))
	if _, err := d.WriteBytes(ctx, "k", nil, filesystem.WriteOptions{}); filesystem.CodeOf(err) != filesystem.CodePermission {
		t.Errorf("write: %v", err)
	}
	if _, err := d.Stat(ctx, "k"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("stat should be untouched: %v", err)
	}

	// Latency slows every operation.
	d = disk(t, memory.New(), layer.Fault(layer.FaultOptions{Latency: 30 * time.Millisecond}))
	start := time.Now()
	_, _ = d.Stat(ctx, "k")
	if took := time.Since(start); took < 30*time.Millisecond {
		t.Errorf("latency not added: %s", took)
	}
}

// slowFirst is a memory driver whose first Stat and Read stall until
// their context ends, as a request stuck on a bad connection does, and
// which records how many readers it opened and how many were closed.
type slowFirst struct {
	*memory.Driver
	calls, opened, closed atomic.Int64
}

func (s *slowFirst) stall(ctx context.Context) error {
	if s.calls.Add(1) == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (s *slowFirst) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := s.stall(ctx); err != nil {
		return filesystem.Entry{}, err
	}
	return s.Driver.Stat(ctx, key)
}

func (s *slowFirst) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if err := s.stall(ctx); err != nil {
		return nil, err
	}
	rc, err := s.Driver.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	s.opened.Add(1)
	return &countClose{ReadCloser: rc, n: &s.closed}, nil
}

type countClose struct {
	io.ReadCloser
	n *atomic.Int64
}

func (c *countClose) Close() error { c.n.Add(1); return c.ReadCloser.Close() }

func TestHedgeAnswersFromTheSecondAttempt(t *testing.T) {
	ctx := t.Context()
	drv := &slowFirst{Driver: memory.New()}
	if _, err := drv.Driver.Write(ctx, "k", bytes.NewReader([]byte("hello")), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	d := disk(t, drv, layer.Hedge(10*time.Millisecond))
	start := time.Now()
	b, err := d.ReadAll(ctx, "k")
	if err != nil || string(b) != "hello" {
		t.Fatalf("hedged read %q, %v", b, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("hedged read waited for the stalled attempt: %s", took)
	}
	if drv.calls.Load() != 2 || drv.opened.Load() != drv.closed.Load() {
		t.Errorf("%d calls, %d readers opened, %d closed", drv.calls.Load(), drv.opened.Load(), drv.closed.Load())
	}

	drv.calls.Store(0)
	if e, err := d.Stat(ctx, "k"); err != nil || e.Size != 5 {
		t.Errorf("hedged stat %+v, %v", e, err)
	}

	// A fast first attempt is never hedged, and writes never are.
	if _, err := d.Stat(ctx, "k"); err != nil || drv.calls.Load() != 3 {
		t.Errorf("fast stat made %d calls in all, %v", drv.calls.Load(), err)
	}
	var writes atomic.Int64
	counting := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite {
			writes.Add(1)
			time.Sleep(30 * time.Millisecond)
		}
		return call(ctx)
	}
	w := disk(t, memory.New(), layer.Hedge(time.Millisecond), counting)
	if _, err := w.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil || writes.Load() != 1 {
		t.Errorf("slow write ran %d times, %v", writes.Load(), err)
	}
}

// ctxBound is a memory driver whose readers fail once the context they
// were opened with ends, as an HTTP response body does, and whose first
// Read is slow. It keeps every reader's context.
type ctxBound struct {
	*memory.Driver
	calls atomic.Int64
	mu    sync.Mutex
	ctxs  []context.Context
}

func (c *ctxBound) Read(ctx context.Context, key string, rng filesystem.ByteRange) (io.ReadCloser, error) {
	if c.calls.Add(1) == 1 {
		time.Sleep(30 * time.Millisecond)
	}
	rc, err := c.Driver.Read(ctx, key, rng)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ctxs = append(c.ctxs, ctx)
	c.mu.Unlock()
	return &ctxReader{ReadCloser: rc, ctx: ctx}, nil
}

type ctxReader struct {
	io.ReadCloser
	ctx context.Context
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(p)
}

// TestHedgeReturnsTheReaderOfTheAttemptItPicks forces the race where the
// second attempt opens the object first but answers last: the reader
// returned must be the second attempt's, on a context still alive.
func TestHedgeReturnsTheReaderOfTheAttemptItPicks(t *testing.T) {
	ctx := t.Context()
	drv := &ctxBound{Driver: memory.New()}
	if _, err := drv.Driver.Write(ctx, "k", bytes.NewReader([]byte("hello")), filesystem.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	var seen atomic.Int64
	slowAnswer := func(ctx context.Context, _ *filesystem.Op, call filesystem.Call) error {
		n := seen.Add(1)
		err := call(ctx)
		if n == 2 {
			time.Sleep(100 * time.Millisecond) // opened first, answers last
		}
		return err
	}
	d := disk(t, drv, layer.Hedge(10*time.Millisecond), slowAnswer)
	rc, err := d.Read(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil || string(b) != "hello" {
		t.Fatalf("hedged reader read %q, %v", b, err)
	}
	drv.mu.Lock()
	winner := drv.ctxs[0]
	drv.mu.Unlock()
	if winner.Err() != nil {
		t.Error("the winning attempt's context ended before the stream was closed")
	}
	_ = rc.Close()
	if winner.Err() == nil {
		t.Error("closing the stream did not release the winning attempt's context")
	}
}
