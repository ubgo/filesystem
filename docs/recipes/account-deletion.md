# Account deletion

"Delete my account" has to remove the person's files, not just hide their profile, and data-protection law (GDPR's right to erasure, CCPA and similar) expects it to actually happen. The hard parts are the edges: a file the user shared with a team must not vanish from the team, a deletion that fails half-way must not leave rows pointing at deleted bytes, and a storage backend that is briefly down must not leave personal data behind for ever. This recipe is for any app with user accounts that own files.

The program is [examples/recipe-account-deletion](../../examples/recipe-account-deletion). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `DELETE /users/{id}` removes the user's row and every file only they used, in one database transaction.
- A file also attached to another owner (shared with a team) is only detached; the team keeps it.
- If any step fails, nothing changes: the user, their rows and their bytes are all still there.
- Objects are deleted after the commit, never before, so a rollback cannot leave a row without its bytes.
- An object deletion that fails after the commit is queued in a table and retried by a scheduled pass.
- Trashed files have a retention period, after which the same pass purges them.

## The design

| Piece | Why |
|---|---|
| `Store.Tx` around the whole deletion | Every `Store` call made with the `ctx` it gives joins the transaction, and so does the app's own SQL on the `*sql.Tx` it passes. The user row and the file rows commit or roll back together. |
| `Store.PurgeOwner(ctx, scope, owner)` | Deletes every attachment of the owner, in every role, and purges each file no other attachment uses. Returns how many files it purged. |
| The app's `DELETE FROM users` on the same `*sql.Tx` | The app's tables live in the same database. A foreign key that refuses the delete rolls back the purge too. |
| `records.WithObjectErrorHandler` | Object deletions run after the commit, when there is no caller to return an error to. The handler records the key so a retry job can finish the deletion. |
| `Store.Trash` + `Store.SweepTrash(cutoff)` | Soft deletion with a restore window for single files; the sweep purges what is older than the retention period. |

## Step 1: The app's tables live next to the fs_ tables

<!-- ds:block id=appschema-3nwqefjs -->
**appSchema** · [`examples/recipe-account-deletion/main.go:85-89`](../../examples/recipe-account-deletion/main.go#L85-L89)

```go
`
CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS invoices (id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id));
CREATE TABLE IF NOT EXISTS orphaned_objects (disk TEXT NOT NULL, storage_key TEXT NOT NULL, failed_at TEXT NOT NULL, PRIMARY KEY (disk, storage_key));
`
```
<!-- /ds:block hash=73221e -->

<!-- ds:block id=opendb-vuhkjgvy -->
**openDB** · [`examples/recipe-account-deletion/main.go:102-110`](../../examples/recipe-account-deletion/main.go#L102-L110)

```go
func openDB(ctx context.Context, dataDir string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "app.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, appSchema)
	return db, err
}
```
<!-- /ds:block hash=ba1bd2 -->

`invoices` references `users` without `ON DELETE`, so a user with invoices cannot be deleted: accounting records usually have to be kept, and this is the failure the recipe uses to show a rollback. `orphaned_objects` is the retry queue for Step 4.

## Step 2: Wire the store with an object error handler

<!-- ds:block id=new-n64tswyy -->
**New** · [`examples/recipe-account-deletion/main.go:115-142`](../../examples/recipe-account-deletion/main.go#L115-L142)

```go
func New(ctx context.Context, dataDir string, diskOpts ...filesystem.Option) (*App, error) {
	drv, err := local.New(filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, drv, diskOpts...)
	if err != nil {
		return nil, err
	}
	app := &App{}
	if app.Disks, err = filesystem.NewManager(disk); err != nil {
		return nil, err
	}
	if app.DB, err = openDB(ctx, dataDir); err != nil {
		return nil, err
	}
	app.Store, err = records.New(app.DB, sqlb.SQLiteDialect{}, app.Disks,
		records.WithOwnerTypes(ownerUser, ownerTeam),
		records.WithObjectErrorHandler(app.rememberOrphan))
	if err != nil {
		return nil, err
	}
	if _, err := app.Store.Migrate(ctx); err != nil {
		return nil, err
	}
	app.Handler = app.routes()
	return app, nil
}
```
<!-- /ds:block hash=08689f -->

`WithOwnerTypes("user", "team")` registers both owner types; `PurgeOwner` refuses a type that is not registered. The `diskOpts` parameter exists for the test, which injects a failing delete with an interceptor.

## Step 3: Delete in one transaction

<!-- ds:block id=deleteuserrow-8n5kpwv5 -->
**deleteUserRow** · [`examples/recipe-account-deletion/main.go:164-177`](../../examples/recipe-account-deletion/main.go#L164-L177)

```go
func deleteUserRow(ctx context.Context, tx *sql.Tx, userID string) error {
	res, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", userID)
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY {
		return errHasInvoices
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return filesystem.Errorf(filesystem.CodeNotFound, "no user %q", userID)
	}
	return nil
}
```
<!-- /ds:block hash=0cb093 -->

<!-- ds:block id=app-deleteaccount-24xv49h8 -->
**App.deleteAccount** · [`examples/recipe-account-deletion/main.go:185-200`](../../examples/recipe-account-deletion/main.go#L185-L200)

```go
func (a *App) deleteAccount(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var purged int
	err := a.Store.Tx(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if purged, err = a.Store.PurgeOwner(ctx, "", records.Owner{Type: ownerUser, ID: userID}); err != nil {
			return err
		}
		return deleteUserRow(ctx, tx, userID)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"files_purged": purged})
}
```
<!-- /ds:block hash=ad53ee -->

The order inside the transaction does not matter for correctness, since both commit together. Purging first means the user row is the last thing to go, which reads naturally in logs.

What happens to each kind of file:

| File | Result |
|---|---|
| Attached only to the user (any role) | Rows deleted in the transaction, object deleted after the commit. |
| Attached to the user and to a team | The user's attachment is deleted; the file and the team's attachment stay. |
| In the trash, attached only to the user | Purged like a live one: attachments of trashed files still count. |
| Uploaded by the user but never attached | Not touched by `PurgeOwner`, which works from attachments. Call `PurgeUnattached(ctx, scope, actor)` in the same transaction to purge the files that actor uploaded and nothing uses ([lifecycle](../records/lifecycle.md#step-4--delete-what-an-owner-leaves-behind)). |

A file the team keeps still carries `UploadedBy` (the deleted user's ID) and its original file name. An ID is usually fine to keep; if your file names can hold personal data, rename shared files as part of the deletion.

## Step 4: Failures after the commit

Once the rows are committed away, an object deletion that fails cannot roll anything back. Without a handler the failure is dropped and the object stays until something sweeps it; for personal data that is not good enough, so the handler queues it:

<!-- ds:block id=app-rememberorphan-7sauv2xh -->
**App.rememberOrphan** · [`examples/recipe-account-deletion/main.go:207-214`](../../examples/recipe-account-deletion/main.go#L207-L214)

```go
func (a *App) rememberOrphan(ctx context.Context, f records.File, err error) {
	fmt.Fprintf(os.Stderr, "delete %s/%s after commit: %v (queued for retry)\n", f.Disk, f.Key, err)
	_, qerr := a.DB.ExecContext(ctx, "INSERT OR IGNORE INTO orphaned_objects (disk, storage_key, failed_at) VALUES (?, ?, ?)",
		f.Disk, f.Key, time.Now().UTC().Format(time.RFC3339))
	if qerr != nil {
		fmt.Fprintf(os.Stderr, "queue %s/%s: %v\n", f.Disk, f.Key, qerr)
	}
}
```
<!-- /ds:block hash=570513 -->

<!-- ds:block id=app-retryorphans-jzfew86s -->
**App.retryOrphans** · [`examples/recipe-account-deletion/main.go:222-254`](../../examples/recipe-account-deletion/main.go#L222-L254)

```go
func (a *App) retryOrphans(ctx context.Context) (int, error) {
	rows, err := a.DB.QueryContext(ctx, "SELECT disk, storage_key FROM orphaned_objects")
	if err != nil {
		return 0, err
	}
	var queued []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.disk, &o.key); err != nil {
			_ = rows.Close()
			return 0, err
		}
		queued = append(queued, o)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	done := 0
	for _, o := range queued {
		disk, err := a.Disks.Disk(o.disk)
		if err == nil {
			err = disk.Delete(ctx, o.key)
		}
		if err != nil {
			continue // still failing; the next pass tries again
		}
		if _, err := a.DB.ExecContext(ctx, "DELETE FROM orphaned_objects WHERE disk = ? AND storage_key = ?", o.disk, o.key); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
```
<!-- /ds:block hash=903a0c -->

Deleting a missing object succeeds on every driver, so retrying is always safe. A key still failing stays queued for the next pass.

## Step 5: Trash and retention

Deleting one file from the UI usually means "move to trash", restorable for a while:

<!-- ds:block id=app-trashfile-89ztszy8 -->
**App.trashFile** · [`examples/recipe-account-deletion/main.go:351-357`](../../examples/recipe-account-deletion/main.go#L351-L357)

```go
func (a *App) trashFile(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.Trash(r.Context(), "", r.PathValue("file")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=b60884 -->

The library starts no background work. The app schedules one maintenance pass that sweeps the trash and retries the queue:

<!-- ds:block id=app-maintain-yjfrqhgf -->
**App.maintain** · [`examples/recipe-account-deletion/main.go:260-270`](../../examples/recipe-account-deletion/main.go#L260-L270)

```go
func (a *App) maintain(ctx context.Context, now time.Time) error {
	swept, err := a.Store.SweepTrash(ctx, now.Add(-trashRetention))
	if err != nil {
		return err
	}
	retried, err := a.retryOrphans(ctx)
	if swept+retried > 0 {
		fmt.Printf("maintenance: purged %d trashed files, deleted %d queued objects\n", swept, retried)
	}
	return err
}
```
<!-- /ds:block hash=804ea5 -->

<!-- ds:block id=runmaintenance-vhjstp3z -->
**runMaintenance** · [`examples/recipe-account-deletion/main.go:387-400`](../../examples/recipe-account-deletion/main.go#L387-L400)

```go
func runMaintenance(ctx context.Context, a *App) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := a.maintain(ctx, now); err != nil {
				fmt.Fprintln(os.Stderr, "maintenance:", err)
			}
		}
	}
}
```
<!-- /ds:block hash=286988 -->

`SweepTrash` purges every file, in every scope, trashed before the cutoff. A file it cannot purge does not stop it: the others are purged, and the error names each file that failed. With several instances, run the pass from one (a cron job or a leader), or accept that two instances may try the same purge, which is harmless: the loser finds the file already gone.

Sharing, for completeness: attaching an existing file to a second owner is one `Attach`.

<!-- ds:block id=app-sharewithteam-52gjmkkp -->
**App.shareWithTeam** · [`examples/recipe-account-deletion/main.go:362-369`](../../examples/recipe-account-deletion/main.go#L362-L369)

```go
func (a *App) shareWithTeam(w http.ResponseWriter, r *http.Request) {
	_, err := a.Store.Attach(r.Context(), "", r.PathValue("file"), records.Owner{Type: ownerTeam, ID: r.PathValue("id")}, roleShared)
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```
<!-- /ds:block hash=19edfa -->

## Try it

```sh
cd examples
go run ./recipe-account-deletion
```

```text
listening on http://localhost:8114 data in /var/folders/sk/.../T/recipe-account-deletion-2681024269
```

Create a user with two files, and share one of them with a team:

```sh
curl -s -X POST -d '{"email":"ada@example.com"}' localhost:8114/users/ada
printf 'notes\n' > notes.txt; printf 'Q3 plan\n' > plan.txt
curl -s -F file=@notes.txt localhost:8114/users/ada/files
curl -s -F file=@plan.txt localhost:8114/users/ada/files
curl -s -i -X PUT localhost:8114/teams/research/files/fil_v7sb5uqytdgb4fhbppakr3akib
```

```text
{"id":"fil_mycvkw7vvy2zhgby7ehdssu5jh","filename":"notes.txt","size":6}
{"id":"fil_v7sb5uqytdgb4fhbppakr3akib","filename":"plan.txt","size":8}
HTTP/1.1 204 No Content
```

An invoice blocks the deletion, and the rollback leaves everything:

```sh
curl -s -X POST localhost:8114/users/ada/invoices
curl -s -X DELETE localhost:8114/users/ada
curl -s localhost:8114/users/ada/files
```

```text
{"error":"Conflict","code":"exists"}
[{"id":"fil_mycvkw7vvy2zhgby7ehdssu5jh","filename":"notes.txt","size":6},{"id":"fil_v7sb5uqytdgb4fhbppakr3akib","filename":"plan.txt","size":8}]
```

Check: both objects are still in the data directory's `files/files/`. `PurgeOwner` had already run inside the transaction, and its object deletions were scheduled for after a commit that never came.

With the invoice settled (the demo removes it with `sqlite3 app.db 'DELETE FROM invoices'`), the deletion goes through:

```sh
curl -s -X DELETE localhost:8114/users/ada
curl -s localhost:8114/users/ada/files
curl -s localhost:8114/teams/research/files
curl -s -X DELETE localhost:8114/users/ada
```

```text
{"files_purged":1}
[]
[{"id":"fil_v7sb5uqytdgb4fhbppakr3akib","filename":"plan.txt","size":8}]
{"error":"Not Found","code":"not_found"}
```

Check: one object is left on disk, the team's `plan.txt`; `notes.txt` is gone. A second deletion is a `404`, and changes nothing.

The test `TestObjectFailureIsQueuedAndRetried` makes every delete fail with an interceptor, deletes an account, and checks that the rows are gone, the object is queued in `orphaned_objects`, and the next maintenance pass deletes it once the backend is back.

## Edge cases this handles

- A deletion refused by the app's own constraints: `409`, nothing deleted, no object touched.
- A crash between commit and object deletion: the rows are gone and nothing points at the object; it is an orphan that the queue (if the handler ran) or a prefix sweep removes ([consistency](../concepts/consistency.md)).
- A storage outage at deletion time: the account is deleted, the objects are queued and deleted on a later pass.
- Files shared with other owners: detached, not deleted.
- Deleting twice: the second request is a `404` and changes nothing.
- A file attached to the user under several roles: purged once.

## For production

- Authenticate the request, and for self-service deletion re-confirm (password or recent sign-in): it cannot be undone.
- Offer an export before deleting ([exports](exports.md)), and delete the export afterwards too: it is personal data.
- Many products delay the deletion (a grace period of 14 or 30 days): mark the user as pending deletion, hide the profile, and run this deletion from a job when the period ends.
- Add the queue table to your monitoring: a growing `orphaned_objects` means a backend keeps refusing deletions.
- Backups and mirrors hold copies: a `filesystem.Mirror` deletes on every mirror, but point-in-time backups keep the bytes until they expire. State that retention in your privacy policy ([backups and mirrors](backups-and-mirrors.md)).
- The `audit` plugin's log outlives the files it describes, by design; its `Prune` applies a retention period ([audit](../plugins/audit.md)).
- Deleting a whole tenant rather than one user is `Store.PurgeScope(ctx, scope)`: every file of the scope, then every plugin's rows for it, in batches, rerunnable after a failure ([offboard a whole scope](../records/lifecycle.md#offboard-a-whole-scope)). It leaves the audit log; call `audit.PruneScope` when that must go too.
- On Postgres or MySQL the same code works; the app's statements use that driver's placeholders.

## Related

- [Lifecycle](../records/lifecycle.md): trash, restore, purge, `PurgeOwner`, `PurgeUnattached`, `PurgeScope`, `SweepTrash`.
- [Consistency](../concepts/consistency.md): why objects are deleted after the commit, and what can be left behind.
- [Attachments](../records/attachments.md): owners, roles and shared files.
- [SaaS tenants](saas-tenants.md): removing a whole tenant instead of one user.
- [Avatars](avatars.md): `SetAttachment` with `PreviousPurge`, the per-file version of this.
