# OpenTelemetry

You will have a span for every storage operation and three metrics per disk, operation and outcome, exported wherever your OpenTelemetry pipeline sends the rest of your app's telemetry. The module is `github.com/ubgo/filesystem/contrib/otel`; it is two interceptors, so it works on every backend and every disk, including scoped and mirrored ones.

Read this page if you run OpenTelemetry already, or if you want storage latency and error rates on a dashboard. If you only need log lines and a counter or two, [logging and metrics](logging-and-metrics.md) does it with the standard library.

## Before you start

- [Interceptors](../concepts/interceptors.md) explains how interceptors wrap operations and why their order matters.
- The program is [`examples/integrations-otel`](../../examples/integrations-otel/main.go). `go run ./integrations-otel` in `examples/` prints the spans and metrics of three operations; `go test ./integrations-otel` checks them.
- Install the module: `go get github.com/ubgo/filesystem/contrib/otel`. It is separate from the core so an app without OpenTelemetry never downloads it.

## Step 1 — Have providers

`contrib/otel` never reads OpenTelemetry's global providers; you pass a `trace.TracerProvider` and a `metric.MeterProvider`. In production they are the providers your app already builds with the OTLP exporters (`go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` and `.../otlpmetric/otlpmetricgrpc`, or their HTTP versions); if your app sets the globals, pass `otel.GetTracerProvider()` and `otel.GetMeterProvider()`. The example uses in-memory providers so it runs without a collector:

<!-- ds:block id=newtelemetry-ns6rgcpj -->
**newTelemetry** · [`examples/integrations-otel/main.go:58-67`](../../examples/integrations-otel/main.go#L58-L67)

```go
func newTelemetry() Telemetry {
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	return Telemetry{
		Spans:  spans,
		Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)),
		Reader: reader,
		Meter:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
	}
}
```
<!-- /ds:block hash=7ce05e -->

## Step 2 — Install the interceptors

`fsotel.Metrics` creates its instruments and returns an error only when the meter cannot. Install both outside `layer.Retry`, so a retried operation is one span and one count, with its attempts on the span:

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

To add them to every disk from configuration, pass them as options to `Mux.Open` or `Mux.OpenManager`, which apply them after the DSN's own settings: `mux.OpenManager(ctx, "STORAGE_", os.LookupEnv, filesystem.WithInterceptors(metrics, fsotel.Tracing(tp)))`.

Check: `go run ./integrations-otel` prints a span per operation, the failed `stat` with an error status and its code, and the metric points:

```
span filesystem.write   status=Unset filesystem.disk=media filesystem.operation=write filesystem.attempts=1 filesystem.bytes=18
span filesystem.read    status=Unset filesystem.disk=media filesystem.operation=read filesystem.attempts=1
span filesystem.stat    status=Error filesystem.disk=media filesystem.operation=stat filesystem.attempts=1 filesystem.error_code=not_found
metric filesystem.bytes_written filesystem.disk=media = 18
metric filesystem.operation.duration filesystem.disk=media filesystem.error_code=not_found filesystem.operation=stat filesystem.outcome=error count=1
metric filesystem.operation.duration filesystem.disk=media filesystem.operation=read filesystem.outcome=ok count=1
metric filesystem.operation.duration filesystem.disk=media filesystem.operation=write filesystem.outcome=ok count=1
metric filesystem.operations filesystem.disk=media filesystem.error_code=not_found filesystem.operation=stat filesystem.outcome=error = 1
metric filesystem.operations filesystem.disk=media filesystem.operation=read filesystem.outcome=ok = 1
metric filesystem.operations filesystem.disk=media filesystem.operation=write filesystem.outcome=ok = 1
```

## Step 3 — Run operations and read what was recorded

Nothing else changes in your code: every call on the disk now produces a span and metric points. The example writes, reads and stats a missing key:

<!-- ds:block id=work-b684493n -->
**work** · [`examples/integrations-otel/main.go:99-110`](../../examples/integrations-otel/main.go#L99-L110)

```go
func work(ctx context.Context, d *filesystem.Disk) error {
	if _, err := d.WriteBytes(ctx, "reports/q3.txt", []byte("quarterly numbers\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if _, err := d.ReadAll(ctx, "reports/q3.txt"); err != nil {
		return err
	}
	if _, err := d.Stat(ctx, "reports/missing.txt"); filesystem.CodeOf(err) != filesystem.CodeNotFound {
		return fmt.Errorf("want not_found, got %v", err)
	}
	return nil
}
```
<!-- /ds:block hash=991561 -->

Then it prints what the in-memory exporters hold. In production the same data leaves through your OTLP exporters on their own schedule; in a test, collecting from a `ManualReader` and reading a `tracetest.SpanRecorder` is how you assert on it:

<!-- ds:block id=run-qwm7599d -->
**run** · [`examples/integrations-otel/main.go:114-130`](../../examples/integrations-otel/main.go#L114-L130)

```go
func run(ctx context.Context, out io.Writer) error {
	t := newTelemetry()
	d, err := newDisk(t)
	if err != nil {
		return err
	}
	if err := work(ctx, d); err != nil {
		return err
	}
	printSpans(out, t.Spans.Ended())
	var rm metricdata.ResourceMetrics
	if err := t.Reader.Collect(ctx, &rm); err != nil {
		return err
	}
	printMetrics(out, rm)
	return nil
}
```
<!-- /ds:block hash=92ac35 -->

The output is the transcript under Step 2. The missing key is a failure for telemetry (`status=Error`, `filesystem.outcome=error`) even though the program expected it: interceptors see an error, not your intent. `Disk.Exists` does not change that: it runs a `stat`, and a missing key is recorded as a `not_found` failure before `Exists` turns it into `false`. Where not-found is a normal answer (existence checks, cache probes), leave `filesystem.error_code=not_found` out of your error-rate alert.

## What is recorded

Spans:

| Field | Value |
|---|---|
| Name | `filesystem.` + the operation kind: `filesystem.write`, `filesystem.stat`, `filesystem.multipart_complete` (every kind: [operations](../reference/operations.md)) |
| Kind | `SpanKindClient` |
| Tracer | `github.com/ubgo/filesystem` (`fsotel.InstrumentationName`) |
| `filesystem.disk` | the disk name |
| `filesystem.operation` | the operation kind |
| `filesystem.key`, `filesystem.target` | only with `fsotel.WithKeys()`: the key, and the destination of a copy or move |
| `filesystem.attempts` | how many times the call started; above 1 when a retry inside the span retried it |
| `filesystem.bytes` | bytes stored, on a successful write |
| `filesystem.error_code` | the error's code, on failure |
| Status | `Error` with the code as description on failure, plus an `exception` event; without `WithKeys` its message is only `filesystem: <op> <disk>: <code>`, with it the full error |

Metrics, each with attributes `filesystem.disk`, `filesystem.operation` and `filesystem.outcome` (`ok` or `error`, `fsotel.OutcomeOK` and `fsotel.OutcomeError`), plus `filesystem.error_code` with the error's code on failures only, so a dashboard splits successes from failures on one closed-set label and breaks failures down by code:

| Metric | Instrument | Unit | Meaning |
|---|---|---|---|
| `filesystem.operations` | Int64Counter | | operations finished |
| `filesystem.operation.duration` | Float64Histogram | `s` | wall time of each operation, retries and backoff included when installed outside Retry |
| `filesystem.bytes_written` | Int64Counter | `By` | bytes stored by successful writes; attribute `filesystem.disk` only |

The span is the parent of whatever the driver does with the context: if your S3, GCS or Azure client is instrumented (for example an `otelhttp` transport on the AWS SDK's HTTP client), its HTTP spans appear as children of `filesystem.write`. Without that instrumentation the span still measures the operation, without children.

## Keys stay off spans unless you ask

Keys often contain user IDs and file names (`users/42/passport.jpg`), which a tracing backend should not store by default. Without options no span attribute names the key; `fsotel.WithKeys()` adds `filesystem.key` (and `filesystem.target`) when your tracing data is allowed to hold them. The same holds for a failed operation's `exception` event: an error's text usually names the object (the driver's message, or a `*filesystem.Error` rendering its key), so without the option the event's `exception.message` is a key-free stand-in carrying only the operation, the disk and the code (`filesystem: stat media: not_found`), and its `exception.type` is that stand-in's type. With `WithKeys` the error is recorded as it is.

<!-- ds:block id=newdiskwithkeys-79ndd6w9 -->
**newDiskWithKeys** · [`examples/integrations-otel/main.go:91-95`](../../examples/integrations-otel/main.go#L91-L95)

```go
func newDiskWithKeys(t Telemetry) (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(
		fsotel.Tracing(t.Tracer, fsotel.WithKeys()),
	))
}
```
<!-- /ds:block hash=fcc1a6 -->

Check: `TestSpansCarryNoKeysUnlessAsked` stats `users/42/secret.png` and requires no `filesystem.key` attribute without the option, and the key with it. It checks attributes; the module's own `TestFailedSpanCarriesNoKeyUnlessAsked` (`contrib/otel/otel_test.go`) scans every string a failed span holds, its name, status, attributes and events, for the key. Metrics never carry keys, with or without the option.

## Placement

| Where | You see |
|---|---|
| Outside `layer.Retry` (the example) | one span and one count per operation; `filesystem.attempts` says how many tries; duration includes backoff |
| Inside `layer.Retry` | one span and one count per attempt; failed attempts count as failures even when the operation then succeeded |

## Pitfalls and limits

- **A nil provider panics.** `fsotel.Tracing(nil)` and `fsotel.Metrics(nil)` call methods on the provider while building the interceptor. To switch telemetry off, pass `noop.NewTracerProvider()` and `noop.NewMeterProvider()` (from `go.opentelemetry.io/otel/trace/noop` and `.../metric/noop`), or leave the interceptors out.
- **The globals are not read for you.** Setting `otel.SetTracerProvider` after building the disk changes nothing: the interceptor holds the provider it was given. Build the disk after your telemetry setup. (`otel.GetTracerProvider()` called before any `SetTracerProvider` returns a delegate that forwards to the provider set later, so passing it early also works.)
- **A read's span covers opening the stream, not consuming it.** `Disk.Read` returns a reader that outlives the operation, so the span ends when the reader is returned. Time spent streaming the body to a client is not in `filesystem.read`.
- **Validation failures have no span.** An invalid key or option is refused before the interceptor chain runs, so `filesystem.invalid_key` never appears; count those where you call the library.
- **Records operations are not traced as such.** A `records.Store.Upload` shows as the `filesystem.write` of its object; its database statements need your database driver's own instrumentation.
- **Disk names become attribute values.** A scoped disk inherits its parent's interceptors and reports its own name, so a scoped disk per tenant (`disk.Scope("tenant-"+id, …)`) gives every tenant its own `filesystem.disk` value and multiplies metric series. Name scoped disks per feature rather than per tenant, or record metrics with your own `layer.Observe` callback that maps disk names to a bounded set ([logging and metrics](logging-and-metrics.md)).

## Next

- [`examples/integrations-otel`](../../examples/integrations-otel/README.md): the runnable program behind this page.
- [Logging and metrics](logging-and-metrics.md): `log/slog` lines and your own counters without OpenTelemetry.
- [Write an interceptor](../extending/write-an-interceptor.md): when you need a measurement the module does not record.
- [`contrib/otel` reference](../../contrib/otel/README.md): every exported name.
