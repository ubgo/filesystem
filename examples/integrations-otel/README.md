# Integrations: OpenTelemetry traces and metrics

Every disk operation traced and measured with OpenTelemetry through `contrib/otel`: one span per operation, and an operation counter, a latency histogram and a bytes-written counter, labelled by disk, operation and outcome. This is how storage shows up next to the rest of a service in Jaeger, Tempo, Honeycomb or any OTLP backend. It uses `github.com/ubgo/filesystem`, `github.com/ubgo/filesystem/contrib/otel`, `layer` and the `memory` driver, with the OpenTelemetry SDK. The guide is [OpenTelemetry](../../docs/integrations/otel.md).

## Run it

```sh
cd examples
go run ./integrations-otel
go test ./integrations-otel
```

It records spans and metrics in memory and prints them, so it needs no collector and opens no port. An application passes its own providers instead, built with the OTLP exporters (`go.opentelemetry.io/otel/exporters/otlp/...`); `contrib/otel` never reads OpenTelemetry's global providers. The tests also check that spans carry no object keys unless the app opts in with `WithKeys`.

## What it does

1. Writes `reports/q3.txt`, reads it back, and stats a missing key.
2. Prints the three spans: the write records the bytes stored, and the failed stat has `status=Error` with the error code. No span carries the key.
3. Prints the metrics: operation counts and latency histogram counts by outcome, and the bytes written.

```text
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

## The code

`newTelemetry` builds providers that keep what they record in memory: a span recorder behind a tracer provider, and a manual reader behind a meter provider. In an app these are the providers you already export with.

<!-- ds:block id=newtelemetry-ns6rgcpj -->
**newTelemetry** · [`examples/integrations-otel/main.go:58-67`](main.go#L58-L67)

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

`newDisk` installs `Metrics` and `Tracing` outside `layer.Retry`, so a retried operation is one span and one count, with its attempts recorded on the span. Inside `Retry` they would see each attempt instead.

<!-- ds:block id=newdisk-5sx46brr -->
**newDisk** · [`examples/integrations-otel/main.go:75-86`](main.go#L75-L86)

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

`newDiskWithKeys` records object keys on spans as well. Keys often hold user IDs and file names, so they are off unless an app opts in.

<!-- ds:block id=newdiskwithkeys-79ndd6w9 -->
**newDiskWithKeys** · [`examples/integrations-otel/main.go:91-95`](main.go#L91-L95)

```go
func newDiskWithKeys(t Telemetry) (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(
		fsotel.Tracing(t.Tracer, fsotel.WithKeys()),
	))
}
```
<!-- /ds:block hash=fcc1a6 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The in-memory providers, the two disks (without and with keys on spans), the demo work and the printers |
| `main_test.go` | The demo output, and that spans carry no keys unless asked |

## Next

- [OpenTelemetry](../../docs/integrations/otel.md): the guide for this program, with every span attribute and metric.
- [Logging and metrics](../integrations-logging): `log/slog` lines and your own counters without OpenTelemetry.
- [`contrib/otel` reference](../../contrib/otel/README.md): every exported name.
