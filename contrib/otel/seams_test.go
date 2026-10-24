package otel_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ubgo/filesystem"
	fsotel "github.com/ubgo/filesystem/contrib/otel"
	"github.com/ubgo/filesystem/memory"
)

type tenantKey struct{}

// tenant adds the tenant ctx carries.
func tenant(ctx context.Context, _ filesystem.Op) []attribute.KeyValue {
	if v, ok := ctx.Value(tenantKey{}).(string); ok {
		return []attribute.KeyValue{attribute.String("tenant", v)}
	}
	return nil
}

// TestSeams pins WithAttributes on spans and metrics, and
// WithKeyRedactor: the redacted key is recorded, the raw one appears
// nowhere on the span (attributes or the failed operation's exception),
// and it wins over WithKeys.
func TestSeams(t *testing.T) {
	ctx := context.WithValue(t.Context(), tenantKey{}, "acme")
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	mask := func(k string) string { return "masked:" + k[:strings.IndexByte(k+"/", '/')] }
	reader := sdkmetric.NewManualReader()
	m, err := fsotel.Metrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), fsotel.WithAttributes(tenant))
	if err != nil {
		t.Fatal(err)
	}
	d, _ := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(fsotel.Tracing(tp, fsotel.WithKeys(), fsotel.WithKeyRedactor(mask), fsotel.WithAttributes(tenant)), m))
	_, _ = d.WriteBytes(ctx, "users/42/secret.png", []byte("abc"), filesystem.WriteOptions{})
	_, _ = d.Stat(ctx, "users/42/missing.png")
	for _, s := range rec.Ended() {
		if v, _ := attr(s.Attributes(), "tenant"); v != "acme" {
			t.Errorf("%s: tenant = %q", s.Name(), v)
		}
		if k, _ := attr(s.Attributes(), fsotel.AttrKey); k != "masked:users" {
			t.Errorf("%s: key = %q", s.Name(), k)
		}
		for _, e := range s.Events() {
			for _, a := range e.Attributes {
				if strings.Contains(a.Value.Emit(), "42") {
					t.Errorf("%s: the raw key reached event %s: %s", s.Name(), e.Name, a.Value.Emit())
				}
			}
		}
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != fsotel.MetricOperations {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				if v, ok := dp.Attributes.Value("tenant"); !ok || v.AsString() != "acme" {
					t.Errorf("metric point without the tenant: %v", dp.Attributes)
				}
				if _, ok := dp.Attributes.Value(fsotel.AttrKey); ok {
					t.Error("a key reached a metric")
				}
			}
		}
	}
}
