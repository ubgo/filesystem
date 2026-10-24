# Jobs: a deployment's maintenance on a schedule

The library starts no background work, so trash retention and the sweeps of unconfirmed uploads, abandoned multipart uploads and orphaned objects must be scheduled. This program lists them as `jobs.Schedule`s, runs them with the in-process scheduler of `github.com/ubgo/filesystem/jobs`, and prints the report of each job's first run. It uses the core `filesystem` package, `memory`, `records` (with the `audit` plugin) on SQLite, `upload` and `jobs`. The guide [Maintenance jobs](../../docs/concepts/maintenance-jobs.md) explains each job and the scheduler's rules.

## Run it

```sh
cd examples
go run ./jobs-scheduler
go test ./jobs-scheduler
```

It needs no environment variables. It creates a SQLite file in a temporary directory and removes it before exiting. The data is written with a clock set to day 0 and the scheduler's clock is day 60, so every retention period except the audit log's 90 days has passed.

## What it does

```text
filesystem.audit_prune: 0, err <nil>
filesystem.sweep_abandoned: 1, err <nil>
filesystem.sweep_orphans: 1, err <nil>
filesystem.sweep_trash: 1, err <nil>
filesystem.upload_sweep: 1, err <nil>
```

Each job ran once at start (`RunAtStart`) and reported to `OnRun`; the program stops the scheduler once all five reported, and prints the reports sorted by name.
