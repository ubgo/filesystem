# github.com/ubgo/filesystem/records/versions

A records plugin that keeps the previous contents of a file when it is replaced (`records.Store.Replace`, `ReplaceObject`), lists them, and restores one. The guide is [docs/plugins/versions.md](../../docs/plugins/versions.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is part of the records module, which requires only `github.com/ubgo/sqlb` and the core module.

## Wire it

<!-- ds:block id=newstore-2jkdgkez -->
**newStore** · [`examples/records-versions/main.go:83-101`](../../examples/records-versions/main.go#L83-L101)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *versions.Plugin, *quota.Plugin, error) {
	v, err := versions.New(versions.WithKeep(keep))
	if err != nil {
		return nil, nil, nil, err
	}
	q, err := quota.New(unlimited)
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithVisibilityDisks(publicDisk, privateDisk), records.WithPlugins(v, q))
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, nil, err
	}
	return s, v, q, nil
}
```
<!-- /ds:block hash=e847b1 -->

A runnable version is [`examples/records-versions`](../../examples/records-versions).

## Behaviour

- **Keeping.** `KeepVersion` (`records.Versioner`) runs inside the replacement's transaction with the file as it was, records it as the newest version and takes ownership of its object (no copy). Past the cap the oldest versions are dropped: rows in the transaction, objects after the commit.
- **Restoring.** `Restore` copies the version's object to the file's current disk under `versions/<random>` and makes it the content with `ReplaceObject`; the replaced content becomes the newest version. A file in the trash is refused (`not_found`) before the copy. A failed replacement or a rolled-back outer transaction deletes the copy.
- **Visibility and trash.** On `file.visibility_changed`, `file.trashed` and `file.restored` (`records.EventHooks`) every version follows the file's object: copied to the file's disk (and the old object deleted after the commit) when they differ, as with `records.WithVisibilityDisks`, or given the file's visibility on the same disk (private while the file is trashed). A version whose object was lost is skipped. Undone on rollback.
- **Orphan sweeps.** `OwnedObjects` (`records.ObjectOwner`) reports kept versions' objects, so `Store.SweepOrphans` never deletes them.
- **Purge.** `BeforePurge` drops every version of the file.
- **Offboarding.** `records.Store.PurgeScope` purges each file (dropping its versions), then drops any version of the scope left over (`records.ScopePurger`): rows, holds and objects alike.
- **Usage.** A kept version is reported with `records.Store.Hold` (after any version dropped past the cap is released), and every version that goes (past the cap, `Delete`, purge) with `Store.Release`. `HeldBytes` (`records.HeldCounter`) totals a scope's versions for recounts. `quota.WithHeldObjects()` counts them; a refusal there rolls the replacement back.
- Object deletions that fail after a commit go to `records.WithObjectErrorHandler`.

## Table

`fs_file_versions` (constant `TableNameVersions`), migrations `versions/0001_file_versions` and `versions/0002_file_versions_scope` (the `fs_file_versions_scope (scope)` index for `HeldBytes`; on MySQL created only when `information_schema` lacks it, so it can rerun after a crash):

| Column | Constant | Type | Notes |
|---|---|---|---|
| `id` | `ColID` | key | primary key, `ver_…` |
| `scope` | `ColScope` | key | |
| `file_id` | `ColFileID` | key | unique with `number` (`fs_file_versions_number`) |
| `number` | `ColNumber` | 64-bit integer | 1, 2, 3… per file |
| `disk` | `ColDisk` | short | |
| `object_key` | `ColKey` | long | |
| `filename` | `ColFilename` | text | |
| `content_type` | `ColContentType` | text | |
| `size` | `ColSize` | 64-bit integer | |
| `sha256` | `ColSHA256` | short | |
| `replaced_at` | `ColReplacedAt` | timestamp | UTC |

Typed columns: `versions.Table`, `versions.ID`, `versions.Scope`, `versions.FileID`, `versions.Number`, `versions.Disk`, `versions.Key`, `versions.Filename`, `versions.ContentType`, `versions.Size`, `versions.SHA256`, `versions.ReplacedAt`.

## Options

| Option | What it does | Default |
|---|---|---|
| `versions.WithKeep(n)` | How many previous contents each file keeps; below 1 is `CodeInvalidArgument` from `New` | `DefaultKeep` (10) |
| `versions.WithKeepFunc(fn)` | Decides each file's history with `fn(ctx, KeepRequest{File}) (KeepPolicy{Count, MaxAge}, error)` on every replacement and in `Trim`, instead of one count for every file ("30 days of versions on Pro"); fails closed: an error, a `Count` below 1 or a negative `MaxAge` refuses the replacement | `WithKeep`'s count for every file |
| `versions.WithRestoreKey(fn)` | Names the key of each restored copy with `fn(ctx, RestoreKeyRequest{File, Version}) (string, error)`, so it lands under the tenant's own prefix; `versions.RestoreBesideFile` puts it in a `versions/` directory beside the file's object. Fails closed: an error, an empty key, the file's or version's own key, or a key that already holds an object refuses the restore before anything is written | `versions/` and a random name at the disk's root |
| `versions.WithClock(now)` | The clock for `ReplacedAt`, for tests | the Store's clock (`records.WithClock`) |

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...) (*Plugin, error)` | A plugin |
| `(*Plugin).Versions(ctx, s, scope, fileID)` | A file's versions, newest first |
| `(*Plugin).Version(ctx, s, scope, id)` | One version, or `CodeNotFound` |
| `(*Plugin).Restore(ctx, s, scope, id)` | Makes a version the file's content again and returns the file; a trashed file is `CodeNotFound` |
| `(*Plugin).Delete(ctx, s, scope, id)` | Removes one version, its object after the commit |
| `(*Plugin).Trim(ctx, s, scope, fileID)` | Applies the file's history policy now (count and `MaxAge`) and returns how many versions it dropped; schedule it so an age limit holds for files nobody replaces |
| `(*Plugin).TrimScope(ctx, s, scope)`, `TrimPageSize` | `Trim` for every file of the scope that has versions, paged by file ID (`TrimPageSize`, 100) with one transaction per file, so a large scope holds no long transaction; a failing file does not stop the rest (the first failure is returned, naming it). Schedule it per scope |
| `KeepPolicy`, `KeepRequest`, `KeepFunc`, `RestoreKeyRequest`, `RestoreKeyFunc`, `RestoreBesideFile` | The types of `WithKeepFunc` and `WithRestoreKey`, and the ready restore-key function |
| `(*Plugin).MoveFileDisk(ctx, s, f, to)` | `records.FileDiskMover`: called by `Store.MoveFile` inside its transaction, moves the file's kept versions from `f.Disk` to `to` (copy, visibility, row; copies deleted on rollback, old objects after the commit) |
| `(*Plugin).RenameDisk(ctx, s, from, to)` | `records.DiskRenamer`: called by `Store.RenameDisk` inside its transaction, points every kept version on `from` at `to` (rows only) |
| `(*Plugin).HeldBytes(ctx, s, scope)` | The total size of a scope's kept versions (`records.HeldCounter`), which `quota.Recount` adds with `WithHeldObjects` |
| `Version` | `ID, Scope, FileID, Number, Disk, Key, Filename, ContentType, Size, SHA256, ReplacedAt` |
| `DefaultKeep`, `IDPrefix` (`"ver_"`), `PluginName` (`"versions"`) | Constants |

The plugin implements `records.Plugin`, `records.Migrator`, `records.FileHooks`, `records.Versioner`, `records.EventHooks`, `records.ObjectOwner`, `records.HeldCounter`, `records.DiskRenamer` and `records.FileDiskMover`.

## Limits

- Version bytes are counted by the quota plugin only with `quota.WithHeldObjects()`; otherwise the cap is the bound.
- Restoring with a small cap can drop the restored version's own row (its content lives on as the current content).
