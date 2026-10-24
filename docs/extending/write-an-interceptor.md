# Write an interceptor

You will have behaviour of your own around every operation of a disk: a rule that freezes part of a disk, a report of slow operations, and a byte budget charged from what writes actually stored. Interceptors are how the library does retries, timeouts, logging and metrics, and they wrap the operation rather than the driver, so no interceptor can hide a capability or break a fallback.

Read this page when `layer.Guard`, `layer.Observe` and the other built-ins in [logging and metrics](../integrations/logging-and-metrics.md) do not express what you need.

## Before you start

- [Interceptors](../concepts/interceptors.md) explains the chain and the built-ins.
- The program is [`examples/extending-interceptor`](../../examples/extending-interceptor/main.go). `go run ./extending-interceptor` in `examples/` shows each interceptor refusing or reporting; `go test ./extending-interceptor` checks them, including batch deletes and signed URLs.

## The contract

An interceptor is a function:

| Part | Type | Meaning |
|---|---|---|
| `filesystem.Interceptor` | `func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error` | runs around one operation |
| `call` | `filesystem.Call`, `func(ctx context.Context) error` | the rest of the chain and then the driver; each invocation is one attempt |

It may call `call` zero times (refuse), once (observe or adjust the context), or several times (retry), and returns `call`'s error or its own. It may read `op` before and after the call, and must not change `Kind`, `Disk`, `Key`, `Keys` or `Target`.

What `op` carries:

| Field | Before the call | After the call |
|---|---|---|
| `Kind` | the operation (`write`, `stat`, `copy`, …; every kind: [operations](../reference/operations.md)) | same |
| `Disk` | the disk's name | same |
| `Key` | the cleaned key; the directory for `list` and `delete_prefix`; empty for `delete_keys` | same |
| `Keys` | the keys of a `delete_keys`, else nil | same |
| `Target` | the destination of a `copy` or `move`, else empty | same |
| `Mutating` | true when the operation changes data or grants a write (a `presign` for `PUT`) | same |
| `Replayable` | false for a write whose body cannot be rewound, a `move`, a multipart create or complete | same |
| `Streaming` | true for a `read`, whose reader outlives the call | same |
| `Attempt` | attempts started so far (0 before the first) | attempts made |
| `Bytes` | 0 | bytes stored, after a successful `write` |

An interceptor never sees the operation's result value: not the bytes read, not the URL signed. That is what keeps signed URLs and payloads out of every log and metrics sink.

## Step 1 — Refuse before the driver

`Freeze` refuses every mutation that touches a directory, whether the key is the operation's own, a copy's destination or one of a batch's keys. It returns a coded error, so callers and `fshttp.StatusOf` treat it like any other `read_only`:

<!-- ds:block id=freeze-kbrc2z8y -->
**Freeze** · [`examples/extending-interceptor/main.go:40-51`](../../examples/extending-interceptor/main.go#L40-L51)

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

Keying on `op.Mutating` rather than a list of kinds means new mutating operations are covered without changing the interceptor, and a signed `PUT` URL for a frozen key is refused while `GET` links still work. `layer.Guard(check)` is the built-in for the simplest form of this; write your own when the check needs the result of the call, as the next two do.

## Step 2 — Measure around the call

`ReportSlow` times the whole call and reads `op.Attempt` afterwards, so a slow operation that was slow because a retry inside it tried twice says so. The clock is a parameter so the test does not sleep:

<!-- ds:block id=reportslow-5tsfe2t7 -->
**ReportSlow** · [`examples/extending-interceptor/main.go:66-75`](../../examples/extending-interceptor/main.go#L66-L75)

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

## Step 3 — Act on what the Disk measured

`Budget` refuses writes once the disk has stored its allowance. It checks before the call and charges after, from `op.Bytes`, which the Disk sets to the bytes actually stored, never to what the client declared:

<!-- ds:block id=budget-ezqw4enw -->
**Budget** · [`examples/extending-interceptor/main.go:84-96`](../../examples/extending-interceptor/main.go#L84-L96)

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

## Step 4 — Install in order

The first interceptor given runs outermost. `ReportSlow` outermost sees whole operations, retries included. `Freeze` and `Budget` sit outside `layer.Retry`, so a refusal costs no backoff:

<!-- ds:block id=newdisk-hyypbpf7 -->
**newDisk** · [`examples/extending-interceptor/main.go:103-112`](../../examples/extending-interceptor/main.go#L103-L112)

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

Check: `go run ./extending-interceptor` prints:

```
write legal/contract.pdf: filesystem: write media:legal/contract.pdf: read_only: legal/ is frozen
write drafts/contract.txt: <nil>
move into legal/: filesystem: move media:drafts/contract.txt: read_only: legal/ is frozen
after 69 bytes: filesystem: write media:bulk/4.txt: quota_exceeded: disk media used its 64 bytes
```

The Disk stamps the operation, disk and key on the interceptor's error, as it does on a driver's. The budget lets the write that crosses the limit through (5 bytes, then four 16-byte writes reach 69) and refuses the next one, because the check uses what was stored before the call.

## Step 5 — Use the disk as any other

Callers never see the interceptors: they hold an ordinary `*filesystem.Disk` and get coded errors back. This is the whole program that produced the output above. A write into the frozen directory is refused, a write elsewhere succeeds, a `Move` into the frozen directory is refused (on the memory driver it is native; on a backend without `Move` its fallback `copy` is refused for the same reason), and the loop writes until the budget refuses:

<!-- ds:block id=run-qazrcdep -->
**run** · [`examples/extending-interceptor/main.go:122-140`](../../examples/extending-interceptor/main.go#L122-L140)

```go
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
```
<!-- /ds:block hash=3d27d3 -->

Branch on the code, never the message: `filesystem.CodeOf(err) == filesystem.CodeReadOnly` for the freeze, `CodeQuotaExceeded` for the budget. `fshttp.StatusOf` maps them to 403 and 507 ([error codes](../reference/errors.md)), so an HTTP handler in front of this disk answers correctly without knowing the interceptors exist.

## Step 6 — Test the interceptor through a disk

Test an interceptor the way it runs: install it on a memory disk and call the Disk API, so the test exercises the `Op` the Disk really builds. Cover every way a key reaches the operation (the key itself, a batch's keys, a signed `PUT`), and check that what should pass still passes. The `signer` is a stub `URLSigner`, so the memory disk can sign URLs:

<!-- ds:block id=testfreezecoversbatchesandsigneduploads-3sahqzwj -->
**TestFreezeCoversBatchesAndSignedUploads** · [`examples/extending-interceptor/main_test.go:36-54`](../../examples/extending-interceptor/main_test.go#L36-L54)

```go
func TestFreezeCoversBatchesAndSignedUploads(t *testing.T) {
	ctx := t.Context()
	d, err := filesystem.NewDisk("d", memory.New(), filesystem.WithInterceptors(Freeze(frozenDir)), filesystem.WithURLSigner(signer{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteKeys(ctx, []string{"a.txt", "legal/b.txt"}); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("batch delete = %v", err)
	}
	if _, err := d.SignedURL(ctx, "legal/b.txt", filesystem.PresignOptions{Method: filesystem.MethodPut, Expires: time.Minute}); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("signed PUT = %v", err)
	}
	if _, err := d.SignedURL(ctx, "legal/b.txt", filesystem.PresignOptions{Method: filesystem.MethodGet, Expires: time.Minute}); err != nil {
		t.Errorf("signed GET = %v", err)
	}
	if err := d.DeletePrefix(ctx, "legal"); filesystem.CodeOf(err) != filesystem.CodeReadOnly {
		t.Errorf("delete prefix = %v", err)
	}
}
```
<!-- /ds:block hash=d0887f -->

Inject the clock rather than sleeping. `ReportSlow` takes `now` as a parameter, so a clock that advances one second per call makes every operation take exactly one second:

<!-- ds:block id=testreportslow-v2j3tvmf -->
**TestReportSlow** · [`examples/extending-interceptor/main_test.go:66-78`](../../examples/extending-interceptor/main_test.go#L66-L78)

```go
func TestReportSlow(t *testing.T) {
	clock := time.Unix(0, 0)
	now := func() time.Time { clock = clock.Add(time.Second); return clock }
	var got []SlowOp
	d, err := filesystem.NewDisk("d", memory.New(), filesystem.WithInterceptors(ReportSlow(slowThreshold, now, func(s SlowOp) { got = append(got, s) })))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.Stat(t.Context(), "k")
	if len(got) != 1 || got[0].Kind != filesystem.OpStat || got[0].Attempts != 1 || got[0].Took != time.Second {
		t.Errorf("reports = %+v", got)
	}
}
```
<!-- /ds:block hash=e36701 -->

## Try it

From the `examples` directory:

```sh
go run ./extending-interceptor
go test ./extending-interceptor
```

`go run` prints the four lines shown in step 4; `go test` runs the program and the two tests above and prints `ok`.

## Interceptors and fallbacks

When a capability is missing, the Disk runs a fallback as the operations it is made of, each through the chain. A `Move` on a disk without `Move` is a `copy` (with `Target`) then a `delete`; a `Copy` without `Copy` is a `stat`, a `read` and a `write`; `DeletePrefix` without `DeletePrefix` is a `list` and a `delete` per key; `DeleteKeys` without `DeleteKeys` is one `delete` per key. An interceptor keyed on `Mutating`, `Key` and `Target`, like `Freeze`, therefore holds on every backend; one keyed on a specific kind (only `move`) misses the fallback's pieces.

## Pitfalls

- **Wrapping the error loses nothing, replacing it does.** Return `call`'s error as it is, or a coded error of your own. An error without a code becomes `unknown` when the Disk classifies it, and `fshttp` answers 500. (`layer.Guard` turns uncoded errors into `permission`; a hand-written interceptor does not.)
- **Do not hold a lock across `call` for a read.** `call` returns when the stream is open; the bytes are read later by the caller. A read's duration and a limit slot cover opening only.
- **Do not cancel the context of a streaming call.** A timeout that cancels after `call` returns breaks the reader the caller still holds; `layer.Timeout` skips operations with `op.Streaming` for this reason.
- **Calling `call` again is a retry.** It is only safe when `op.Replayable`; a write whose body was a one-shot stream fails on the second attempt with `invalid_argument` (`write body is a stream and cannot be replayed`).
- **Check-then-charge is not atomic.** `Budget` can overshoot by one write per concurrent writer. A hard per-tenant limit belongs in the database transaction: [quota](../plugins/quota.md).
- **Validation errors never reach interceptors.** An invalid key or option is refused before the chain runs.

## Next

- [Logging and metrics](../integrations/logging-and-metrics.md): the built-in interceptors and their order.
- [Operations and events](../reference/operations.md): every kind, with its `Mutating` and `Replayable` flags.
- [Write a driver](write-a-driver.md): when the behaviour belongs to a backend instead.
