# Jobs: maintenance without records, on a calendar

An application that stores files but keeps no file rows still has maintenance to run: uploads nobody confirmed, abandoned multipart uploads, a cache that outgrows its budget. This program runs it with `github.com/ubgo/filesystem/jobs/diskjobs`, a module that requires the core module only, so it builds none of records, sqlb or the SQL drivers. The upload sweep runs on a calendar, every day at 03:00 UTC (`diskjobs.Daily`), the abandoned-upload sweep is previewed on the same calendar as a dry run, and a job of the application's own (`diskjobs.Func` around `filesystem.TrimCache`) runs every hour. It uses the core `filesystem` package, `memory` and `jobs/diskjobs`. The guide [Maintenance: Without records, and on a calendar](../../docs/maintenance/README.md#without-records-and-on-a-calendar) explains each piece.

## Run it

```sh
cd examples
go run ./jobs-calendar
go test ./jobs-calendar
```

Everything runs on memory disks: no database, no network, no environment variables. The objects are written on day 0 at 09:00 and the scheduler's clock is day 2 at 14:30, so every minimum age has passed.

## What it does

```text
filesystem.upload_sweep: next run 2026-10-10T03:00:00Z
filesystem.sweep_abandoned: next run 2026-10-10T03:00:00Z
app.trim_thumb_cache: 1, err <nil>
filesystem.sweep_abandoned: 1, err <nil>, dry run: multipart:1 pending/big.mov
filesystem.upload_sweep: 1, err <nil>
```

1. The two calendar schedules name their next run: 03:00 the next morning, since 03:00 today has passed.
2. The schedules are listed in one place, [`schedules`](ds:block?id=calendarschedules-k7d2qm41) in `main.go`. Each job runs once at start (`RunAtStart`) and reports to `OnRun`; the program stops the scheduler once all three reported, and prints the reports sorted by name.
3. The upload sweep deleted the upload nobody confirmed. The abandoned-upload sweep, scheduled with `DryRun: true`, only lists the multipart upload it would abort (`diskjobs.AbandonedItem` names it); dropping `DryRun` lets it act. The cache trim removed one thumbnail to get back under its 10-byte budget.

The test also parses `main.go` and fails if it imports `jobs` or `records`, which is the point of the example.
