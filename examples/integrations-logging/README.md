# Integrations: logging and metrics with log/slog and expvar

Every disk operation logged with `log/slog` through `layer.Log`, counted with `layer.Observe` into `expvar` maps, and run through the built-in interceptors in their usual order, with a backend that fails once so the retry is visible in the log. This answers "how do I see what storage is doing in production" without any dependency beyond the standard library. It uses `github.com/ubgo/filesystem`, `layer` (`Observe`, `Log`, `Retry`, `Timeout`, `Limit`) and the `memory` driver. The guide is [Logging and metrics](../../docs/integrations/logging-and-metrics.md).

## Run it

```sh
cd examples
go run ./integrations-logging
go test ./integrations-logging
```

It runs on the memory driver: nothing is written to disk and no port is opened. It prints JSON log lines, then the counters, and exits. Timestamps and durations are dropped from the log so the output is the same on every run; a real app keeps them. The counters are not passed to `expvar.Publish` (which panics on a second publish of the same name); an app publishes them once at start-up to serve them on `/debug/vars`.

## What it does

1. Writes `reports/q3.txt`: one log line with the bytes stored.
2. Stats it. The driver's first `Stat` fails with `CodeUnavailable`, `layer.Retry` retries, and the single log line says `attempts: 2`.
3. Copies it to `archive/q3.txt`, then stats a missing key, which logs the `not_found` code.
4. Deletes the empty key: refused with `invalid_key` before the chain runs, so it is neither logged nor counted.
5. Prints the counters by disk, operation and outcome, and the bytes written per disk.

```text
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"write","key":"reports/q3.txt","attempts":1,"bytes":18}
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"stat","key":"reports/q3.txt","attempts":2}
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"copy","key":"reports/q3.txt","attempts":1,"target":"archive/q3.txt"}
{"level":"DEBUG","msg":"filesystem operation","disk":"media","op":"stat","key":"reports/missing.txt","attempts":1,"code":"not_found","error":"filesystem: not_found: no object \"reports/missing.txt\""}
operations {"media.copy.ok": 1, "media.stat.not_found": 1, "media.stat.ok": 1, "media.write.ok": 1}
bytes_written {"media": 18}
```

## The code

`newMetrics` makes the two counter maps `Observe` fills: operations by disk, operation and outcome, and bytes written by disk.

<!-- ds:block id=newmetrics-tczcvcnv -->
**newMetrics** · [`examples/integrations-logging/main.go:48-50`](main.go#L48-L50)

```go
func newMetrics() (ops, written *expvar.Map) {
	return new(expvar.Map).Init(), new(expvar.Map).Init()
}
```
<!-- /ds:block hash=014f70 -->

`record` is the `Observe` callback. The event carries the operation (kind, disk, key, attempts, bytes), its duration and error code, and never a payload, URL or signature.

<!-- ds:block id=record-6z7vsd8a -->
**record** · [`examples/integrations-logging/main.go:56-67`](main.go#L56-L67)

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

`newDisk` installs the interceptors outermost first. `Observe` and `Log` sit outside `Retry`, so each sees one operation with its attempt count; `Timeout` sits inside `Retry`, so each attempt gets its own budget; `Limit` is innermost, so a waiting retry does not hold a slot.

<!-- ds:block id=newdisk-3e5qdbjh -->
**newDisk** · [`examples/integrations-logging/main.go:74-82`](main.go#L74-L82)

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

`newQuietLog` is the production shape: successes at Info, failures at Warn, expected not-found answers at Debug.

<!-- ds:block id=newquietlog-suvqwgca -->
**newQuietLog** · [`examples/integrations-logging/main.go:88-94`](main.go#L88-L94)

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

`flaky` is a memory driver whose first `Stat` fails the way a throttled backend does, with the one code `Retry` retries.

<!-- ds:block id=flaky-stat-ar76ae68 -->
**flaky.Stat** · [`examples/integrations-logging/main.go:105-110`](main.go#L105-L110)

```go
func (f *flaky) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if f.failed.CompareAndSwap(false, true) {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "throttled")
	}
	return f.Driver.Stat(ctx, key)
}
```
<!-- /ds:block hash=66be10 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The counters, the `Observe` callback, the interceptor chain, the quiet logger, the flaky driver and the demo `run` |
| `main_test.go` | The demo output, and the quiet logger's levels |

## Next

- [Logging and metrics](../../docs/integrations/logging-and-metrics.md): the guide for this program.
- [OpenTelemetry](../integrations-otel): the same operations as spans and metrics.
- [Write an interceptor](../extending-interceptor): your own behaviour around every operation.
