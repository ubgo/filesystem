# Maintenance: dry runs, scrubbing, migration, sync and exports

This program walks through the maintenance tools a deployment needs after day one: it previews the trash sweep before running it (`Job.DryRun`), scrubs a disk and finds an object changed behind the Store's back (`jobs.Scrub`), moves every file from a "hot" disk to a "cold" one (`jobs.MigrateDisk`), mirrors the cold disk to a backup with a dry run first (`filesystem.Sync`), and exports everything a user uploaded as a zip with a link that expires, then sweeps the expired export (`jobs.ExportSubject`, `jobs.UploadSweep`). It uses the core `filesystem` package, `memory`, `signedurl`, `upload`, `records` on SQLite and `jobs`. The guide is [Maintenance](../../docs/maintenance/README.md).

## Run it

```sh
cd examples
go run ./maintenance
go test ./maintenance
```

It needs no environment variables. It creates a SQLite file in a temporary directory and removes it before exiting.

## What it does

```text
dry run: would purge 1 file(s)
sweep: purged 1 file(s)
scrub: report.txt changed
scrub: checked 2 file(s)
migrate: moved 2 file(s) to cold
sync dry run: 2 to copy, 1 to delete
sync: copied 2, deleted 1, unchanged 0
export: 2 file(s), 43 bytes, link valid until 9:15AM
export sweep: deleted 1 expired export(s)
```

The scrub checks two files because the trashed one was purged by then; the damaged object is restored before the migration, since `MoveFile` refuses a copy that differs from its row.
