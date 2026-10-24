package layer

import (
	"context"
	"log/slog"
	"time"

	"github.com/ubgo/filesystem"
)

// Event describes one finished operation, for metrics and tracing. It
// carries no payload, no URL and no signature, so nothing secret can reach a
// metrics or log sink through it.
type Event struct {
	// Op is a copy of the operation as it finished: kind, disk, key,
	// target, attempts and bytes written.
	Op filesystem.Op
	// Duration is the wall time of the whole operation, retries included
	// when Observe runs outside Retry.
	Duration time.Duration
	// Err is the operation's error, nil on success.
	Err error
	// Code is filesystem.CodeOf(Err), "" on success.
	Code filesystem.Code
}

// Observe calls fn after every operation. It is the hook metrics and tracing
// adapters build on (contrib/otel, contrib/prometheus): they need no access
// to the driver and cannot change the outcome.
func Observe(fn func(ctx context.Context, e Event)) filesystem.Interceptor {
	return observe(time.Now, fn)
}

func observe(now func() time.Time, fn func(ctx context.Context, e Event)) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		start := now()
		err := call(ctx)
		fn(ctx, Event{Op: *op, Duration: now().Sub(start), Err: err, Code: filesystem.CodeOf(err)})
		return err
	}
}

// Guard calls check before every operation and fails the operation with
// check's error, without reaching the driver, when it returns one. Use it
// for policy that applies to every call on a disk: tenant checks, quotas,
// maintenance windows. Errors without a code of their own (CodeOf reports
// CodeUnknown) are wrapped with CodePermission; a classified error, an
// io/fs sentinel or a context error keeps its code.
func Guard(check func(ctx context.Context, op *filesystem.Op) error) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if err := check(ctx, op); err != nil {
			if filesystem.CodeOf(err) == filesystem.CodeUnknown {
				return filesystem.NewError(filesystem.CodePermission, err)
			}
			return err
		}
		return call(ctx)
	}
}

// Log attributes. Named once so dashboards and log queries can rely on them.
const (
	AttrDisk     = "disk"
	AttrOp       = "op"
	AttrKey      = "key"
	AttrTarget   = "target"
	AttrAttempts = "attempts"
	AttrBytes    = "bytes"
	AttrDuration = "duration"
	AttrCode     = "code"
	AttrError    = "error"
)

// LogMessage is the message of every record Log writes.
const LogMessage = "filesystem operation"

// LogOptions configure Log. Without options, successes log at Debug and
// failures at Warn, except CodeNotFound from an operation that changes
// nothing (Stat, Exists, Read, List), which is an expected answer rather
// than a failure and logs at Debug. Not-found from a mutation is a failure.
type LogOptions struct {
	SuccessLevel  slog.Level
	FailureLevel  slog.Level
	NotFoundLevel slog.Level
}

// Log writes one structured record per operation to logger. It logs keys
// (object names), never signed URLs or credentials: an operation's result is
// not visible to interceptors, so a URL cannot leak through it.
func Log(logger *slog.Logger, opts ...LogOptions) filesystem.Interceptor {
	o := LogOptions{SuccessLevel: slog.LevelDebug, FailureLevel: slog.LevelWarn, NotFoundLevel: slog.LevelDebug}
	if len(opts) > 0 {
		o = opts[0]
	}
	return observe(time.Now, func(ctx context.Context, e Event) {
		level := o.SuccessLevel
		switch {
		case e.Code == filesystem.CodeNotFound && !e.Op.Mutating:
			level = o.NotFoundLevel
		case e.Err != nil:
			level = o.FailureLevel
		}
		if !logger.Enabled(ctx, level) {
			return
		}
		attrs := []slog.Attr{
			slog.String(AttrDisk, e.Op.Disk),
			slog.String(AttrOp, string(e.Op.Kind)),
			slog.String(AttrKey, e.Op.Key),
			slog.Int(AttrAttempts, e.Op.Attempt),
			slog.Duration(AttrDuration, e.Duration),
		}
		if e.Op.Target != "" {
			attrs = append(attrs, slog.String(AttrTarget, e.Op.Target))
		}
		if e.Op.Bytes > 0 {
			attrs = append(attrs, slog.Int64(AttrBytes, e.Op.Bytes))
		}
		if e.Err != nil {
			attrs = append(attrs, slog.String(AttrCode, string(e.Code)), slog.String(AttrError, e.Err.Error()))
		}
		logger.LogAttrs(ctx, level, LogMessage, attrs...)
	})
}
