package otel_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ubgo/filesystem"
	fsotel "github.com/ubgo/filesystem/contrib/otel"
	"github.com/ubgo/filesystem/memory"
)

func attr(kvs []attribute.KeyValue, k attribute.Key) (string, bool) {
	for _, kv := range kvs {
		if kv.Key == k {
			return kv.Value.Emit(), true
		}
	}
	return "", false
}

func TestTracing(t *testing.T) {
	ctx := t.Context()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	d, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(fsotel.Tracing(tp)))
	_, _ = d.WriteBytes(ctx, "users/42/secret.png", []byte("abc"), filesystem.WriteOptions{})
	_, _ = d.Stat(ctx, "missing")
	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	w := spans[0]
	if w.Name() != "filesystem.write" || w.Status().Code == codes.Error {
		t.Errorf("write span %s %v", w.Name(), w.Status())
	}
	if v, _ := attr(w.Attributes(), fsotel.AttrBytes); v != "3" {
		t.Errorf("bytes = %q", v)
	}
	if _, ok := attr(w.Attributes(), fsotel.AttrKey); ok {
		t.Error("key recorded without WithKeys")
	}
	s := spans[1]
	if code, _ := attr(s.Attributes(), fsotel.AttrCode); s.Status().Code != codes.Error || code != "not_found" || len(s.Events()) == 0 {
		t.Errorf("failed span: status %v code %q events %d", s.Status(), code, len(s.Events()))
	}

	rec2 := tracetest.NewSpanRecorder()
	tp2 := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec2))
	d2, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(fsotel.Tracing(tp2, fsotel.WithKeys())))
	_, _ = d2.WriteBytes(ctx, "a", []byte("x"), filesystem.WriteOptions{})
	_, _ = d2.Copy(ctx, "a", "b")
	got := rec2.Ended()
	if k, _ := attr(got[1].Attributes(), fsotel.AttrKey); k != "a" {
		t.Errorf("key = %q", k)
	}
	if tg, _ := attr(got[1].Attributes(), fsotel.AttrTarget); tg != "b" {
		t.Errorf("target = %q", tg)
	}
}

func TestMetrics(t *testing.T) {
	ctx := t.Context()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := fsotel.Metrics(mp)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(m))
	_, _ = d.WriteBytes(ctx, "a", []byte("hello"), filesystem.WriteOptions{})
	_, _ = d.Stat(ctx, "missing")
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			found[mt.Name] = true
			if mt.Name == fsotel.MetricBytesWritten {
				sum := mt.Data.(metricdata.Sum[int64])
				if sum.DataPoints[0].Value != 5 {
					t.Errorf("bytes written = %d", sum.DataPoints[0].Value)
				}
			}
			if mt.Name == fsotel.MetricOperations {
				sum := mt.Data.(metricdata.Sum[int64])
				// A success has outcome ok and no error code; a failure has
				// outcome error and its code.
				var outcomes []string
				for _, dp := range sum.DataPoints {
					outcome, _ := dp.Attributes.Value(fsotel.AttrOutcome)
					code, hasCode := dp.Attributes.Value(fsotel.AttrCode)
					outcomes = append(outcomes, outcome.AsString()+"/"+code.AsString())
					if hasCode != (outcome.AsString() == fsotel.OutcomeError) {
						t.Errorf("data point %v: error code present = %v", dp.Attributes, hasCode)
					}
				}
				slices.Sort(outcomes)
				if want := []string{fsotel.OutcomeError + "/not_found", fsotel.OutcomeOK + "/"}; !slices.Equal(outcomes, want) {
					t.Errorf("outcomes = %v, want %v", outcomes, want)
				}
			}
		}
	}
	for _, name := range []string{fsotel.MetricOperations, fsotel.MetricDuration, fsotel.MetricBytesWritten} {
		if !found[name] {
			t.Errorf("metric %s missing", name)
		}
	}
}

// failingMeter fails to create the n-th instrument.
type failingMeter struct {
	noop.Meter
	n, calls int
}

var errInstrument = errors.New("cannot create instrument")

func (f *failingMeter) fail() bool { f.calls++; return f.calls == f.n }

func (f *failingMeter) Int64Counter(name string, o ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	if f.fail() {
		return nil, errInstrument
	}
	return f.Meter.Int64Counter(name, o...)
}

func (f *failingMeter) Float64Histogram(name string, o ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	if f.fail() {
		return nil, errInstrument
	}
	return f.Meter.Float64Histogram(name, o...)
}

type failingProvider struct {
	noop.MeterProvider
	meter *failingMeter
}

func (p failingProvider) Meter(string, ...metric.MeterOption) metric.Meter { return p.meter }

func TestMetricsInstrumentErrors(t *testing.T) {
	for n := 1; n <= 3; n++ {
		if _, err := fsotel.Metrics(failingProvider{meter: &failingMeter{n: n}}); !errors.Is(err, errInstrument) {
			t.Errorf("instrument %d failure = %v", n, err)
		}
	}
}

// TestFailedSpanCarriesNoKeyUnlessAsked pins that a failure does not leak
// the key through the exception event or the status description, which
// RecordError fills from err.Error() and a *filesystem.Error renders with
// its key. Every string the span holds is scanned, not just attributes.
func TestFailedSpanCarriesNoKeyUnlessAsked(t *testing.T) {
	const src, dst = "users/42/secret.png", "users/42/copy.png"
	const marker = "users/42"
	failedSpan := func(opts ...fsotel.Option) sdktrace.ReadOnlySpan {
		t.Helper()
		rec := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
		d, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(fsotel.Tracing(tp, opts...)))
		if _, err := d.Copy(t.Context(), src, dst); !errors.Is(err, filesystem.ErrNotFound) {
			t.Fatalf("copy of a missing key: %v", err)
		}
		spans := rec.Ended()
		if len(spans) != 1 || spans[0].Status().Code != codes.Error {
			t.Fatalf("want one failed span, got %d", len(spans))
		}
		return spans[0]
	}
	// texts gathers every string a tracing backend would store for s.
	texts := func(s sdktrace.ReadOnlySpan) []string {
		out := []string{s.Name(), s.Status().Description}
		for _, kv := range s.Attributes() {
			out = append(out, kv.Value.Emit())
		}
		for _, ev := range s.Events() {
			out = append(out, ev.Name)
			for _, kv := range ev.Attributes {
				out = append(out, kv.Value.Emit())
			}
		}
		return out
	}

	s := failedSpan()
	if len(s.Events()) == 0 {
		t.Fatal("failed span has no exception event")
	}
	for _, txt := range texts(s) {
		if strings.Contains(txt, marker) {
			t.Errorf("key leaked without WithKeys: %q", txt)
		}
	}
	msg, _ := attr(s.Events()[0].Attributes, "exception.message")
	if !strings.Contains(msg, "not_found") || !strings.Contains(msg, "copy") || !strings.Contains(msg, "media") {
		t.Errorf("redacted message lost the code, operation or disk: %q", msg)
	}

	// The inverse: WithKeys keeps the full error, key included.
	s = failedSpan(fsotel.WithKeys())
	msg, _ = attr(s.Events()[0].Attributes, "exception.message")
	if !strings.Contains(msg, src) {
		t.Errorf("WithKeys message lost the key: %q", msg)
	}
}
