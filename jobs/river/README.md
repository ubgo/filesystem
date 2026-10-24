# jobs/river

`github.com/ubgo/filesystem/jobs/river` runs the maintenance jobs of [`jobs`](../README.md) on [River](https://github.com/riverqueue/river), the Postgres job queue, for applications that already run River. One worker kind (`filesystem_maintenance`) executes every job by name; each `jobs.Schedule` becomes a River periodic job, so River's leader inserts it once for the whole fleet, retries failed runs and keeps every job single-flight across processes.

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

| Name | What it is |
|---|---|
| `NewWorker(WorkerOptions{OnRun, Now}, jobs...)` | the River worker for these jobs; an unnamed job, one without `Run`, or a repeated name is `CodeInvalidArgument` |
| `Worker.Work` | runs the job named in the arguments; an unknown name cancels the River job (`river.JobCancel`, `CodeNotFound`) instead of retrying it; a job's error is returned so River retries |
| `Periodic(schedules...)` | River periodic jobs: one per schedule, every `Every`, at start when `RunAtStart`, with the job's name as its ID; `Jitter` is not used, since River's leader is the only one inserting |
| `PeriodicWith(PeriodicOptions{Queue, MaxAttempts}, schedules...)`, `EnqueueWith(ctx, client, name, PeriodicOptions)`, `PeriodicOptions.InsertOpts()` | a dedicated queue and attempt budget; uniqueness is always kept. A `Schedule.Next` schedule becomes River's calendar for that job |
| `WorkerOptions.Timeout`, `WorkerOptions.TimeoutFor` | bound one run (`Worker.Timeout`): `TimeoutFor(job)` per job name, so an export and a sweep need not share one bound; nil or 0 falls back to `Timeout`; 0 there keeps the client's `JobTimeout` |
| `Enqueue(ctx, client, name)` | inserts one run now, for an admin button; a no-op while that job is queued or running |
| `Args`, `Kind`, `InsertOpts()` | the job arguments (the job's name), its kind, and the insert options: unique by name while queued or running |

## Tests

The unit tests need nothing. `TestOnPostgres` runs a real River client against Postgres when `FILESYSTEM_RIVER_PG_DSN` is set (the repository's Postgres task sets it to the host's `filesystem_test` database); it creates a schema of its own, migrates River into it, and drops it afterwards.
