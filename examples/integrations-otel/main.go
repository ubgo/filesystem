// Command integrations-otel traces and measures every disk operation with
// OpenTelemetry through contrib/otel: one span per operation, and an
// operation counter, a latency histogram and a bytes-written counter per
// disk, operation and outcome.
//
//	go run ./integrations-otel
//
// It records spans and metrics in memory and prints them, so it runs with
// no collector. An application passes its own providers instead, built
// with the OTLP exporters (go.opentelemetry.io/otel/exporters/otlp/...).
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ubgo/filesystem"
	fsotel "github.com/ubgo/filesystem/contrib/otel"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// Names and limits this example chose.
const (
	diskName  = "media"
	opTimeout = 10 * time.Second
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// Telemetry holds in-memory providers: a span recorder behind a tracer
// provider, and a manual reader behind a meter provider.
type Telemetry struct {
	Spans  *tracetest.SpanRecorder
	Tracer *sdktrace.TracerProvider
	Reader *sdkmetric.ManualReader
	Meter  *sdkmetric.MeterProvider
}

// newTelemetry builds providers that keep what they record in memory.
// ds:def id=newtelemetry-ns6rgcpj
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

// newDisk installs the two interceptors outside Retry, so a retried
// operation is one span and one count, with its attempts recorded on the
// span. Placed inside Retry they would see each attempt instead. The
// providers are passed in; contrib/otel never reads OpenTelemetry's
// globals.
// ds:def id=newdisk-5sx46brr
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

// newDiskWithKeys records object keys on spans too. Keys often hold user
// IDs and file names, so they are off unless an application opts in.
// ds:def id=newdiskwithkeys-79ndd6w9
func newDiskWithKeys(t Telemetry) (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(
		fsotel.Tracing(t.Tracer, fsotel.WithKeys()),
	))
}

// work runs a few operations, one of which fails with not_found.
// ds:def id=work-b684493n
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

// run does the work and prints the spans and metrics it produced.
// ds:def id=run-qwm7599d
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

// printSpans writes one line per span: name, status and attributes.
func printSpans(out io.Writer, spans []sdktrace.ReadOnlySpan) {
	for _, s := range spans {
		fmt.Fprintf(out, "span %-18s status=%-5s", s.Name(), s.Status().Code)
		for _, kv := range s.Attributes() {
			fmt.Fprintf(out, " %s=%s", kv.Key, kv.Value.Emit())
		}
		fmt.Fprintln(out)
	}
}

// printMetrics writes each counter's data points, and each histogram's
// count, sorted for stable output.
func printMetrics(out io.Writer, rm metricdata.ResourceMetrics) {
	var lines []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range data.DataPoints {
					lines = append(lines, fmt.Sprintf("metric %s %s = %d", m.Name, attrs(p.Attributes.ToSlice()), p.Value))
				}
			case metricdata.Histogram[float64]:
				for _, p := range data.DataPoints {
					lines = append(lines, fmt.Sprintf("metric %s %s count=%d", m.Name, attrs(p.Attributes.ToSlice()), p.Count))
				}
			}
		}
	}
	slices.Sort(lines)
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
}

// attrs renders attributes as "key=value" pairs.
func attrs(kvs []attribute.KeyValue) string {
	parts := make([]string, len(kvs))
	for i, kv := range kvs {
		parts[i] = string(kv.Key) + "=" + kv.Value.Emit()
	}
	return strings.Join(parts, " ")
}
