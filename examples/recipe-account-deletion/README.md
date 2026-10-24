# Account deletion: "delete my account" that holds up to a data-protection request

An HTTP server where deleting a user removes their row and every file only they used, in one transaction, while files they shared with a team stay for the team. It answers the "right to erasure" need every product with user accounts has. It uses `records` (`Store.Tx`, `PurgeOwner`, `Attach`, `Trash`, `SweepTrash`, `WithObjectErrorHandler`) over SQLite and a local disk: objects are deleted only after the commit, a deletion that fails afterwards is queued in the app's own `orphaned_objects` table and retried, and trashed files are purged after a 30-day retention period. The guide is [Account deletion](../../docs/recipes/account-deletion.md).

## Run it

```sh
cd examples
go run ./recipe-account-deletion
```

It creates a new temporary directory (printed at start-up) holding `app.db` (SQLite, with the app's `users`, `invoices` and `orphaned_objects` tables next to the `fs_` tables) and the stored files under `files/`, and listens on `http://localhost:8114`. A maintenance pass (purge expired trash, retry queued deletions) runs every hour. Stop it with Ctrl-C (or SIGTERM): it shuts down cleanly and removes the temporary directory.

Run the tests with `go test ./recipe-account-deletion`. Authentication is out of scope: a real app checks that the signed-in user is `{id}` before these routes run.

## Walkthrough

1. Create a user with two files, and share the second with a team:

```sh
curl -s -w '%{http_code}\n' -X POST -d '{"email":"ada@example.com"}' localhost:8114/users/ada
printf 'notes\n' > notes.txt; printf 'Q3 plan\n' > plan.txt
curl -s -F file=@notes.txt localhost:8114/users/ada/files
curl -s -F file=@plan.txt localhost:8114/users/ada/files
curl -s -i -X PUT localhost:8114/teams/research/files/fil_tfttkzgpndnaqbkgoajs4ozcu4 | head -1
```

```text
201
{"id":"fil_y5fq5omxiygheapuoqv5tooqq4","filename":"notes.txt","size":6}
{"id":"fil_tfttkzgpndnaqbkgoajs4ozcu4","filename":"plan.txt","size":8}
HTTP/1.1 204 No Content
```

2. An unpaid invoice (a foreign key to `users`) blocks the deletion with `409`. The transaction rolls back, so both files and both objects are still there:

```sh
curl -s -w '%{http_code}\n' -X POST localhost:8114/users/ada/invoices
curl -s -w '%{http_code}\n' -X DELETE localhost:8114/users/ada
curl -s localhost:8114/users/ada/files
```

```text
201
{"error":"Conflict","code":"exists"}
409
[{"id":"fil_y5fq5omxiygheapuoqv5tooqq4","filename":"notes.txt","size":6},{"id":"fil_tfttkzgpndnaqbkgoajs4ozcu4","filename":"plan.txt","size":8}]
```

3. With the invoice settled (here by hand: `sqlite3 <data dir>/app.db 'DELETE FROM invoices'`), the deletion goes through. Only `notes.txt` is purged; the team keeps `plan.txt`, and its object is the one file left under `files/`. A second deletion is a `404` and changes nothing:

```sh
curl -s -X DELETE localhost:8114/users/ada
curl -s localhost:8114/users/ada/files
curl -s localhost:8114/teams/research/files
curl -s -w '%{http_code}\n' -X DELETE localhost:8114/users/ada
```

```text
{"files_purged":1}
[]
[{"id":"fil_tfttkzgpndnaqbkgoajs4ozcu4","filename":"plan.txt","size":8}]
{"error":"Not Found","code":"not_found"}
404
```

4. Deleting one file moves it to the trash: it leaves the listing at once, and the maintenance pass purges it after the retention period.

```sh
curl -s -w '%{http_code}\n' -X POST -d '{"email":"bob@example.com"}' localhost:8114/users/bob
curl -s -F file=@notes.txt localhost:8114/users/bob/files
curl -s -w '%{http_code}\n' -X DELETE localhost:8114/users/bob/files/fil_kseybto2regmdye67e3idpjmtj
curl -s localhost:8114/users/bob/files
```

```text
201
{"id":"fil_kseybto2regmdye67e3idpjmtj","filename":"notes.txt","size":6}
204
[]
```

A storage failure after the commit cannot be shown by hand against a local disk; `TestObjectFailureIsQueuedAndRetried` makes every delete fail with an interceptor, deletes an account, and checks that the rows are gone, the object is queued, and the next maintenance pass deletes it once the backend is back.

## The code

`appSchema` puts the app's own tables in the same database as the `fs_` tables, so one transaction covers both. `invoices` references `users` without `ON DELETE`, which is the failure step 2 shows rolling back.

<!-- ds:block id=appschema-3nwqefjs -->
**appSchema** · [`examples/recipe-account-deletion/main.go:85-89`](main.go#L85-L89)

```go
`
CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS invoices (id INTEGER PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id));
CREATE TABLE IF NOT EXISTS orphaned_objects (disk TEXT NOT NULL, storage_key TEXT NOT NULL, failed_at TEXT NOT NULL, PRIMARY KEY (disk, storage_key));
`
```
<!-- /ds:block hash=73221e -->

`New` wires the local disk, the manager and the records store with the two owner types and the object error handler, then migrates.

<!-- ds:block id=new-n64tswyy -->
**New** · [`examples/recipe-account-deletion/main.go:115-142`](main.go#L115-L142)

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

`deleteAccount` is the whole deletion: `PurgeOwner` detaches every attachment of the user and purges each file nobody else uses, then the user's row goes on the same `*sql.Tx`. Objects are deleted only after the commit, so a rollback never leaves a row without its bytes.

<!-- ds:block id=app-deleteaccount-24xv49h8 -->
**App.deleteAccount** · [`examples/recipe-account-deletion/main.go:185-200`](main.go#L185-L200)

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

`deleteUserRow` deletes the app's row and turns the foreign-key failure into a `409` the client can act on.

<!-- ds:block id=deleteuserrow-8n5kpwv5 -->
**deleteUserRow** · [`examples/recipe-account-deletion/main.go:164-177`](main.go#L164-L177)

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

`rememberOrphan` runs when an object could not be deleted after its rows were committed away: the object is recorded so personal data does not linger because a backend blinked. `retryOrphans` deletes the queued objects; deleting a missing object succeeds, so a retry is always safe.

<!-- ds:block id=app-rememberorphan-7sauv2xh -->
**App.rememberOrphan** · [`examples/recipe-account-deletion/main.go:207-214`](main.go#L207-L214)

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
**App.retryOrphans** · [`examples/recipe-account-deletion/main.go:222-254`](main.go#L222-L254)

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

`shareWithTeam` gives a file a second owner, which is why deleting the user only detaches it. `trashFile` moves one file to the trash.

<!-- ds:block id=app-sharewithteam-52gjmkkp -->
**App.shareWithTeam** · [`examples/recipe-account-deletion/main.go:362-369`](main.go#L362-L369)

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

<!-- ds:block id=app-trashfile-89ztszy8 -->
**App.trashFile** · [`examples/recipe-account-deletion/main.go:351-357`](main.go#L351-L357)

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

`maintain` is the scheduled pass (purge trash older than the retention period, then retry queued deletions), and `runMaintenance` runs it every hour. The library starts no background work; the app decides when this runs.

<!-- ds:block id=app-maintain-yjfrqhgf -->
**App.maintain** · [`examples/recipe-account-deletion/main.go:260-270`](main.go#L260-L270)

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
**runMaintenance** · [`examples/recipe-account-deletion/main.go:387-400`](main.go#L387-L400)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The schema, the wiring, the deletion transaction, the orphan queue, trash and the maintenance loop, and the HTTP routes. |
| `main_test.go` | Shared files survive a deletion, a blocked deletion rolls back, a failed object deletion is queued and retried, and trash retention. |

## Next

- The guide: [Account deletion](../../docs/recipes/account-deletion.md), with edge cases and production notes.
- [Lifecycle](../../docs/records/lifecycle.md), [Attachments](../../docs/records/attachments.md) and [Consistency](../../docs/concepts/consistency.md).
- Related recipes: [recipe-saas-tenants](../recipe-saas-tenants) (removing a whole tenant), [recipe-avatars](../recipe-avatars) (the per-file version with `PreviousPurge`), and the [records-lifecycle](../records-lifecycle) example.
