# Interceptors

Retries, timeouts, concurrency limits, logging, metrics, tracing, read-only disks and policy checks are cross-cutting: they apply to every operation on a disk, whatever the backend. In this library they are interceptors, functions that run around each operation. This page explains the operation model they see, the order they run in, and every built-in one, with the rules that keep them safe (which calls Retry may repeat, which calls Timeout leaves alone). Anyone configuring a production disk needs it.

The snippets come from `examples/concepts-interceptors`. Run it with `go run ./concepts-interceptors` from the `examples` directory.

## Why interceptors and not decorators

The usual Go way to add retry or logging to a storage client is a decorator: a type that wraps the driver and implements the same interface. It breaks optional interfaces. A decorator that wraps an S3 driver and forgets to forward `Presign` makes signed URLs disappear, and the type system cannot notice. Interceptors never wrap the driver; they wrap the operation. The driver underneath, its optional interfaces and its declared capabilities are untouched, so adding logging can never remove a capability.

## Step 1 — The operation model

Every Disk method that reaches a driver runs as exactly one `Op` passed through the chain. A portable fallback runs as the operations it is made of: a streamed Copy is a `stat`, a `read` and a `write`.

| `Op` field | Meaning |
|---|---|
| `Kind` | the operation: `stat`, `read`, `write`, `delete`, `delete_prefix`, `delete_keys`, `list`, `copy`, `move`, `set_visibility`, `presign`, `public_url`, and the six `multipart_*` kinds (`filesystem.OpKindValues`) |
| `Disk` | the disk's name |
| `Key` | the object key, or the directory for `list` and `delete_prefix`; empty for `delete_keys` |
| `Keys` | the keys of a `delete_keys` |
| `Target` | the destination of a `copy` or `move` |
| `Mutating` | the operation changes data or authorises a change: the kind's `Mutates()`, plus a presign for PUT |
| `Replayable` | the call may safely run again; false for a write whose body cannot be rewound, a `move`, a `multipart_create`, a `multipart_complete`, and an `UploadPart` with a one-shot body |
| `Streaming` | the call returns a stream that outlives it (a `read`) |
| `Attempt` | how many times the call has started, from 1 |
| `Bytes` | set after a successful write to the bytes stored |

An `Interceptor` is `func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error`. It must call `call` to proceed (zero, one or several times) and return its error or its own. It may read `op` before and after, and must not change `Kind`, `Disk`, `Key` or `Target`. It never sees the operation's result value, so a signed URL or an object's bytes cannot leak through a logging or metrics interceptor.

## Step 2 — Order

`filesystem.WithInterceptors(a, b, c)` makes `a` the outermost: it runs first on the way in and last on the way out. A tracing interceptor shows it:

<!-- ds:block id=trace-c9p637eb -->
**trace** · [`examples/concepts-interceptors/main.go:85-92`](../../examples/concepts-interceptors/main.go#L85-L92)

```go
func trace(name string, out io.Writer) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		fmt.Fprintf(out, "  enter %s (%s %s)\n", name, op.Kind, op.Key)
		err := call(ctx)
		fmt.Fprintf(out, "  leave %s\n", name)
		return err
	}
}
```
<!-- /ds:block hash=01540f -->

<!-- ds:block id=order-t2nkyn8y -->
**order** · [`examples/concepts-interceptors/main.go:96-105`](../../examples/concepts-interceptors/main.go#L96-L105)

```go
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
```
<!-- /ds:block hash=0fa4de -->

```text
order:
  enter outer (stat a.txt)
  enter inner (stat a.txt)
  leave inner
  leave outer
```

The recommended order for a production disk is the one in the `layer` package documentation:

| Position | Interceptor | Why there |
|---|---|---|
| 1 | `layer.Observe` (or `otel`) | sees the whole operation, retries included, and its total duration |
| 2 | `layer.Log` | one record per operation, with the final attempt count and error |
| 3 | `layer.Retry` | repeats what is inside it |
| 4 | `layer.Timeout` | inside Retry, so each attempt gets its own budget |
| 5 | `layer.Limit` | innermost, so a slot is held only while the backend works, not during backoff |

`ReadOnly` and `Guard` refuse before reaching the driver, so anywhere works; put them first to keep refused calls out of retries and timeouts.

## Step 3 — Retry

`layer.Retry(layer.RetryOptions{})` retries failed attempts of replayable operations. The defaults are three attempts in total (`DefaultAttempts`), a backoff starting at 100 ms (`DefaultBaseDelay`) and doubling up to 2 s (`DefaultMaxDelay`), with full jitter so many clients do not retry in lockstep, and only errors whose code is `unavailable`. `Retryable` changes which errors are retried and `Sleep` replaces the timer (tests use it to avoid waiting). A context that has ended (cancelled, or past its own deadline) is never retried: it stops the backoff, or prevents it, and returns the last error.

When the backend says how long to wait (a `Retry-After` on a 429 or 503, which a driver records with `filesystem.NewError(filesystem.CodeUnavailable, err).WithRetryAfter(d)`), Retry waits at least that long before the next attempt. A wait longer than `MaxDelay` ends the retries at once: holding the request past the cap is not allowed, and retrying earlier than the backend asked is a request it has said it will refuse. The caller gets the error with `RetryAfter` still set (`filesystem.RetryAfterOf`), and `fshttp` and the upload handlers pass it on as a `Retry-After` header.

The demo's backend fails twice with `unavailable` and then works. With Observe outside Retry, the observer sees one operation with three attempts:

<!-- ds:block id=flaky-d97z36uh -->
**flaky** · [`examples/concepts-interceptors/main.go:111-114`](../../examples/concepts-interceptors/main.go#L111-L114)

```go
type flaky struct {
	*memory.Driver
	stats, writes int
}
```
<!-- /ds:block hash=fde6d6 -->

<!-- ds:block id=retry-vgfqxqfc -->
**retry** · [`examples/concepts-interceptors/main.go:145-159`](../../examples/concepts-interceptors/main.go#L145-L159)

```go
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
```
<!-- /ds:block hash=cf80e1 -->

Check: `observed write a.txt: attempts=3 code=""` and `observed stat a.txt: attempts=3 code=""`.

A write is retried only when its body implements `io.Seeker` and can seek (a `*bytes.Reader`, a `*strings.Reader`, an `*os.File` of a regular file): the Disk records the start position and rewinds it before each attempt. An `*os.File` on a pipe, socket or terminal has a `Seek` that fails, so it is written once as a stream. A one-shot stream, such as an HTTP request body, cannot be replayed, so the Disk marks the operation not replayable and Retry returns the first failure instead of writing a truncated object:

<!-- ds:block id=oneshotstream-heknvesg -->
**oneShotStream** · [`examples/concepts-interceptors/main.go:165-178`](../../examples/concepts-interceptors/main.go#L165-L178)

```go
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
```
<!-- /ds:block hash=04439d -->

Check: `observed write b.txt: attempts=1 code="unavailable"`. To make an upload retryable, buffer it to a temporary file and write the `*os.File`.

`move` and `multipart_complete` are never retried: repeating a rename or a completion after an unknown outcome is unsafe.

## Step 4 — Timeout

`layer.Timeout(d)` gives each attempt its own deadline. It leaves `read` alone: its result is a stream that outlives the call, and cancelling the context when the call returned would break the stream mid-download. Bound reads with the caller's own context. `d` must be positive; `Timeout` panics otherwise, because a zero timeout is a configuration bug.

What a deadline looks like matters for Retry. A context that ended classifies as `unavailable`, whether the driver returns the bare context error (the demo's slow driver does, and so do the built-in local and memory drivers) or wraps it in an error it could not otherwise classify, so the default `Retryable` retries an attempt that `Timeout` cut short. Retry never retries once its own context has ended: a caller that cancelled, or whose own deadline passed, gets the error back at once, so only the per-attempt budget of a `Timeout` placed inside Retry is retried:

<!-- ds:block id=slow-3dqtn4yr -->
**slow** · [`examples/concepts-interceptors/main.go:183-186`](../../examples/concepts-interceptors/main.go#L183-L186)

```go
type slow struct {
	*memory.Driver
	release chan struct{}
}
```
<!-- /ds:block hash=af5362 -->

<!-- ds:block id=timeout-e4rdgcdr -->
**timeout** · [`examples/concepts-interceptors/main.go:202-216`](../../examples/concepts-interceptors/main.go#L202-L216)

```go
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
```
<!-- /ds:block hash=b7d991 -->

Check: `observed stat a.txt: attempts=3 code="unavailable"` and `deadline exceeded: true`: every attempt timed out, each was retried, and the cause is still `context.DeadlineExceeded` for `errors.Is`.

## Step 5 — Limit

`layer.Limit(n)` allows at most `n` operations at once on the disk; the rest wait, or give up with `unavailable` when their context ends first. For a `read` the slot is held while the stream is opened, not while it is consumed. Use it to keep a burst of uploads from opening hundreds of connections to a backend that throttles.

<!-- ds:block id=limit-4vspbm26 -->
**limit** · [`examples/concepts-interceptors/main.go:221-243`](../../examples/concepts-interceptors/main.go#L221-L243)

```go
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
```
<!-- /ds:block hash=36e09c -->

Check: `limit: second caller unavailable`.

## Step 6 — ReadOnly

`filesystem.ReadOnly()` refuses every operation whose `Mutating` is true with `read_only` before it reaches the driver. Reads, listings, signed GET URLs and public URLs still work; signed PUT URLs do not, because they authorise a write. It lives in the core package so the `readonly=true` DSN parameter can add it. Typical uses: a disk holding a previous system's files during a migration, a replica, a reporting job's view of production storage.

<!-- ds:block id=readonly-vmd3t6en -->
**readOnly** · [`examples/concepts-interceptors/main.go:248-258`](../../examples/concepts-interceptors/main.go#L248-L258)

```go
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
```
<!-- /ds:block hash=5147db -->

Check: `read-only write: read_only` and `read-only exists: <nil>`.

## Step 7 — Guard

`layer.Guard(check)` calls `check(ctx, op)` before every operation and fails it, without reaching the driver, when the check returns an error. A plain error becomes `permission`; an error that already carries a code keeps it. Use it for rules that apply to every call: a tenant check, a maintenance window, a per-disk feature flag.

<!-- ds:block id=guard-ru54uzvj -->
**guard** · [`examples/concepts-interceptors/main.go:263-279`](../../examples/concepts-interceptors/main.go#L263-L279)

```go
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
```
<!-- /ds:block hash=19adb5 -->

Check: `guard inside: <nil>` and `guard outside: permission`. [Multi-tenancy](multi-tenancy.md) has a complete tenant guard that also checks the target of a copy and the keys of a batch delete.

## Step 8 — Log

`layer.Log(logger)` writes one structured `log/slog` record per operation with the attributes `disk`, `op`, `key`, `attempts`, `duration`, plus `target` and `bytes` when set and `code` and `error` on failure (exported as `layer.Attr*` so queries can rely on them; the message is `layer.LogMessage`). Successes log at Debug and failures at Warn, except `not_found` from an operation that changes nothing (`stat`, `exists`, `read`, `list`), which is an answer rather than a failure and logs at Debug. `not_found` from a mutation is a failure. `layer.LogOptions` sets the three levels. Keys are logged; signed URLs and credentials never are, because interceptors do not see results.

<!-- ds:block id=logops-rk8jecbk -->
**logOps** · [`examples/concepts-interceptors/main.go:293-305`](../../examples/concepts-interceptors/main.go#L293-L305)

```go
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
```
<!-- /ds:block hash=fd2e38 -->

```text
level=DEBUG msg="filesystem operation" disk=media op=write key=a.txt attempts=1 bytes=5
level=DEBUG msg="filesystem operation" disk=media op=stat key=missing.txt attempts=1 code=not_found error="filesystem: not_found: no object \"missing.txt\""
level=WARN msg="filesystem operation" disk=media op=set_visibility key=missing.txt attempts=1 code=not_found error="filesystem: not_found: no object \"missing.txt\""
```

(The demo drops the time and duration attributes so its output is the same on every run.) If keys are personal data in your application, filter them in the slog handler's `ReplaceAttr`.

## Step 9 — Observe, metrics and tracing

`layer.Observe(fn)` calls `fn(ctx, layer.Event)` after every operation. An `Event` holds a copy of the `Op` as it finished, the `Duration` of the whole operation, the `Err` and its `Code`. It carries no payload, no URL and no signature, so nothing secret can reach a metrics sink through it, and `fn` cannot change the outcome. It is what metrics adapters build on: count operations by `Op.Kind` and `Code`, record `Duration` as a histogram, sum `Op.Bytes`. `contrib/otel` provides OpenTelemetry tracing and metrics interceptors built the same way; see [OpenTelemetry](../integrations/otel.md) and [Logging and metrics](../integrations/logging-and-metrics.md).

## Step 10 — Bandwidth, faults and hedged reads

Three more interceptors in `layer`, each off until installed. In a production chain they sit like this; the three sub-steps below take them one at a time:

```go
disk, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(
	layer.Observe(recordMetrics),
	layer.Bandwidth(50<<20), // 50 MiB/s for the whole disk
	layer.Retry(layer.RetryOptions{}),
	layer.Hedge(150*time.Millisecond), // about the p95 of a read
))
```

### Step 10a — Cap a disk's bandwidth

`layer.Bandwidth(bytesPerSec)` throttles bytes, not calls: the bodies of writes, appends and uploaded parts as the driver reads them, and the readers that reads and version reads return as the caller reads them. Every operation of the disk draws from one token bucket, so the cap is the disk's total, not each transfer's; install it on the disk (or the scoped disk) whose traffic should share a cap, such as a backup job that must not starve user traffic on the same link. A transfer waiting for tokens gives up with `CodeUnavailable` when its context ends. It reaches the bytes through `Op.WrapBody`, the hook any interceptor can use to count or throttle a stream. Combine it with `Limit` when you also need to bound concurrency. A test passes `layer.Bandwidth(rate, layer.BandwidthOptions{Now: clock, Sleep: advance})`, a fake clock and a `Sleep` that moves it, and checks the waits the bucket asks for without waiting them.

<!-- ds:block id=bandwidth-gx8pkgzr -->
**bandwidth** · [`examples/concepts-interceptors/main.go:311-323`](../../examples/concepts-interceptors/main.go#L311-L323)

```go
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
```
<!-- /ds:block hash=58e45b -->

Check: `bandwidth: 6000 bytes at 20000 B/s took at least 150ms: true`. The bucket starts with one burst (a tenth of a second's worth), so a short transfer is not slowed at all; the cap bites on sustained traffic.

### Step 10b — Inject faults in tests

`layer.Fault(layer.FaultOptions{Rate, Codes, Latency, Ops, Seed})` fails a share of operations before they reach the driver, with codes drawn from `Codes` (default `CodeUnavailable`, the code `Retry` retries), and adds `Latency` to every operation it applies to. `Ops` limits it to some operation kinds. The same `Seed` fails the same operations in the same order, so a test is repeatable. Where you place it decides what the test proves: inside `Retry` (after it in the list) it proves the retries cover the failures; outside, that your callers handle them.

<!-- ds:block id=faults-edeunkmc -->
**faults** · [`examples/concepts-interceptors/main.go:330-358`](../../examples/concepts-interceptors/main.go#L330-L358)

```go
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
```
<!-- /ds:block hash=784395 -->

Check: `fault without retry: unavailable`, then `fault inside retry: 20 writes stored in 43 attempts`. Half the attempts failed and every write still landed; with seed 7 the count is 43 on every run, which is what lets a test assert it. An injected failure says so in its message, so it is easy to tell from a real one. Fault is for tests and chaos runs only: never build it into a production chain behind a flag someone can forget.

### Step 10c — Hedge slow reads

`layer.Hedge(after)` cuts tail latency. When a `Stat` or `Read` (an operation marked `Op.Hedgeable`) has not answered after `after`, it starts a second attempt and returns whichever answers first. It sets `Op.Concurrent`, so when both succeed the Disk keeps the first success and the other returns `filesystem.ErrSuperseded`; it then cancels the other attempt and waits for it, and the Disk closes any reader the loser opened. The winning stream's context is released when the caller closes it, through `Op.OnClose`. Nothing but `Stat` and `Read` is ever run twice: a write or a delete is never hedged.

The program's driver loses its first read, as a request to a stuck backend node would be lost:

<!-- ds:block id=stuck-95ayc2cx -->
**stuck** · [`examples/concepts-interceptors/main.go:364-367`](../../examples/concepts-interceptors/main.go#L364-L367)

```go
type stuck struct {
	*memory.Driver
	reads atomic.Int32
}
```
<!-- /ds:block hash=57e5ad -->

<!-- ds:block id=hedge-v7agwz6w -->
**hedge** · [`examples/concepts-interceptors/main.go:381-396`](../../examples/concepts-interceptors/main.go#L381-L396)

```go
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
```
<!-- /ds:block hash=9775fe -->

Check: `hedge: read "answered" after 2 driver reads`. Without `Hedge` the read would wait for its context to end. Pick `after` near the operation's 95th-percentile latency: lower doubles the load on the backend, higher rarely hedges. Place it after `Observe`, `Log`, `Retry` and `Bandwidth`, so they see one operation and each retry may be hedged; interceptors placed after it run once per attempt, possibly concurrently, and must not read `Op.Attempt` or call `Op.WrapBody`.

## Interceptors on derived disks

A scoped disk (`Disk.Scope`), a mirrored disk (`filesystem.Mirror`), a failover disk and a cached-read disk copy the interceptors of the disk they derive from, so a tenant's disk logs and retries like its parent. A mirror's own operations run through the primary's chain, and each mirror disk's writes run through that mirror's chain. A `Limit` interceptor is shared: the derived disk holds the same function, so its slots count against the same `n`. A routed or overlay disk (see [Composing disks](composing-disks.md)) starts with no interceptors of its own (pass them as options) and calls each underlying disk through that disk's chain.

## Pitfalls

- Putting Timeout outside Retry: one deadline then covers every attempt and the backoff, and the second attempt starts with little or no time left.
- Expecting uploads from `upload.Handler` to be retried: the request body is a one-shot stream, so they are not. That is deliberate; a retried upload would store a truncated file.
- Expecting a timeout to show as its own code in dashboards: a deadline or a cancelled context is `unavailable`, like throttling. Check `errors.Is(err, context.DeadlineExceeded)` or `context.Canceled` in the metrics callback if you want it separate.
- Doing slow work in an Observe callback: it runs in the request path after every operation. Hand events to a buffered metrics client.

## Next

- [Write an interceptor](../extending/write-an-interceptor.md): rules for your own.
- [Errors](errors.md): the codes Retry and Log look at.
- Reference: [the layer package](../../layer/README.md).
