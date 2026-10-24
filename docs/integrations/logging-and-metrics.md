# Logging and metrics

You will have one structured `log/slog` record per storage operation, counters of operations by outcome and bytes written, and an interceptor chain in the order production needs: measurement outside, retries in the middle, per-attempt timeouts and a concurrency limit inside. Everything here is in the core module (`filesystem/layer`) and the standard library; nothing to install.

Read this page when you set up a disk for production, or when you want storage numbers in Prometheus, StatsD, expvar or any metrics system the library has no adapter for. If you run OpenTelemetry, [otel](otel.md) records spans and metrics with no code of your own.

## Before you start

- [Interceptors](../concepts/interceptors.md) explains the model: interceptors wrap operations, never the driver, so they cannot hide a capability.
- The program is [`examples/integrations-logging`](../../examples/integrations-logging/main.go). `go run ./integrations-logging` in `examples/` writes JSON log lines and the counters; `go test ./integrations-logging` checks them.

## Step 1 — Install the interceptors in order

The first interceptor given runs outermost. `layer.Observe` and `layer.Log` go outside `layer.Retry`, so each sees one operation with its attempt count; `layer.Timeout` goes inside Retry, so each attempt gets its own budget; `layer.Limit` is innermost, so a retry waiting out its backoff does not hold one of the slots:

<!-- ds:block id=newdisk-3e5qdbjh -->
**newDisk** · [`examples/integrations-logging/main.go:74-82`](../../examples/integrations-logging/main.go#L74-L82)

```go
func newDisk(drv filesystem.Driver, logger *slog.Logger, ops, written *expvar.Map) (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, drv, filesystem.WithInterceptors(
		layer.Observe(record(ops, written)),
		layer.Log(logger),
		layer.Retry(layer.RetryOptions{}),
		layer.Timeout(opTimeout),
		layer.Limit(maxConcurrent),
	))
}
```
<!-- /ds:block hash=dc3e1e -->

| Interceptor | Does | Defaults and limits |
|---|---|---|
| `layer.Observe(fn)` | calls `fn(ctx, layer.Event)` after every operation | cannot change the outcome |
| `layer.Log(logger, opts...)` | one `slog` record per operation | successes at Debug, failures at Warn, not-found from a read at Debug |
| `layer.Retry(layer.RetryOptions{})` | retries replayable operations that failed with `unavailable` | 3 attempts, 100 ms base delay doubling to at most 2 s, full jitter |
| `layer.Timeout(d)` | bounds each attempt to `d` | leaves streaming reads alone; panics on `d <= 0` |
| `layer.Limit(n)` | at most `n` operations at once; waiters give up with `unavailable` when their context ends | panics on `n <= 0`; a read holds its slot while opening, not while being consumed |
| `layer.Guard(check)` | refuses an operation before it reaches the driver when `check` returns an error | an error without a code becomes `permission` |
| `filesystem.ReadOnly()` | refuses every mutation with `read_only` | also set by the DSN parameter `readonly=true` |

## Step 2 — Read the log

Each record has the message `filesystem operation` and the attributes `disk`, `op`, `key`, `attempts` and `duration`, plus `target` for a copy or move, `bytes` for a write, and `code` and `error` on failure. The attribute names are exported as `layer.AttrDisk`, `layer.AttrOp` and so on, so log queries can rely on them. The example drops the time and duration so its output is the same on every run:

```sh
go run ./integrations-logging
```

```
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"write","key":"reports/q3.txt","attempts":1,"bytes":18}
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"stat","key":"reports/q3.txt","attempts":2}
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"copy","key":"reports/q3.txt","attempts":1,"target":"archive/q3.txt"}
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"stat","key":"reports/missing.txt","attempts":1,"code":"not_found","error":"filesystem: not_found: no object \"reports/missing.txt\""}
operations {"media.copy.ok": 1, "media.stat.not_found": 1, "media.stat.ok": 1, "media.write.ok": 1}
bytes_written {"media": 18}
```

The second line is one operation that took two attempts: the example's backend fails its first `Stat` the way a throttled store does, and Retry tried again. The failure itself is not logged; `attempts` is where it shows:

<!-- ds:block id=flaky-stat-ar76ae68 -->
**flaky.Stat** · [`examples/integrations-logging/main.go:105-110`](../../examples/integrations-logging/main.go#L105-L110)

```go
func (f *flaky) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if f.failed.CompareAndSwap(false, true) {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "throttled")
	}
	return f.Driver.Stat(ctx, key)
}
```
<!-- /ds:block hash=66be10 -->

The fourth line is a lookup that found nothing, which is an answer rather than a failure, so it stays at Debug. A not-found from a mutation, such as setting the visibility of a missing key, is a failure and logs at Warn.

The example also passes an empty key to `Delete`. It is refused with `invalid_key` before the chain runs, so it is neither logged nor counted: validation errors never reach interceptors.

Signed URLs, credentials and payloads never reach the log: an interceptor sees the operation, not its result, so the URL `SignedURL` returns cannot pass through `layer.Log`.

## Step 3 — Choose the levels

Most production loggers want every change visible and lookups only on demand. `layer.LogOptions` sets the three levels:

<!-- ds:block id=newquietlog-suvqwgca -->
**newQuietLog** · [`examples/integrations-logging/main.go:88-94`](../../examples/integrations-logging/main.go#L88-L94)

```go
func newQuietLog(logger *slog.Logger) filesystem.Interceptor {
	return layer.Log(logger, layer.LogOptions{
		SuccessLevel:  slog.LevelInfo,
		FailureLevel:  slog.LevelWarn,
		NotFoundLevel: slog.LevelDebug,
	})
}
```
<!-- /ds:block hash=d5db20 -->

Check: `TestQuietLog` writes, stats a missing key and sets visibility on a missing key through a logger at Info. The write is logged at Info, the missing stat is not logged, and the failed visibility change is logged at Warn. Records below the logger's level are skipped before any attribute is built (`logger.Enabled` is checked first).

## Step 4 — Count operations

`layer.Observe` is the hook for any metrics system: the callback gets a `layer.Event` with the operation (`Kind`, `Disk`, `Key`, `Target`, `Attempt`, `Bytes`), its `Duration`, `Err` and `Code`, and nothing else, so no payload or signature can reach a metrics sink. The example counts into `expvar` maps:

<!-- ds:block id=newmetrics-tczcvcnv -->
**newMetrics** · [`examples/integrations-logging/main.go:48-50`](../../examples/integrations-logging/main.go#L48-L50)

```go
func newMetrics() (ops, written *expvar.Map) {
	return new(expvar.Map).Init(), new(expvar.Map).Init()
}
```
<!-- /ds:block hash=014f70 -->

<!-- ds:block id=record-6z7vsd8a -->
**record** · [`examples/integrations-logging/main.go:56-67`](../../examples/integrations-logging/main.go#L56-L67)

```go
func record(ops, written *expvar.Map) func(context.Context, layer.Event) {
	return func(_ context.Context, e layer.Event) {
		outcome := outcomeOK
		if e.Err != nil {
			outcome = string(e.Code)
		}
		ops.Add(e.Op.Disk+"."+string(e.Op.Kind)+"."+outcome, 1)
		if e.Err == nil && e.Op.Bytes > 0 {
			written.Add(e.Op.Disk, e.Op.Bytes)
		}
	}
}
```
<!-- /ds:block hash=29e9f9 -->

The same callback shape feeds Prometheus (`counterVec.WithLabelValues(e.Op.Disk, string(e.Op.Kind), outcome).Inc()` and a histogram observing `e.Duration.Seconds()`), StatsD or anything else. Label by disk, operation and code; never by key, which is unbounded.

## Step 5 — Put it together

The whole program is one function: a JSON `slog` logger, the two expvar maps, the disk with its interceptors over a driver whose first `Stat` fails, and a handful of operations, one of them refused before any interceptor runs:

<!-- ds:block id=run-kswmtmzn -->
**run** · [`examples/integrations-logging/main.go:115-135`](../../examples/integrations-logging/main.go#L115-L135)

```go
func run(ctx context.Context, out io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropTime}))
	ops, written := newMetrics()
	d, err := newDisk(&flaky{Driver: memory.New()}, logger, ops, written)
	if err != nil {
		return err
	}
	if _, err := d.WriteBytes(ctx, "reports/q3.txt", []byte("quarterly numbers\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if _, err := d.Stat(ctx, "reports/q3.txt"); err != nil {
		return err
	}
	if _, err := d.Copy(ctx, "reports/q3.txt", "archive/q3.txt"); err != nil {
		return err
	}
	_, _ = d.Stat(ctx, "reports/missing.txt")
	_ = d.Delete(ctx, "")
	fmt.Fprintf(out, "operations %s\nbytes_written %s\n", ops.String(), written.String())
	return nil
}
```
<!-- /ds:block hash=8e8cca -->

The last call, `Delete` of the empty key, appears nowhere in the output above: an invalid key is refused with `invalid_key` while the Disk cleans it, before the interceptor chain runs, so neither the log nor the counters see it. Count those where you call the library if you need them. In your application the logger is the one you already have (`slog.Default()` or your handler), and the maps are published once with `expvar.Publish` so `/debug/vars` serves them.

Check: `go test ./integrations-logging` runs the program and compares every line above.

## Pitfalls

- **Logging inside Retry logs every attempt**, failed ones included, with `attempts` counting up from 1. Put Log outside Retry, as above, to get one record per operation.
- **Timeout outside Retry bounds the whole operation**, retries and backoff included, not each attempt. Either is valid; know which you chose.
- **A write whose body is a stream is never retried.** Retry repeats only replayable operations: reads, stats, lists, deletes, and writes whose body implements `io.Seeker` (`WriteBytes`, an `*os.File` of a regular file, a `bytes.Reader`). A move and a multipart completion are never retried either. `attempts` stays 1 for them whatever the error.
- **Only `unavailable` is retried by default.** A driver that cannot classify a network failure reports `unknown`, which is not retried; `RetryOptions.Retryable` widens the rule if your backend needs it.
- **Context cancellation is `unavailable`.** A request whose client went away fails with a cancelled context, which classifies as `unavailable` like a deadline or throttling; filter it with `errors.Is(err, context.Canceled)` before alerting on backend availability.
- **The log carries keys and error messages.** `key`, `target` and `error` name objects (`reports/missing.txt` above), and keys often hold user IDs and file names. If your logs must not, use a `layer.Observe` callback of your own that writes only disk, operation and code, or a `slog.HandlerOptions.ReplaceAttr` that drops `layer.AttrKey`, `layer.AttrTarget` and `layer.AttrError`.
- **Invalid keys never reach the interceptors.** A key refused while it is cleaned (`..`, an empty key, a control character) fails before the chain runs, so it is neither logged nor counted (Step 5).
- **`expvar.Publish` panics on a name published twice**, which is why the example builds unpublished maps; publish them once in `main` if you serve `/debug/vars`.

## Next

- [`examples/integrations-logging`](../../examples/integrations-logging/README.md): the runnable program behind this page.
- [OpenTelemetry](otel.md): spans and metrics through your OTel pipeline.
- [Write an interceptor](../extending/write-an-interceptor.md): your own behaviour around every operation.
- [Operations and events](../reference/operations.md): every operation kind, and which are mutating or replayable.
