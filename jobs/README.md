# jobs

`github.com/ubgo/filesystem/jobs` names the maintenance work of a filesystem deployment as plain functions, and runs them with a small in-process scheduler. The library starts no background work on its own; this module is how trash retention, sweeps and recounts get scheduled. It depends only on the standard library, the core and `records`; River and Asynq adapters are separate modules.

```go
s := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: func(r jobs.Report) { log.Println(r.Job, r.Result.Count, r.Err) }})
go s.Run(ctx,
	jobs.Schedule{Job: jobs.SweepTrash(store, 30*24*time.Hour), Every: time.Hour, Jitter: 5 * time.Minute},
	jobs.Schedule{Job: jobs.UploadSweep(disk, "incoming", 24*time.Hour), Every: time.Hour},
)
```

The guide is [Maintenance jobs](../docs/concepts/maintenance-jobs.md); dry runs, scrubbing, migration and exports are in [Maintenance](../docs/maintenance/README.md). Runnable demos are `examples/jobs-scheduler` and `examples/maintenance`.

## Jobs

The records-free half (`Job`, `Result`, `Func`, the `Scheduler`, `UploadSweep`, `SweepAbandoned`, `AbandonedItem`, `Promote`, and the `RetentionJob` and `WithDryRun` builders) also lives in `github.com/ubgo/filesystem/jobs/diskjobs`, a module of its own that requires the core module only: an application without records requires it and downloads and builds none of records, sqlb or the SQL drivers. `jobs` requires it and re-exports those names as aliases, so a `jobs.Job` is a `diskjobs.Job` and the two mix freely; the runners below take `diskjobs` types and require `diskjobs` alone, never `jobs` or records (`deps_test.go` checks their `go.mod` files, and `diskjobs/deps_test.go` its own).

| Name | What it is |
|---|---|
| `Job` | `Name`, `Run(ctx, now) (Result, error)`, safe to run again after a failure, and an optional `DryRun` with the same signature that changes nothing |
| `Result` | `Count`, the items one run processed (or, in a dry run, would process); `Planned`, set by a dry run only, names them (file IDs or keys) |
| `Job.Named(name)` | a copy under another name, for two jobs of one kind |
| `Func(name, run)` | a job of any function: the hook for eager variants or an application's own cleanup |
| `SweepTrash(store, retention)` | purges files trashed more than `retention` ago; dry run: `FindTrash` |
| `SweepExpired(store)` | purges expired and already-viewed view-once files (`records.Store.SweepExpired`) |
| `SweepRetention(store)` | applies the Store's retention rules (`records.WithRetention`) through `records.Store.SweepRetention`; dry run: its preview |
| `SweepOrphans(store, disk, prefix, minAge)` | deletes unowned objects under `prefix` older than `minAge`; dry run: `FindOrphans` |
| `UploadSweep(disk, dir, minAge)` | deletes unconfirmed uploads under `dir` (`upload.Sweep`); dry run: `upload.FindStale` |
| `SweepAbandoned(disk, minAge)` | removes multipart uploads and staging files idle for `minAge`; on a disk with the `FindAbandoned` capability its dry run lists them (`Disk.FindAbandoned`) as `AbandonedItem` names them, otherwise it has none and a dry-run schedule is `CodeUnsupported` |
| `AbandonedItem(a)` | how that dry run names a leftover in `Result.Planned`: `<kind>:<id>`, then a space and the key for a multipart upload whose key is known (`multipart:7 videos/big.mp4`, `staging:ABC…`) |
| `AuditPrune(plugin, store, retention)` | deletes audit entries older than `retention` |
| `SharePrune(plugin, store, retention)` | deletes share links expired or revoked more than `retention` ago |
| `SweepLocks(plugin, store)` | deletes check-out locks (`records/locks`) expired at the run's time |
| `QuotaRecount(plugin, store, scopes)` | rebuilds the usage of every scope `scopes` returns |
| `VersionsTrim(plugin, store, scopes)` | applies each file's history policy (count and `MaxAge`) in every scope `scopes` returns (`versions.Plugin.TrimScope`); the count is the versions dropped, and a failing scope does not stop the others |
| `Promote(staging, dir, store, minAge)` | accept fast, store later: moves every object under `dir` on `staging` older than `minAge` to `store`, without the `dir` prefix; one failing object does not stop the others; a root `dir` or a negative `minAge` is `CodeInvalidArgument` |
| `QuotaSweepReservations(plugin, store)` | releases quota reservations expired at the run's time (`quota.Plugin.Sweep`) |
| `EgressReport(plugin, store)` | emits `egress.daily_total` events for finished days that grew (`egress.Plugin.Report`); run at least daily |
| `WebhookDeliver(plugin, store)` | attempts the webhook deliveries due at the run's time (`webhooks.Plugin.Deliver`); its interval bounds the delay before a first attempt and after each backoff |
| `WebhookPrune(plugin, store, retention)` | deletes delivered and failed webhook deliveries older than `retention` |
| `NameQuotaSweepReservations`, `NameEgressReport`, `NameWebhookDeliver`, `NameWebhookPrune` | the default names of the metering jobs |
| `Scrub(store, ScrubOptions{Disk, Cursor, Batch, Interval, OnMissing, OnMismatch, OnError})` | re-reads the objects of a disk's files a batch per run, re-hashes them and reports missing or changed ones; an unreadable object goes to `OnError` and the run moves past it; never deletes; rate-limited by `Interval`, resumable by `Cursor` |
| `MigrateDisk(store, from, to, MigrateOptions{Cursor, Batch, OnMoved, OnFailed})` | moves every file from one disk to another with `records.Store.MoveFile` (copy, verify, cut over, delete the old object after the commit), a batch per run; a crash loses nothing and a rerun finishes |
| `ExportSubject(ctx, store, disk, Subject, ExportOptions)` | bundles a user's (`UploadedBy`) or an owner's files into a zip with `manifest.json` under `Dir` and returns a signed link that always expires (`TTL` required); the archive is always private; sweep `Dir` with `UploadSweep` |
| `Cursor`, `ObjectCursor(disk, key)` | where a resumable job stopped (`Load`, `Save`), kept by the application; `ObjectCursor` keeps it in an object |
| `NameScrub`, `NameMigrateDisk` | the default names of the integrity and migration jobs |
| `NameSweepTrash`, `NameSweepOrphans`, `NameUploadSweep`, `NameSweepAbandoned`, `NameAuditPrune`, `NameSharePrune`, `NameSweepLocks`, `NameQuotaRecount`, `NameVersionsTrim`, `NamePromote` | the default names |

A zero or negative retention or age fails every run with `CodeInvalidArgument` instead of purging everything.

## Scheduler

| Name | What it is |
|---|---|
| `NewScheduler(SchedulerOptions{OnRun, Now, After})` | a scheduler; `OnRun` receives every `Report`, `Now` and `After` replace the clock and the timer for tests |
| `Schedule` | `Job`, `Every` (positive) or `Next` (a calendar: `func(now) time.Time` returning the next run, such as `diskjobs.Daily(3, 0, loc)` for 03:00 or a cron schedule's `Next`; exactly one of the two; a `Next` that does not move forward stops the schedule with an `invalid_argument` report), `Jitter` (a random extra wait up to this), `RunAtStart`, `DryRun` (run `Job.DryRun` instead; a job without one, such as `SweepAbandoned` on a disk without `FindAbandoned`, is refused with `CodeUnsupported`) |
| `Scheduler.Run(ctx, schedules...)` | validates the schedules (`CodeInvalidArgument`), runs them until `ctx` ends, waits for runs in progress, returns nil |
| `Scheduler.RunNow(ctx, name)` | runs a scheduled job now, in the caller's goroutine; `CodeNotFound` for an unknown name |
| `Report` | `Job`, `DryRun`, `Started`, `Duration`, `Result`, `Err` |
| `ErrRunning` | a run skipped because the same job was still running: jobs are single-flight per scheduler |

A job that panics is reported with `CodeUnknown` and runs again at its next interval.
