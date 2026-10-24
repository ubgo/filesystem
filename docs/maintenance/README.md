# Maintenance: dry runs, scrubbing, migration, sync, exports and bucket events

The tools here keep a running deployment honest after day one: preview a sweep before it deletes anything, find objects that rotted or vanished, move every file to a new backend without downtime, mirror one disk to another, hand a user everything they uploaded, and learn about objects the moment the bucket stores them. Each one is a function or a `jobs.Job`, so it runs under the in-process scheduler, River, Asynq or a cron of your own ([Maintenance jobs](../concepts/maintenance-jobs.md)).

The program is [examples/maintenance](../../examples/maintenance); the code blocks below are copied from it, and its output is:

```
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

## Before you start

- Every tool but `filesystem.Sync` and the bucket consumers works on the records layer: a `records.Store` over a `filesystem.Manager` ([Records](../records/README.md)). The jobs come from `github.com/ubgo/filesystem/jobs`, a module of its own.
- Each tool is a `jobs.Job` or a function you call from one, so decide where jobs run first: the in-process scheduler for one instance, River or Asynq for several ([Maintenance jobs](../concepts/maintenance-jobs.md)).
- The program is [examples/maintenance](../../examples/maintenance), on memory disks and SQLite; [examples/maintenance-notify](../../examples/maintenance-notify) runs the bucket notifications section on an in-memory queue. Neither needs a network or an account.

## Which tool

| You want to | Use | Section |
|---|---|---|
| See what a sweep would delete before it deletes | `Job.DryRun`, `Schedule{DryRun: true}`, the `Find…` functions | [Dry runs](#dry-runs) |
| Know that every stored object still matches its row | `jobs.Scrub` | [Integrity scrubbing](#integrity-scrubbing) |
| Move every file to a new backend while serving | `jobs.MigrateDisk` | [Moving every file to another disk](#moving-every-file-to-another-disk) |
| Keep a backup disk in step with the primary | `filesystem.Sync`, `fs sync` | [Syncing two disks](#syncing-two-disks) |
| Answer a "give me my data" request | `jobs.ExportSubject` | [Exporting a subject's files](#exporting-a-subjects-files) |
| React the moment an object lands in the bucket | `contrib/notify` | [Bucket notifications](#bucket-notifications) |

## Without records, and on a calendar

The jobs that need no database (`UploadSweep`, `SweepAbandoned`, `Promote`), the `Job` type and the `Scheduler` also live in `github.com/ubgo/filesystem/jobs/diskjobs`, a module of its own that requires the core module only. An application without records requires it and downloads and builds none of records, sqlb or the SQL drivers (the River and Asynq runners require it alone too); `jobs` re-exports the same names as aliases, so the two mix.

A schedule runs every `Every`, or at the times `Next` names:

```go
s := diskjobs.NewScheduler(diskjobs.SchedulerOptions{OnRun: logReport})
err := s.Run(ctx,
	diskjobs.Schedule{Job: diskjobs.UploadSweep(disk, "pending", 24*time.Hour), Next: diskjobs.Daily(3, 0, time.UTC)},
	diskjobs.Schedule{Job: diskjobs.SweepAbandoned(disk, 48*time.Hour), Next: cronSchedule.Next},
)
```

`Daily(hour, minute, loc)` is "every day at 03:00"; a cron library's schedule plugs in by its `Next` method (robfig/cron's has that signature). Set `Every` or `Next`, not both. A `Next` that does not return a later time stops that schedule with an `invalid_argument` report instead of spinning.

On a queue, `jobs/asynq`'s `fsasynq.RegisterWith(ctx, scheduler, client, fsasynq.RegisterOptions{Queue: "maintenance", MaxRetry: 3, Timeout: time.Hour, Spec: specOf}, schedules...)` gives maintenance its own queue, retry budget and timeout; `Spec` returns each schedule's cronspec (`"0 3 * * *"`), which a `Next` schedule needs. `jobs/river`'s `fsriver.PeriodicWith(fsriver.PeriodicOptions{Queue: "maintenance", MaxAttempts: 3}, schedules...)` does the same, a `Next` schedule becoming River's calendar, and `WorkerOptions.Timeout` bounds one run. `EnqueueWith` on both puts an admin run on the same queue. Uniqueness is kept whatever the options.

One bound or window rarely fits every job: a sweep finishes in seconds while an export can take hours. Both queues take a per-job function. On Asynq, `RegisterOptions.Unique` sets how long a queued run blocks a second one (nil or 0 keeps the schedule's interval, else one hour; under one second is refused, since uniqueness cannot be turned off). On River, `WorkerOptions.TimeoutFor` bounds each job by name, falling back to `Timeout`:

```go
window := func(job string) time.Duration {
	if job == "export" {
		return 6 * time.Hour // an export that outlives an hour must not be queued twice
	}
	return 0 // the default
}
err := fsasynq.RegisterWith(ctx, scheduler, client, fsasynq.RegisterOptions{Queue: "maintenance", Unique: window}, schedules...)

worker, err := fsriver.NewWorker(fsriver.WorkerOptions{Timeout: 5 * time.Minute, TimeoutFor: func(job string) time.Duration {
	if job == "export" {
		return 3 * time.Hour
	}
	return 0 // Timeout
}}, jobList...)
```

## Dry runs

Every sweep that can say what it would take has a dry run that changes nothing:

| Sweep | Dry run | Reports |
|---|---|---|
| `records.Store.SweepTrash` | `records.Store.FindTrash` | every file it would purge |
| `records.Store.SweepOrphans` | `records.Store.FindOrphans` | every key it would delete |
| `records.Store.SweepRetention` | its `dryRun` argument | the attachments it would detach, the files it would purge |
| `upload.Sweep` | `upload.FindStale` | every object it would delete |
| `Disk.SweepAbandoned` | `Disk.FindAbandoned` (the `FindAbandoned` capability) | every unfinished multipart upload or staged file it would remove |

The jobs carry the same: `Job.DryRun` returns a `Result` whose `Count` is what a real run would act on and whose `Planned` names each item (file IDs or keys; for `SweepAbandoned`, `jobs.AbandonedItem` names each leftover). `Schedule{DryRun: true}` schedules the preview in place of the job, so a new sweep can run for a week, reporting through `OnRun` (`Report.DryRun` is true), before it is allowed to delete; a job without a dry run (`SweepAbandoned` on a disk without the `FindAbandoned` capability) is refused as a dry-run schedule with `CodeUnsupported`, whose message names the reason, and is never run for real in its place. On the command line, `fs sweep -dry-run` and `fs sync -dry-run` do the same.

<!-- ds:block id=previewtrash-k2m7qx4n -->
**previewTrash** · [`examples/maintenance/main.go:55-65`](../../examples/maintenance/main.go#L55-L65)

```go
func previewTrash(ctx context.Context, out io.Writer, store *records.Store, now time.Time) error {
	sweep := jobs.SweepTrash(store, trashRetention)
	plan, err := sweep.DryRun(ctx, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "dry run: would purge %d file(s)\n", plan.Count)
	done, err := sweep.Run(ctx, now)
	fmt.Fprintf(out, "sweep: purged %d file(s)\n", done.Count)
	return err
}
```
<!-- /ds:block hash=4ba815 -->

Steps for a new sweep:

1. Build the job (`jobs.SweepTrash(store, retention)` here) and call `DryRun` with the time the real run would use. Its `Count` and `Planned` are what the run would act on.
2. Schedule it with `Schedule{DryRun: true}` for a while and read the reports in `OnRun`, where `Report.DryRun` is true.
3. When the reports match what you expect, drop `DryRun` from the schedule so the job deletes.

Pitfalls:

- A dry run and the run that follows are two reads of a changing database: something trashed between them is purged by the run without having been in the plan. Treat the plan as a preview, not a lock.
- A job with no dry run is refused as a dry-run schedule with `CodeUnsupported`; it is never run for real in its place. `jobs.SweepAbandoned` has a dry run only on disks with the `FindAbandoned` capability ([Capabilities reference](../reference/capabilities.md)).
- `upload.Sweep` deletes every object under its prefix older than the cutoff, recorded or not. Point it only at a staging prefix; recorded files are swept with `SweepTrash`, `SweepOrphans` and `SweepRetention`.

## Integrity scrubbing

`jobs.Scrub` re-reads the objects of the files recorded on one disk, re-hashes each and compares it with the SHA-256 and size its row recorded. A missing object goes to `OnMissing`, a changed one to `OnMismatch`; nothing is deleted or rewritten, because restoring from a backup, re-uploading or purging is the application's decision. It checks `Batch` files per run (500 by default) and pauses `Interval` after each read, which caps the read rate and the egress bill. An object that cannot be read (an archived object, a permission error) goes to `OnError` and the run moves past it, so one bad object never stalls the scrub; only a database failure or a cancelled context stops a run. Its `Cursor` keeps the last file checked, so a pass over a large disk spreads over many runs and survives restarts; when a run reaches the last file the cursor goes back to empty and the next run starts a new pass. `jobs.ObjectCursor(disk, key)` keeps the cursor in an object; any `Cursor` (a database row, a Redis key) works.

<!-- ds:block id=scrub-p9w3re6t -->
**scrub** · [`examples/maintenance/main.go:70-81`](../../examples/maintenance/main.go#L70-L81)

```go
func scrub(ctx context.Context, out io.Writer, d deployment, now time.Time) error {
	job := jobs.Scrub(d.store, jobs.ScrubOptions{
		Disk:       "hot",
		Cursor:     jobs.ObjectCursor(d.cold, "_jobs/scrub-hot"),
		Interval:   10 * time.Millisecond, // at most 100 objects a second
		OnMissing:  func(f records.File) { fmt.Fprintf(out, "scrub: %s is missing\n", f.Filename) },
		OnMismatch: func(f records.File, _ int64, _ string) { fmt.Fprintf(out, "scrub: %s changed\n", f.Filename) },
	})
	res, err := job.Run(ctx, now)
	fmt.Fprintf(out, "scrub: checked %d file(s)\n", res.Count)
	return err
}
```
<!-- /ds:block hash=b4a5f8 -->

Steps:

1. Pick the disk to scrub and a `Cursor` that outlives the process (`jobs.ObjectCursor` keeps it as a small object on a disk), so a pass continues where the last run stopped.
2. Set `Interval` from the read rate you can afford: 10 ms here is at most 100 objects a second. Set `Batch` if 500 files per run is too many or too few.
3. Handle `OnMissing` and `OnMismatch`: alert, restore from a backup, or purge. Handle `OnError` for objects that cannot be read now.
4. Schedule it; one pass over the disk completes when the cursor returns to empty.

Pitfalls:

- Scrubbing reads every object, so on a cloud bucket it costs a GET and the egress of each one. Run it from inside the cloud's region, and size `Interval` and the schedule to the bill.
- A file recorded without a SHA-256 is checked on size only. Files the records layer stored itself always have one.
- Objects in an archive tier cannot be read without a restore: they arrive at `OnError` on every pass. Exclude that disk, or restore first.

## Moving every file to another disk

`jobs.MigrateDisk(store, from, to, opts)` moves the files recorded on `from` to `to`, a batch per run, while the application keeps serving them. Each file moves with `records.Store.MoveFile`: the object is copied and checked against the row (size, and SHA-256 when known) before the row changes, the row and the plugins' objects change in one transaction, and the old object is deleted only after the commit. A crash at any moment leaves every file readable from the disk its row names: before the commit the old object is untouched (at most an extra copy waits on `to`, overwritten by the next attempt), after it the new copy was verified. A file that fails goes to `OnFailed` and stays whole on `from`; the next pass retries it. The migration is complete when a pass moves nothing and fails nothing. Trashed files stay on `from` until purged.

<!-- ds:block id=migrate-b5n8ja2c -->
**migrate** · [`examples/maintenance/main.go:85-93`](../../examples/maintenance/main.go#L85-L93)

```go
func migrate(ctx context.Context, out io.Writer, d deployment, now time.Time) error {
	job := jobs.MigrateDisk(d.store, "hot", "cold", jobs.MigrateOptions{
		Cursor:   jobs.ObjectCursor(d.cold, "_jobs/migrate-hot"),
		OnFailed: func(f records.File, err error) { fmt.Fprintf(out, "migrate: %s stays on hot: %v\n", f.Filename, err) },
	})
	res, err := job.Run(ctx, now)
	fmt.Fprintf(out, "migrate: moved %d file(s) to cold\n", res.Count)
	return err
}
```
<!-- /ds:block hash=f7226b -->

Steps:

1. Add the new disk to the Manager beside the old one, under its own name.
2. Point new uploads at the new disk, so the set of files to move stops growing.
3. Schedule `jobs.MigrateDisk(store, from, to, opts)` with a `Cursor`; each run moves one batch while the application keeps serving every file from the disk its row names.
4. Watch `OnFailed`. A failed file stays whole on the old disk and is retried by the next pass.
5. When a pass moves nothing and fails nothing, purge or empty the trash on the old disk (trashed files stay there), then remove the old disk from the Manager.

Pitfalls:

- `MoveFile` refuses a copy that differs from the row, so a damaged object never moves: scrub first, restore what the scrub finds, then migrate (the program does exactly that).
- During the migration both disks must be reachable by every instance, and both must be in the Manager under the names the rows use.
- Each file is copied through your process when the two disks are different backends; budget the egress of the old backend.

## Syncing two disks

`filesystem.Sync(ctx, src, srcDir, dst, dstDir, opts)` copies every object under `srcDir` that is missing under `dstDir` or differs there. Two objects are the same when their sizes match and their SHA-256 digests, or else their ETags, match (`filesystem.SameContent`); anything less is copied again rather than wrongly skipped. Backends whose listings cannot prove equal content (a local disk, two clouds with different ETag schemes) would recopy every object on every run: `Checksum: true` reads and hashes both sides of a same-size pair instead, and `Same` replaces the comparison entirely. `Delete: true` removes target objects the source no longer has, only ever inside `dstDir` (each key is checked again before its deletion). When both disks reach the same backend, a scoped disk and its parent for instance, the directories are compared as that backend sees them and any overlap is refused (`CodeInvalidArgument`), so a sync can never copy into or delete its own source. Routed, overlaid, mirrored, cached and failover disks are followed to every target they reach; because a target could still escape that walk, `Delete` through any of them is refused (`CodeInvalidArgument`): sync the disks underneath instead. On a target with the `CaseInsensitive` capability, source keys that differ only in case (`A.txt`, `a.txt`) would overwrite each other there: each is reported as a failed copy with `CodeConflict` and neither is copied. Both listings are read before anything changes, each object succeeds or fails on its own, and `OnItem` receives every action. `fs sync [-delete] [-dry-run] [-checksum] SOURCE TARGET` is the same from a shell, with `-json` output.

<!-- ds:block id=syncbackup-r4t6vu8w -->
**syncBackup** · [`examples/maintenance/main.go:97-106`](../../examples/maintenance/main.go#L97-L106)

```go
func syncBackup(ctx context.Context, out io.Writer, cold, backup *filesystem.Disk) error {
	plan, err := filesystem.Sync(ctx, cold, "files", backup, "files", filesystem.SyncOptions{Delete: true, DryRun: true})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "sync dry run: %d to copy, %d to delete\n", plan.Copied, plan.Deleted)
	rep, err := filesystem.Sync(ctx, cold, "files", backup, "files", filesystem.SyncOptions{Delete: true})
	fmt.Fprintf(out, "sync: copied %d, deleted %d, unchanged %d\n", rep.Copied, rep.Deleted, rep.Unchanged)
	return err
}
```
<!-- /ds:block hash=49e502 -->

Steps:

1. Run `filesystem.Sync` with `DryRun: true` and read the counts (or `OnItem`) before the first real run, especially with `Delete: true`.
2. Choose how equality is decided: the default compares sizes and SHA-256 digests or ETags; set `Checksum: true` when the listings cannot prove equal content (a local disk, two clouds), or pass `Same`.
3. Run it for real, and schedule it if the target is a backup. From a shell, `fs sync -dry-run` and then `fs sync` do the same.

Pitfalls:

- Without `Checksum`, two backends whose listings report no common digest copy every object again on every run. That is correct but slow and costly; it is the reason for the option.
- `Delete` through a routed, overlaid, mirrored, cached or failover disk is refused; sync the disks underneath instead.
- On a case-insensitive target, keys that differ only in case are reported as `CodeConflict` and neither is copied: rename one at the source.

## Exporting a subject's files

`jobs.ExportSubject` bundles every live file a user uploaded (`Subject.UploadedBy`), or every file attached to an owner (`Subject.Owner`), into one zip on an export disk and returns a signed link that expires after `TTL`; a zero `TTL` is refused, so an export link always expires. The archive starts with `manifest.json` (ID, path, name, type, size, SHA-256 and dates of every file) and holds each file as `<file ID>/<safe filename>`. It is always written private, whatever the export disk's default visibility, so only the signed link reads it. It is streamed into storage as it is built, files are read from whichever disk holds them, and `MaxBytes` and `MaxFiles` refuse an oversized export before anything is written. Schedule `jobs.UploadSweep(exportDisk, Dir, TTL)` so expired archives are deleted; `Dir` may not be the root, so that sweep can only reach exports. Authorise the request before calling it: it exports whatever subject it is given.

<!-- ds:block id=exportuser-h3c9mz1d -->
**exportUser** · [`examples/maintenance/main.go:111-123`](../../examples/maintenance/main.go#L111-L123)

```go
func exportUser(ctx context.Context, out io.Writer, d deployment, user string, now time.Time) error {
	exp, err := jobs.ExportSubject(ctx, d.store, d.exports, jobs.Subject{Scope: "acme", UploadedBy: user}, jobs.ExportOptions{
		Dir: exportDir, TTL: exportTTL, Filename: user + "-export.zip",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "export: %d file(s), %d bytes, link valid until %s\n", exp.Files, exp.Bytes, exp.ExpiresAt.Format(time.Kitchen))
	// Scheduled in production; here run once, after the link expired.
	swept, err := jobs.UploadSweep(d.exports, exportDir, exportTTL).Run(ctx, now.Add(exportTTL+time.Minute))
	fmt.Fprintf(out, "export sweep: deleted %d expired export(s)\n", swept.Count)
	return err
}
```
<!-- /ds:block hash=667939 -->

Steps:

1. Authorise the request: the user asking may export only their own files, an admin only within their scope.
2. Call `jobs.ExportSubject` with the subject (`UploadedBy` or `Owner`), an export disk, a `Dir` that is not the root and a `TTL`; set `MaxBytes` and `MaxFiles` for your largest expected account.
3. Send the user the signed link it returns; it expires after `TTL`.
4. Schedule `jobs.UploadSweep(exportDisk, Dir, TTL)` so expired archives are deleted.

Pitfalls:

- An export runs for as long as reading every file takes. For large accounts run it in a job and mail the link, rather than inside an HTTP request.
- Only live files are exported; trashed files are not. Say so in your data-request policy.
- The export disk needs signed URLs: `Presign`, or a `URLSigner` with `fshttp.Signed` mounted.

## Bucket notifications

`contrib/notify` turns a backend's own object events into one `notify.Event` (provider, bucket, key, created or deleted, size, ETag, time) and calls a `notify.Handler`: the place to confirm a direct upload the moment its object lands instead of waiting for the browser to say so. Each cloud has its own module, so an application downloads only its SDK:

| Module | Source | Authentication |
|---|---|---|
| `contrib/notify/sqs` | S3 (or S3-compatible) events in an SQS queue, directly or through SNS | the SQS client signs every request with IAM credentials |
| `contrib/notify/pubsub` | GCS notifications in a Pub/Sub subscription, pulled | the client's Google credentials; pull-only, so there is no push endpoint to forge |
| `contrib/notify/eventgrid` | Azure Blob events delivered by Event Grid to a webhook | `Authorize` (required): `QuerySecret` or `HeaderSecret`, compared in constant time; the subscription validation handshake is answered only for authorised requests |

Every consumer requires `Buckets` and acknowledges events from any other bucket without handling them (`OnRejected`), since anyone allowed to publish to the queue could name any bucket; for Event Grid an entry is `<storage account>/<container>`, since container names repeat across accounts. `Event.ID` (`notify.EventID` of account, bucket, key and the provider's ID) is the deduplication key in every consumer. A message that is not a notification is reported to `OnError` (wrapping `notify.ErrMalformed`) and removed so it is not delivered forever, or with `KeepMalformed` left for a dead-letter queue. A Handler error leaves the event to be delivered again (SQS: the message stays; Pub/Sub: nack; Event Grid: 500), so handlers must be idempotent, and an event is a claim about an object: `Stat` it before trusting its size.

The program [examples/maintenance-notify](../../examples/maintenance-notify) confirms direct uploads this way, with `contrib/notify/sqs` reading an in-memory queue that stands in for SQS (the consumer takes the `sqs.API` interface, which `*sqs.Client` implements). The handler confirms each object created under `incoming/` against the upload policy, and remembers the events it has handled, because a queue delivers at least once:

<!-- ds:block id=confirmer-handle-9z5tacrg -->
**Confirmer.Handle** · [`examples/maintenance-notify/main.go:70-95`](../../examples/maintenance-notify/main.go#L70-L95)

```go
func (c *Confirmer) Handle(ctx context.Context, e notify.Event) error {
	if e.Kind != notify.KindCreated || !filesystem.KeyInDir(e.Key, incomingDir) {
		return nil
	}
	if c.seen[e.ID] {
		fmt.Fprintf(c.Out, "duplicate %s: skipped\n", e.Key)
		return nil
	}
	if err := c.fail; err != nil {
		c.fail = nil
		return err
	}
	res, err := upload.Confirm(ctx, c.Disk, e.Key, path.Base(e.Key), avatars)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeRejected || filesystem.CodeOf(err) == filesystem.CodeTooLarge:
		fmt.Fprintf(c.Out, "refused %s: %s, object deleted\n", e.Key, filesystem.CodeOf(err))
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		fmt.Fprintf(c.Out, "gone %s: deleted before the event arrived\n", e.Key)
	case err != nil:
		return err // anything else is worth another delivery
	default:
		fmt.Fprintf(c.Out, "confirmed %s: %s, %d bytes\n", res.Key, res.ContentType, res.Size)
	}
	c.seen[e.ID] = true
	return nil
}
```
<!-- /ds:block hash=95ae1a -->

Returning nil acknowledges the event; returning an error leaves it in the queue for another delivery. So the handler returns an error only for what may succeed later (the database is down), and acknowledges everything that never will: a refused file (which `upload.Confirm` has already deleted), an object deleted before its event arrived, a key outside `incoming/`.

The consumer names the one bucket it serves, and reports what it acknowledges without handling:

<!-- ds:block id=newconsumer-9gz6djdp -->
**newConsumer** · [`examples/maintenance-notify/main.go:101-117`](../../examples/maintenance-notify/main.go#L101-L117)

```go
func newConsumer(q sqs.API, c *Confirmer, out io.Writer) (*sqs.Consumer, error) {
	return sqs.New(q, sqs.Options{
		QueueURL: queueURL,
		Buckets:  notify.Buckets{bucketName},
		Handler:  c.Handle,
		OnRejected: func(e notify.Event) {
			fmt.Fprintf(out, "rejected an event from bucket %q\n", e.Bucket)
		},
		OnError: func(err error) {
			if errors.Is(err, notify.ErrMalformed) {
				fmt.Fprintln(out, "dropped a message that is not an S3 event")
				return
			}
			fmt.Fprintf(out, "left for redelivery: %v\n", err)
		},
	})
}
```
<!-- /ds:block hash=1ccd3a -->

Steps:

1. Configure the bucket to publish object-created events for the upload prefix to a queue (S3 to SQS, directly or through SNS), a Pub/Sub topic (GCS), or an Event Grid subscription to your webhook (Azure).
2. Write one `notify.Handler`: filter on `Kind` and the key's directory, deduplicate on `Event.ID`, then `Stat` or `Confirm` the object; never trust the event's size alone.
3. Build the consumer for your provider with `Buckets` set to the buckets you own, and `OnError` wired to your logs.
4. Run it: `consumer.Run(ctx)` in a goroutine for SQS and Pub/Sub, or mount `eventgrid.Handler` with `Authorize` set for Azure.

Check: `go run ./maintenance-notify` in `examples/` prints:

```
left for redelivery: sqs: message m1: database unavailable
refused incoming/avatar-2.png: rejected, object deleted
rejected an event from bucket "other-bucket"
dropped a message that is not an S3 event
poll 1: 3 acknowledged, 1 left in the queue
confirmed incoming/avatar 1.png: image/png, 72 bytes
poll 2: 1 acknowledged, 0 left in the queue
duplicate incoming/avatar 1.png: skipped
poll 3: 1 acknowledged, 0 left in the queue
stored: [incoming/avatar 1.png]
```

The first event fails on purpose and is received again by the second poll; the HTML file named `.png` is refused and deleted; the same event delivered a third time is skipped.

Pitfalls:

- Events arrive at least once and not always in order. Keep handled `Event.ID`s in your database (a unique column), not in memory, so a restart or a second instance does not handle an event twice.
- An event is a claim about an object, possibly a stale one: the object may have been overwritten or deleted since. Read the object, not the event.
- Returning an error for a file that will never pass (a refused type) makes the queue deliver it until its dead-letter limit. Acknowledge permanent failures.
- Never trust the bucket name in an event to pick a disk: anyone allowed to publish to the queue can name any bucket. `Buckets` is required for that reason.
- Notifications are a second path, not a replacement: keep the client's own "upload finished" call too, and let whichever arrives second find the file already confirmed.

### Versions, delete markers and other events

Every `notify.Event` carries `Name`, the provider's own event type, and `VersionID`, the S3 `versionId` or GCS generation (empty for Azure). On a versioned S3 bucket a delete creates a delete marker and older versions remain: it arrives as `KindDeleted` with `Name` `ObjectRemoved:DeleteMarkerCreated`, so check `Name` before treating the data as gone.

Consumers drop other event types by default. Set `PassOther` on `sqs.Options`, `pubsub.Options` or `eventgrid.Options` to receive them as `notify.KindOther` (a Glacier restore completing, a tier or metadata change):

```go
consumer, err := sqs.New(client, sqs.Options{
	QueueURL: queueURL, Buckets: notify.Buckets{"media"}, PassOther: true,
	Handler: func(ctx context.Context, e notify.Event) error {
		if e.Kind == notify.KindOther && e.Name == "ObjectRestore:Completed" {
			return markRestored(ctx, e.Key, e.VersionID)
		}
		return handle(ctx, e)
	},
})
```

An Event Grid subscription created with the CloudEvents 1.0 schema needs `eventgrid.Options{CloudEvents: true}`: the handler then reads single events and batches in that schema and answers the `OPTIONS` abuse-protection handshake once `Authorize` accepts it.

## Try it

From the `examples` directory:

```sh
go run ./maintenance
go run ./maintenance-notify
go test ./maintenance ./maintenance-notify
```

The first prints the output at the top of this page; the second the output in [Bucket notifications](#bucket-notifications).

## Related

- [Maintenance jobs](../concepts/maintenance-jobs.md): the scheduler, River and Asynq, and every built-in job.
- [Integrity](../concepts/integrity.md): what scrubbing checks, and the checks every write already makes.
- [Consistency](../concepts/consistency.md): why sweeps exist, and what each one may delete.
- [Move to S3](../recipes/move-to-s3.md) and [Backups and mirrors](../recipes/backups-and-mirrors.md): migration and sync as recipes.
- [Browser direct uploads](../recipes/browser-direct-uploads.md): the uploads bucket notifications confirm.
