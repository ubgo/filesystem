# An audit log of file changes

The audit plugin of the records layer: every change to a file or an attachment writes an entry in `fs_audit`, inside the same transaction, with the actor who made it. That gives a file page its history, a user page its activity feed, and compliance its trail, and it never logs a change that rolled back. This program installs the plugin, names the actor of each request, makes one change of every kind, reads a file's history and an actor's activity page by page, shows the rollback guarantee, refuses an oversized actor, and prunes old entries. It uses `github.com/ubgo/filesystem/records` and `records/audit` on SQLite over an in-memory disk. The guide [Audit log](../../docs/plugins/audit.md) explains each step.

## Run it

```sh
cd examples
go run ./records-audit
go test ./records-audit
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-audit
```

```text
history of the purged file (12 entries, newest first):
  file.purged              actor=user:alice owner=/ role=
  file.restored            actor=user:alice owner=/ role=
  file.trashed             actor=user:alice owner=/ role=
  attachment.deleted       actor=user:alice owner=post/42 role=gallery
  file.moved               actor=user:alice owner=/ role= from=media to=archive
  file.visibility_changed  actor=user:alice owner=/ role=
  file.released            actor=user:alice owner=/ role=
  file.status_changed      actor=user:alice owner=/ role=
  file.status_changed      actor=user:alice owner=/ role=
  file.replaced            actor=user:alice owner=/ role=
  attachment.created       actor=user:alice owner=post/42 role=gallery
  file.created             actor=user:alice owner=/ role=
activity of user:alice: 13 entries in 5 pages
rollback: demo: request failed after the upload
activity after the rollback: 13 entries
long actor: invalid_argument
pruned 14 entries
change feed: 1:file.created, 2:file.replaced, 4:file.trashed (reads left out)
next poll from the stored cursor: 5:file.restored
verify: 5 entries, err=<nil>
after an edit with SQL: conflict, chain broken true
rotated chain: 1:file.created key="" source="" "" ip=""
rotated chain: 2:file.replaced key="2026-10" source="ios" "app/3.2" ip=""
verify across the rotation: 2 entries, err=<nil>
```

1. As `user:alice`, one file goes through upload, attach to post 42, reorder, replace, make public, mark its processing as failed, move to the `archive` disk, detach, trash, restore and purge. A second upload as `system:cron` adds an entry under another actor.
2. The file's history lists its twelve entries newest first (the rescan's quarantine and release are two status changes and `file.released`) (the move with the disks it went from and to), and stays readable after the file is purged. The reorder is an owner event, so it appears in Alice's activity but not in the file's history.
3. Alice's activity, read three entries at a time by following `NextCursor`, holds all thirteen of her entries.
4. An upload inside a transaction that then fails leaves no entry: the activity count is still thirteen.
5. An actor longer than `audit.MaxActorBytes` is refused with `invalid_argument` rather than logged truncated.
6. `Prune` with a cutoff past the 90-day retention deletes all fourteen entries, Alice's thirteen and the background upload's. The library starts no background work: an app schedules this.
7. `chain.go` opens a second Store with a plugin made `WithHashChain` and `WithAccessLog`. Dana uploads, replaces, streams and trashes a file. The change feed returns the three changes in commit order (sequence 3 is the read, left out of the feed), two to a page, and the stored cursor picks up the restore made afterwards.
8. `Verify` walks the five entries; after one entry's actor is rewritten with SQL it fails with `conflict` wrapping `audit.ErrChainBroken`.
9. `rotate.go` rotates the chain key on a database of its own. Erin's upload is chained with `WithHashChain(oldKey)`, so its entry carries the key ID `""`. The Store is then opened with `audit.WithHashChainKeyring(ring)`, whose current key is `2026-10` and which still holds the old key under `""`, and with `audit.WithSource()`.
10. A replacement made from the iOS app (the request's `records.WithSource`) is hashed under `2026-10` and logs the channel and client. The IP is dropped, since logging it needs `WithSourceIP` as well.
11. `Verify` checks each entry under the key it recorded and passes across the rotation. The test `TestRotationNeedsTheOldKey` shows a ring that dropped the old key fails `Verify` rather than skipping the entry.

## The code

Installing the plugin; `Migrate` creates `fs_audit` next to the core tables:

<!-- ds:block id=newstore-4az2gwj7 -->
**newStore** · [`examples/records-audit/main.go:97-107`](main.go#L97-L107)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, au *audit.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerPost), records.WithPlugins(au))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=d59042 -->

For an app that already keeps the user in its own context key, the plugin can read the actor from there instead of `audit.WithActor`:

<!-- ds:block id=newpluginwithownkey-ss59fp5h -->
**newPluginWithOwnKey** · [`examples/records-audit/main.go:116-123`](main.go#L116-L123)

```go
func newPluginWithOwnKey() *audit.Plugin {
	return audit.New(audit.WithActorFunc(func(ctx context.Context) string {
		if id, ok := ctx.Value(sessionUserKey{}).(string); ok {
			return "user:" + id
		}
		return ""
	}))
}
```
<!-- /ds:block hash=70ffae -->

One change of every event kind:

<!-- ds:block id=everychange-aq6m25az -->
**everyChange** · [`examples/records-audit/main.go:128-167`](main.go#L128-L167)

```go
func everyChange(ctx context.Context, s *records.Store, actor string) (records.File, error) {
	ctx = audit.WithActor(ctx, actor)
	post := records.Owner{Type: ownerPost, ID: postID}
	f, err := s.Upload(ctx, tenant, diskName, strings.NewReader("draft one"), "notes.txt", textPolicy) // file.created
	if err != nil {
		return records.File{}, err
	}
	a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery) // attachment.created
	if err != nil {
		return records.File{}, err
	}
	if err := s.Reorder(ctx, tenant, post, roleGallery, []string{a.ID}); err != nil { // attachments.reordered
		return records.File{}, err
	}
	if _, err := s.Replace(ctx, tenant, f.ID, strings.NewReader("draft two"), "", textPolicy); err != nil { // file.replaced
		return records.File{}, err
	}
	if _, err := s.SetStatus(ctx, tenant, f.ID, records.StatusQuarantined, "rescan"); err != nil { // file.status_changed
		return records.File{}, err
	}
	if _, err := s.PassScan(ctx, tenant, f.ID); err != nil { // file.status_changed, file.released
		return records.File{}, err
	}
	if err := s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic); err != nil { // file.visibility_changed
		return records.File{}, err
	}
	if _, err := s.MoveFile(ctx, tenant, f.ID, archiveDisk); err != nil { // file.moved
		return records.File{}, err
	}
	if err := s.Detach(ctx, tenant, a.ID); err != nil { // attachment.deleted
		return records.File{}, err
	}
	if err := s.Trash(ctx, tenant, f.ID); err != nil { // file.trashed
		return records.File{}, err
	}
	if err := s.Restore(ctx, tenant, f.ID); err != nil { // file.restored
		return records.File{}, err
	}
	return f, s.Purge(ctx, tenant, f.ID) // file.purged
}
```
<!-- /ds:block hash=fbe576 -->

Reading a file's history and walking an actor's feed page by page:

<!-- ds:block id=history-23qsz95p -->
**history** · [`examples/records-audit/main.go:171-174`](main.go#L171-L174)

```go
func history(ctx context.Context, s *records.Store, au *audit.Plugin, fileID string) ([]audit.Entry, error) {
	page, err := au.Entries(ctx, s, audit.Query{Scope: tenant, FileID: fileID})
	return page.Entries, err
}
```
<!-- /ds:block hash=78dc27 -->

<!-- ds:block id=activity-n7j5jaye -->
**activity** · [`examples/records-audit/main.go:179-193`](main.go#L179-L193)

```go
func activity(ctx context.Context, s *records.Store, au *audit.Plugin, actor string) ([]audit.Entry, int, error) {
	var all []audit.Entry
	q := audit.Query{Scope: tenant, Actor: actor, Limit: activityPage}
	for pages := 1; ; pages++ {
		page, err := au.Entries(ctx, s, q)
		if err != nil {
			return nil, pages, err
		}
		all = append(all, page.Entries...)
		if page.NextCursor == "" {
			return all, pages, nil
		}
		q.Cursor = page.NextCursor
	}
}
```
<!-- /ds:block hash=31cbdc -->

The guarantees: a rolled-back change is never logged, and an actor that does not fit is refused:

<!-- ds:block id=rolledback-df6ptbdc -->
**rolledBack** · [`examples/records-audit/main.go:199-207`](main.go#L199-L207)

```go
func rolledBack(ctx context.Context, s *records.Store) error {
	ctx = audit.WithActor(ctx, actorAlice)
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.Upload(ctx, tenant, diskName, strings.NewReader("lost"), "lost.txt", textPolicy); err != nil {
			return err
		}
		return errDemoRollback
	})
}
```
<!-- /ds:block hash=5ea3a8 -->

<!-- ds:block id=toolongactor-vjp5uu4w -->
**tooLongActor** · [`examples/records-audit/main.go:212-216`](main.go#L212-L216)

```go
func tooLongActor(ctx context.Context, s *records.Store) error {
	ctx = audit.WithActor(ctx, "user:"+strings.Repeat("x", audit.MaxActorBytes))
	_, err := s.Upload(ctx, tenant, diskName, bytes.NewReader([]byte("hi")), "hi.txt", textPolicy)
	return err
}
```
<!-- /ds:block hash=fb7627 -->

Retention, run from the app's scheduler:

<!-- ds:block id=prune-vhnhy5ef -->
**prune** · [`examples/records-audit/main.go:221-223`](main.go#L221-L223)

```go
func prune(ctx context.Context, s *records.Store, au *audit.Plugin, now time.Time) (int64, error) {
	return au.Prune(ctx, s, now.Add(-retention))
}
```
<!-- /ds:block hash=c6ea22 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the plugin, every event kind, history, activity, rollback, the actor limit and pruning |
| `chain.go` | The hash chain, the access log and the change feed, with a tampered entry |
| `rotate.go` | Chain key rotation with `WithHashChainKeyring`, and the request's source with `WithSource`; code: [`newRotatedPlugin`](ds:block?id=newrotatedplugin-v2k7r4wn) |
| `main_test.go` | Runs the same steps, plus a check that every event kind is logged, the actor from the app's own key, pruning that keeps recent entries, a bad limit or cursor, and a keyring without the old key failing `Verify` |

## Next

- The guide: [Audit log](../../docs/plugins/audit.md), and [Events and hooks](../../docs/records/events-and-hooks.md) for the events the plugin listens to.
- Related examples: [records-events](../records-events) to write a plugin of your own on the same events, [start-production](../start-production) for audit pruning in a scheduled job.
