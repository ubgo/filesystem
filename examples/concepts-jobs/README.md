# Maintenance jobs by hand: promote, preview, your own jobs and refusals

The `jobs` module names every piece of background maintenance as a `Job` that any runner can call. This program runs jobs by hand, the way a test or an admin button does: it accepts an upload on a fast disk and promotes it to the durable store later (`jobs.Promote`), previews a sweep with its dry run before letting it delete, wraps a function of its own as a job and renames a built-in one, and shows a misconfigured job and a misconfigured scheduler refusing to run. It uses the core `filesystem` package, `memory`, `upload` and `jobs`. The guide [Maintenance jobs](../../docs/concepts/maintenance-jobs.md) explains each step; the scheduler itself runs in [jobs-scheduler](../jobs-scheduler).

## Run it

```sh
cd examples
go run ./concepts-jobs
go test ./concepts-jobs
```

Everything runs on memory disks: no database, no network, no environment variables. The jobs run at a time two hours ahead, so the uploads they act on are past the one-hour minimum age.

## What it does

```sh
go run ./concepts-jobs
```

```text
before promote: read from staging; filesystem.promote moved 1; after: read from store
dry run: would delete 2: incoming/a.png incoming/b.png
sweep: deleted 2, files/kept.png still there: true
trim-media-cache: 1 trimmed; a second sweep named sweep-exports
sweep with minAge 0: invalid_argument
two schedules with one name: invalid_argument
```

1. An upload stored under `pending/` on the staging disk is read from there until `jobs.Promote` moves it, then from the store.
2. The upload sweep's dry run names the two unconfirmed uploads and deletes nothing; the real run deletes them and leaves `files/` alone.
3. `jobs.Func` makes a cache trim a job; `Named` gives a second upload sweep a name of its own.
4. A zero minimum age fails the run with `invalid_argument`, and a scheduler given two schedules with one name refuses to start.

## The code

Reading a file that may not have been promoted yet.

<!-- ds:block id=readeither-zfzw99jw -->
**readEither** · [`examples/concepts-jobs/main.go:68-75`](main.go#L68-L75)

```go
func readEither(ctx context.Context, store, staging *filesystem.Disk, key string) ([]byte, string, error) {
	b, err := store.ReadAll(ctx, key)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		b, err = staging.ReadAll(ctx, filesystem.JoinKey(pendingDir, key))
		return b, staging.Name(), err
	}
	return b, store.Name(), err
}
```
<!-- /ds:block hash=e6febe -->

Accept fast, store later.

<!-- ds:block id=acceptfaststorelater-ezzbbkyu -->
**acceptFastStoreLater** · [`examples/concepts-jobs/main.go:81-111`](main.go#L81-L111)

```go
func acceptFastStoreLater(ctx context.Context, out io.Writer) error {
	staging, err := mem("staging")
	if err != nil {
		return err
	}
	store, err := mem("store")
	if err != nil {
		return err
	}
	p := upload.Policy{MaxSize: 1 << 20, Allowed: []string{"image/*"}, Key: upload.RandomKey(pendingDir)}
	res, err := upload.Receive(ctx, staging, strings.NewReader(pngFile), "photo.png", p)
	if err != nil {
		return err
	}
	key := strings.TrimPrefix(res.Key, pendingDir+"/")
	_, before, err := readEither(ctx, store, staging, key)
	if err != nil {
		return err
	}
	promote := jobs.Promote(staging, pendingDir, store, minAge)
	r, err := promote.Run(ctx, later)
	if err != nil {
		return err
	}
	_, after, err := readEither(ctx, store, staging, key)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "before promote: read from %s; %s moved %d; after: read from %s\n", before, promote.Name, r.Count, after)
	return nil
}
```
<!-- /ds:block hash=b08f76 -->

A dry run, then the sweep.

<!-- ds:block id=previewthensweep-n9pdx253 -->
**previewThenSweep** · [`examples/concepts-jobs/main.go:116-142`](main.go#L116-L142)

```go
func previewThenSweep(ctx context.Context, out io.Writer) error {
	disk, err := mem("uploads")
	if err != nil {
		return err
	}
	for _, k := range []string{"incoming/a.png", "incoming/b.png", "files/kept.png"} {
		if _, err := disk.WriteBytes(ctx, k, []byte(pngFile), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	sweep := jobs.UploadSweep(disk, incomingDir, minAge)
	preview, err := sweep.DryRun(ctx, later)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dry run: would delete %d: %s\n", preview.Count, strings.Join(preview.Planned, " "))
	done, err := sweep.Run(ctx, later)
	if err != nil {
		return err
	}
	left, err := disk.Exists(ctx, "files/kept.png")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sweep: deleted %d, files/kept.png still there: %t\n", done.Count, left)
	return nil
}
```
<!-- /ds:block hash=f0284b -->

A job of your own, and a renamed one.

<!-- ds:block id=customjob-eyspfne3 -->
**customJob** · [`examples/concepts-jobs/main.go:148-169`](main.go#L148-L169)

```go
func customJob(ctx context.Context, out io.Writer) error {
	cache, err := mem("cache")
	if err != nil {
		return err
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, err := cache.WriteBytes(ctx, k, []byte("12345"), filesystem.WriteOptions{}); err != nil {
			return err
		}
	}
	trim := jobs.Func("trim-media-cache", func(ctx context.Context, _ time.Time) (jobs.Result, error) {
		n, err := filesystem.TrimCache(ctx, cache, 10)
		return jobs.Result{Count: int64(n)}, err
	})
	r, err := trim.Run(ctx, later)
	if err != nil {
		return err
	}
	exports := jobs.UploadSweep(cache, "exports", minAge).Named("sweep-exports")
	fmt.Fprintf(out, "%s: %d trimmed; a second sweep named %s\n", trim.Name, r.Count, exports.Name)
	return nil
}
```
<!-- /ds:block hash=57aad1 -->

The refusals.

<!-- ds:block id=refusedconfig-wbkq5hk3 -->
**refusedConfig** · [`examples/concepts-jobs/main.go:175-187`](main.go#L175-L187)

```go
func refusedConfig(ctx context.Context, out io.Writer) error {
	disk, err := mem("uploads")
	if err != nil {
		return err
	}
	_, err = jobs.UploadSweep(disk, incomingDir, 0).Run(ctx, later)
	fmt.Fprintf(out, "sweep with minAge 0: %s\n", filesystem.CodeOf(err))
	s := jobs.NewScheduler(jobs.SchedulerOptions{})
	sweep := jobs.UploadSweep(disk, incomingDir, minAge)
	err = s.Run(ctx, jobs.Schedule{Job: sweep, Every: time.Hour}, jobs.Schedule{Job: sweep, Every: time.Hour})
	fmt.Fprintf(out, "two schedules with one name: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=bac78e -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: one function per step |
| `main_test.go` | Runs the program and checks the lines the guide quotes |

## Next

- The guide: [Maintenance jobs](../../docs/concepts/maintenance-jobs.md).
- The scheduler with the record sweeps: [jobs-scheduler](../jobs-scheduler).
- Scrubbing, migration, sync and exports: [maintenance](../maintenance).
