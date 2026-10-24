// Command extending-interceptor writes three interceptors of its own: one
// that freezes part of a disk, one that reports slow operations, and one
// that enforces a byte budget from what writes actually stored. Together
// they use every part of the contract: the Op an interceptor reads before
// the call, the fields the Disk fills in after it, and the choice to call
// the rest of the chain or to refuse.
//
//	go run ./extending-interceptor
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/layer"
	"github.com/ubgo/filesystem/memory"
)

// Settings this example chose.
const (
	diskName      = "media"
	frozenDir     = "legal"
	slowThreshold = 250 * time.Millisecond
	budgetBytes   = 64
)

// Freeze refuses every mutation that touches a key under dir, before it
// reaches the driver. It reads op.Key, op.Target (the destination of a
// copy or move) and op.Keys (a batch delete). op.Mutating is the Disk's
// own answer to "does this change data or grant a write", so a signed PUT
// URL for a frozen key is refused too, while reads and GET links pass.
// ds:def id=freeze-kbrc2z8y
func Freeze(dir string) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if !op.Mutating {
			return call(ctx)
		}
		keys := append([]string{op.Key, op.Target}, op.Keys...)
		if slices.ContainsFunc(keys, func(k string) bool { return k == dir || filesystem.KeyInDir(k, dir) }) {
			return filesystem.Errorf(filesystem.CodeReadOnly, "%s/ is frozen", dir)
		}
		return call(ctx)
	}
}

// SlowOp is one operation that took longer than the threshold.
type SlowOp struct {
	Kind     filesystem.OpKind
	Key      string
	Attempts int
	Took     time.Duration
}

// ReportSlow calls report for every operation slower than threshold. It
// reads op.Attempt after the call, which a retry interceptor inside it has
// advanced, so a slow operation that was slow because it was retried says
// so. now is the clock, for tests.
// ds:def id=reportslow-5tsfe2t7
func ReportSlow(threshold time.Duration, now func() time.Time, report func(SlowOp)) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		start := now()
		err := call(ctx)
		if took := now().Sub(start); took > threshold {
			report(SlowOp{Kind: op.Kind, Key: op.Key, Attempts: op.Attempt, Took: took})
		}
		return err
	}
}

// Budget refuses writes once the bytes stored through this disk pass max.
// It checks before the call and charges after it, from op.Bytes, which
// the Disk sets to what was actually stored: the client's declared size
// never enters into it. The check and the charge are not one atomic step,
// so concurrent writes can pass the limit by one write each; a hard limit
// belongs in the database (see records/quota).
// ds:def id=budget-ezqw4enw
func Budget(max int64) (filesystem.Interceptor, *atomic.Int64) {
	var used atomic.Int64
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		if op.Kind == filesystem.OpWrite && used.Load() >= max {
			return filesystem.Errorf(filesystem.CodeQuotaExceeded, "disk %s used its %d bytes", op.Disk, max)
		}
		err := call(ctx)
		if err == nil && op.Bytes > 0 {
			used.Add(op.Bytes)
		}
		return err
	}, &used
}

// newDisk installs the interceptors. Order matters: ReportSlow outermost
// sees whole operations, retries included; Freeze and Budget sit outside
// Retry so a refusal is never retried (it is not CodeUnavailable anyway,
// but a refusal should not cost a backoff either).
// ds:def id=newdisk-hyypbpf7
func newDisk(report func(SlowOp)) (*filesystem.Disk, *atomic.Int64, error) {
	budget, used := Budget(budgetBytes)
	d, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithInterceptors(
		ReportSlow(slowThreshold, time.Now, report),
		Freeze(frozenDir),
		budget,
		layer.Retry(layer.RetryOptions{}),
	))
	return d, used, err
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run shows each interceptor refusing or reporting.
// ds:def id=run-qazrcdep
func run(ctx context.Context, out io.Writer) error {
	d, used, err := newDisk(func(s SlowOp) { fmt.Fprintf(out, "slow: %+v\n", s) })
	if err != nil {
		return err
	}
	_, err = d.WriteBytes(ctx, "legal/contract.pdf", []byte("%PDF-1.7"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "write legal/contract.pdf: %v\n", err)
	_, err = d.WriteBytes(ctx, "drafts/contract.txt", []byte("draft"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "write drafts/contract.txt: %v\n", err)
	_, err = d.Move(ctx, "drafts/contract.txt", "legal/contract.txt")
	fmt.Fprintf(out, "move into legal/: %v\n", err)
	for i := 0; ; i++ {
		if _, err = d.WriteBytes(ctx, fmt.Sprintf("bulk/%d.txt", i), []byte("0123456789abcdef"), filesystem.WriteOptions{}); err != nil {
			break
		}
	}
	fmt.Fprintf(out, "after %d bytes: %v\n", used.Load(), err)
	return nil
}
