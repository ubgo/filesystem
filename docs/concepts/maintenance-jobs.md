# Maintenance jobs

The library starts no background work. Trash retention, the sweep of uploads nobody confirmed, of abandoned multipart uploads and of orphaned objects, audit and share-link retention, and quota recounts all wait for something to call them. The `github.com/ubgo/filesystem/jobs` module names each of them as a `Job` and ships a small in-process scheduler to run them; the same jobs run under River or Asynq when an application already has a queue. This page covers the jobs, the scheduler, and how to run them elsewhere.

The snippets come from `examples/jobs-scheduler`, which schedules the sweeps against a records store, and `examples/concepts-jobs`, which runs jobs by hand: promotion, a dry run, a job of your own and the refusals. Run them with `go run ./jobs-scheduler` and `go run ./concepts-jobs` from the `examples` directory.

## Before you start

- Add `github.com/ubgo/filesystem/jobs` to your `go.mod`. It is its own module because the record sweeps need the `records` module; the core never starts background work.
- Build the disks and the `records.Store` the jobs work on, as the rest of the application does. A job holds them; it opens nothing itself.
- Decide where jobs run: the in-process scheduler (step 2) for one binary, River or Asynq (step 5) when a queue already exists.

<a id="the-jobs"></a>

## Step 1 — Pick the jobs

A `Job` is a name and a function `Run(ctx, now) (Result, error)`. It gets "now" from its runner, computes its cutoffs from it, and reports how many items it processed in `Result.Count`. Every job here finishes what an earlier, interrupted run left, so running it again after a failure or a crash is always safe.

| Constructor | Default name | What one run does |
|---|---|---|
| `jobs.SweepTrash(store, retention)` | `filesystem.sweep_trash` | purges files trashed more than `retention` ago (`records.Store.SweepTrash`) |
| `jobs.SweepExpired(store)` | `filesystem.sweep_expired` | purges temporary and viewed view-once files whose expiry has passed (`records.Store.SweepExpired`) |
| `jobs.SweepRetention(store)` | `filesystem.sweep_retention` | applies `records.WithRetention` rules: detaches attachments past their age, purges files left with none (`records.Store.SweepRetention`; dry-run it by hand first) |
| `jobs.SweepOrphans(store, disk, prefix, minAge)` | `filesystem.sweep_orphans` | deletes objects under `prefix` that no file or plugin owns, older than `minAge` |
| `jobs.UploadSweep(disk, dir, minAge)` | `filesystem.upload_sweep` | deletes uploads under `dir` nobody confirmed or attached (`upload.Sweep`) |
| `jobs.SweepAbandoned(disk, minAge)` | `filesystem.sweep_abandoned` | removes multipart uploads and staging files idle for `minAge`; its dry run lists them (`Disk.FindAbandoned`, as `jobs.AbandonedItem` names them) on a disk with the `FindAbandoned` capability |
| `jobs.AuditPrune(plugin, store, retention)` | `filesystem.audit_prune` | deletes audit entries older than `retention` |
| `jobs.SharePrune(plugin, store, retention)` | `filesystem.share_prune` | deletes share links expired or revoked more than `retention` ago |
| `jobs.SweepLocks(plugin, store)` | `filesystem.sweep_locks` | deletes check-out locks (`records/locks`) expired at the run's time |
| `jobs.QuotaRecount(plugin, store, scopes)` | `filesystem.quota_recount` | rebuilds the usage of every scope `scopes(ctx)` returns; a failing scope is named and does not stop the others |
| `jobs.VersionsTrim(plugin, store, scopes)` | `filesystem.versions_trim` | applies each file's history policy (`versions.WithKeep` or `WithKeepFunc`: count and `MaxAge`) in every scope `scopes(ctx)` returns (`versions.Plugin.TrimScope`, paged, one transaction per file), so old versions go for files nobody replaces; `Result.Count` is the versions dropped, and a failing scope is named and does not stop the others |
| `jobs.Func(name, run)` | the name given | any function: the hook point for work defined elsewhere, such as generating image variants eagerly |

A retention or minimum age that is zero or negative fails every run with `invalid_argument`: "keep nothing" would purge everything the moment the job first ran, which is never what a misconfigured zero means. `job.Named("…")` gives a job another name, for two jobs of one kind (two disks to sweep).

<a id="the-scheduler"></a>

## Step 2 — Schedule them

A deployment's maintenance is a list of `Schedule`s: a job, its interval, a jitter that spreads several instances of the application apart, and whether to run once at start.

<!-- ds:block id=schedules-3acmgv45 -->
**schedules** · [`examples/jobs-scheduler/main.go:55-63`](../../examples/jobs-scheduler/main.go#L55-L63)

```go
func schedules(store *records.Store, disk *filesystem.Disk, auditLog *audit.Plugin) []jobs.Schedule {
	return []jobs.Schedule{
		{Job: jobs.SweepTrash(store, trashRetention), Every: time.Hour, Jitter: 5 * time.Minute, RunAtStart: true},
		{Job: jobs.UploadSweep(disk, "incoming", uploadMaxAge), Every: time.Hour, Jitter: 5 * time.Minute, RunAtStart: true},
		{Job: jobs.SweepAbandoned(disk, uploadMaxAge), Every: 6 * time.Hour, Jitter: 10 * time.Minute, RunAtStart: true},
		{Job: jobs.SweepOrphans(store, diskName, "files", uploadMaxAge), Every: 24 * time.Hour, Jitter: time.Hour, RunAtStart: true},
		{Job: jobs.AuditPrune(auditLog, store, auditRetention), Every: 24 * time.Hour, Jitter: time.Hour, RunAtStart: true},
	}
}
```
<!-- /ds:block hash=098e71 -->

`jobs.NewScheduler(SchedulerOptions{OnRun, Now, After})` returns a scheduler and `Run(ctx, schedules...)` runs them until the context ends, then waits for runs in progress (which see the context cancelled) and returns. Every run, failed or not, is reported to `OnRun` with the job's name, start, duration, result and error: log it, and count failures for an alert.

<!-- ds:block id=startscheduler-mcxjtq47 -->
**startScheduler** · [`examples/jobs-scheduler/main.go:68-73`](../../examples/jobs-scheduler/main.go#L68-L73)

```go
func startScheduler(ctx context.Context, all []jobs.Schedule, now func() time.Time, report func(jobs.Report)) <-chan error {
	s := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: report, Now: now})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, all...) }()
	return done
}
```
<!-- /ds:block hash=ca7173 -->

The demo writes one leftover of each kind on day 0 and starts the scheduler on day 60, so every period but the audit's 90 days has passed:

```text
filesystem.audit_prune: 0, err <nil>
filesystem.sweep_abandoned: 1, err <nil>
filesystem.sweep_orphans: 1, err <nil>
filesystem.sweep_trash: 1, err <nil>
filesystem.upload_sweep: 1, err <nil>
```

The rules the scheduler keeps:

- **Validated before anything starts.** An empty or repeated job name, a missing `Run`, a non-positive interval or a negative jitter is `invalid_argument` and nothing runs.
- **Single-flight per job.** Two runs of one job never overlap in one scheduler: a run that falls due while the previous one is still going is skipped and reported with `jobs.ErrRunning`. Across several processes, use a runner with uniqueness (River, Asynq), or accept that two instances may sweep at once: every job here tolerates it.
- **A panic is a report, not a crash.** A job that panics is reported with code `unknown`, and the scheduler runs it again at its next interval.
- **No hidden state.** Nothing is persisted; a restart starts every wait afresh, and `RunAtStart` runs a job at once instead of after its first interval.
- **`RunNow(ctx, name)`** runs a scheduled job immediately, in the caller's goroutine, for an admin button or a test.
- **`Now` and `After`** replace the clock and the timer, so tests never wait.

## Step 3 — Preview before a job deletes anything

The sweeps that delete (`SweepTrash`, `SweepExpired`, `SweepOrphans`, `UploadSweep`, `SweepAbandoned`) have a dry run: `job.DryRun(ctx, now)` returns what `Run` would act on now, `Result.Count` and the names in `Result.Planned`, and changes nothing. A schedule with `DryRun: true` runs it in place of `Run`, so a new sweep or a changed retention can run in production for a while with `OnRun` reporting what it would take, before it is allowed to delete; a job without a dry run is then refused with `unsupported`, never run for real.

<!-- ds:block id=previewthensweep-n9pdx253 -->
**previewThenSweep** · [`examples/concepts-jobs/main.go:116-142`](../../examples/concepts-jobs/main.go#L116-L142)

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

Check: `dry run: would delete 2: incoming/a.png incoming/b.png`, then `sweep: deleted 2, files/kept.png still there: true`. Run a job by hand like this from a test or an admin endpoint; under a running scheduler, `scheduler.RunNow(ctx, name)` does the same while keeping the job single-flight.

## Step 4 — Jobs of your own, and the refusals

`jobs.Func(name, run)` makes any function a job, so the scheduler and the other runners treat it like the built-in ones: trimming a `CachedRead` cache, generating missing variants, a recount of your own. `job.Named(name)` renames a built-in job, to schedule two of one kind (two disks to sweep):

<!-- ds:block id=customjob-eyspfne3 -->
**customJob** · [`examples/concepts-jobs/main.go:148-169`](../../examples/concepts-jobs/main.go#L148-L169)

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

Check: `trim-media-cache: 1 trimmed; a second sweep named sweep-exports`.

A misconfiguration is refused, never guessed at. A zero retention or minimum age fails every run with `invalid_argument`, because "keep nothing" read from a missing setting would purge everything; and the scheduler validates every schedule before starting any:

<!-- ds:block id=refusedconfig-wbkq5hk3 -->
**refusedConfig** · [`examples/concepts-jobs/main.go:175-187`](../../examples/concepts-jobs/main.go#L175-L187)

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

Check: `sweep with minAge 0: invalid_argument` and `two schedules with one name: invalid_argument`.

## Accept fast, store later

A slow store (S3 across an ocean, a bucket behind a congested link) makes every upload request wait for it. `jobs.Promote(staging, "pending", store, minAge)` lets the request answer as soon as the bytes are on a fast disk next to the application: store each upload on `staging` under `pending/` (`upload.Receive` with `upload.RandomKey("pending")`), answer, and let the job move every object under `pending/` older than `minAge` to `store`, under the same key without the prefix (`pending/a.png` becomes `a.png`). Schedule it on a short interval and call `scheduler.RunNow(ctx, jobs.NamePromote)` after an upload for the shortest delay; the scheduler keeps it single-flight.

Each move is a copy, held to the source's size, then the deletion of the staging object, so a crash between the two leaves the object on both disks and the next run finishes it. One failing object does not stop the others; the error names each one left. Until an object has moved, read it from staging: `Stat` the store first and fall back to `pending/<key>` on staging. With records, a file's row names its disk, so record the file once it is on the store (or move the row with the records disk-move API when it exists); the job itself moves bytes only.

The program stores an upload on a fast staging disk, reads it from there while it waits, runs the job, and reads it from the store:

<!-- ds:block id=readeither-zfzw99jw -->
**readEither** · [`examples/concepts-jobs/main.go:68-75`](../../examples/concepts-jobs/main.go#L68-L75)

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

<!-- ds:block id=acceptfaststorelater-ezzbbkyu -->
**acceptFastStoreLater** · [`examples/concepts-jobs/main.go:81-111`](../../examples/concepts-jobs/main.go#L81-L111)

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

Check: `before promote: read from staging; filesystem.promote moved 1; after: read from store`. The program runs the job directly at a time past `minAge`; under the scheduler it runs on its interval, with `RunNow` after each upload.

Deleting in the background needs no job of its own: `Trash` hides the file at once, and `jobs.SweepTrash` with a short retention deletes its bytes later.

<a id="other-runners"></a>

## Step 5 — Other runners

A `Job` is a plain function, so any runner can call it: a cron job that runs one binary per job, a Kubernetes CronJob, or a queue the application already has.

`github.com/ubgo/filesystem/jobs/river` runs the same jobs on River, the Postgres queue. `river.NewWorker(river.WorkerOptions{OnRun}, jobs...)` is one River worker that executes every maintenance job by name, and `river.Periodic(schedules...)` turns the schedules into River periodic jobs: River's elected leader inserts each one for the whole fleet, failed runs are retried by River, and every job is unique while it is queued or running, so two instances never run it at once. An unknown job name cancels the River job instead of retrying it forever. `river.Enqueue(ctx, client, name)` queues one run now.

```go
worker, err := fsriver.NewWorker(fsriver.WorkerOptions{OnRun: logReport}, jobList...)
workers := river.NewWorkers()
river.AddWorker(workers, worker)
client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
	Queues:       map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
	Workers:      workers,
	PeriodicJobs: fsriver.Periodic(schedules...),
})
```

`github.com/ubgo/filesystem/jobs/asynq` does the same on Asynq, the Redis queue. `asynq.NewHandler(asynq.HandlerOptions{OnRun}, jobs...)` handles the one task type `filesystem:maintenance` (the payload is the job's name), and `asynq.Register(ctx, scheduler, client, schedules...)` adds an `@every` entry per schedule, each run unique for one interval, and enqueues the `RunAtStart` ones at once. An unknown job name is not retried.

```go
handler, err := fsasynq.NewHandler(fsasynq.HandlerOptions{OnRun: logReport}, jobList...)
mux := asynq.NewServeMux()
mux.Handle(fsasynq.TaskType, handler)
err = fsasynq.Register(ctx, asynq.NewScheduler(redisOpt, nil), client, schedules...)
```

## Image variants from a job

When an upload request must not wait for resizing, generate image variants in a job instead of with `variants.WithEager`. The variants plugin exposes two calls ([Image variants](../plugins/variants.md#generating-from-a-job)):

- `(*variants.Plugin).Generate(ctx, store, scope, fileID, names...)` makes one file's variants. It sets the file's processing status (`processing`, then `ready` or `failed`) and emits `variants.ready` or `variants.failed`. It returns the first failure so the queue retries, and is safe to run again.
- `(*variants.Plugin).GenerateMissing(ctx, store, variants.MissingOptions{Scope, MaxFiles})` runs `Generate` for a scope's images that lack a variant, newest first, and resumes through `NextCursor`.

The maintenance runners in this package run named jobs without arguments, so the per-upload job is your queue's own, with the file as its arguments. With River, for example:

```go
type VariantsArgs struct{ Scope, FileID string }

func (VariantsArgs) Kind() string { return "variants.generate" }

type VariantsWorker struct {
	river.WorkerDefaults[VariantsArgs]
	Store    *records.Store
	Variants *variants.Plugin
}

func (w *VariantsWorker) Work(ctx context.Context, job *river.Job[VariantsArgs]) error {
	return w.Variants.Generate(ctx, w.Store, job.Args.Scope, job.Args.FileID)
}
```

Insert it in the same transaction that records the file, so a rolled-back upload queues nothing: with River's `database/sql` driver (`riverdatabasesql`), `InsertTx` takes the `*sql.Tx` that `Store.Tx` hands you. With Asynq, enqueue a task whose payload is the scope and file ID after `Upload` returns.

The safety net is a scheduled `jobs.Func` around `GenerateMissing`, for uploads whose job was lost:

```go
missing := jobs.Func("variants.generate_missing", func(ctx context.Context, _ time.Time) (jobs.Result, error) {
	res, err := plugin.GenerateMissing(ctx, store, variants.MissingOptions{Scope: tenant, MaxFiles: 500})
	return jobs.Result{Count: int64(res.Generated)}, err
})
```

## Pitfalls

- Treating a dry run as a promise. It reports what a run would act on at that moment; uploads that finish or age past the cutoff in between change the real run.
- Promoting from staging on a fleet without a shared staging disk. A file accepted on one instance is invisible on the others until it is promoted, and only that instance's job can promote it.
- A `jobs.Func` that ignores its context. The scheduler cancels the context on shutdown and waits for runs in progress; a job that never checks it delays every shutdown.
- Pointing `SweepOrphans` at a prefix where direct uploads wait for a later `CreateFile`: keep `minAge` well above the longest such wait, or the sweep deletes uploads that were about to be recorded.
- A short `minAge` for `SweepAbandoned`: an upload that is still sending parts after `minAge` is aborted.
- Sweeping trash on every instance of a large fleet without jitter: they all hit the database at the same moment.

## Next

- [Uploads](uploads.md): `upload.Sweep` and `SweepAbandoned`, which two of these jobs run.
- [Consistency](consistency.md): orphaned objects and missing ones.
