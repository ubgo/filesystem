# Interceptors: retries, timeouts, limits and policy around every operation

Retries, timeouts, concurrency limits, logging, read-only disks and policy checks apply to every operation whatever the backend. In this library they are interceptors: functions that wrap each operation rather than the driver, so adding one can never hide a capability. This program shows the order the chain runs in, `Retry` against a flaky backend (and the one-shot stream it refuses to replay), `Timeout` and how its deadline is classified, `Limit`, `ReadOnly`, `Guard`, `Log` and `Observe`, then `Bandwidth`, `Fault` and `Hedge`. It uses the core `filesystem` package with `memory` and `layer`. The guide [Interceptors](../../docs/concepts/interceptors.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-interceptors
go test ./concepts-interceptors
```

It needs no network, files or environment variables and finishes in milliseconds. The flaky and slow backends are memory drivers that fail or stall on purpose, and Retry's backoff timer is replaced so nothing sleeps.

## What it does

```sh
go run ./concepts-interceptors
```

```text
order:
  enter outer (stat a.txt)
  enter inner (stat a.txt)
  leave inner
  leave outer
retry:
  observed write a.txt: attempts=3 code=""
  observed stat a.txt: attempts=3 code=""
one-shot stream:
  observed write b.txt: attempts=1 code="unavailable"
  write: unavailable
timeout:
  observed stat a.txt: attempts=3 code="unavailable"
  deadline exceeded: true
limit: second caller unavailable
read-only write: read_only
read-only exists: <nil>
guard inside: <nil>
guard outside: permission
level=DEBUG msg="filesystem operation" disk=media op=write key=a.txt attempts=1 bytes=5
level=DEBUG msg="filesystem operation" disk=media op=stat key=missing.txt attempts=1 code=not_found error="filesystem: not_found: no object \"missing.txt\""
level=WARN msg="filesystem operation" disk=media op=set_visibility key=missing.txt attempts=1 code=not_found error="filesystem: not_found: no object \"missing.txt\""
bandwidth: 6000 bytes at 20000 B/s took at least 150ms: true
fault without retry: unavailable
fault inside retry: 20 writes stored in 43 attempts
hedge: read "answered" after 2 driver reads
```

1. The first interceptor given is the outermost one.
2. A backend that fails twice with `unavailable` succeeds on the third attempt; `Observe`, placed outside `Retry`, sees one operation with its attempt count.
3. A body that cannot be rewound is marked not replayable, so `Retry` returns the first failure instead of writing a truncated object.
4. `Timeout` bounds each attempt; the deadline classifies as `unavailable`, so it is retried, and the cause is still `context.DeadlineExceeded`.
5. `Limit` allows one operation at a time; a second caller whose context ends while waiting gets `unavailable`.
6. `ReadOnly` refuses mutations before the driver while reads still work; `Guard` allows only keys under one tenant's prefix and answers `permission` otherwise.
7. `Log` writes one record per operation: successes and not-found reads at Debug, failures at Warn (time and duration are dropped here so the output is stable).
8. `Bandwidth` caps the disk's byte rate: a 6000-byte write at 20000 B/s cannot finish in under 150 ms.
9. `Fault` outside `Retry` reaches the caller; inside it, with a fixed seed, all 20 writes land in 43 attempts on every run.
10. `Hedge` starts a second read when the first has not answered after 10 ms, and returns the one that answers.

## The code

An interceptor that prints entry and exit, and the chain order it reveals.

<!-- ds:block id=trace-c9p637eb -->
**trace** · [`examples/concepts-interceptors/main.go:85-92`](main.go#L85-L92)

```go
func trace(name string, out io.Writer) filesystem.Interceptor {
	return func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		fmt.Fprintf(out, "  enter %s (%s %s)\n", name, op.Kind, op.Key)
		err := call(ctx)
		fmt.Fprintf(out, "  leave %s\n", name)
		return err
	}
}
```
<!-- /ds:block hash=01540f -->

<!-- ds:block id=order-t2nkyn8y -->
**order** · [`examples/concepts-interceptors/main.go:96-105`](main.go#L96-L105)

```go
func order(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New(),
		filesystem.WithInterceptors(trace("outer", out), trace("inner", out)))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "order:")
	_, err = disk.Exists(ctx, "a.txt")
	return err
}
```
<!-- /ds:block hash=0fa4de -->

The flaky backend, and Retry with Observe outside it.

<!-- ds:block id=flaky-d97z36uh -->
**flaky** · [`examples/concepts-interceptors/main.go:111-114`](main.go#L111-L114)

```go
type flaky struct {
	*memory.Driver
	stats, writes int
}
```
<!-- /ds:block hash=fde6d6 -->

<!-- ds:block id=retry-vgfqxqfc -->
**retry** · [`examples/concepts-interceptors/main.go:145-159`](main.go#L145-L159)

```go
func retry(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", &flaky{Driver: memory.New()}, filesystem.WithInterceptors(
		layer.Observe(report(out)),
		layer.Retry(layer.RetryOptions{Sleep: noWait}),
	))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "retry:")
	if _, err := disk.Write(ctx, "a.txt", strings.NewReader("seekable"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, err = disk.Stat(ctx, "a.txt")
	return err
}
```
<!-- /ds:block hash=cf80e1 -->

The one-shot stream Retry will not replay.

<!-- ds:block id=oneshotstream-heknvesg -->
**oneShotStream** · [`examples/concepts-interceptors/main.go:165-178`](main.go#L165-L178)

```go
func oneShotStream(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", &flaky{Driver: memory.New()}, filesystem.WithInterceptors(
		layer.Observe(report(out)),
		layer.Retry(layer.RetryOptions{Sleep: noWait}),
	))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "one-shot stream:")
	stream := io.MultiReader(strings.NewReader("streamed once"))
	_, err = disk.Write(ctx, "b.txt", stream, filesystem.WriteOptions{})
	fmt.Fprintf(out, "  write: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=04439d -->

A backend that stops answering, a per-attempt timeout, and a concurrency limit.

<!-- ds:block id=slow-3dqtn4yr -->
**slow** · [`examples/concepts-interceptors/main.go:183-186`](main.go#L183-L186)

```go
type slow struct {
	*memory.Driver
	release chan struct{}
}
```
<!-- /ds:block hash=af5362 -->

<!-- ds:block id=timeout-e4rdgcdr -->
**timeout** · [`examples/concepts-interceptors/main.go:202-216`](main.go#L202-L216)

```go
func timeout(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", &slow{Driver: memory.New(), release: make(chan struct{})},
		filesystem.WithInterceptors(
			layer.Observe(report(out)),
			layer.Retry(layer.RetryOptions{Sleep: noWait}),
			layer.Timeout(attemptBudget),
		))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "timeout:")
	_, err = disk.Stat(ctx, "a.txt")
	fmt.Fprintf(out, "  deadline exceeded: %t\n", errors.Is(err, context.DeadlineExceeded))
	return nil
}
```
<!-- /ds:block hash=b7d991 -->

<!-- ds:block id=limit-4vspbm26 -->
**limit** · [`examples/concepts-interceptors/main.go:221-243`](main.go#L221-L243)

```go
func limit(ctx context.Context, out io.Writer) error {
	drv := &slow{Driver: memory.New(), release: make(chan struct{})}
	disk, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(layer.Limit(1)))
	if err != nil {
		return err
	}
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		_, _ = disk.Stat(ctx, "a.txt")
	}()
	<-started
	time.Sleep(waitForSlot) // let the first Stat take the slot
	short, cancel := context.WithTimeout(ctx, waitForSlot)
	defer cancel()
	_, err = disk.Stat(short, "b.txt")
	fmt.Fprintf(out, "limit: second caller %s\n", filesystem.CodeOf(err))
	close(drv.release)
	<-done
	return nil
}
```
<!-- /ds:block hash=36e09c -->

Policy: a read-only disk and a tenant guard.

<!-- ds:block id=readonly-vmd3t6en -->
**readOnly** · [`examples/concepts-interceptors/main.go:248-258`](main.go#L248-L258)

```go
func readOnly(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("archive", memory.New(), filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "read-only write: %s\n", filesystem.CodeOf(err))
	_, err = disk.Exists(ctx, "a.txt")
	fmt.Fprintf(out, "read-only exists: %v\n", err)
	return nil
}
```
<!-- /ds:block hash=5147db -->

<!-- ds:block id=guard-ru54uzvj -->
**guard** · [`examples/concepts-interceptors/main.go:263-279`](main.go#L263-L279)

```go
func guard(ctx context.Context, out io.Writer) error {
	onlyAcme := func(_ context.Context, op *filesystem.Op) error {
		if !strings.HasPrefix(op.Key, tenantPrefix) {
			return fmt.Errorf("key %q is outside %s", op.Key, tenantPrefix)
		}
		return nil
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(layer.Guard(onlyAcme)))
	if err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, tenantPrefix+"logo.png", []byte("png"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "guard inside: %v\n", err)
	_, err = disk.WriteBytes(ctx, "tenants/globex/logo.png", []byte("png"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "guard outside: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=19adb5 -->

One structured log record per operation.

<!-- ds:block id=logops-rk8jecbk -->
**logOps** · [`examples/concepts-interceptors/main.go:293-305`](main.go#L293-L305)

```go
func logOps(ctx context.Context, out io.Writer) error {
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: dropVolatile}))
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(layer.Log(logger)))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("hello"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	_, _ = disk.Exists(ctx, "missing.txt")
	_ = disk.SetVisibility(ctx, "missing.txt", filesystem.VisibilityPublic)
	return nil
}
```
<!-- /ds:block hash=fd2e38 -->

A byte-rate cap for the whole disk.

<!-- ds:block id=bandwidth-gx8pkgzr -->
**bandwidth** · [`examples/concepts-interceptors/main.go:311-323`](main.go#L311-L323)

```go
func bandwidth(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(layer.Bandwidth(bandwidthCap)))
	if err != nil {
		return err
	}
	start := time.Now()
	if _, err := disk.Write(ctx, "big.bin", strings.NewReader(strings.Repeat("x", bandwidthBody)), filesystem.WriteOptions{}); err != nil {
		return err
	}
	fmt.Fprintf(out, "bandwidth: %d bytes at %d B/s took at least %s: %t\n",
		bandwidthBody, bandwidthCap, bandwidthFloor, time.Since(start) >= bandwidthFloor)
	return nil
}
```
<!-- /ds:block hash=58e45b -->

Injected failures, outside and inside `Retry`.

<!-- ds:block id=faults-edeunkmc -->
**faults** · [`examples/concepts-interceptors/main.go:330-358`](main.go#L330-L358)

```go
func faults(ctx context.Context, out io.Writer) error {
	always := layer.Fault(layer.FaultOptions{Rate: 1, Seed: faultSeed})
	bare, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(always))
	if err != nil {
		return err
	}
	_, err = bare.WriteBytes(ctx, "a.txt", []byte("x"), filesystem.WriteOptions{})
	fmt.Fprintf(out, "fault without retry: %s\n", filesystem.CodeOf(err))
	attempts := 0
	count := func(ctx context.Context, op *filesystem.Op, call filesystem.Call) error {
		attempts++
		return call(ctx)
	}
	disk, err := filesystem.NewDisk("media", memory.New(), filesystem.WithInterceptors(
		layer.Retry(layer.RetryOptions{Attempts: 10, Sleep: noWait}),
		count,
		layer.Fault(layer.FaultOptions{Rate: faultRate, Seed: faultSeed, Ops: []filesystem.OpKind{filesystem.OpWrite}}),
	))
	if err != nil {
		return err
	}
	for i := range faultWrites {
		if _, err := disk.WriteBytes(ctx, fmt.Sprintf("f%d.txt", i), []byte("x"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "fault inside retry: %d writes stored in %d attempts\n", faultWrites, attempts)
	return nil
}
```
<!-- /ds:block hash=784395 -->

A driver that loses its first read, and `Hedge` answering anyway.

<!-- ds:block id=stuck-95ayc2cx -->
**stuck** · [`examples/concepts-interceptors/main.go:364-367`](main.go#L364-L367)

```go
type stuck struct {
	*memory.Driver
	reads atomic.Int32
}
```
<!-- /ds:block hash=57e5ad -->

<!-- ds:block id=hedge-v7agwz6w -->
**hedge** · [`examples/concepts-interceptors/main.go:381-396`](main.go#L381-L396)

```go
func hedge(ctx context.Context, out io.Writer) error {
	drv := &stuck{Driver: memory.New()}
	disk, err := filesystem.NewDisk("media", drv, filesystem.WithInterceptors(layer.Hedge(hedgeAfter)))
	if err != nil {
		return err
	}
	if _, err := disk.WriteBytes(ctx, "a.txt", []byte("answered"), filesystem.WriteOptions{}); err != nil {
		return err
	}
	b, err := disk.ReadAll(ctx, "a.txt")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "hedge: read %q after %d driver reads\n", b, drv.reads.Load())
	return nil
}
```
<!-- /ds:block hash=9775fe -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: order, Retry, the one-shot stream, Timeout, Limit, ReadOnly, Guard, Log |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Interceptors](../../docs/concepts/interceptors.md), and the [layer package reference](../../layer/README.md).
- [Write an interceptor](../../docs/extending/write-an-interceptor.md) and its example [extending-interceptor](../extending-interceptor).
- Metrics and tracing: [integrations-logging](../integrations-logging) and [integrations-otel](../integrations-otel).
- [Errors](../../docs/concepts/errors.md): the codes Retry and Log look at; example [concepts-errors](../concepts-errors).
