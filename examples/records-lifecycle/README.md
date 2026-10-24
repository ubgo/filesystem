# Trash, restore and delete for good

The end of a file's life in the records layer: the trash (`Trash`, `Restore`, counting trashed files), deleting for good (`Purge`, which deletes the object only after the transaction commits), a rollback that keeps both row and object, deleting everything a user leaves behind (`PurgeOwner`) in the same transaction that deletes the user's own row, a trash retention sweep (`SweepTrash`), and where object deletion failures go (`WithObjectErrorHandler`). This is what account deletion, a "recently deleted" view and GDPR erasure need. It uses `github.com/ubgo/filesystem/records` on SQLite over in-memory disks, with a test clock. The guide [Lifecycle: trash, restore, purge](../../docs/records/lifecycle.md) explains each step.

## Run it

```sh
cd examples
go run ./records-lifecycle
go test ./records-lifecycle
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-lifecycle
```

```text
after Trash: live=0 trashed=1 <nil>
after Restore: live=1 trashed=0 <nil>
rolled-back purge: err=request failed after the purge row kept=true object kept=true
purged: row=not_found object kept=false
PurgeOwner: purged=1 err=<nil> bob still has 1 file(s)
SweepTrash: purged=1 err=<nil> still in trash=1
Purge outside a Tx returns the object error: read_only
Purge inside a Tx: err=<nil> handler got ["archive/archive/b.txt: read_only"]
retention dry run: detach 2, purge 1; real run: detached 2, purged 1
old chat photo: not_found; logo still used by a user: true
story now: 200; view-once first: 200, second: 404
a day later, SweepExpired purged 2 (the story and the viewed note)
takedown: 1 kept version(s) before, 0 after
retention by plan, free-co after 60 days: detached 1, purged 1
retention by plan, pro-co after 60 days: detached 0, purged 0
```

1. `Trash` hides a file from listings and `Restore` brings it back; the counts show both states.
2. A purge inside a transaction that then fails rolls back: the row is still there, and the object, which `Purge` deletes only after a commit, was never touched.
3. A purge that commits removes the row (`not_found`) and then the object.
4. Deleting user `usr_1` deletes the app's `users` row and calls `PurgeOwner` in one transaction. The file only that user owned is purged; a file also attached to `usr_2` is kept for them.
5. Two files are trashed 40 and 10 days before the sweep; `SweepTrash` with a 30-day retention purges the old one and leaves the recent one in the trash.
6. On a read-only `archive` disk the object cannot be deleted. Outside a transaction `Purge` returns that error (`read_only`); inside one, the row is already committed when the deletion fails, so the error goes to the object error handler instead.
7. `retention.go` opens a second Store with a retention rule (chat attachments kept 90 days) and the versions plugin. The dry run and the real run agree: two chat attachments are detached, the old photo is purged, and the logo, still attached to a user, is kept.
8. A story expires in a day and a view-once note serves once: `Viewed` answers 200, 200, then 404. A day later `SweepExpired` purges both.
9. A takedown is `Purge`: the file's kept version goes with it.
10. `plans.go` keeps chat attachments for as long as each tenant's plan says: 30 days on the free plan, a year on pro. The Store registers the `chat` owner type with no `WithRetention` rule, and [`sweepByPlan`](ds:block?id=sweepbyplan-f8c2n5tj) calls `SweepRetentionIn` once per tenant with that tenant's rule. Sixty days after both tenants posted an attachment, the free tenant's is detached and its file purged; the pro tenant's is kept. The test `TestRetentionByPlanRefusesAnUnregisteredOwnerType` shows a rule for an owner type the Store did not register is `invalid_argument`.

## The code

Two disks over one memory driver: `media`, which can delete, and `archive`, wrapped in `filesystem.ReadOnly`:

<!-- ds:block id=newdisks-hvtukymk -->
**newDisks** · [`examples/records-lifecycle/main.go:85-96`](main.go#L85-L96)

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

The Store with a test clock and an object error handler for deletions that fail after a commit, when no caller is left to return them to:

<!-- ds:block id=newstore-8hms7sx8 -->
**newStore** · [`examples/records-lifecycle/main.go:108-133`](main.go#L108-L133)

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

Counting live and trashed files, and the trash round trip:

<!-- ds:block id=trashcounts-q9ep7g6t -->
**trashCounts** · [`examples/records-lifecycle/main.go:149-159`](main.go#L149-L159)

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
**trashDemo** · [`examples/records-lifecycle/main.go:247-263`](main.go#L247-L263)

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

A purge that rolls back:

<!-- ds:block id=purgethenfail-b6b94u7z -->
**purgeThenFail** · [`examples/records-lifecycle/main.go:174-181`](main.go#L174-L181)

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

Deleting a user and everything only they owned, both or neither:

<!-- ds:block id=deleteuser-mk6djv64 -->
**deleteUser** · [`examples/records-lifecycle/main.go:187-198`](main.go#L187-L198)

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

The retention sweep, which an app schedules (the library starts no background work):

<!-- ds:block id=sweep-q4hu6fkd -->
**sweep** · [`examples/records-lifecycle/main.go:203-205`](main.go#L203-L205)

```go
func sweep(ctx context.Context, s *records.Store, now time.Time) (int, error) {
	return s.SweepTrash(ctx, now.Add(-trashRetention))
}
```
<!-- /ds:block hash=bebe48 -->

Object deletion failures outside and inside a transaction:

<!-- ds:block id=objecterrordemo-z4zrdxnd -->
**objectErrorDemo** · [`examples/records-lifecycle/main.go:334-348`](main.go#L334-L348)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: two disks, a test clock, trash, purge, owner deletion, sweep and object errors |
| `retention.go` | The second half: retention rules with a dry run, temporary and view-once files, and a takedown that removes kept versions |
| `plans.go` | Retention by plan: `SweepRetentionIn` per tenant with that tenant's own rules |
| `main_test.go` | Runs the same steps, plus a user deletion rolled back by an outer transaction, re-trashing keeping the original time, a failed purge keeping everything, and a per-tenant rule for an unregistered owner type refused |

## Next

- The guide: [Lifecycle: trash, restore, purge](../../docs/records/lifecycle.md).
- Related examples: [recipe-account-deletion](../recipe-account-deletion) for a full account deletion, [start-production](../start-production) for the sweep in a scheduled job, [records-attachments](../records-attachments) for files shared between owners.
