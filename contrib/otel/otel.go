// Package otel adds OpenTelemetry tracing and metrics to disks as
// interceptors: one span per operation (so SDK calls made inside it become
// children), and counters and a latency histogram labelled by disk,
// operation and outcome (plus the error code on failures).
//
// Providers are passed in, never read from OpenTelemetry's globals, so a
// test or a second component can use its own. Object keys are left off
// spans unless WithKeys is set, because keys often contain user IDs or file
// names that a tracing backend should not store.
package otel

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
)

// InstrumentationName names the tracer and meter.
const InstrumentationName = "github.com/ubgo/filesystem"

// Attribute keys.
const (
	AttrDisk     = attribute.Key("filesystem.disk")
	AttrOp       = attribute.Key("filesystem.operation")
	AttrKey      = attribute.Key("filesystem.key")
	AttrTarget   = attribute.Key("filesystem.target")
	AttrCode     = attribute.Key("filesystem.error_code")
	AttrOutcome  = attribute.Key("filesystem.outcome")
	AttrAttempts = attribute.Key("filesystem.attempts")
	AttrBytes    = attribute.Key("filesystem.bytes")
)

// Metric names.
const (
	MetricOperations   = "filesystem.operations"
	MetricDuration     = "filesystem.operation.duration"
	MetricBytesWritten = "filesystem.bytes_written"
)

// spanPrefix starts every span name: "filesystem.read", "filesystem.write".
const spanPrefix = "filesystem."

// Values of AttrOutcome on metrics, a closed set so a dashboard can split
// successes from failures without listing every error code; the code of a
// failure is in AttrCode beside it.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// Option configures Tracing and Metrics.
type Option func(*options)

type options struct {
	keys bool
	// redact, when set, is how a key is recorded (WithKeyRedactor).
	redact func(key string) string
	// attrs adds attributes per operation (WithAttributes).
	attrs func(ctx context.Context, op filesystem.Op) []attribute.KeyValue
}

// newOptions applies opts.
func newOptions(opts []Option) options {
	o := options{}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithKeys records object keys (and copy or move targets) on spans: as
// attributes, and inside the exception message of a failed operation's
// error event. Without it neither appears anywhere on the span. Metrics
// never record keys, whatever the options.
func WithKeys() Option { return func(o *options) { o.keys = true } }

// WithKeyRedactor records keys (and targets) on spans as f returns them
// instead of leaving them off: a hash, the key with its user segment
// masked, its prefix only. The exception message of a failed operation
// stays key-free, as without WithKeys, since a driver's error text can
// hold the raw key. It replaces WithKeys when both are given; a nil f
// leaves keys off.
func WithKeyRedactor(f func(key string) string) Option {
	return func(o *options) { o.redact = f }
}

// WithAttributes adds the attributes f returns to every operation's span
// and, under Metrics, to its measurements: the tenant or backend taken
// from ctx, say. f runs once per operation, before it, and gets a copy of
// the operation, so it cannot change it; nil adds none.
// Keep what it returns free of object keys (use WithKeys or
// WithKeyRedactor for those) and, for Metrics, low in cardinality: every
// distinct value is a new time series.
func WithAttributes(f func(ctx context.Context, op filesystem.Op) []attribute.KeyValue) Option {
	return func(o *options) { o.attrs = f }
}

// extra returns WithAttributes' attributes for op, or none.
func (o options) extra(ctx context.Context, op filesystem.Op) []attribute.KeyValue {
	if o.attrs == nil {
		return nil
	}
	return o.attrs(ctx, op)
}

// key is how k is recorded on a span, and whether it is at all.
func (o options) key(k string) (string, bool) {
	switch {
	case o.redact != nil:
		return o.redact(k), true
	case o.keys:
		return k, true
	}
	return "", false
}

// recorded returns the error to put in a failed span's exception event.
//
// Why it exists: span.RecordError stores err.Error() as the event's
// exception.message, and a *filesystem.Error renders its key there
// ("filesystem: write media:users/42/secret.png: exists: ..."), as does many
// a driver's cause (a local *fs.PathError holds the full path, an S3 error
// names the object). Without WithKeys that text would put the key into the
// trace by the back door even though no key attribute is set.
//
// Invariant: with keys off, nothing derived from err's text reaches the
// span; only the operation, the disk name and the classified code do, which
// the span's attributes already carry. With WithKeys, err is recorded as is.
func (o options) recorded(op *filesystem.Op, err error) error {
	if o.keys && o.redact == nil {
		return err
	}
	return &keylessError{op: op.Kind, disk: op.Disk, code: filesystem.CodeOf(err)}
}

// keylessError is the key-free stand-in recorded on spans when WithKeys is
// off. Its Error() reads like a *filesystem.Error without the key or the
// cause: "filesystem: write media: exists". Its type name becomes the
// event's exception.type, so a backend can tell a redacted error apart.
type keylessError struct {
	op   filesystem.OpKind
	disk string
	code filesystem.Code
}

func (e *keylessError) Error() string {
	var b strings.Builder
	b.WriteString("filesystem")
	if context := strings.TrimSpace(string(e.op) + " " + e.disk); context != "" {
		b.WriteString(": ")
		b.WriteString(context)
	}
	b.WriteString(": ")
	b.WriteString(string(e.code))
	return b.String()
}

// Tracing returns an interceptor that wraps every operation in a span from
// tp. Failed operations get an error status and the filesystem code, and
// an exception event whose message names the key and the cause only under
// WithKeys (otherwise it carries just the operation, disk and code). Place
// it outside Retry to see one span per operation, inside to see one per
// attempt.
func Tracing(tp trace.TracerProvider, opts ...Option) filesystem.Interceptor {
	o := newOptions(opts)
	tracer := tp.Tracer(InstrumentationName)
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		attrs := []attribute.KeyValue{AttrDisk.String(op.Disk), AttrOp.String(string(op.Kind))}
		if k, ok := o.key(op.Key); ok {
			attrs = append(attrs, AttrKey.String(k))
			if op.Target != "" {
				t, _ := o.key(op.Target)
				attrs = append(attrs, AttrTarget.String(t))
			}
		}
		attrs = append(attrs, o.extra(ctx, *op)...)
		ctx, span := tracer.Start(ctx, spanPrefix+string(op.Kind), trace.WithAttributes(attrs...), trace.WithSpanKind(trace.SpanKindClient))
		defer span.End()
		err := call(ctx)
		span.SetAttributes(AttrAttempts.Int(op.Attempt))
		if op.Bytes > 0 {
			span.SetAttributes(AttrBytes.Int64(op.Bytes))
		}
		if err != nil {
			span.SetAttributes(AttrCode.String(string(filesystem.CodeOf(err))))
			span.RecordError(o.recorded(op, err))
			span.SetStatus(codes.Error, string(filesystem.CodeOf(err)))
		}
		return err
	}
}

// Metrics returns an interceptor recording, per disk, operation and
// outcome (AttrOutcome: OutcomeOK or OutcomeError, with AttrCode carrying
// the error's code on failures only): an operation counter, a duration
// histogram in seconds, and a counter of bytes written. WithAttributes
// adds its attributes to all three; WithKeys and WithKeyRedactor do
// nothing here, since a key per series would be unbounded. It fails only
// if the meter cannot create its instruments.
func Metrics(mp metric.MeterProvider, opts ...Option) (filesystem.Interceptor, error) {
	o := newOptions(opts)
	m := mp.Meter(InstrumentationName)
	ops, err := m.Int64Counter(MetricOperations, metric.WithDescription("Storage operations by disk, operation and outcome."))
	if err != nil {
		return nil, err
	}
	dur, err := m.Float64Histogram(MetricDuration, metric.WithUnit("s"), metric.WithDescription("Duration of storage operations, retries included when placed outside Retry."))
	if err != nil {
		return nil, err
	}
	written, err := m.Int64Counter(MetricBytesWritten, metric.WithUnit("By"), metric.WithDescription("Bytes stored by successful writes."))
	if err != nil {
		return nil, err
	}
	return layer.Observe(func(ctx context.Context, e layer.Event) {
		extra := o.extra(ctx, e.Op)
		attrs := []attribute.KeyValue{AttrDisk.String(e.Op.Disk), AttrOp.String(string(e.Op.Kind)), AttrOutcome.String(OutcomeOK)}
		if e.Err != nil {
			attrs[2] = AttrOutcome.String(OutcomeError)
			attrs = append(attrs, AttrCode.String(string(e.Code)))
		}
		set := metric.WithAttributes(append(attrs, extra...)...)
		ops.Add(ctx, 1, set)
		dur.Record(ctx, e.Duration.Seconds(), set)
		if e.Err == nil && e.Op.Bytes > 0 {
			written.Add(ctx, e.Op.Bytes, metric.WithAttributes(append([]attribute.KeyValue{AttrDisk.String(e.Op.Disk)}, extra...)...))
		}
	}), nil
}
