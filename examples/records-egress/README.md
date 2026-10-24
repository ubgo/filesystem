# Egress metering

The `egress` plugin of the records layer: every link the Store issues (`URL`, `DownloadURL`) and every stream the app reports with `Store.Served` adds bytes and a request to its scope's bucket for the UTC day; `Days` and `Total` read a period, `Report` turns finished days into `egress.daily_total` events for billing, and `Prune` drops reported days. It runs on SQLite over an in-memory disk, with a clock the demo moves from one day to the next. The guide [Egress metering](../../docs/plugins/egress.md) explains each step.

## Run it

```sh
cd examples
go run ./records-egress
go test ./records-egress
```

It needs no network or environment variables.

## What it does

```text
2026-10-03 2400 bytes in 3 requests
2026-10-04 1000 bytes in 1 requests
total 3400 bytes in 4 requests
report on day 2: 1 event(s)
report on day 3 (first run): 1 event(s)
report on day 3 (second run): 0 event(s)
billed: 2026-10-03 +2400 bytes +3 requests; 2026-10-04 +2000 bytes +2 requests
pruned 2 reported day(s)
acme-tokyo: read at 2026-10-03 20:00 UTC lands on 2026-10-04
globex-newyork: read at 2026-10-03 20:00 UTC lands on 2026-10-03
report at 2026-10-04 10:00 UTC: 1 event(s): 2026-10-03 +500 bytes +1 requests
per link: press 2000 bytes in 2 requests
per link: partner 1000 bytes in 1 requests
per link: scope 3000 bytes in 3 requests
```

1. On 3 October two view links of a 1,000-byte file and a 400-byte stream reported with `Served` count 2,400 bytes in three requests. On 4 October a download link counts 1,000.
2. `Days` lists the two buckets and `Total` sums them.
3. A report run on 4 October reports the finished 3 October only. Another link the same day grows 4 October to 2,000 bytes.
4. On 5 October the first run reports 4 October; a second run reports nothing, because nothing grew. The billing consumer (an `EventHooks` plugin here, a webhook endpoint in production) keeps the event IDs it saw.
5. `Prune` deletes the fully reported days before 5 October.
6. `zones.go` meters in each tenant's own day with `egress.WithBucket(tenantDay)`, on a Store and database of its own. One stream for each tenant at 20:00 UTC on 3 October lands on 4 October for the Tokyo tenant (UTC+9) and on 3 October for the New York tenant (UTC-5); the default `egress.UTCDay` would have put both on 3 October.
7. A report at 10:00 UTC on 4 October treats a bucket as finished scope by scope: New York's 3 October is over and is billed, Tokyo's 4 October is still running and is not.
8. `shares.go` installs egress with `egress.WithPerSubject(records.ServeShare)` beside the share plugin, on a Store and database of its own. One 1,000-byte deck is shared through two links; the share handler serves the press link twice and the partner link once, reporting each download with the link's ID. `SubjectDays` gives each link its own bytes, while the scope's buckets, which billing reads, still hold all three. The test `TestPerSubjectNeedsTheKind` shows a read of a kind not listed in `WithPerSubject` counts in the scope only.

## Files

| File | What it holds |
|---|---|
| `main.go` | The Store with the plugin, the clock, the billing consumer and the steps |
| `zones.go` | Metering in each tenant's own time zone with `egress.WithBucket`; code: [`newZonedStore`](ds:block?id=newzonedstore-b9t4m2xc) |
| `shares.go` | Egress per share link with `egress.WithPerSubject`; code: [`newPerLinkStore`](ds:block?id=newperlinkstore-w6d3k8qz), [`linkUsage`](ds:block?id=linkusage-b5m2v9xt) |
| `main_test.go`, `helpers_test.go` | Runs the steps, and pins that negative bytes are refused and that the per-subject breakdown is opt-in per kind |

## Next

- The guide: [Egress metering](../../docs/plugins/egress.md); the plugin's [README](../../records/egress/README.md).
- Related examples: [records-quota](../records-quota) for storage limits, [jobs-scheduler](../jobs-scheduler) to run `Report` on a schedule.
