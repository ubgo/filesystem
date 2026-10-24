# Egress metering

The `egress` plugin counts the bytes each scope serves, per UTC day, from the reads handlers report with `records.Store.Served`, and turns finished days into `egress.daily_total` events a billing pipeline can sum exactly once. Package `github.com/ubgo/filesystem/records/egress`; reference in its [README](../../records/egress/README.md).

## Before you start

- The example is [`examples/records-egress`](../../examples/records-egress): `go run ./records-egress` in `examples/` prints every step below, moving a fixed clock across three days; `go test ./records-egress` runs them.
- When to use it: you bill or limit tenants by bandwidth, or you want to see which tenants cost you transfer. For storage held rather than served, use [quota](quota.md).

You have a records Store ([Files](../records/files.md)). Every link `Store.URL` or `Store.DownloadURL` issues is reported, so recordshttp, share links, image variants and your own code built on them are metered; bytes you stream yourself are reported with `store.Served` (see [Events and hooks](../records/events-and-hooks.md#reads-served-and-servehooks)).

## Step 1 — Install the plugin

`egress.New()` takes no options. Install it like any plugin; `Migrate` creates `fs_egress_daily`. The example also passes `records.WithClock`, to move the demo from day to day, and a billing consumer of its own as a second plugin:

<!-- ds:block id=newegressstore-n7c3w9kp -->
**newStore** · [`examples/records-egress/main.go:74-96`](../../examples/records-egress/main.go#L74-L96)

```go
func newStore(ctx context.Context, db *sql.DB, now func() time.Time, plugins ...records.Plugin) (*records.Store, *egress.Plugin, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return nil, nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	eg := egress.New()
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithClock(now), records.WithPlugins(append([]records.Plugin{eg}, plugins...)...))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, eg, nil
}
```
<!-- /ds:block hash=df2b1f -->

Every reported read now adds its bytes and one request to its scope's bucket for the day of the Store's clock, with one `UPDATE` (an `INSERT` for the day's first read), so concurrent reads never lose a count. A link counts the size of the object it points at: an upper bound, since a client may not follow it or may fetch a range. A public file's URL is permanent and fetched without the application seeing it, so for public files egress counts links issued, not fetches; real byte egress for them comes from CDN or bucket access logs.

Bytes your own handler sends (a proxy route, a range response) are reported once the response is written, with the bytes actually sent:

<!-- ds:block id=streamed-h5q2m8vx -->
**streamed** · [`examples/records-egress/main.go:102-104`](../../examples/records-egress/main.go#L102-L104)

```go
func streamed(ctx context.Context, s *records.Store, f records.File, n int64) error {
	return s.Served(ctx, records.Served{Kind: records.ServeStream, Scope: f.Scope, FileID: f.ID, Bytes: n})
}
```
<!-- /ds:block hash=4859b4 -->

`Served` refuses a negative byte count and an unknown kind with `invalid_argument`, and a [quarantined](../records/processing-status.md#quarantine-until-scanned) file with `conflict`, before any plugin counts it.

## Step 2 — Read a period

<!-- ds:block id=usage-t9w4c2hn -->
**usage** · [`examples/records-egress/main.go:109-116`](../../examples/records-egress/main.go#L109-L116)

```go
func usage(ctx context.Context, s *records.Store, eg *egress.Plugin, from, to time.Time) ([]egress.Bucket, egress.Bucket, error) {
	days, err := eg.Days(ctx, s, tenant, from, to)
	if err != nil {
		return nil, egress.Bucket{}, err
	}
	total, err := eg.Total(ctx, s, tenant, from, to)
	return days, total, err
}
```
<!-- /ds:block hash=a36a18 -->

`Days` returns one `Bucket` per day with reads, oldest first; days without reads have none. `Total` is their sum. Check: the example prints `2026-10-03 2400 bytes in 3 requests` (two view links of a 1,000-byte file and a 400-byte stream), `2026-10-04 1000 bytes in 1 requests` (a download link) and `total 3400 bytes in 4 requests`.

Both take the UTC days from `from`'s day up to, not including, `to`'s day. Limit bandwidth in your own handler or `upload.Resolver` by comparing `Total` with the plan: the library meters, the application decides.

## Step 3 — Report finished days for billing

Schedule `jobs.EgressReport(eg, store)` at least daily. Each run emits one `egress.daily_total` event (through `records.Store.Emit`, so [webhooks](webhooks.md) and [audit](audit.md) see it) for every bucket of a finished day whose totals grew since its last report:

| Detail key | Holds |
|---|---|
| `day` | the UTC date, `2026-10-03` |
| `bytes`, `requests` | what the day gained since its previous report: the amounts to add |
| `total_bytes`, `total_requests` | the day's totals so far |

Check: the example runs `Report` on 4 October (`1 event(s)`: 3 October), adds a link that day, then runs it twice on 5 October: `first run: 1 event(s)`, `second run: 0 event(s)`. Its billing consumer, an `EventHooks` plugin that keeps the IDs it saw, prints `billed: 2026-10-03 +2400 bytes +3 requests; 2026-10-04 +2000 bytes +2 requests`.

The event and the bucket's reported mark commit together, so a crash repeats or loses nothing, and two runs racing each other report each growth once. A read that lands on an already reported day (a request finishing just after midnight) is sent by the next run as a further event. Each event's `ID` is derived from the bucket and its totals, so the same report always carries the same ID: a consumer that keeps the IDs it processed counts every byte exactly once.

## Step 4 — Prune

`eg.Prune(ctx, store, cutoff)` deletes the fully reported buckets of days before `cutoff`'s; a bucket with unreported reads is kept whatever its age. Buckets outlive purged files and scopes, so a departed tenant's last reads are still billed. Check: the example prints `pruned 2 reported day(s)`.

## Other buckets: hours, or each tenant's own day

A bucket is a UTC day by default (`egress.UTCDay`). `egress.WithBucket(fn)` meters in `fn(scope, at)`'s buckets instead: an hour for fine-grained metering, or a day in the tenant's own time zone so a bill matches the customer's calendar. `Report` then treats a bucket as finished once `fn(scope, now)` names a later one, scope by scope, and `Days`, `Total` and `Prune` compare keys the same way. Keys must sort as text in time order within a scope and be 1 to `egress.MaxBucketBytes` (64) bytes; a bad key fails the read's metering (`invalid_argument`) rather than being stored cut short. Keep one function for the life of the data.

```go
zoneOf := func(scope string) *time.Location { return tenants.Location(scope) } // your tenant settings, cached
p := egress.New(egress.WithBucket(func(scope string, at time.Time) string {
	return at.In(zoneOf(scope)).Format(egress.DayLayout)
}))
```

An hourly meter is `func(_ string, at time.Time) string { return at.UTC().Format("2006-01-02T15") }`; the event's `DetailDay` then carries the hour's key.

When the zone lives in your database and you would rather not keep a cache, use `egress.WithBucketContext(fn)`: `fn(ctx, scope, at)` gets the read's context and returns `(key, error)`. An error fails the read's metering, so the handler refuses the read, and fails `Days`, `Total`, `Report` and `Prune` too, so nothing is metered in a bucket the function did not name:

```go
p := egress.New(egress.WithBucketContext(func(ctx context.Context, scope string, at time.Time) (string, error) {
	loc, err := tenants.LoadLocation(ctx, scope) // a query; an error refuses the read
	if err != nil {
		return "", err
	}
	return at.In(loc).Format(egress.DayLayout), nil
}))
```

Try it: [`examples/records-egress`](../../examples/records-egress) runs this section in `zones.go` ([`tenantDay`](ds:block?id=tenantday-r6k3v8wq)): `go run ./records-egress` from `examples/`.

## Per share link: a breakdown by subject

The buckets are per scope. `egress.WithPerSubject(kinds...)` meters each read of those kinds per subject as well: with `records.ServeShare`, each share link's bytes per day, since the [share](share.md) handler reports every download with the link's ID as `records.Served.Subject`; with `records.ServeVariant`, each image variant's.

```go
eg := egress.New(egress.WithPerSubject(records.ServeShare))

// "This link used 4.2 GB this month", or a cap your handler enforces with share.WithAccessCheck.
days, err := eg.SubjectDays(ctx, store, scope, records.ServeShare, link.ID, monthStart, now)
```

`SubjectDays` returns `[]egress.SubjectBucket{Scope, Kind, Subject, Day, Bytes, Requests}`, oldest first, in the same buckets as `Days` (`WithBucket` applies). A read of a listed kind without a subject (a share read reported by your own code with no link ID) is metered per scope only. The breakdown is not reported: `Report` and the `egress.daily_total` events stay per scope, so billing does not change. `Prune` deletes subject buckets before its cutoff. The breakdown is written in a second statement after the scope's, so a failure there fails the read (500, nothing served) with the scope already charged: billing errs on the side of the bytes.

The subject is `Served.Subject` for the listed kinds. To break reads down by something else, give `egress.WithSubjectFunc(fn)`: `fn(ctx, served)` sees every read of every kind and returns its subject, `""` for none. Plain file links (`records.ServeLink`) carry no subject, so this is how you get "top downloaded files" or a per-file bandwidth cap, or a breakdown per downloader or API key taken from the context. The row keeps the read's kind, so `SubjectDays` reads it back with that kind. An error from `fn` fails the read's metering after the scope was charged, as a failed breakdown write does:

```go
eg := egress.New(egress.WithSubjectFunc(func(ctx context.Context, sv records.Served) (string, error) {
	if sv.Kind == records.ServeLink {
		return sv.FileID, nil // bytes per file
	}
	return sv.Subject, nil // share links and variants, as by default
}))

perFile, err := eg.SubjectDays(ctx, store, scope, records.ServeLink, fileID, monthStart, now)
```

Try it: [`examples/records-egress`](../../examples/records-egress) runs this section in `shares.go` ([`linkUsage`](ds:block?id=linkusage-b5m2v9xt)): `go run ./records-egress` from `examples/`.

## Try it

```sh
cd examples
go run ./records-egress
go test ./records-egress
```

## The table

`fs_egress_daily`, migration `egress/0001_egress_daily`: `scope` and `day` (the primary key, `day` as `YYYY-MM-DD` text), `bytes`, `requests`, `reported_bytes`, `reported_requests`, `updated_at`; index `(day, scope)`.

`fs_egress_subjects`, migration `egress/0002_egress_subjects`, written only with `WithPerSubject` or `WithSubjectFunc`: `scope`, `kind`, `subject` and `day` (the primary key), `bytes`, `requests`, `updated_at`; index on `day`.

## What can go wrong

- A recordshttp, share or variants request answers 500 — a `ServeHooks` plugin failed (the database is down): reads are refused rather than served unmetered.
- No events — `Report` only reports days before the run's UTC day; today's bucket is still filling.
- Billing counts a day twice — the consumer did not drop repeated event IDs; webhooks deliver at least once.
- Egress looks low for public files — a public URL is issued once and fetched many times without the app; meter those through CDN or bucket access logs.
- Egress looks high — links issued are counted, not links followed. A page that builds a signed URL for every thumbnail it might show counts them all; build URLs for what the page renders.
