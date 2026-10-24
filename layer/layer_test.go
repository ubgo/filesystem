package layer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// flaky is a memory driver whose Stat and Write fail transiently the first
// failures times.
type flaky struct {
	*memory.Driver
	failures int64
	calls    atomic.Int64
	err      error
}

func (f *flaky) fail() error {
	if f.calls.Add(1) <= f.failures {
		return f.err
	}
	return nil
}

func (f *flaky) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if err := f.fail(); err != nil {
		return filesystem.Entry{}, err
	}
	return f.Driver.Stat(ctx, key)
}

func (f *flaky) Write(ctx context.Context, key string, body io.Reader, o filesystem.WriteOptions) (filesystem.Entry, error) {
	if err := f.fail(); err != nil {
		_, _ = io.Copy(io.Discard, body)
		return filesystem.Entry{}, err
	}
	return f.Driver.Write(ctx, key, body, o)
}

func disk(t *testing.T, d filesystem.Driver, is ...filesystem.Interceptor) *filesystem.Disk {
	t.Helper()
	out, err := filesystem.NewDisk("d", d, filesystem.WithInterceptors(is...))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestRetry(t *testing.T) {
	ctx := t.Context()
	f := &flaky{Driver: memory.New(), failures: 2, err: filesystem.ErrUnavailable}
	d := disk(t, f, layer.Retry(layer.RetryOptions{Sleep: noSleep}))
	if _, err := d.WriteBytes(ctx, "k", []byte("x"), filesystem.WriteOptions{}); err != nil {
		t.Fatalf("write not retried: %v", err)
	}
	if f.calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", f.calls.Load())
	}

	// Gives up after Attempts.
	f = &flaky{Driver: memory.New(), failures: 10, err: filesystem.ErrUnavailable}
	d = disk(t, f, layer.Retry(layer.RetryOptions{Attempts: 2, Sleep: noSleep}))
	_, err := d.Stat(ctx, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || f.calls.Load() != 2 {
		t.Errorf("err %v after %d calls", err, f.calls.Load())
	}

	// Non-retryable errors and non-replayable ops run once.
	f = &flaky{Driver: memory.New(), failures: 10, err: filesystem.ErrPermission}
	d = disk(t, f, layer.Retry(layer.RetryOptions{Sleep: noSleep}))
	_, _ = d.Stat(ctx, "k")
	if f.calls.Load() != 1 {
		t.Errorf("permission error retried %d times", f.calls.Load())
	}
	f = &flaky{Driver: memory.New(), failures: 10, err: filesystem.ErrUnavailable}
	d = disk(t, f, layer.Retry(layer.RetryOptions{Sleep: noSleep}))
	_, _ = d.Write(ctx, "k", io.MultiReader(strings.NewReader("stream")), filesystem.WriteOptions{})
	if f.calls.Load() != 1 {
		t.Errorf("one-shot stream retried %d times", f.calls.Load())
	}

	// A custom predicate.
	f = &flaky{Driver: memory.New(), failures: 1, err: filesystem.ErrPermission}
	d = disk(t, f, layer.Retry(layer.RetryOptions{Sleep: noSleep, Retryable: func(error) bool { return true }}))
	if _, err := d.Stat(ctx, "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("custom predicate: %v", err)
	}

	// A context that ends during the backoff returns the last error.
	f = &flaky{Driver: memory.New(), failures: 10, err: filesystem.ErrUnavailable}
	cctx, cancel := context.WithCancel(ctx)
	cancelling := func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	d = disk(t, f, layer.Retry(layer.RetryOptions{Sleep: cancelling}))
	_, err = d.Stat(cctx, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || f.calls.Load() != 1 {
		t.Errorf("cancelled backoff: %v after %d calls", err, f.calls.Load())
	}

	// The real sleeper gives up when the context ends during the backoff.
	f = &flaky{Driver: memory.New(), failures: 10, err: filesystem.ErrUnavailable}
	d = disk(t, f, layer.Retry(layer.RetryOptions{BaseDelay: time.Hour, MaxDelay: time.Hour}))
	soon, stop := context.WithTimeout(ctx, 10*time.Millisecond)
	defer stop()
	_, err = d.Stat(soon, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || f.calls.Load() != 1 {
		t.Errorf("real sleeper after the context ended: %v after %d calls", err, f.calls.Load())
	}

	// A caller whose context has already ended is not retried, even with a
	// retryable error and a sleeper that would not notice.
	f = &flaky{Driver: memory.New(), failures: 10, err: filesystem.ErrUnavailable}
	d = disk(t, f, layer.Retry(layer.RetryOptions{Sleep: noSleep}))
	_, err = d.Stat(cctx, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || f.calls.Load() != 1 {
		t.Errorf("ended caller context: %v after %d calls", err, f.calls.Load())
	}
}

// expiring is a memory driver whose first failures Stat calls block until
// their context ends and return the bare context error, as a driver over a
// context-aware client does.
type expiring struct {
	*memory.Driver
	failures int64
	calls    atomic.Int64
}

func (e *expiring) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if e.calls.Add(1) <= e.failures {
		<-ctx.Done()
		return filesystem.Entry{}, ctx.Err()
	}
	return e.Driver.Stat(ctx, key)
}

func TestTimeoutInsideRetryIsRetried(t *testing.T) {
	ctx := t.Context()
	e := &expiring{Driver: memory.New(), failures: 2}
	d := disk(t, e, layer.Retry(layer.RetryOptions{Sleep: noSleep}), layer.Timeout(5*time.Millisecond))
	if _, err := d.Stat(ctx, "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound || e.calls.Load() != 3 {
		t.Fatalf("timed-out attempts not retried: %v after %d calls", err, e.calls.Load())
	}

	// Every attempt timing out ends as unavailable, still naming the deadline.
	e = &expiring{Driver: memory.New(), failures: 10}
	d = disk(t, e, layer.Retry(layer.RetryOptions{Sleep: noSleep}), layer.Timeout(5*time.Millisecond))
	_, err := d.Stat(ctx, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || !errors.Is(err, context.DeadlineExceeded) || e.calls.Load() != layer.DefaultAttempts {
		t.Fatalf("all attempts timed out: %v after %d calls", err, e.calls.Load())
	}

	// The caller's own deadline is not retried: it would only time out again.
	e = &expiring{Driver: memory.New(), failures: 10}
	d = disk(t, e, layer.Retry(layer.RetryOptions{Sleep: noSleep}))
	short, cancel := context.WithTimeout(ctx, 5*time.Millisecond)
	defer cancel()
	_, err = d.Stat(short, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable || e.calls.Load() != 1 {
		t.Fatalf("caller deadline: %v after %d calls", err, e.calls.Load())
	}
}

func TestRetryBackoffIsBoundedAndReal(t *testing.T) {
	var waits []time.Duration
	record := func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	f := &flaky{Driver: memory.New(), failures: 100, err: filesystem.ErrUnavailable}
	d := disk(t, f, layer.Retry(layer.RetryOptions{Attempts: 40, BaseDelay: time.Millisecond, MaxDelay: 8 * time.Millisecond, Sleep: record}))
	_, _ = d.Stat(t.Context(), "k")
	if len(waits) != 39 {
		t.Fatalf("%d waits", len(waits))
	}
	for i, w := range waits {
		if w < 0 || w > 8*time.Millisecond {
			t.Errorf("wait %d = %v outside [0, MaxDelay]", i, w)
		}
	}
	// The real sleeper waits.
	f = &flaky{Driver: memory.New(), failures: 1, err: filesystem.ErrUnavailable}
	d = disk(t, f, layer.Retry(layer.RetryOptions{BaseDelay: time.Millisecond}))
	if _, err := d.Stat(t.Context(), "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Errorf("real sleeper: %v", err)
	}
}

func TestRetryHonoursRetryAfter(t *testing.T) {
	throttled := func(after time.Duration) error {
		return filesystem.NewError(filesystem.CodeUnavailable, errors.New("slow down")).WithRetryAfter(after)
	}
	opts := func(waits *[]time.Duration) layer.RetryOptions {
		return layer.RetryOptions{BaseDelay: time.Millisecond, MaxDelay: time.Second, Sleep: func(_ context.Context, d time.Duration) error {
			*waits = append(*waits, d)
			return nil
		}}
	}

	// Within the cap: the wait is at least what the backend asked for.
	var waits []time.Duration
	f := &flaky{Driver: memory.New(), failures: 2, err: throttled(300 * time.Millisecond)}
	if _, err := disk(t, f, layer.Retry(opts(&waits))).Stat(t.Context(), "missing"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		t.Fatalf("err %v", err)
	}
	if len(waits) != 2 {
		t.Fatalf("waits %v", waits)
	}
	for _, w := range waits {
		if w < 300*time.Millisecond || w > time.Second {
			t.Errorf("wait %v, want in [Retry-After, MaxDelay]", w)
		}
	}

	// Over the cap: no wait, no second attempt, and the caller still sees
	// how long the backend asked for.
	waits = nil
	f = &flaky{Driver: memory.New(), failures: 1, err: throttled(time.Minute)}
	_, err := disk(t, f, layer.Retry(opts(&waits))).Stat(t.Context(), "missing")
	if after, ok := filesystem.RetryAfterOf(err); !ok || after != time.Minute || len(waits) != 0 || f.calls.Load() != 1 {
		t.Errorf("over the cap: err %v, after %v, waits %v, calls %d", err, after, waits, f.calls.Load())
	}
}

// slow blocks Stat and Read until their context ends, reporting the error.
type slow struct{ *memory.Driver }

func (slow) Stat(ctx context.Context, _ string) (filesystem.Entry, error) {
	<-ctx.Done()
	return filesystem.Entry{}, filesystem.NewError(filesystem.CodeUnavailable, ctx.Err())
}

func (s slow) Read(ctx context.Context, key string, r filesystem.ByteRange) (io.ReadCloser, error) {
	return s.Driver.Read(ctx, key, r)
}

func TestTimeout(t *testing.T) {
	d := disk(t, slow{memory.New()}, layer.Timeout(10*time.Millisecond))
	_, err := d.Stat(t.Context(), "k")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout: %v", err)
	}
	// Streaming reads are not bounded by the interceptor.
	ctx := t.Context()
	inner := memory.New()
	_, _ = inner.Write(ctx, "k", strings.NewReader("data"), filesystem.WriteOptions{})
	d = disk(t, slow{inner}, layer.Timeout(time.Nanosecond))
	if b, err := d.ReadAll(ctx, "k"); err != nil || string(b) != "data" {
		t.Errorf("streaming read: %q, %v", b, err)
	}
	defer func() {
		if recover() == nil {
			t.Error("non-positive timeout accepted")
		}
	}()
	layer.Timeout(0)
}

// gated blocks Stat until released, counting how many run at once.
type gated struct {
	*memory.Driver
	release chan struct{}
	running atomic.Int64
	peak    atomic.Int64
}

func (g *gated) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	n := g.running.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-g.release
	g.running.Add(-1)
	return g.Driver.Stat(ctx, key)
}

func TestLimit(t *testing.T) {
	g := &gated{Driver: memory.New(), release: make(chan struct{})}
	d := disk(t, g, layer.Limit(2))
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Stat(t.Context(), "k")
		}()
	}
	for range 5 {
		g.release <- struct{}{}
	}
	wg.Wait()
	if g.peak.Load() > 2 {
		t.Errorf("peak concurrency %d > 2", g.peak.Load())
	}
	// A waiter whose context ends gives up.
	block := &gated{Driver: memory.New(), release: make(chan struct{})}
	d = disk(t, block, layer.Limit(1))
	go func() { _, _ = d.Stat(t.Context(), "k") }()
	for block.running.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := d.Stat(ctx, "k")
	if filesystem.CodeOf(err) != filesystem.CodeUnavailable {
		t.Errorf("waiting past the deadline: %v", err)
	}
	block.release <- struct{}{}
	defer func() {
		if recover() == nil {
			t.Error("non-positive limit accepted")
		}
	}()
	layer.Limit(0)
}

func TestObserveAndGuard(t *testing.T) {
	ctx := t.Context()
	var events []layer.Event
	obs := layer.Observe(func(_ context.Context, e layer.Event) { events = append(events, e) })
	deny := errors.New("tenant mismatch")
	guard := layer.Guard(func(_ context.Context, op *filesystem.Op) error {
		switch {
		case op.Key == "forbidden":
			return deny
		case op.Key == "quota":
			return filesystem.Errorf(filesystem.CodeTooLarge, "quota exceeded")
		}
		return nil
	})
	d := disk(t, memory.New(), obs, guard)
	_, _ = d.WriteBytes(ctx, "ok", []byte("abc"), filesystem.WriteOptions{})
	_, err := d.Stat(ctx, "forbidden")
	if filesystem.CodeOf(err) != filesystem.CodePermission || !errors.Is(err, deny) {
		t.Errorf("guard error = %v", err)
	}
	_, err = d.Stat(ctx, "quota")
	if filesystem.CodeOf(err) != filesystem.CodeTooLarge {
		t.Errorf("guard kept code: %v", err)
	}
	if len(events) != 3 || events[0].Op.Bytes != 3 || events[0].Err != nil || events[1].Code != filesystem.CodePermission || events[0].Duration < 0 {
		t.Errorf("events = %+v", events)
	}
}

func TestLog(t *testing.T) {
	ctx := t.Context()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := disk(t, memory.New(), layer.Log(logger))
	_, _ = d.WriteBytes(ctx, "a.txt", []byte("abc"), filesystem.WriteOptions{})
	_, _ = d.Copy(ctx, "a.txt", "b.txt")
	_, _ = d.Stat(ctx, "missing")
	_ = d.SetVisibility(ctx, "missing", filesystem.VisibilityPublic)
	out := buf.String()
	for _, want := range []string{
		"level=DEBUG msg=\"filesystem operation\" disk=d op=write key=a.txt attempts=1",
		"bytes=3",
		"op=copy key=a.txt attempts=1",
		"target=b.txt",
		"level=DEBUG msg=\"filesystem operation\" disk=d op=stat key=missing",
		"level=WARN msg=\"filesystem operation\" disk=d op=set_visibility key=missing",
		"code=not_found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	// Disabled levels write nothing.
	buf.Reset()
	quiet := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	d = disk(t, memory.New(), layer.Log(quiet, layer.LogOptions{SuccessLevel: slog.LevelInfo, FailureLevel: slog.LevelError, NotFoundLevel: slog.LevelInfo}))
	_, _ = d.Stat(ctx, "missing")
	if buf.Len() != 0 {
		t.Errorf("disabled level logged: %s", buf.String())
	}
}

func TestSignedURLsNeverReachTheLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := filesystem.NewDisk("d", memory.New(), filesystem.WithInterceptors(layer.Log(logger)), filesystem.WithURLSigner(signer{}))
	if err != nil {
		t.Fatal(err)
	}
	req, err := d.SignedURL(t.Context(), "k", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "secret-signature") || !strings.Contains(req.URL, "secret-signature") {
		t.Errorf("signature leaked into the log or missing from the URL: %s", buf.String())
	}
}

type signer struct{}

func (signer) SignURL(_ context.Context, _, key string, o filesystem.PresignOptions) (filesystem.SignedRequest, error) {
	return filesystem.SignedRequest{Method: o.Method, URL: "https://app.test/" + key + "?sig=secret-signature"}, nil
}
