# Lifecycle: trash, restore, purge

Files end in two steps. The trash hides a file without losing it, so a user can undo a delete; purge removes the rows and then the bytes for good. This page covers both, a purge that rolls back, deleting everything an owner leaves behind when your app deletes the owner, a retention sweep that empties the trash, and what happens when deleting an object fails after the database has committed. Read it before you build a delete button, an account deletion, or a "deleted files are kept for 30 days" promise.

## Before you start

- A Store, as built in [Files](files.md), and attachments as in [Attachments](attachments.md).
- The example is [`examples/records-lifecycle`](../../examples/records-lifecycle): SQLite, two disks over one memory driver (one of them read-only, to make deletions fail on purpose), a settable clock, and an application `users` table. `go run ./records-lifecycle` in `examples/` prints each step; `go test ./records-lifecycle` runs them, including a rolled-back account deletion.
- [Consistency](../concepts/consistency.md) explains why rows go first and objects last.

## Step 1 — Wire the Store

<!-- ds:block id=newdisks-hvtukymk -->
**newDisks** · [`examples/records-lifecycle/main.go:85-96`](../../examples/records-lifecycle/main.go#L85-L96)

```go
func newDisks() (*filesystem.Manager, error) {
	drv := memory.New()
	media, err := filesystem.NewDisk(diskName, drv)
	if err != nil {
		return nil, err
	}
	archive, err := filesystem.NewDisk(archiveName, drv, filesystem.WithInterceptors(filesystem.ReadOnly()))
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(media, archive)
}
```
<!-- /ds:block hash=ac12aa -->

<!-- ds:block id=newstore-8hms7sx8 -->
**newStore** · [`examples/records-lifecycle/main.go:108-133`](../../examples/records-lifecycle/main.go#L108-L133)

```go
func newStore(ctx context.Context, db *sql.DB, clock *Clock, failed *[]string) (*records.Store, error) {
	disks, err := newDisks()
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser),
		records.WithClock(clock.Now),
		records.WithObjectErrorHandler(func(_ context.Context, f records.File, err error) {
			*failed = append(*failed, f.Disk+"/"+f.Key+": "+string(filesystem.CodeOf(err)))
		}),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	st, err := usersDDL(records.EngineSQLite)
	for _, q := range st {
		if err == nil {
			_, err = db.ExecContext(ctx, q)
		}
	}
	return s, err
}
```
<!-- /ds:block hash=fcf48a -->

The clock (`records.WithClock`) is there so the demo can age the trash; production code leaves it out. `records.WithObjectErrorHandler` is explained in step 6.

## Step 2 — Trash and restore

`Trash(ctx, scope, id)` sets the file's `DeletedAt`. The file stays stored and stays attached, but `Files` (unless `Trashed: true`) and `Attachments` no longer list it. `Restore(ctx, scope, id)` clears `DeletedAt`, and the file reappears in its folder, its tags and its owners' lists exactly where it was.

A public file's object stops answering at its public URL while it is trashed. With `WithVisibilityDisks` the object moves to the private disk (the row's `Disk` changes, its `Visibility` stays `public`) and `Restore` moves it back; otherwise its disk makes it private in place and `Restore` makes it public again. The object changes before the row and is put back if the transaction fails, as with `SetVisibility`. A disk without the `SetVisibility` capability cannot hide one object, so there the object stays as it is: keep public files on their own disk with `WithVisibilityDisks`. The versions plugin moves or hides a file's kept versions the same way; a signed URL issued before the trash still works until it expires.

<!-- ds:block id=trashcounts-q9ep7g6t -->
**trashCounts** · [`examples/records-lifecycle/main.go:149-159`](../../examples/records-lifecycle/main.go#L149-L159)

```go
func trashCounts(ctx context.Context, s *records.Store) (live, trashed int, err error) {
	page, err := s.Files(ctx, records.FileQuery{Scope: tenant})
	if err != nil {
		return 0, 0, err
	}
	bin, err := s.Files(ctx, records.FileQuery{Scope: tenant, Trashed: true})
	if err != nil {
		return 0, 0, err
	}
	return len(page.Files), len(bin.Files), nil
}
```
<!-- /ds:block hash=ac9a3a -->

<!-- ds:block id=trashdemo-jwggkmyz -->
**trashDemo** · [`examples/records-lifecycle/main.go:247-263`](../../examples/records-lifecycle/main.go#L247-L263)

```go
func trashDemo(ctx context.Context, w io.Writer, s *records.Store, _ *Clock) error {
	f, err := store(ctx, s, diskName, "trash/a.txt")
	if err != nil {
		return err
	}
	if err := s.Trash(ctx, tenant, f.ID); err != nil {
		return err
	}
	live, trashed, err := trashCounts(ctx, s)
	fmt.Fprintf(w, "after Trash: live=%d trashed=%d %v\n", live, trashed, err)
	if err := s.Restore(ctx, tenant, f.ID); err != nil {
		return err
	}
	live, trashed, err = trashCounts(ctx, s)
	fmt.Fprintf(w, "after Restore: live=%d trashed=%d %v\n", live, trashed, err)
	return s.Purge(ctx, tenant, f.ID)
}
```
<!-- /ds:block hash=31033f -->

Check: `after Trash: live=0 trashed=1`, then `after Restore: live=1 trashed=0`.

Details that matter:

- Trashing a trashed file, or restoring a live one, does nothing and emits no event. Re-trashing in particular does not restart the clock `SweepTrash` purges by.
- `File(ctx, scope, id)` still returns a trashed file; check `DeletedAt` before serving it.
- While trashed, a file cannot be attached, replaced or shared: `Attach`, `Replace` and `share.Create` answer `not_found`, and `recordshttp`'s `GET /files/{id}` answers 404.
- A trashed file still occupies storage, so the [quota plugin](../plugins/quota.md) still counts it until it is purged.
- Unknown IDs are `not_found`.

## Step 3 — Purge, and why a rollback is safe

`Purge(ctx, scope, id)` deletes a file for good, live or trashed (there is no need to trash first): in one transaction it runs every plugin's `BeforePurge` hook (the bundled plugins drop the file's folder place, tags, share links, versions and variants, and quota releases its space), deletes the file's attachments and its row, and emits `file.purged`. The object is deleted only after the transaction commits. The file's row is locked for the transaction, so an `Attach` racing the purge either commits first (and its attachment is purged with the file) or waits and then finds no file. `Purge(ctx, scope, id, records.IfTrashed(true))` purges only if the file is still in the trash when its row is locked, and `unavailable` otherwise: use it when the decision to purge was made from a file read earlier, as the [HTTP purge route](http-endpoints.md) does.

<!-- ds:block id=purgethenfail-b6b94u7z -->
**purgeThenFail** · [`examples/records-lifecycle/main.go:174-181`](../../examples/records-lifecycle/main.go#L174-L181)

```go
func purgeThenFail(ctx context.Context, s *records.Store, f records.File) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := s.Purge(ctx, tenant, f.ID); err != nil {
			return err
		}
		return errAbort
	})
}
```
<!-- /ds:block hash=e7816c -->

Here the purge runs inside a transaction that fails afterwards. Because the object deletion was only scheduled for the commit, the rollback leaves both the row and the bytes. Check: `rolled-back purge: err=request failed after the purge row kept=true object kept=true`. Purged for real, the demo prints `purged: row=not_found object kept=false`.

A plugin's `BeforePurge` can refuse the purge (a legal hold, see [Events and hooks](events-and-hooks.md)); its error rolls everything back.

## Step 4 — Delete what an owner leaves behind

When your app deletes a user, a post or a workspace, its files must go too, except the ones something else still uses. `PurgeOwner(ctx, scope, owner)` detaches every attachment of the owner, in every role, and purges every file that was attached only to that owner, objects after the commit. It returns how many files it purged. Call it in the transaction that deletes the owner's own row, so both happen or neither:

<!-- ds:block id=deleteuser-mk6djv64 -->
**deleteUser** · [`examples/records-lifecycle/main.go:187-198`](../../examples/records-lifecycle/main.go#L187-L198)

```go
func deleteUser(ctx context.Context, s *records.Store, userID string) (int, error) {
	purged := 0
	err := s.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID); err != nil {
			return err
		}
		n, err := s.PurgeOwner(ctx, tenant, records.Owner{Type: ownerUser, ID: userID})
		purged = n
		return err
	})
	return purged, err
}
```
<!-- /ds:block hash=8f1671 -->

`Store.Tx` passes your function the `*sql.Tx`; your own `DELETE` runs on it, and every Store call made with the `ctx` it receives joins the same transaction. Check: Alice owns one file alone and shares another with Bob; the demo prints `PurgeOwner: purged=1 err=<nil> bob still has 1 file(s)`. The example's `TestDeleteUserRollsBackTogether` fails the outer transaction after `deleteUser` and finds the user row, the file row and the object all still there.

An unregistered owner type is `invalid_argument`. `PurgeOwner` works from attachments, so it cannot find a file the user uploaded and never attached (or detached with `PreviousKeep`). An owner is a row of your app and an uploader is an actor (`File.UploadedBy`, from `records.WithActor`), and only your app knows which actor an owner was, so purge those uploads by actor in the same transaction: `PurgeUnattached(ctx, scope, actor, filters...)` purges every file of the scope that the actor uploaded and no attachment uses, live or trashed, and returns how many. Files the actor uploaded that another owner uses are attached, so they stay. Each file is checked again under a lock on its row just before it is purged, and `Attach` holds a shared lock on the file it attaches, so a file attached while the purge runs is kept rather than purged under the new attachment. A file a plugin keeps somewhere (a folder of a shared drive) has no attachment and counts as unattached: pass `filters`, conditions on `records.TableFiles` as in `FileQuery.Filters`, to narrow the purge (a plugin's condition, `sqlb.Less(records.FileCreatedAt, cutoff)`). An empty actor is `invalid_argument`, because `""` stands for every upload made without one. The [account deletion recipe](../recipes/account-deletion.md) puts this together.

## Step 5 — Empty the trash on a schedule

`SweepTrash(ctx, cutoff)` purges every file, in every scope, trashed before `cutoff`, and returns how many it purged. The library starts no background work: call it from your scheduler (a cron job, a ticker in a worker) with `now` minus your retention.

<!-- ds:block id=sweep-q4hu6fkd -->
**sweep** · [`examples/records-lifecycle/main.go:203-205`](../../examples/records-lifecycle/main.go#L203-L205)

```go
func sweep(ctx context.Context, s *records.Store, now time.Time) (int, error) {
	return s.SweepTrash(ctx, now.Add(-trashRetention))
}
```
<!-- /ds:block hash=bebe48 -->

Check: one file trashed 40 days ago and one 10 days ago, with 30 days of retention: `SweepTrash: purged=1 err=<nil> still in trash=1`.

Each file is purged in its own transaction, so call `SweepTrash` outside one. A file that cannot be purged (a database failure, or a plugin refusing it, such as a legal hold) does not stop the sweep: the other files are still purged, and the returned error joins one error per failed file, naming its ID and scope, so one stuck file never holds back the rest. An object that cannot be deleted after its rows were purged goes to the `WithObjectErrorHandler` handler (step 6) rather than into the sweep's error, and is left for `SweepOrphans`. A cancelled context stops the sweep. Log the error and keep the schedule.

## Bulk changes

`TrashMany`, `RestoreMany` and `PurgeMany(ctx, scope, ids)` and `SetVisibilityMany(ctx, scope, ids, v)` apply one change to a selection, the toolbar of a file manager. Each file changes in its own transaction, as if you had called `Trash` (or `Restore`, `Purge`, `SetVisibility`) for it, so one file that fails (not found, refused by a plugin such as a legal hold, a backend error) never undoes or blocks the others. They return one `records.BulkResult{ID, Err}` per distinct ID, in the order given; a duplicate ID counts once. An object whose deletion fails after `PurgeMany` purged its rows goes to the `WithObjectErrorHandler` handler, as with `SweepTrash`.

The call itself is refused, changing nothing, with `invalid_argument` for more than `records.MaxBulk` (1000) IDs, an unknown visibility, or when it runs inside a `Store.Tx`: there every file would share one transaction, and on Postgres the first failure would abort it for the rest. A cancelled context stops the run; the files not reached get its error.

<!-- ds:block id=trashselection-h2p6xw -->
**trashSelection** · [`examples/records-query/main.go:194-208`](../../examples/records-query/main.go#L194-L208)

```go
func trashSelection(ctx context.Context, s *records.Store, ids []string) (string, error) {
	results, err := s.TrashMany(ctx, tenant, ids)
	if err != nil {
		return "", err
	}
	var out []string
	for _, r := range results {
		status := "ok"
		if r.Err != nil {
			status = string(filesystem.CodeOf(r.Err))
		}
		out = append(out, status)
	}
	return strings.Join(out, ", "), nil
}
```
<!-- /ds:block hash=43c400 -->

Check: `trash 3 selected: ok, not_found, ok`.

## Audit storage against the rows

Two read-only checks, for after a restore from backup, a bucket edited by hand, or before the first `SweepOrphans` on a disk:

- `FindOrphans(ctx, disk, prefix, olderThan, found)` is `SweepOrphans` without the deletions: it calls `found` with every key a sweep would delete and returns how many.
- `MissingObjects(ctx, disk, missing)` is the inverse: it calls `missing` with every file row on the disk (any scope, live or trashed) whose object is gone and returns how many. It only reports. Whether to purge such a row, restore the object from a backup or ask for a re-upload is your decision, so it never changes a row. It checks each object with one `Exists` call; objects that plugins keep (versions, variants) are not checked.

<!-- ds:block id=storageaudit-u9f3ld -->
**storageAudit** · [`examples/records-query/main.go:213-224`](../../examples/records-query/main.go#L213-L224)

```go
func storageAudit(ctx context.Context, s *records.Store) (orphans, missing []string, err error) {
	_, err = s.FindOrphans(ctx, diskName, "", time.Now().Add(time.Minute), func(key string) {
		orphans = append(orphans, key)
	})
	if err != nil {
		return nil, nil, err
	}
	_, err = s.MissingObjects(ctx, diskName, func(f records.File) {
		missing = append(missing, f.Key)
	})
	return orphans, missing, err
}
```
<!-- /ds:block hash=8fa5b5 -->

Check: `orphans: [tmp/left-behind.bin]; rows without an object: [docs/other.pdf]`.

## Move a file to another disk

`MoveFile(ctx, scope, id, to)` moves one file's bytes to the disk named `to`, under the same key, and records it there. The file keeps its ID, key, attachments, folder, tags and links; only where its bytes live changes. Two uses: "accept fast, store later" (uploads land on a fast local disk so the request returns at once, and a job promotes them to the bucket), and storage tiering (old files to a cheaper disk). To rename a disk for every file at once, use `RenameDisk` instead ([Replace and visibility](replace-and-visibility.md#step-5--rename-a-disk)).

It follows the object-first rule in three steps. First the object is copied to `to` and the copy is checked against the row: its size, and its SHA-256 when both sides know it. Then, in one transaction with the file's row locked, the row's disk changes and every plugin implementing `records.FileDiskMover` moves its own objects for that file (kept versions, image variants, video posters, PDF previews and texts, OCR texts). The old object is deleted after the commit; a failure there goes to the `WithObjectErrorHandler` handler and leaves an orphan for `SweepOrphans`. Any failure before the commit deletes the copy, so the file stays where it was, whole.

It is safe to run again. A crash leaves at most an extra copy on one disk, never a row without its object. A rerun overwrites a copy left on `to`, and a file already on `to` is returned as it is, so a job that promotes files can simply run on a schedule:

<!-- ds:block id=promote-4a2sna36 -->
**promote** · [`examples/records-move/main.go:70-90`](../../examples/records-move/main.go#L70-L90)

```go
func promote(ctx context.Context, s *records.Store) (int, error) {
	moved := 0
	for {
		page, err := s.Files(ctx, records.FileQuery{
			Scope:   tenant,
			Filters: []sqlb.Predicate{sqlb.Eq(records.FileDisk, incoming)},
		})
		if err != nil {
			return moved, err
		}
		if len(page.Files) == 0 {
			return moved, nil
		}
		for _, f := range page.Files {
			if _, err := s.MoveFile(ctx, tenant, f.ID, kept); err != nil {
				return moved, fmt.Errorf("promote %s: %w", f.ID, err)
			}
			moved++
		}
	}
}
```
<!-- /ds:block hash=bf5901 -->

Check: the [records-move example](../../examples/records-move) prints `promoted 3`, then `first note: disk=kept, still on incoming=false, reads "first note"`, and `promote again: 0, <nil>`.

It emits `file.moved` inside its transaction, with `Event.File` on its new disk and `Detail` naming the old and new disk (`records.DetailFrom`, `records.DetailTo`), so the [audit](../plugins/audit.md) log records where a file went. A file already on `to` emits nothing. It refuses, changing nothing:

- a trashed file: `not_found`, as `Replace` does;
- a disk the `Manager` does not have: `not_found`;
- another file recorded under the same key on `to`: `exists`, because the copy would overwrite its bytes (keys from upload policies are random, so this only guards keys your app chose);
- a target disk that cannot give the file its visibility (no per-object visibility, and a default that differs): `unsupported`, as `CreateFile` refuses it;
- with `WithVisibilityDisks`, a move that would leave the file off the disk its visibility routes to, whether out of one of those disks or into the wrong one: `invalid_argument`. Moving from a staging disk onto the right one is allowed;
- a row that changed while the bytes were copied (replaced, trashed, moved by another request): `unavailable`; retry.

A plugin that keeps objects beside files implements `FileDiskMover` (`MoveFileDisk(ctx, s, f, to)`, where `f.Disk` is the old disk). It must copy its objects for that file, update its rows with `s.Conn(ctx)`, delete its copies on rollback (`s.OnRollback`) and its old objects after the commit (`s.DeleteObjectAfterCommit`). A plugin that does not implement it leaves its objects on the old disk, where they keep working, since its rows name that disk.

## Offboard a whole scope

When a tenant leaves (a workspace deleted, a customer's contract ended), `PurgeScope(ctx, scope)` deletes everything the Store and its plugins keep for that scope. First it purges every file of the scope, live and trashed, through `Purge`, so each file's attachments go with it, every plugin's `BeforePurge` runs (versions, variants, tags, share links, quota) and each object is deleted after its own commit. Then, in one transaction, it calls every plugin implementing `records.ScopePurger` (`PurgeScope(ctx, s, scope) error`) for the rows that belong to the scope rather than to a file: the folders plugin's tree and lock row, the quota usage row, any tag, share link or kept version left over. It returns how many files it purged.

It works on a scope of any size: it reads `records.PurgeScopeBatch` (100) file IDs at a time and purges each file in its own transaction, so call it outside a `Tx`, typically from a background job. It stops at the first file that cannot be purged (a database failure, a plugin refusing it such as a legal hold, a cancelled context) and returns the count so far with the error. Nothing it purged comes back, so run it again once the cause is fixed: it continues where it stopped, and it runs the `ScopePurger` plugins only once no file is left. An object whose deletion fails goes to the `WithObjectErrorHandler` handler (step 6) and is left for `SweepOrphans`. Every purged file emits `file.purged`.

An upload that lands while a scope is being offboarded is caught: the last transaction counts the scope's files after the `ScopePurger` plugins ran, and if one appeared it rolls their work back, purges the new files and tries again. After three rounds it gives up with `unavailable`, since the scope keeps receiving files: stop uploads to a scope before offboarding it.

Two things it does not do. It does not erase the [audit](../plugins/audit.md) log, which is meant to outlive the files it describes: when the tenant's history must go too, call `audit.PruneScope(ctx, s, scope, cutoff)` yourself, with a cutoff after the last entry. And it refuses the empty scope (`invalid_argument`), because in an application without tenants that scope is every file. Your app's own rows for the tenant are yours to delete, in your own transaction, after it returns.

A plugin that keeps rows per scope implements `ScopePurger`; it must delete only rows of that scope, through `s.Conn(ctx)`, and succeed when there are none, since a rerun calls it again.

## Temporary and view-once files

A temporary file disappears at a time you set: a story, a share-once export, an upload a user never finished attaching. A view-once file disappears at its first view. Nothing happens in the background: a route asks before it serves, and a sweep purges on your schedule. The second half of [`examples/records-lifecycle`](../../examples/records-lifecycle) (`retention.go`) runs this section and the next two.

**1. Make it temporary.** `Upload(..., records.Expires(at))`, or `NewFile.ExpiresAt` with `CreateFile`:

<!-- ds:block id=poststory-w2c8t5hm -->
**postStory** · [`examples/records-lifecycle/retention.go:74-76`](../../examples/records-lifecycle/retention.go#L74-L76)

```go
func postStory(ctx context.Context, s *records.Store, now time.Time) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader("sunset"), "story.txt", textPolicy, records.Expires(now.Add(storyLife)))
}
```
<!-- /ds:block hash=aa1ba5 -->

**2. Or view-once.** `records.ViewOnce()` (or `NewFile.ViewOnce`):

<!-- ds:block id=sendviewonce-r6h2k9vc -->
**sendViewOnce** · [`examples/records-lifecycle/retention.go:80-82`](../../examples/records-lifecycle/retention.go#L80-L82)

```go
func sendViewOnce(ctx context.Context, s *records.Store) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader("secret"), "once.txt", textPolicy, records.ViewOnce())
}
```
<!-- /ds:block hash=10c541 -->

**3. Ask before serving.** `Viewed(ctx, scope, id)` returns false for an expired or trashed file, and for a view-once file true exactly once: one conditional update sets `ExpiresAt` at that first view, so two concurrent first views get one true between them, even on servers whose clocks disagree. A quarantined file is false with `records.ErrQuarantined`, and spends no view.

<!-- ds:block id=serve-n3q7w4kz -->
**serve** · [`examples/records-lifecycle/retention.go:87-96`](../../examples/records-lifecycle/retention.go#L87-L96)

```go
func serve(ctx context.Context, s *records.Store, id string) string {
	ok, err := s.Viewed(ctx, tenant, id)
	switch {
	case err != nil:
		return string(filesystem.CodeOf(err))
	case !ok:
		return "404"
	}
	return "200"
}
```
<!-- /ds:block hash=b05c31 -->

**4. Sweep.** `SweepExpired(ctx, now)` purges every expired file, live or trashed, each through `Purge` in its own transaction, so plugins' hooks run.

Check: the example prints `story now: 200; view-once first: 200, second: 404`, then `a day later, SweepExpired purged 2 (the story and the viewed note)`.

[recordshttp](http-endpoints.md)'s `GET /files/{id}` asks `Viewed` before it redirects and answers `404` for a spent or expired file, and its owner listing leaves expired files out and lists a view-once file without a link. A link it hands out stays valid for `LinkLifetime`, so keep that short where view-once files are served: `Viewed` stops a second link, not a second fetch of the first one.

## Retention rules

A retention rule says how long attachments of an owner type (and optionally a role) are kept: chat attachments for 90 days, support-ticket files for a year. It is a Store option:

<!-- ds:block id=newretentionstore-k4m9w2tc -->
**newRetentionStore** · [`examples/records-lifecycle/retention.go:35-57`](../../examples/records-lifecycle/retention.go#L35-L57)

```go
func newRetentionStore(ctx context.Context, db *sql.DB, clock *Clock) (*records.Store, *versions.Plugin, error) {
	disks, err := newDisks()
	if err != nil {
		return nil, nil, err
	}
	vp, err := versions.New()
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerUser, ownerChat),
		records.WithClock(clock.Now),
		records.WithRetention(records.Retention{OwnerType: ownerChat, MaxAge: chatRetention}),
		records.WithPlugins(vp),
	)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, vp, nil
}
```
<!-- /ds:block hash=b02ca6 -->

Role `""` covers every role of the owner type. A rule without an owner type, with an invalid role, or with a `MaxAge` that is not positive is `invalid_argument` from `records.New`.

`SweepRetention(ctx, now, dryRun)` detaches every attachment past its rule's age, each in its own transaction, and purges a file left with no attachment; a file still used elsewhere is kept. Run it with `dryRun` true first: the `RetentionReport{Detached, Purged}` lists exactly what a real run would do, and nothing changes:

<!-- ds:block id=applyretention-p7v3n8qx -->
**applyRetention** · [`examples/records-lifecycle/retention.go:63-69`](../../examples/records-lifecycle/retention.go#L63-L69)

```go
func applyRetention(ctx context.Context, s *records.Store, now time.Time) (preview, done records.RetentionReport, err error) {
	if preview, err = s.SweepRetention(ctx, now, true); err != nil {
		return preview, done, err
	}
	done, err = s.SweepRetention(ctx, now, false)
	return preview, done, err
}
```
<!-- /ds:block hash=baaec9 -->

Check: a chat photo and a logo attached to a chat and to a user are swept 90 days and an hour later. The example prints `retention dry run: detach 2, purge 1; real run: detached 2, purged 1`, then `old chat photo: not_found; logo still used by a user: true`. One failing attachment does not stop the sweep; the error names each. Schedule it; the library starts no background work.

### Retention by plan

`WithRetention`'s rules apply to every scope. When the age depends on the tenant ("free plan 30 days, pro 1 year"), keep the plans in your own tables and sweep each tenant with its own rules: `SweepRetentionIn(ctx, scope, rules, now, dryRun)` is `SweepRetention` limited to one scope, with the rules you pass. Rules are checked as `WithRetention` checks them, and the owner type must be registered; a bad rule is `invalid_argument` and sweeps nothing.

```go
for _, t := range tenants { // from your billing tables
	keep := 30 * 24 * time.Hour
	if t.Plan == "pro" {
		keep = 365 * 24 * time.Hour
	}
	rules := []records.Retention{{OwnerType: "chat", MaxAge: keep}}
	if _, err := store.SweepRetentionIn(ctx, t.Scope, rules, time.Now(), false); err != nil {
		log.Printf("retention of %s: %v", t.Scope, err)
	}
}
```

Do not give the same owner types to `WithRetention` as well: `SweepRetention` would apply its rules to every tenant on top.

Try it: [`examples/records-lifecycle`](../../examples/records-lifecycle) runs this section in `plans.go` (`sweepByPlan`): `go run ./records-lifecycle` from `examples/`.

## Takedown

A takedown (a copyright notice, an abuse report, a court order) must remove the file and everything made from it, not only hide it. That is `Purge`, not `Trash`: trash keeps the bytes for a restore.

<!-- ds:block id=takedown-x5t9c2mw -->
**takedown** · [`examples/records-lifecycle/retention.go:103-105`](../../examples/records-lifecycle/retention.go#L103-L105)

```go
func takedown(ctx context.Context, s *records.Store, id string) error {
	return s.Purge(records.WithActor(ctx, "admin:trust-and-safety"), tenant, id)
}
```
<!-- /ds:block hash=32d727 -->

`Purge` runs every installed plugin's `BeforePurge` in the same transaction, so the file's [kept versions](../plugins/versions.md), [image variants](../plugins/variants.md), [share links](../plugins/share.md) and [grants](../plugins/acl.md) go with its row, and every object is deleted after the commit (a shared object only once no other file names it). With the [audit plugin](../plugins/audit.md) the purge is recorded with the actor set here. Check: the example replaces a document once, so one version is kept, and prints `takedown: 1 kept version(s) before, 0 after`.

A file someone has [checked out](../plugins/locks.md) refuses the purge with `locks.ErrLocked`; `Break` the lock first. Two things a purge cannot reach: copies already cached by a browser or CDN (purge your CDN on `file.purged`), and objects a [mirrored disk](../concepts/composing-disks.md) wrote to a target the Store does not manage.

## Step 6 — When deleting an object fails

The rows are gone before the object is deleted, so an object deletion failure can never leave a row pointing at missing bytes; it leaves bytes nothing points at. Where that failure goes depends on whether a caller is still waiting:

<!-- ds:block id=objecterrordemo-z4zrdxnd -->
**objectErrorDemo** · [`examples/records-lifecycle/main.go:334-348`](../../examples/records-lifecycle/main.go#L334-L348)

```go
func objectErrorDemo(ctx context.Context, w io.Writer, s *records.Store, failed *[]string) error {
	a, err := store(ctx, s, archiveName, "archive/a.txt")
	if err != nil {
		return err
	}
	err = s.Purge(ctx, tenant, a.ID)
	fmt.Fprintf(w, "Purge outside a Tx returns the object error: %s\n", filesystem.CodeOf(err))
	b, err := store(ctx, s, archiveName, "archive/b.txt")
	if err != nil {
		return err
	}
	err = s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error { return s.Purge(ctx, tenant, b.ID) })
	fmt.Fprintf(w, "Purge inside a Tx: err=%v handler got %q\n", err, *failed)
	return nil
}
```
<!-- /ds:block hash=287de2 -->

- **`Purge` outside a `Tx`** returns the deletion error. The purge itself succeeded: the row is gone and will not come back. Check: `Purge outside a Tx returns the object error: read_only`.
- **`Purge` inside a `Tx`** deletes after the outer commit, and so do, whether or not you opened a transaction, `PurgeOwner`, `SetAttachment` with `PreviousPurge`, `Replace` and `ReplaceObject` (the previous content), `SetVisibility` moving between disks, and plugins dropping versions or variants. The deletion runs after the call has returned, so no caller is left to tell. The failure goes to the handler set with `WithObjectErrorHandler(func(ctx, f records.File, err error))`, with the file's `Disk` and `Key`. Check: `Purge inside a Tx: err=<nil> handler got ["archive/archive/b.txt: read_only"]`.

Without a handler such failures are dropped and the object stays as an orphan. Log them, or queue the disk and key for a retry. To clean orphans up in bulk, schedule `SweepOrphans(ctx, disk, prefix, olderThan)`: it deletes the objects under `prefix` that no `fs_files` row names and no plugin holds (kept versions, image variants), last modified before `olderThan`, and returns how many it deleted. Unlike `upload.Sweep` or an age-only bucket lifecycle rule, which delete by age whether a file is recorded or not, it never deletes a recorded file. Point it only at prefixes that hold records' objects, not at exports or backups the application keeps outside records, and keep `olderThan` well before the oldest upload that may still be waiting for its row.

## Work at the end of a transaction

The Store exposes the same tools it uses, for your code and for plugins:

| Method | Runs |
|---|---|
| `AfterCommit(ctx, fn)` | `fn` once the transaction in `ctx` commits; at once when there is none; never on rollback. For webhooks, cache invalidation, emails. |
| `OnRollback(ctx, fn)` | `fn` if the transaction in `ctx` rolls back; never when there is none. `Upload` uses it to delete the object of a row a rollback undid. |
| `DeleteObjectAfterCommit(ctx, f)` | Deletes `f.Disk`/`f.Key` after the commit, reporting a failure to the object error handler: the object-last rule for plugins that own objects. |

## What can go wrong

- `not_found` from `Trash`, `Restore` or `Purge`: wrong ID, or the file belongs to another scope.
- A `Purge` error that is a storage error (`read_only`, `unavailable`, `permission`): the row is already purged; only the object remains. Do not retry the purge; `SweepOrphans` deletes the object later.
- `PurgeOwner` purged nothing: the owner's files are attached to another owner too, or were never attached (`PurgeUnattached` purges those by uploader).
- A trashed file never goes away: nothing calls `SweepTrash`. Schedule it.
- `PurgeScope` returned an error naming a file: that file could not be purged (a legal hold, a database failure). Fix the cause and run it again; it continues where it stopped.

## Next

- [Replace and visibility](replace-and-visibility.md): change a file's content or access without deleting it.
- [Events and hooks](events-and-hooks.md): refuse a purge, or act on one.
- [Versions](../plugins/versions.md): keep old contents instead of deleting them.
