# Extending: interceptors of your own

Three interceptors written from scratch: one that freezes part of a disk, one that reports slow operations, and one that enforces a byte budget from what writes actually stored. Together they use every part of the interceptor contract: the `Op` an interceptor reads before the call, the fields the Disk fills in after it, and the choice to call the rest of the chain or to refuse. This is how an app adds a rule that applies to every operation on a disk (a legal freeze, a per-disk budget, a latency alarm) without touching a driver. It uses `github.com/ubgo/filesystem`, `layer.Retry` and the `memory` driver. The guide is [Write an interceptor](../../docs/extending/write-an-interceptor.md).

## Run it

```sh
cd examples
go run ./extending-interceptor
go test ./extending-interceptor
```

It runs on the in-memory driver: nothing is written to disk and no port is opened. The program prints each refusal and exits. The tests also cover a batch delete and a signed upload URL into the frozen directory, and the slow-operation report with a fake clock.

## What it does

1. Writes `legal/contract.pdf`: `Freeze("legal")` refuses it before the driver is called.
2. Writes `drafts/contract.txt`, which is allowed, then tries to move it into `legal/`: refused, because `Freeze` checks the move's target as well as its source.
3. Writes 16-byte files under `bulk/` until `Budget(64)` refuses. The 5 bytes of the draft count too, so the budget trips once 69 bytes are stored.

```text
write legal/contract.pdf: filesystem: write media:legal/contract.pdf: read_only: legal/ is frozen
write drafts/contract.txt: <nil>
move into legal/: filesystem: move media:drafts/contract.txt: read_only: legal/ is frozen
after 69 bytes: filesystem: write media:bulk/4.txt: quota_exceeded: disk media used its 64 bytes
```

No operation on the memory driver is slower than 250 ms, so `ReportSlow` prints nothing here; the test drives it with a fake clock.

## The code

`Freeze` refuses every mutation whose key, target or batch keys fall under the directory. It keys on `op.Mutating`, the Disk's own answer to "does this change data or grant a write", so a signed PUT URL is refused while reads and GET links pass.

<!-- ds:block id=freeze-kbrc2z8y -->
**Freeze** · [`examples/extending-interceptor/main.go:40-51`](main.go#L40-L51)

```go
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
```
<!-- /ds:block hash=8ba8cc -->

`ReportSlow` measures around the call and reads `op.Attempt` afterwards, which a retry interceptor inside it has advanced, so a report says when an operation was slow because it was retried.

<!-- ds:block id=reportslow-5tsfe2t7 -->
**ReportSlow** · [`examples/extending-interceptor/main.go:66-75`](main.go#L66-L75)

```go
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
```
<!-- /ds:block hash=818aeb -->

`Budget` checks before the call and charges after it from `op.Bytes`, which the Disk sets to what was actually stored, never the client's declared size. The check and the charge are not one atomic step, so concurrent writes can pass the limit by one write each; a hard limit belongs in the database (`records/quota`).

<!-- ds:block id=budget-ezqw4enw -->
**Budget** · [`examples/extending-interceptor/main.go:84-96`](main.go#L84-L96)

```go
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
```
<!-- /ds:block hash=ce3a73 -->

`newDisk` installs them in order: `ReportSlow` outermost so it sees whole operations, retries included; `Freeze` and `Budget` outside `layer.Retry` so a refusal never costs a backoff.

<!-- ds:block id=newdisk-hyypbpf7 -->
**newDisk** · [`examples/extending-interceptor/main.go:103-112`](main.go#L103-L112)

```go
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
```
<!-- /ds:block hash=90f24c -->

## Files

| File | What it holds |
|---|---|
| `main.go` | `Freeze`, `ReportSlow`, `Budget`, `newDisk` and the demo `run` |
| `main_test.go` | The demo output, `Freeze` against batch deletes and signed uploads, `ReportSlow` with a fake clock |

## Next

- [Write an interceptor](../../docs/extending/write-an-interceptor.md): the guide for this program.
- [Logging and metrics](../integrations-logging): the built-in interceptors and how to order them.
- [Concepts: interceptors](../concepts-interceptors) and [Write a driver](../extending-driver) when the behaviour belongs to a backend instead.
