# Move a local store to S3: a resumable, verified copy tool

A command-line tool that moves an existing uploads directory to S3 or any S3-compatible bucket (R2, B2, Wasabi, Spaces, SeaweedFS, Garage) without losing a file, for the day a product outgrows writing uploads to the server's disk. It walks the source with `Disk.List`, copies each object with `filesystem.CopyAcross` through a bounded pool of workers, skips keys the bucket already holds at the same size (so an interrupted run is finished by running it again), verifies every object by SHA-256 on both sides, and opens the source behind the `filesystem.ReadOnly()` interceptor so a mistake cannot change it. Real disks are opened from DSNs with `filesystem.Mux` and the `local` and `adapters/s3` openers. Without DSNs it runs a demo against an in-process fake S3 (gofakes3). The guide is [Move a local store to S3](../../docs/recipes/move-to-s3.md).

## Run it

```sh
cd examples
go run ./recipe-move-to-s3
```

The demo seeds six files in a temporary directory, copies them to an in-process fake S3, and removes the directory when it exits. Nothing listens and nothing is left behind.

With `MOVE_SOURCE` and `MOVE_TARGET` set, it moves real disks. S3 credentials come from the AWS default chain (`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, shared config, instance roles), never from the DSN:

```sh
MOVE_SOURCE='file:///var/app/uploads' \
MOVE_TARGET='s3://media?region=auto&endpoint=https://<account>.r2.cloudflarestorage.com' \
go run ./recipe-move-to-s3 -workers 16
```

Flags: `-workers` (concurrent copies, default 8), `-dry-run` (list what would be copied, copy nothing) and `-fake-s3 <addr>` (serve an empty fake S3 with bucket `media` that accepts any credentials, until Ctrl-C). Setting only one of the two variables is an error. A run with failed objects exits 1; run it again to retry them.

Run the tests with `go test ./recipe-move-to-s3`.

## What it does

1. The demo: a first run copies everything and verifies it, a second run skips everything, and after a new file appears a dry run lists only that file. The PDF has a content type on the bucket although the local disk stored none: the target detects it from the bytes, as an upload would.

```sh
go run ./recipe-move-to-s3
```

```text
first run:
copied 6, skipped 0, failed 0, 115 bytes in 4ms
verified: 0 objects differ
on the bucket: invoices/2025/0001.pdf application/pdf 23 bytes
second run (resumes, nothing to do):
copied 0, skipped 6, failed 0, 0 bytes in 1ms
verified: 0 objects differ
after a new upload, dry run:
would copy avatars/u3.png (22 bytes)
to copy 1, skipped 6, failed 0, 0 bytes in 1ms
```

2. Against a bucket by DSN. Start the fake S3 as a server in one terminal:

```sh
go run ./recipe-move-to-s3 -fake-s3 localhost:8116
```

```text
fake S3 on http://localhost:8116, bucket "media" (any credentials)
```

In another, with an `uploads` directory holding `invoices/0001.pdf` and `avatars/readme.txt`, do a dry run, a real run, and a run after one more file arrives:

```sh
export AWS_ACCESS_KEY_ID=demo AWS_SECRET_ACCESS_KEY=demo
T='s3://media?region=us-east-1&path_style=true&endpoint=http://localhost:8116'
MOVE_SOURCE="file://$PWD/uploads" MOVE_TARGET="$T" go run ./recipe-move-to-s3 -dry-run -workers 4
MOVE_SOURCE="file://$PWD/uploads" MOVE_TARGET="$T" go run ./recipe-move-to-s3 -workers 4
printf 'new' > uploads/avatars/new.txt
MOVE_SOURCE="file://$PWD/uploads" MOVE_TARGET="$T" go run ./recipe-move-to-s3 -workers 4
```

```text
would copy invoices/0001.pdf (23 bytes)
would copy avatars/readme.txt (16 bytes)
to copy 2, skipped 0, failed 0, 0 bytes in 3ms
copied 2, skipped 0, failed 0, 39 bytes in 4ms
verified: 0 objects differ
copied 1, skipped 2, failed 0, 3 bytes in 4ms
verified: 0 objects differ
```

The cut-over (copy until a run copies little, pause writes, copy once more and verify, change the DSN keeping the disk name `media`) is in the guide's "Switch" step. Keeping the disk name means no `fs_files` row changes.

Limits the guide states: the skip check compares sizes only, so a same-size object that differs is caught by the verification pass, not the copy; deletions on the source after a copy are not propagated; empty directories are not copied.

## The code

`Options` and `Report` define a run: a failure on one object is recorded and the run continues, a failure to list stops it.

<!-- ds:block id=options-rgyqvp8b -->
**Options** · [`examples/recipe-move-to-s3/main.go:68-73`](main.go#L68-L73)

```go
type Options struct {
	// Workers is the number of concurrent copies; values below 1 mean 1.
	Workers int
	// DryRun lists what would be copied and copies nothing.
	DryRun bool
}
```
<!-- /ds:block hash=a76408 -->

<!-- ds:block id=report-4jzszwze -->
**Report** · [`examples/recipe-move-to-s3/main.go:78-83`](main.go#L78-L83)

```go
type Report struct {
	Copied  int64
	Skipped int64
	Bytes   int64
	Failed  map[string]error
}
```
<!-- /ds:block hash=d83e91 -->

`needsCopy` is the skip check: a missing key or a different size means copy. Size is the one attribute every backend reports on `Stat`; ETags are not comparable across backends.

<!-- ds:block id=needscopy-9f8fz5c9 -->
**needsCopy** · [`examples/recipe-move-to-s3/main.go:89-98`](main.go#L89-L98)

```go
func needsCopy(ctx context.Context, dst *filesystem.Disk, e filesystem.Entry) (bool, error) {
	got, err := dst.Stat(ctx, e.Key)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return true, nil
	case err != nil:
		return false, err
	}
	return got.Size != e.Size, nil
}
```
<!-- /ds:block hash=63b939 -->

`copyOne` copies one object with `filesystem.CopyAcross`, which streams the bytes and keeps the content type, and metadata and visibility where the target stores them.

<!-- ds:block id=copyone-75r2dq2z -->
**copyOne** · [`examples/recipe-move-to-s3/main.go:105-124`](main.go#L105-L124)

```go
func copyOne(ctx context.Context, src, dst *filesystem.Disk, e filesystem.Entry, o Options, r *Report) error {
	need, err := needsCopy(ctx, dst, e)
	if err != nil || !need {
		if err == nil {
			atomic.AddInt64(&r.Skipped, 1)
		}
		return err
	}
	if o.DryRun {
		fmt.Printf("would copy %s (%d bytes)\n", e.Key, e.Size)
		atomic.AddInt64(&r.Copied, 1)
		return nil
	}
	if _, err := filesystem.CopyAcross(ctx, src, e.Key, dst, e.Key); err != nil {
		return err
	}
	atomic.AddInt64(&r.Copied, 1)
	atomic.AddInt64(&r.Bytes, e.Size)
	return nil
}
```
<!-- /ds:block hash=e56201 -->

`Copy` lists the source recursively in the calling goroutine and feeds a fixed pool of workers, so listing and copying overlap and memory stays bounded.

<!-- ds:block id=copy-fx35x5fx -->
**Copy** · [`examples/recipe-move-to-s3/main.go:130-157`](main.go#L130-L157)

```go
func Copy(ctx context.Context, src, dst *filesystem.Disk, o Options) (Report, error) {
	r := Report{Failed: map[string]error{}}
	var mu sync.Mutex
	jobs := make(chan filesystem.Entry)
	var wg sync.WaitGroup
	for range max(o.Workers, 1) {
		wg.Go(func() {
			for e := range jobs {
				if err := copyOne(ctx, src, dst, e, o, &r); err != nil {
					mu.Lock()
					r.Failed[e.Key] = err
					mu.Unlock()
				}
			}
		})
	}
	var listErr error
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			listErr = err
			break
		}
		jobs <- e
	}
	close(jobs)
	wg.Wait()
	return r, listErr
}
```
<!-- /ds:block hash=7b58a6 -->

`digest` and `Verify` compare every object by SHA-256 on both sides. They read every byte twice, so run them once before the switch.

<!-- ds:block id=digest-kqeqrzyz -->
**digest** · [`examples/recipe-move-to-s3/main.go:161-172`](main.go#L161-L172)

```go
func digest(ctx context.Context, d *filesystem.Disk, key string) (string, error) {
	rc, err := d.Read(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```
<!-- /ds:block hash=d6529b -->

<!-- ds:block id=verify-u9yjwc9b -->
**Verify** · [`examples/recipe-move-to-s3/main.go:178-194`](main.go#L178-L194)

```go
func Verify(ctx context.Context, src, dst *filesystem.Disk) ([]string, error) {
	var bad []string
	for e, err := range src.List(ctx, "", filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return bad, err
		}
		want, err := digest(ctx, src, e.Key)
		if err != nil {
			return bad, err
		}
		got, err := digest(ctx, dst, e.Key)
		if err != nil || got != want {
			bad = append(bad, e.Key)
		}
	}
	return bad, nil
}
```
<!-- /ds:block hash=802560 -->

`openFromEnv` opens both disks from DSNs through a `filesystem.Mux`, with the source behind `filesystem.ReadOnly()` and both disks named `media`.

<!-- ds:block id=openfromenv-ucj3efgh -->
**openFromEnv** · [`examples/recipe-move-to-s3/main.go:265-284`](main.go#L265-L284)

```go
func openFromEnv(ctx context.Context) (src, dst *filesystem.Disk, ok bool, err error) {
	srcDSN, dstDSN := os.Getenv(envSource), os.Getenv(envTarget)
	if srcDSN == "" && dstDSN == "" {
		return nil, nil, false, nil
	}
	if srcDSN == "" || dstDSN == "" {
		return nil, nil, true, fmt.Errorf("set both %s and %s", envSource, envTarget)
	}
	mux := filesystem.NewMux()
	if err := errors.Join(mux.Register(local.Scheme, local.Opener), mux.Register(fss3.Scheme, fss3.Opener)); err != nil {
		return nil, nil, true, err
	}
	if src, err = mux.Open(ctx, diskName, srcDSN, filesystem.WithInterceptors(filesystem.ReadOnly())); err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", envSource, err)
	}
	if dst, err = mux.Open(ctx, diskName, dstDSN); err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", envTarget, err)
	}
	return src, dst, true, nil
}
```
<!-- /ds:block hash=a99ac9 -->

`run` copies, prints the report, and verifies only when the copy fully succeeded.

<!-- ds:block id=run-eqa8vn2g -->
**run** · [`examples/recipe-move-to-s3/main.go:288-311`](main.go#L288-L311)

```go
func run(ctx context.Context, src, dst *filesystem.Disk, o Options) error {
	start := time.Now()
	r, err := Copy(ctx, src, dst, o)
	verb := "copied"
	if o.DryRun {
		verb = "to copy"
	}
	fmt.Printf("%s %d, skipped %d, failed %d, %d bytes in %s\n", verb, r.Copied, r.Skipped, len(r.Failed), r.Bytes, time.Since(start).Round(time.Millisecond))
	for key, ferr := range r.Failed {
		fmt.Printf("  failed %s: %v\n", key, ferr)
	}
	if err != nil || len(r.Failed) > 0 || o.DryRun {
		return errors.Join(err, failedErr(len(r.Failed)))
	}
	bad, err := Verify(ctx, src, dst)
	if err != nil {
		return err
	}
	fmt.Printf("verified: %d objects differ\n", len(bad))
	if len(bad) > 0 {
		return fmt.Errorf("objects differ: %v", bad)
	}
	return nil
}
```
<!-- /ds:block hash=944cd0 -->

`serveFakeS3` is the `-fake-s3` mode: an empty in-memory S3 server to try the tool by DSN offline.

<!-- ds:block id=servefakes3-7btu4b8k -->
**serveFakeS3** · [`examples/recipe-move-to-s3/main.go:362-370`](main.go#L362-L370)

```go
func serveFakeS3(ctx context.Context, addr string) error {
	backend := s3mem.New()
	if err := backend.CreateBucket(demoBucket); err != nil {
		return err
	}
	fmt.Printf("fake S3 on http://%s, bucket %q (any credentials)\n", addr, demoBucket)
	srv := &http.Server{Addr: addr, Handler: gofakes3.New(backend).Server(), ReadHeaderTimeout: readHeaderTimeout}
	return serve.UntilDone(ctx, srv)
}
```
<!-- /ds:block hash=b54323 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The copy tool (skip check, worker pool, verification), DSN opening, the demo and the `-fake-s3` server. |
| `main_test.go` | A copy that resumes and verifies, failed objects retried by a second run, and the demo and environment modes. |

## Next

- The guide: [Move a local store to S3](../../docs/recipes/move-to-s3.md), including the cut-over steps and what to do when the disk name has to change.
- [Backups and mirrors](../../docs/recipes/backups-and-mirrors.md) and the [recipe-backups-and-mirrors](../recipe-backups-and-mirrors) example: `filesystem.Mirror` for a migration without a write pause.
- [S3](../../docs/adapters/s3.md) and [S3-compatible stores](../../docs/adapters/s3-compatible.md) for DSNs and provider quirks; [Configuration](../../docs/concepts/configuration.md) for `Mux` and DSNs.
- [The `fs` command](../../docs/integrations/cli.md) to copy or inspect single objects by hand.
