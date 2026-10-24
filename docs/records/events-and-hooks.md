# Events and hooks

Every change the Store makes can be seen, extended or refused by code you install: write an audit row, update a search index, call a webhook, stamp metadata on new files, refuse to purge a file under legal hold, refuse new content of the wrong type. All of it is done with plugins: a value with a `Name()` that implements one or more optional interfaces. The bundled plugins ([audit](../plugins/audit.md), [quota](../plugins/quota.md), [versions](../plugins/versions.md) and the rest) use exactly these interfaces; nothing is reserved for them.

Read this page when you need to react to file changes, or before writing a plugin; [Write a records plugin](../extending/write-a-records-plugin.md) goes further into packaging one.

## Before you start

- A Store, as built in [Files](files.md).
- The example is [`examples/records-events`](../../examples/records-events): three small plugins installed together on SQLite. `go run ./records-events` in `examples/` prints the changelog of one run that produces every event kind, a rolled-back change, a refused purge and a refused replacement; `go test ./records-events` checks them.

## The interfaces

| Interface | Methods | Called |
|---|---|---|
| `records.Plugin` | `Name() string` | Required. Unique per Store, matches `^[a-z][a-z0-9_.]{0,63}$`; `records` is reserved. Owns the plugin's migrations. |
| `records.Migrator` | `Migrations(engine string) ([]records.Migration, error)` | By `Migrate`, `Migrations` and `MigrationSQL`. See [Migrations](migrations.md). |
| `records.FileHooks` | `BeforeCreate(ctx, s, *File)`, `AfterCreate(ctx, s, File)`, `BeforePurge(ctx, s, File)` | Around `CreateFile` (and so `Upload`) and `Purge` (and so `PurgeOwner`, `SweepTrash`, `SetAttachment` with `PreviousPurge`). A Go interface: implement all three, with no-ops for the ones you do not need. |
| `records.ReplaceHooks` | `BeforeReplace(ctx, s, current File, next *File)` | By `Replace` and `ReplaceObject`, before the row changes. |
| `records.ReleaseHooks` | `BeforeRelease(ctx, s, f *File)` | By `PassScan` (and `Scan`) when a quarantined file is released, before the row changes; may set `f.Metadata` only. Where a describer reads the bytes it skipped while the file was quarantined. |
| `records.Versioner` | `KeepVersion(ctx, s, previous File) (bool, error)` | By `Replace` and `ReplaceObject`, after the row changed. Returning true takes ownership of the previous object. |
| `records.EventHooks` | `OnEvent(ctx, s, records.Event)` | After every change, for every event kind. |
| `records.ScopePurger` | `PurgeScope(ctx, s, scope string) error` | By `Store.PurgeScope`, in one transaction, once every file of the scope has been purged: the plugin deletes its rows that belong to the scope rather than to a file. Must succeed when there are none, since a rerun calls it again. See [Lifecycle](lifecycle.md#offboard-a-whole-scope). |
| `records.FileDiskMover` | `MoveFileDisk(ctx, s, f File, to string) error` | By `Store.MoveFile`, inside its transaction after the file's row changed (`f.Disk` is the old disk): the plugin moves its own objects for that one file. See [Lifecycle](lifecycle.md#move-a-file-to-another-disk). |
| `records.DiskRenamer` | `RenameDisk(ctx, s, from, to string) error` | By `Store.RenameDisk`, inside its transaction, after `fs_files.disk` changed. A plugin that stores disk names of its own rewrites them here. See [Replace and visibility](replace-and-visibility.md#step-5--rename-a-disk). |
| `records.UsageHooks` | `OnHold(ctx, s, records.HeldObject)`, `OnRelease(ctx, s, records.HeldObject)` | When another plugin calls `Store.Hold` or `Store.Release` for an object it keeps beside a file (a kept version, an image variant), inside that plugin's transaction. An `OnHold` error (such as `quota_exceeded`) aborts the holder's change. [Quota](../plugins/quota.md) uses it to charge held bytes. |
| `records.HeldCounter` | `HeldBytes(ctx, s, scope string) (int64, error)` | By `Store.HeldBytes`, which sums every counter: the bytes a plugin holds in a scope, for a recount. |
| `records.ObjectOwner` | `OwnedObjects(ctx, s, disk string, keys []string) (map[string]bool, error)` | By `SweepOrphans`, outside any transaction, before it deletes objects no file row names. A plugin that stores objects of its own reports them here so the sweep keeps them. |

Every hook runs inside the operation's transaction, with a `ctx` that carries it. Statements a plugin runs through the Store (`s.Exec`, `s.One`, `s.Each`, or `s.Conn(ctx)` directly) join that transaction, so they commit or roll back with the change. A hook that returns an error aborts the operation and rolls it all back. Plugins run in the order they were installed with `WithPlugins`.

## Step 1 — React to every change

`Changelog` is an `EventHooks` plugin with a table of its own. Its `OnEvent` writes one row per event in the change's transaction, and queues a notification (standing in for a webhook) with `AfterCommit`, so a change that rolls back is never announced.

<!-- ds:block id=changelog-95pvcy28 -->
**Changelog** · [`examples/records-events/main.go:79-82`](../../examples/records-events/main.go#L79-L82)

```go
type Changelog struct {
	// Sent collects notifications, standing in for webhook calls.
	Sent []string
}
```
<!-- /ds:block hash=0e4a82 -->

<!-- ds:block id=changelog-onevent-smrprhjr -->
**Changelog.OnEvent** · [`examples/records-events/main.go:107-116`](../../examples/records-events/main.go#L107-L116)

```go
func (c *Changelog) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	ins := sqlb.NewInsert(s.Dialect()).Into(changes).Row(
		sqlb.Set(changeScope, e.Scope), sqlb.Set(changeKind, string(e.Kind)), sqlb.Set(changeFileID, e.FileID), sqlb.Set(changeAt, e.At),
	)
	if _, err := s.Exec(ctx, ins); err != nil {
		return err
	}
	s.AfterCommit(ctx, func(context.Context) { c.Sent = append(c.Sent, string(e.Kind)) })
	return nil
}
```
<!-- /ds:block hash=c38355 -->

Its table comes from `Migrations`, built with `records.DDL` from the same typed columns `OnEvent` writes, spelled for the engine by `records.TypesFor(engine)`, so the same plugin runs on SQLite, Postgres and MySQL ([Migrations](migrations.md#build-them-from-the-typed-columns-recordsddl)):

<!-- ds:block id=changelog-migrations-b8j9my5y -->
**Changelog.Migrations** · [`examples/records-events/main.go:91-101`](../../examples/records-events/main.go#L91-L101)

```go
func (c *Changelog) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: changes,
		Columns: []ddl.ColumnSpec{
			records.Column(changeScope, t.Key), records.Column(changeKind, t.Short),
			records.Column(changeFileID, t.Key), records.Column(changeAt, t.Timestamp),
		},
	}).Statements()
	return []records.Migration{{Owner: c.Name(), Name: "0001_changes", Statements: st}}, err
}
```
<!-- /ds:block hash=d0d169 -->

The demo installs it with the other two plugins:

<!-- ds:block id=newstore-9yrcnb6u -->
**newStore** · [`examples/records-events/main.go:191-211`](../../examples/records-events/main.go#L191-L211)

```go
func newStore(ctx context.Context, db *sql.DB, log *Changelog) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	archive, err := filesystem.NewDisk(archiveDisk, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk, archive)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerPost),
		records.WithPlugins(log, Guard{}, TypeLock{}))
	if err != nil {
		return nil, err
	}
	_, err = s.Migrate(ctx)
	return s, err
}
```
<!-- /ds:block hash=e67b61 -->

Check: `Migrate` applies `changelog/0001_changes` after the core migrations.

## Step 2 — See every event kind

One run that uploads, attaches, reorders, trashes, restores, replaces, quarantines the file for a rescan and releases it (`PassScan`), publishes, moves it to another disk, detaches and purges:

<!-- ds:block id=everykind-nd3v32m9 -->
**everyKind** · [`examples/records-events/main.go:215-250`](../../examples/records-events/main.go#L215-L250)

```go
func everyKind(ctx context.Context, s *records.Store) (records.File, error) {
	post := records.Owner{Type: ownerPost, ID: "post_1"}
	f, err := s.Upload(ctx, tenant, diskName, strings.NewReader("hello"), "a.txt", textPolicy)
	if err != nil {
		return f, err
	}
	a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery)
	if err != nil {
		return f, err
	}
	steps := []func() error{
		func() error { return s.Reorder(ctx, tenant, post, roleGallery, []string{a.ID}) },
		func() error { return s.Trash(ctx, tenant, f.ID) },
		func() error { return s.Restore(ctx, tenant, f.ID) },
		func() error {
			_, err := s.Replace(ctx, tenant, f.ID, strings.NewReader("hello again"), "", textPolicy)
			return err
		},
		func() error {
			_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusQuarantined, "rescan")
			return err
		},
		// The scan passed: file.status_changed (ready), then file.released.
		func() error { _, err := s.PassScan(ctx, tenant, f.ID); return err },
		func() error { return s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic) },
		func() error { _, err := s.MoveFile(ctx, tenant, f.ID, archiveDisk); return err },
		func() error { return s.Detach(ctx, tenant, a.ID) },
		func() error { return s.Purge(ctx, tenant, f.ID) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return f, err
		}
	}
	return f, nil
}
```
<!-- /ds:block hash=22e10c -->

Check: `changelog: file.created attachment.created attachments.reordered file.trashed file.restored file.replaced file.status_changed file.status_changed file.released file.visibility_changed file.moved attachment.deleted file.purged`, and `sent: 13`. The two `file.status_changed` are the file quarantined for a rescan and its release, which `file.released` follows.

## The events

`records.Event` has `Kind`, `Scope`, `At` (the Store's clock, UTC) always, and these fields by kind (`records.EventKindValues` lists them all):

| Kind | Emitted by | Fields |
|---|---|---|
| `file.created` | `CreateFile`, `Upload` | `FileID`, `File` |
| `file.trashed` | `Trash`, `SetAttachment` with `PreviousTrash` | `FileID`, `File` |
| `file.restored` | `Restore` | `FileID`, `File` |
| `file.replaced` | `Replace`, `ReplaceObject`, `versions.Restore` | `FileID`, `File` |
| `file.visibility_changed` | `SetVisibility` | `FileID`, `File` |
| `file.moved` | `MoveFile` | `FileID`, `File` (on its new disk), `Detail` with `records.DetailFrom` and `records.DetailTo`, the old and new disk names; see [Lifecycle](lifecycle.md#move-a-file-to-another-disk) |
| `file.status_changed` | `SetStatus`, `PassScan`, `FailScan` | `FileID`, `File` (its new `Status` and `StatusDetail`); see [Processing status](processing-status.md) |
| `file.released` | `PassScan`, `Scan` | `FileID`, `File` (ready, with what `ReleaseHooks` set), right after its `file.status_changed`: the signal for plugins that skipped a quarantined file to do their work now |
| `file.purged` | `Purge`, `PurgeOwner`, `SweepTrash`, `SetAttachment` with `PreviousPurge` | `FileID`, `File` (as it was); the file's attachments go with it without events of their own |
| `attachment.created` | `Attach`, `SetAttachment` | `FileID`, `File`, `AttachmentID`, `Owner`, `Role` |
| `attachment.deleted` | `Detach`, `SetAttachment`, `PurgeOwner` | `FileID`, `File`, `AttachmentID`, `Owner`, `Role` |
| `attachments.reordered` | `Reorder` | `Owner`, `Role` |

`Event.File` is a `*records.File`: the file as the change left it (trashed with its `DeletedAt`, the new content, the new visibility or status), or as it was for `file.purged`, whose row is gone. A hook, a webhook or a log has the file's name, size, type and status without a query of its own. It is nil for `attachments.reordered`, and for a plugin's event unless that plugin sets it. Hooks share it, so they must not change it.

Changes that change nothing emit nothing: trashing a trashed file, restoring a live one, detaching an unknown attachment, setting the current visibility or status, setting the attachment that is already the role's only file.

### Events from plugins

Plugins report their own changes through the same hooks, so an audit log or a webhook sees a folder move, a tag or a share link the way it sees an upload. Their kinds are named `<plugin name>.<action>`, and they use two more fields of `records.Event`: `Subject`, the ID of what the event is about when it is not a file (a folder, a share link), and `Detail`, a `map[string]string` of particulars under keys the plugin exports as constants. The Store's own kinds never set `Subject`; only `file.moved` sets `Detail` (`records.DetailFrom` and `records.DetailTo`). The bundled plugins emit:

| Kind | Emitted by | Fields |
|---|---|---|
| `folders.created`, `folders.deleted` | `folders.Create`, `folders.Delete` | `Subject` (the folder); `Detail` `parent`, `name` |
| `folders.renamed` | `folders.Rename` | `Subject`; `Detail` `name` (new), `from` (old) |
| `folders.moved` | `folders.Move` | `Subject`; `Detail` `parent` (new), `from` (old parent) |
| `folders.file_put` | `folders.Put` | `FileID`; `Detail` `folder` (new), `from` (old); the root is `""` |
| `folders.shortcut_added`, `folders.shortcut_removed` | `folders.AddShortcut`; `folders.RemoveShortcut`, and a `Put` or `Delete` that removes a shortcut (`WithShortcuts`) | `FileID`; `Detail` `folder` |
| `tags.added`, `tags.removed` | `tags.Set`, `tags.Add`, `tags.Remove` | `FileID`; `Detail` `tag`; one event per tag |
| `tags.renamed` | `tags.Rename` | `Detail` `tag` (new), `from` (old) |
| `tags.deleted` | `tags.Delete` | `Detail` `tag` |
| `share.created` | `share.Create` | `FileID`, `Subject` (the link); `Detail` `expires_at` (RFC 3339, UTC), `max_downloads` (`0` is unlimited) |
| `share.downloaded` | `share.Resolve` (and so `GET` on `share.Handler`) | `FileID`, `Subject`; `Detail` `downloads`, the count including this one |
| `share.revoked` | `share.Revoke` | `FileID`, `Subject` |

Each plugin lists its kinds in its own `EventKindValues`. As with the Store's events, a call that changes nothing emits nothing (a rename to the current name, a move to where it already is, revoking a revoked link), and so does housekeeping on rows that can no longer matter (`share.Prune`). An `OnEvent` error rolls the plugin's change back. Share tokens never appear in events. A hook that keeps only the Store's kinds checks `slices.Contains(records.EventKindValues, e.Kind)`.

A plugin of your own emits with `s.Emit(ctx, p, records.Event{...})`, inside the `Tx` of the change it reports. The Store stamps `At` and refuses, with `invalid_argument`, a kind that is not `<p.Name()>.<action>` (action `[a-z][a-z0-9_]*`, at most `records.MaxEventKindBytes` = 64 bytes in all), a kind of the Store's own, a plugin that is not installed, and a call outside a transaction: a plugin cannot speak for the Store or for another plugin, and an event outside the change's transaction could be logged for a change that rolled back.

The event carries IDs, not the file: a hook that needs the row reads it with `s.File(ctx, e.Scope, e.FileID)` inside the same transaction. For `file.purged` the row is already gone; keep what you need in `BeforePurge`.

## Step 3 — Only after the commit

An `OnEvent` error rolls the change back, so do not call slow or external services there: a webhook that times out would undo the user's upload, and one that succeeded would announce a change a later step rolled back. Queue that work with `s.AfterCommit(ctx, fn)`, which runs `fn` after the commit and never after a rollback.

<!-- ds:block id=rolledback-94kuxr9u -->
**rolledBack** · [`examples/records-events/main.go:255-262`](../../examples/records-events/main.go#L255-L262)

```go
func rolledBack(ctx context.Context, s *records.Store) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.Upload(ctx, tenant, diskName, strings.NewReader("x"), "x.txt", textPolicy); err != nil {
			return err
		}
		return errAbort
	})
}
```
<!-- /ds:block hash=458206 -->

Check: the upload inside a failing transaction leaves `rolled back: handler failed; rows 9, sent 9`: no changelog row and no notification were added. `s.OnRollback(ctx, fn)` is the opposite: work that undoes something outside the database (an object written, a visibility changed) when the transaction fails.

## Step 4 — Stamp and guard files

`Guard` implements `FileHooks`. `BeforeCreate` runs before the row is inserted and may change the file's metadata and visibility (not its ID, disk or key); here it adds a default classification. `BeforePurge` refuses to purge a file under legal hold.

<!-- ds:block id=guard-beforecreate-abrznwmq -->
**Guard.BeforeCreate** · [`examples/records-events/main.go:138-147`](../../examples/records-events/main.go#L138-L147)

```go
func (Guard) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	if f.Metadata[metaClass] == "" {
		meta := map[string]string{metaClass: defaultClass}
		for k, v := range f.Metadata {
			meta[k] = v
		}
		f.Metadata = meta
	}
	return nil
}
```
<!-- /ds:block hash=f01504 -->

<!-- ds:block id=guard-beforepurge-nq6eteax -->
**Guard.BeforePurge** · [`examples/records-events/main.go:154-159`](../../examples/records-events/main.go#L154-L159)

```go
func (Guard) BeforePurge(_ context.Context, _ *records.Store, f records.File) error {
	if f.Metadata[metaHold] == "true" {
		return filesystem.Errorf(filesystem.CodePermission, "file %s is under legal hold", f.ID)
	}
	return nil
}
```
<!-- /ds:block hash=3c84aa -->

Check: `purge under hold: permission; metadata map[classification:internal legal_hold:true]`. The file keeps its row and object; the refusal reaches the caller unchanged. `AfterCreate` runs after the insert, with the final row, for work that needs the file to exist (a row of your own that references it).

`BeforeCreate` copies the map before changing it: `f.Metadata` may be the caller's own map.

## Step 5 — Vet new content

`TypeLock` implements `ReplaceHooks`: new content must keep the file's content type.

<!-- ds:block id=typelock-beforereplace-947bgqwy -->
**TypeLock.BeforeReplace** · [`examples/records-events/main.go:170-175`](../../examples/records-events/main.go#L170-L175)

```go
func (TypeLock) BeforeReplace(_ context.Context, _ *records.Store, cur records.File, next *records.File) error {
	if cur.ContentType != next.ContentType {
		return filesystem.Errorf(filesystem.CodeRejected, "content type %s cannot become %s", cur.ContentType, next.ContentType)
	}
	return nil
}
```
<!-- /ds:block hash=2cee6f -->

Check: replacing a text file with PNG bytes prints `replace text with PNG: rejected`, and the uploaded PNG object is deleted again by `Replace`.

## Reads: `Served` and `ServeHooks`

Changes are events; reads are not, because a read changes nothing and there are far more of them. Every read is reported with `Store.Served`, and every installed `records.ServeHooks` plugin hears of it: [egress](../plugins/egress.md) meters the bytes per scope and day, and [audit](../plugins/audit.md) logs it when `WithAccessLog` is on.

The chokepoint is the link itself: `Store.URL` and `Store.DownloadURL` report every link they issue (`access.link`, `Bytes` the file's size) and return none when the report fails, so every caller is metered without remembering to be: recordshttp's file route and its listings (one report per file listed), the share handler (`records.ServedAs(records.ServeShare, linkID)`), the variants handler, `variants.URL` and `SrcSet` (`access.variant`, `Subject` the variant name, `Bytes` the variant's own size), and any application code. Only bytes the application streams itself need a call of their own, before writing them:

```go
err := store.Served(ctx, records.Served{Kind: records.ServeStream, Scope: scope, FileID: f.ID, Bytes: written})
```

A read that none of the four kinds names (a zip of a folder, a rendered PDF page) reports a kind of its own, `access.<name>.<action>` with each part `[a-z][a-z0-9_]*`, at most 64 bytes (`ServeKind.Valid`): `records.Served{Kind: "access.zip.download", …}`. The `access.` prefix is required because the audit plugin keeps reads beside changes and tells them apart by it.

For a signed link, `Bytes` is the size of the object it points at, an upper bound of what the client fetches. A public file's URL never expires and is fetched without the application seeing it, so for public files the count is of links issued, not of fetches: the bytes actually served are in your CDN's or the backend's access logs. `OnServed` runs outside any transaction; every hook runs even when one fails, and their errors are joined.

Every `Event` also carries an `ID`: `evt_` and random from the Store, or a deterministic one a plugin passes to `Emit` (egress does, so a daily total reported twice has one ID). Consumers that may see an event twice (webhook receivers, billing) drop the IDs they have processed.

## What a plugin can use

| Store method | Use |
|---|---|
| `Conn(ctx)` | The open transaction, or the database: run your own statements on it. |
| `Exec(ctx, stmt)`, `One(ctx, stmt)`, `Each(ctx, stmt, fn)` | Run a `sqlb` builder on `Conn(ctx)`. |
| `Dialect()` | The dialect to build statements for. |
| `Disks()` | The disk manager, for plugins that work on bytes (variants, versions). |
| `Tx(ctx, fn)` | A transaction of the plugin's own, or join the caller's. |
| `AfterCommit`, `OnRollback`, `DeleteObjectAfterCommit` | Work at the end of the transaction; see [Lifecycle](lifecycle.md). |
| `records.TypesFor`, `records.NewDDL` | Engine-portable, re-runnable DDL built from typed columns; see [Migrations](migrations.md). |
| `records.IsUniqueViolation(err)` | Recognise a unique-constraint failure on every engine. |
| `records.EncodeCursor`, `records.DecodeCursor` | Keyset pagination like `Files`. |
| `Actor(ctx)` | Who the request acts for: `records.WithActorFunc`'s lookup, else the actor set with `records.WithActor` (`records.ActorFrom`). Read it here rather than with `ActorFrom`, so an application's own lookup rules your plugin too. |
| `Hold(ctx, o)`, `Release(ctx, o)`, `HeldBytes(ctx, scope)` | Report an object the plugin keeps beside a file (`records.HeldObject`: `Plugin`, `Scope`, `FileID`, `Disk`, `Key`, `Size`) to every `UsageHooks` plugin in install order, first error wins; call them inside the transaction that adds or removes the object's row. `HeldBytes` sums every `HeldCounter`. |
| `Served(ctx, Served)` | Report a read to every `ServeHooks` plugin; see [Reads](#reads-served-and-servehooks). |
| `Now()` | The Store's clock (`WithClock`), UTC at microseconds: stamp and compare times with it. A plugin with a `WithClock` option of its own defaults to it, so one clock rules a deployment and its tests. |
| `NewID(prefix)` | A new row ID from the Store's generator (`WithIDs`, else the prefix plus 128 random bits): create your rows' IDs with it, so an application's ID scheme (UUIDv7, a publicid) covers your table too. |
| `Emit(ctx, p, Event)` | Report the plugin's own change to every `EventHooks` plugin, inside the change's transaction; kinds are `<p.Name()>.<action>` (see [Events from plugins](#events-from-plugins)). |

## What can go wrong

- A change fails with your plugin's error: every hook error aborts and rolls back the operation. Return errors only to refuse; log anything else.
- A webhook fires for a change that does not exist: it was called from `OnEvent` instead of `AfterCommit`.
- `invalid_argument: records: invalid plugin name` or `exists: records: plugin "…" installed twice` from `records.New`.
- `invalid_argument: records: plugin "x" ships a migration owned by "y"`: every `Migration.Owner` must be the plugin's `Name()`.
- `invalid_argument: records: plugin "x" cannot emit …` or `records: Emit must run inside the change's transaction`: a plugin's `Emit` used a kind outside its own name, or ran outside `Store.Tx`.
- A hook sees a stale row: it read through the database instead of the transaction. Use `s.Conn(ctx)` or the `Exec`/`One`/`Each` helpers with the `ctx` you were given.

## Next

- [Write a records plugin](../extending/write-a-records-plugin.md): a full plugin with options, tables, tests.
- [Audit](../plugins/audit.md): the bundled `EventHooks` plugin.
- [Operations reference](../reference/operations.md): event kinds next to the disk's op kinds.
