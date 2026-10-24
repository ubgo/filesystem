# github.com/ubgo/filesystem/records/egress

A records plugin that meters the bytes each scope serves, in daily buckets, from the reads handlers report with `records.Store.Served`, and reports finished days as `egress.daily_total` events for billing. The guide is [docs/plugins/egress.md](../../docs/plugins/egress.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

Part of the records module; no dependency of its own.

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...)` | The plugin; install with `records.WithPlugins` |
| `WithBucket(fn BucketFunc)` | Meters in `fn(scope, at)`'s buckets instead of UTC days: hourly, or each tenant's own time zone. Keys must sort as text in time order per scope and be 1 to `MaxBucketBytes` (64) bytes; a bad key fails the read's metering (`invalid_argument`). Default `UTCDay` |
| `WithBucketContext(fn BucketContextFunc)` | `WithBucket` with `fn(ctx, scope, at) (string, error)`: a bucket that needs a lookup, such as a tenant's zone read from the database per read. An error fails the read's metering and `Days`, `Total`, `Report` and `Prune`. Default `UTCDay` |
| `(*Plugin).OnServed` | `records.ServeHooks`: adds a read's bytes and one request to its scope's bucket for `Served.At` (its UTC day, or `WithBucket`'s key) |
| `(*Plugin).Days(ctx, s, scope, from, to)` | The scope's `Bucket{Scope, Day, Bytes, Requests}` for the days from `from`'s up to `to`'s, oldest first |
| `(*Plugin).Total(ctx, s, scope, from, to)` | Their sum |
| `(*Plugin).Report(ctx, s, now)` | Emits `EventDailyTotal` for every bucket before `now`'s (per scope with `WithBucket`) that grew since its last report, with a deterministic event ID; returns how many. Schedule `jobs.EgressReport` |
| `(*Plugin).Prune(ctx, s, cutoff)` | Deletes fully reported buckets of days before `cutoff`'s, and the per-subject buckets before it |
| `WithPerSubject(kinds...)`, `(*Plugin).SubjectDays(ctx, s, scope, kind, subject, from, to)`, `SubjectBucket` | Also meters each read of those `records.ServeKind`s per subject: `WithPerSubject(records.ServeShare)` gives each share link's bytes per bucket (the share handler reports the link's ID as `Served.Subject`), `records.ServeVariant` each image variant's. A read without a subject counts per scope only; an invalid kind fails every read's metering, a subject over `MaxSubjectBytes` (191) its own. For display and per-link limits; `Report` stays per scope. Default off |
| `WithSubjectFunc(fn SubjectFunc)` | Breaks reads down by `fn(ctx, served)`'s subject (`""` for none) instead of `Served.Subject`; `fn` sees every kind, so plain links can be metered per file (`sv.FileID`) or per API key from `ctx`. An error fails the read's metering after the scope was charged. Default: `Served.Subject` for the `WithPerSubject` kinds |
| `EventDailyTotal` | `"egress.daily_total"`; `Detail` keys `DetailDay`, `DetailBytes`, `DetailRequests` (the growth), `DetailTotalBytes`, `DetailTotalRequests` |
| `PluginName`, `TableNameDaily`, `TableNameSubjects`, `DayLayout` | `"egress"`, `"fs_egress_daily"`, `"fs_egress_subjects"`, `time.DateOnly` |

## Table

`fs_egress_daily`, migration `egress/0001_egress_daily`: `scope`, `day` (primary key together), `bytes`, `requests`, `reported_bytes`, `reported_requests`, `updated_at`. Typed columns: `egress.Table`, `Scope`, `Day`, `Bytes`, `Requests`, `ReportedBytes`, `ReportedRequests`, `UpdatedAt`.

`fs_egress_subjects`, migration `egress/0002_egress_subjects` (written only with `WithPerSubject` or `WithSubjectFunc`): `scope`, `kind`, `subject`, `day` (primary key together), `bytes`, `requests`, `updated_at`; index on `day`. Typed columns: `egress.SubjectTable`, `SubjectScope`, `SubjectKind`, `SubjectID`, `SubjectDay`, `SubjectBytes`, `SubjectRequests`, `SubjectUpdatedAt`.
