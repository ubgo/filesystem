# Consistency

An object store and a database never share a transaction. An upload writes bytes to S3 and a row to Postgres; either step can fail, and a crash can come between them. This page explains the guarantees the library gives for a single object, the ordering rules the records module uses so a row never points at missing bytes, what can still be left behind and how to clean it up, and the guarantees (and limits) of moving, mirroring and conditional writes. Anyone storing file rows next to their bytes needs it.

The snippets come from `examples/concepts-consistency`, which uses the records module on SQLite and a memory disk. Run it with `go run ./concepts-consistency` from the `examples` directory.

## One object

Within a single disk, these hold on every driver (the conformance suite checks them):

- A `Write` replaces the object atomically: readers see the old bytes or the new bytes, never a mix. The local driver writes to a staging file and renames it into place, so a crash leaves the old file or the new one; object stores commit a PUT atomically.
- A write whose body fails, or passes `MaxSize`, stores nothing.
- `Delete` of a missing key succeeds, so deletes are idempotent and safe to retry.
- `IfNotExists` (with the `ConditionalWrite` capability) fails with `CodeExists` when the key exists, atomically: of two writers, exactly one wins.

<!-- ds:block id=conditionalwrite-d37uctzq -->
**conditionalWrite** · [`examples/concepts-consistency/mirror.go:20-32`](../../examples/concepts-consistency/mirror.go#L20-L32)

```go
func conditionalWrite(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("locks", memory.New())
	if err != nil {
		return err
	}
	opts := filesystem.WriteOptions{IfNotExists: true}
	if _, err := disk.WriteBytes(ctx, "jobs/nightly.lock", []byte("worker-1"), opts); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "jobs/nightly.lock", []byte("worker-2"), opts)
	fmt.Fprintf(out, "second IfNotExists write: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=fb4b27 -->

Check: `second IfNotExists write: exists`. On S3 the capability must be declared with `conditional_write=true`, because a provider that ignores `If-None-Match` would turn the conditional write into a silent overwrite; see [S3-compatible stores](../adapters/s3-compatible.md).

What is not atomic: a `Move` on a backend without the `Move` capability (S3, GCS, Azure), which is a copy then a delete; `MoveAcross` between disks; a `DeletePrefix` or `DeleteKeys` interrupted part-way. Each returns the error, and each is safe to run again.

## Bytes and rows: the two rules

The records module keeps files as rows (`fs_files`) next to their objects. It follows two rules, so the only inconsistency that can ever exist is an object without a row, never a row without bytes:

| Direction | Rule | Why |
|---|---|---|
| In (upload, replace) | **Object first, row second.** The object is stored, then the row is inserted. `CreateFile` checks the object exists before inserting. | a row is visible to other requests at commit; its bytes must already be there |
| Out (purge, replace) | **Row first, object last.** The row is deleted inside the transaction; the object is deleted only after the transaction commits. | a rollback must find the bytes still there for the row it restores |

A row without bytes is a broken file a user can see. An object without a row is wasted storage nobody sees. The rules choose the harmless failure.

## Step 1 — A rolled-back upload leaves nothing

`Store.Upload` stores the object, then inserts the row. If the insert fails, it deletes the object again. If the upload ran inside the application's transaction (`Store.Tx`) and something later in that transaction fails, the rollback removes the row, and the Store deletes the object it wrote (`Store.OnRollback`):

<!-- ds:block id=rolledbackupload-v4hfbev6 -->
**rolledBackUpload** · [`examples/concepts-consistency/steps.go:19-32`](../../examples/concepts-consistency/steps.go#L19-L32)

```go
func rolledBackUpload(ctx context.Context, d *demo, out io.Writer) error {
	err := d.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "invoice.pdf", policy); err != nil {
			return err
		}
		return errAppFailed
	})
	objects, rows, serr := d.state(ctx)
	if serr != nil {
		return serr
	}
	fmt.Fprintf(out, "rolled back upload: %v; objects %d, rows %d\n", errors.Is(err, errAppFailed), objects, rows)
	return nil
}
```
<!-- /ds:block hash=d239a4 -->

Check: `rolled back upload: true; objects 0, rows 0`.

`Store.Tx(ctx, fn)` runs `fn` in one transaction; every Store call made with the `ctx` that `fn` receives joins it, and so can the application's own statements on the `*sql.Tx`. That is how a post and its attachments are saved together or not at all.

## Step 2 — No row for missing bytes

`CreateFile` records an object that is already stored (an upload confirmed by `upload.Confirm`, a file a job produced). It checks the object exists first:

<!-- ds:block id=objectfirst-e5ygz2n4 -->
**objectFirst** · [`examples/concepts-consistency/steps.go:37-41`](../../examples/concepts-consistency/steps.go#L37-L41)

```go
func objectFirst(ctx context.Context, d *demo, out io.Writer) error {
	_, err := d.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskName, Key: "uploads/never-written.pdf", Filename: "x.pdf"})
	fmt.Fprintf(out, "record a missing object: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=bcc3ff -->

Check: `record a missing object: not_found`.

## Step 3 — Purge deletes the object after the commit

`Store.Purge` deletes the row (and its attachments, after the plugins' `BeforePurge` hooks) in one transaction, and the object only once that transaction commits. Purge inside a transaction that rolls back leaves both; purge that commits removes both:

<!-- ds:block id=purgeaftercommit-cjatkcy8 -->
**purgeAfterCommit** · [`examples/concepts-consistency/steps.go:47-72`](../../examples/concepts-consistency/steps.go#L47-L72)

```go
func purgeAfterCommit(ctx context.Context, d *demo, out io.Writer) error {
	f, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "keep.pdf", policy)
	if err != nil {
		return err
	}
	err = d.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := d.store.Purge(ctx, scope, f.ID); err != nil {
			return err
		}
		return errAppFailed
	})
	objects, rows, serr := d.state(ctx)
	if serr != nil {
		return serr
	}
	fmt.Fprintf(out, "purge rolled back: %v; objects %d, rows %d\n", errors.Is(err, errAppFailed), objects, rows)
	if err := d.store.Purge(ctx, scope, f.ID); err != nil {
		return err
	}
	objects, rows, err = d.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "purge committed: objects %d, rows %d\n", objects, rows)
	return nil
}
```
<!-- /ds:block hash=9729db -->

Check: `purge rolled back: true; objects 1, rows 1`, then `purge committed: objects 0, rows 0`.

If the object deletion fails after the commit (the bucket is unreachable at that moment), the row is already gone, so the object becomes an orphan. Called outside a transaction, `Purge` returns that error; inside one, the failure goes to the handler set with `records.WithObjectErrorHandler`, because the transaction has committed and cannot report back. Log it there and let the orphan cleanup below catch the object. `Replace`, `SetVisibility` with two disks, `PurgeOwner` and `SweepTrash` follow the same rules; plugins that own objects (kept versions, image variants) use `Store.DeleteObjectAfterCommit` to do the same.

## Step 4 — Cleaning up orphans

Orphan objects come from a crash between storing an object and committing its row, from an upload whose cleanup also failed, and from a post-commit deletion that failed. They are harmless to users and cost storage. Two ways to remove them:

- **A staging prefix with a sweep or a lifecycle rule.** When objects land under a prefix nothing records (presigned uploads waiting for `Confirm`), `upload.Sweep` or a bucket lifecycle rule deletes what is older than a cutoff. Sweep deletes by age only, so never point it at a prefix holding recorded files.
- **A reference check.** For the prefix where recorded files live, delete only objects no row points at, and only those older than a grace period, because an upload in flight has stored its object and not yet committed its row. Applications may read the `fs_` tables freely (only writes must go through the Store), so the check is one query per object:

<!-- ds:block id=findorphans-4kdhftcx -->
**findOrphans** · [`examples/concepts-consistency/orphans.go:30-69`](../../examples/concepts-consistency/orphans.go#L30-L69)

```go
func findOrphans(ctx context.Context, d *demo, out io.Writer) error {
	cutoff := time.Now().Add(-orphanGrace)
	if _, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "kept.pdf", policy); err != nil {
		return err
	}
	if _, err := d.disk.WriteBytes(ctx, uploadDir+"/in-flight.pdf", []byte(pdf), filesystem.WriteOptions{}); err != nil {
		return err
	}
	d.age = 2 * orphanGrace
	_, err := d.disk.WriteBytes(ctx, uploadDir+"/crashed.pdf", []byte(pdf), filesystem.WriteOptions{})
	d.age = 0
	if err != nil {
		return err
	}
	var orphans []string
	for e, err := range d.disk.List(ctx, uploadDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		if !e.ModTime.Before(cutoff) {
			continue // possibly an upload still in flight
		}
		var n int
		if err := d.db.QueryRowContext(ctx, recordedQuery, d.disk.Name(), e.Key).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			orphans = append(orphans, e.Key)
		}
	}
	if err := d.disk.DeleteKeys(ctx, orphans); err != nil {
		return err
	}
	objects, rows, err := d.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "orphans deleted %v; objects %d, rows %d\n", orphans, objects, rows)
	return nil
}
```
<!-- /ds:block hash=1e8f5b -->

Check: `orphans deleted [uploads/crashed.pdf]; objects 2, rows 1`: the old unrecorded object was deleted, the recorded file and the fresh unrecorded one (an upload in flight) were kept. With the records module, `Store.SweepOrphans(ctx, disk, prefix, olderThan)` is this check built in: it lists in batches, checks each batch with one query on `fs_files` (by disk and key, live or trashed), asks every plugin that implements `records.ObjectOwner` (versions, image variants) which objects it holds, and deletes the rest that are older than the cutoff. Schedule it; the library starts no background work.

## Step 5 — Mirrors converge

`filesystem.Mirror(name, primary, mirrors...)` returns a disk that reads from the primary and writes to the primary and then to every mirror: the way to move to a new backend without downtime (mirror old to new, backfill, switch the primary) or to keep a warm copy. Its semantics are deliberate:

- Not atomic across disks. A write succeeds on the primary first; if a mirror then fails, the error is `unavailable` (naming the mirrors that failed) and the object exists on the primary only. Writes are overwrites, so writing again converges.
- Mirrors receive the bytes by reading the object back from the primary, because the caller's body can be read only once.
- Signed PUT URLs and presigned parts are refused: a client uploading straight to the primary would bypass the mirrors. Server-side multipart is allowed and replicated on completion.
- Deletes, copies, moves and visibility changes go to the primary, then to each mirror.

<!-- ds:block id=mirrorconverges-zsn6p4n9 -->
**mirrorConverges** · [`examples/concepts-consistency/mirror.go:38-70`](../../examples/concepts-consistency/mirror.go#L38-L70)

```go
func mirrorConverges(ctx context.Context, out io.Writer) error {
	down := true
	outage := layer.Guard(func(context.Context, *filesystem.Op) error {
		if down {
			return filesystem.NewError(filesystem.CodeUnavailable, errMirrorDown)
		}
		return nil
	})
	primary, err := filesystem.NewDisk("s3-old", memory.New())
	if err != nil {
		return err
	}
	replica, err := filesystem.NewDisk("s3-new", memory.New(), filesystem.WithInterceptors(outage))
	if err != nil {
		return err
	}
	both, err := filesystem.Mirror("media", primary, replica)
	if err != nil {
		return err
	}
	body := []byte("%PDF-1.7 mirrored")
	_, err = both.WriteBytes(ctx, "a.pdf", body, filesystem.WriteOptions{})
	onPrimary, _ := primary.Exists(ctx, "a.pdf")
	onReplica, _ := replica.Exists(ctx, "a.pdf")
	fmt.Fprintf(out, "mirror down: %s; primary %t, replica %t\n", filesystem.CodeOf(err), onPrimary, onReplica)
	down = false
	if _, err := both.WriteBytes(ctx, "a.pdf", body, filesystem.WriteOptions{}); err != nil {
		return err
	}
	onReplica, _ = replica.Exists(ctx, "a.pdf")
	fmt.Fprintf(out, "written again: replica %t\n", onReplica)
	return nil
}
```
<!-- /ds:block hash=18ad75 -->

Check: `mirror down: unavailable; primary true, replica false`, then `written again: replica true`. A backfill job (list the primary, `CopyAcross` what the mirror lacks) brings over everything written before the mirror existed. See [Backups and mirrors](../recipes/backups-and-mirrors.md) and [Move to S3](../recipes/move-to-s3.md).

## Retries and idempotency

Every operation the Disk marks replayable is safe to repeat: deletes succeed on missing keys, writes of a rewindable body overwrite with the same bytes, copies overwrite their destination. `layer.Retry` retries only those. A write from a one-shot stream, a `move` and a multipart completion are never retried automatically, because repeating them after an unknown outcome is unsafe or impossible. Code that retries at a higher level (a job queue) should give each upload a key of its own choosing, so a second run overwrites rather than duplicating.

## Pitfalls

- Inserting your own row for a file before the upload has stored its object: another request can read the row and find nothing. Use `Store.Upload` or `CreateFile`, which follow the order.
- Deleting an object before the database transaction that deletes its row has committed: a rollback restores a row whose bytes are gone. Use `Purge`, or `Store.DeleteObjectAfterCommit` in your own code.
- Pointing `upload.Sweep` at the prefix where recorded files live: it deletes them.
- Treating a mirror error as "nothing was written": the primary has the object. Retry the write, do not compensate by deleting.

## Next

- [Files as records](../records/README.md) and [Lifecycle](../records/lifecycle.md): trash, restore, purge and the sweeps built on these rules.
- [Interceptors](interceptors.md): what Retry repeats.
- [Uploads](uploads.md): Confirm and Sweep for direct uploads.
