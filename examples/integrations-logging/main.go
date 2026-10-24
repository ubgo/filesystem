// Command integrations-logging logs every disk operation with log/slog
// through layer.Log, counts operations with layer.Observe into an expvar
// map, and shows the order interceptors run in by retrying a backend that
// fails once.
//
//	go run ./integrations-logging
//
// It writes JSON log lines and then the counters to standard output.
package main

import (
	"context"
	"expvar"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// Names and limits this example chose.
const (
	diskName      = "media"
	opTimeout     = 10 * time.Second
	maxConcurrent = 32
	// outcomeOK labels a successful operation in the counters, the same
	// word contrib/otel uses.
	outcomeOK = "ok"
)

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// newMetrics returns the counters Observe fills: operations by disk,
// operation and outcome, and bytes written by disk. They are not passed to
// expvar.Publish here, which panics when a name is published twice; an app
// publishes them once at start-up to serve them on /debug/vars.
// ds:def id=newmetrics-tczcvcnv
func newMetrics() (ops, written *expvar.Map) {
	return new(expvar.Map).Init(), new(expvar.Map).Init()
}

// record is the Observe callback. Event carries the operation (kind,
// disk, key, attempts, bytes), its duration and error code, and never a
// payload, URL or signature.
// ds:def id=record-6z7vsd8a
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

// newDisk installs the interceptors in the usual order, outermost first.
// Observe and Log sit outside Retry, so each sees one operation with its
// attempt count; Timeout sits inside Retry, so each attempt gets its own
// budget; Limit is innermost, so a waiting retry does not hold a slot.
// ds:def id=newdisk-3e5qdbjh
func newDisk(drv filesystem.Driver, logger *slog.Logger, ops, written *expvar.Map) (*filesystem.Disk, error) {
	return filesystem.NewDisk(diskName, drv, filesystem.WithInterceptors(
		layer.Observe(record(ops, written)),
		layer.Log(logger),
		layer.Retry(layer.RetryOptions{}),
		layer.Timeout(opTimeout),
		layer.Limit(maxConcurrent),
	))
}

// newQuietLog logs successes at Info instead of Debug, and keeps failures
// at Warn and expected not-found answers at Debug, which is the shape most
// production loggers want: every change visible, lookups only on demand.
// ds:def id=newquietlog-suvqwgca
func newQuietLog(logger *slog.Logger) filesystem.Interceptor {
	return layer.Log(logger, layer.LogOptions{
		SuccessLevel:  slog.LevelInfo,
		FailureLevel:  slog.LevelWarn,
		NotFoundLevel: slog.LevelDebug,
	})
}

// flaky is a memory driver whose first Stat fails the way a throttled
// backend does: with CodeUnavailable, the code Retry retries.
type flaky struct {
	*memory.Driver
	failed atomic.Bool
}

// Stat fails once, then answers normally.
// ds:def id=flaky-stat-ar76ae68
func (f *flaky) Stat(ctx context.Context, key string) (filesystem.Entry, error) {
	if f.failed.CompareAndSwap(false, true) {
		return filesystem.Entry{}, filesystem.Errorf(filesystem.CodeUnavailable, "throttled")
	}
	return f.Driver.Stat(ctx, key)
}

// run writes, stats (retried once), copies, misses a key and passes an
// invalid key, then prints the counters.
// ds:def id=run-kswmtmzn
func run(ctx context.Context, out io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropTime}))
	ops, written := newMetrics()
	d, err := newDisk(&flaky{Driver: memory.New()}, logger, ops, written)
	if err != nil {
		return err
	}
	if _, err := d.WriteBytes(ctx, "reports/q3.txt", []byte("quarterly numbers\n"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	if _, err := d.Stat(ctx, "reports/q3.txt"); err != nil {
		return err
	}
	if _, err := d.Copy(ctx, "reports/q3.txt", "archive/q3.txt"); err != nil {
		return err
	}
	_, _ = d.Stat(ctx, "reports/missing.txt")
	_ = d.Delete(ctx, "")
	fmt.Fprintf(out, "operations %s\nbytes_written %s\n", ops.String(), written.String())
	return nil
}

// dropTime removes the timestamp and duration from log records, so the
// example's output is the same on every run.
func dropTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey || a.Key == layer.AttrDuration {
		return slog.Attr{}
	}
	return a
}
