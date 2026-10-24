# contrib/otel

`github.com/ubgo/filesystem/contrib/otel` (package `otel`, usually imported as `fsotel`) adds OpenTelemetry tracing and metrics to disks as two interceptors: one span per operation, and an operation counter, a latency histogram and a bytes-written counter labelled by disk, operation and outcome. It is its own module, so an app without OpenTelemetry never downloads it.

```sh
go get github.com/ubgo/filesystem/contrib/otel
```

The guide, with output from a run: [OpenTelemetry](../../docs/integrations/otel.md). Installing both interceptors outside `layer.Retry`, from that guide:

<!-- ds:block id=newdisk-5sx46brr -->
**newDisk** · [`examples/integrations-otel/main.go:75-86`](../../examples/integrations-otel/main.go#L75-L86)

```go
func newDisk(t Telemetry) (*filesystem.Disk, error) {
	metrics, err := fsotel.Metrics(t.Meter)
	if err != nil {
		return nil, err
	}
	return filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(
		metrics,
		fsotel.Tracing(t.Tracer),
		layer.Retry(layer.RetryOptions{}),
		layer.Timeout(opTimeout),
	))
}
```
<!-- /ds:block hash=d32493 -->

## API

| Name | What it is |
|---|---|
| `Tracing(tp trace.TracerProvider, opts ...Option) filesystem.Interceptor` | wraps every operation in a client span named `filesystem.<op>`; failed operations get status `Error` with the code, the code attribute and a recorded exception (whose message, without `WithKeys`, is only `filesystem: <op> <disk>: <code>`); outside `Retry` one span per operation, inside one per attempt |
| `Metrics(mp metric.MeterProvider, opts ...Option) (filesystem.Interceptor, error)` | records `MetricOperations`, `MetricDuration` and `MetricBytesWritten`, built on `layer.Observe`; fails only when the meter cannot create its instruments |
| `Option`, `WithKeys()` | `WithKeys` records object keys (and copy or move targets) on spans, as attributes and in a failed operation's exception message; off by default because keys often hold user IDs and file names |
| `WithAttributes(func(ctx, filesystem.Op) []attribute.KeyValue)` | adds attributes per operation (the tenant or backend from `ctx`) to spans and, under `Metrics`, to every measurement; the function gets a copy of the operation. Keep keys out of it and, for metrics, keep cardinality low |
| `WithKeyRedactor(func(key string) string)` | records keys and targets on spans as the function returns them (a hash, a masked path) instead of leaving them off; the exception message of a failure stays key-free; it wins over `WithKeys`. Metrics never record keys |
| `InstrumentationName` | `github.com/ubgo/filesystem`, the tracer and meter name |
| `AttrDisk` | `filesystem.disk` |
| `AttrOp` | `filesystem.operation` |
| `AttrKey`, `AttrTarget` | `filesystem.key`, `filesystem.target` (spans, with `WithKeys` only) |
| `AttrCode` | `filesystem.error_code`: the error's code, on failed spans and on the metric points of failures only |
| `AttrOutcome`, `OutcomeOK`, `OutcomeError` | `filesystem.outcome` on metrics: `ok` or `error` |
| `AttrAttempts` | `filesystem.attempts`, on spans |
| `AttrBytes` | `filesystem.bytes`, on spans of successful writes |
| `MetricOperations` | `filesystem.operations`: Int64Counter of finished operations, by disk, operation and outcome |
| `MetricDuration` | `filesystem.operation.duration`: Float64Histogram in seconds, by disk, operation and outcome |
| `MetricBytesWritten` | `filesystem.bytes_written`: Int64Counter in bytes of successful writes, by disk |

Providers are always passed in; the package never reads OpenTelemetry's global providers. Pass `otel.GetTracerProvider()` and `otel.GetMeterProvider()` if your app uses the globals.

## Limits

- A read's span covers opening the stream, not consuming it.
- Calls refused before the interceptor chain (invalid keys and options) produce no span or count.
- Records operations appear as the disk operations they make; their SQL needs your database driver's instrumentation.
- Scoped disks report their own names, which become `filesystem.disk` values.

Logging and metrics without OpenTelemetry: [logging and metrics](../../docs/integrations/logging-and-metrics.md).
