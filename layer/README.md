# layer

`github.com/ubgo/filesystem/layer` holds the built-in interceptors: retry, timeouts, concurrency limits, bandwidth limits, hedged reads, fault injection, structured logging, an observation hook for metrics and tracing, and policy guards. Interceptors wrap each Disk operation, never the driver, so none of them can hide a capability.

The operation model, ordering and a runnable walk-through are in [Interceptors](../docs/concepts/interceptors.md).

Retry with an observer outside it, from the interceptors guide:

<!-- ds:block id=retry-vgfqxqfc -->
**retry** · [`examples/concepts-interceptors/main.go:145-159`](../examples/concepts-interceptors/main.go#L145-L159)

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

## API

| Name | What it is |
|---|---|
| `Retry(RetryOptions)` | retries failed attempts of replayable operations (`Op.Replayable`) with exponential backoff and full jitter, waiting at least the backend's `Retry-After` (`filesystem.Error.RetryAfter`) and giving up when that is over `MaxDelay`; a write of a one-shot stream, a move and a multipart completion are never retried |
| `RetryOptions` | `Attempts` (total tries; 0 means `DefaultAttempts`, 1 disables), `BaseDelay`, `MaxDelay` (also the longest backend `Retry-After` Retry will wait for), `Retryable` (nil means code `unavailable`), `Sleep` (nil means a real timer) |
| `DefaultAttempts` | 3 |
| `DefaultBaseDelay` | 100 ms, doubled per attempt |
| `DefaultMaxDelay` | 2 s |
| `Timeout(d)` | bounds each attempt to `d`; streaming operations (reads) are left alone; panics if `d` is not positive |
| `Limit(n)` | at most `n` operations at once; others wait, or fail with `unavailable` when their context ends; panics if `n` is not positive |
| `Guard(check)` | runs `check(ctx, op)` before every operation and fails it without reaching the driver when it returns an error; a plain error becomes `permission` |
| `Log(logger, LogOptions...)` | one `log/slog` record per operation; keys are logged, signed URLs and credentials never |
| `LogOptions` | `SuccessLevel` (default Debug), `FailureLevel` (default Warn), `NotFoundLevel` (default Debug, for `not_found` from operations that change nothing) |
| `LogMessage` | `"filesystem operation"`, the message of every record |
| `AttrDisk`, `AttrOp`, `AttrKey`, `AttrTarget`, `AttrAttempts`, `AttrBytes`, `AttrDuration`, `AttrCode`, `AttrError` | the attribute names of a record: `disk`, `op`, `key`, `target`, `attempts`, `bytes`, `duration`, `code`, `error` |
| `Observe(fn)` | calls `fn(ctx, Event)` after every operation; cannot change the outcome |
| `Event` | `Op` (a copy of the finished operation), `Duration` (the whole operation, retries included when outside Retry), `Err`, `Code`; carries no payload, URL or signature |
| `Bandwidth(bytesPerSec, BandwidthOptions{Now, Sleep})` | throttles the bytes of reads, version reads, writes, appends and uploaded parts through one token bucket shared by every operation of the disk (through `Op.WrapBody`); a transfer waiting for tokens fails with `unavailable` when its context ends; place it before `Hedge`; panics if the rate is not positive or more than one `BandwidthOptions` is given. The options are optional: `Now` (nil means `time.Now`) and `Sleep` (nil means a real timer) let a test throttle with a fake clock and no waiting |
| `Fault(FaultOptions)` | for tests and chaos runs: fails a share of operations before the driver with codes drawn from a list, and adds latency; repeatable with a seed |
| `FaultOptions` | `Rate` (0 to 1), `Codes` (nil means `unavailable`), `Latency`, `Ops` (nil means every operation), `Seed` (0 seeds from the clock) |
| `Hedge(after)` | for `Stat` and `Read` (`Op.Hedgeable`): starts a second attempt when the first has not answered after `after`, returns whichever answers first (when both succeed, the one whose result the Disk kept; the other returns `filesystem.ErrSuperseded`), cancels and waits for the other, and releases the winning stream's context when it is closed; interceptors after it must not read `Op.Attempt` or call `Op.WrapBody`; panics if `after` is not positive |

`filesystem.ReadOnly()`, which refuses every mutation with `read_only`, lives in the core package because the `readonly` DSN parameter needs it. OpenTelemetry tracing and metrics are in the `contrib/otel` module, built on `Observe`; see [OpenTelemetry](../docs/integrations/otel.md). Writing your own interceptor is in [Write an interceptor](../docs/extending/write-an-interceptor.md).
