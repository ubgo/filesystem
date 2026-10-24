# Versions

The versions plugin keeps the previous contents of a file when it is replaced, lists them, and restores one: the version history of document managers and drives ("restore the version from Tuesday"). Use it when users overwrite files in place and expect to undo it.

A kept version is the object the file pointed to before the replacement, so keeping it costs no copy. Each file keeps at most a set number of versions, oldest dropped first. Restoring copies a version's object and makes the copy the file's content, so the content it replaces becomes a version in turn and history is never rewritten.

## Before you start

- A Store, as in [Files](../records/files.md), and an understanding of `Replace`, see [Replace and visibility](../records/replace-and-visibility.md). Versions are made only by `Replace` and `ReplaceObject`; uploading a new file with the same name makes a new file, not a version.
- The example is [`examples/records-versions`](../../examples/records-versions): two memory disks, one per visibility, routed with `records.WithVisibilityDisks`, the versions plugin keeping 2 versions per file, and the [quota plugin](quota.md) counting. `go run ./records-versions` in `examples/` prints the history after every step; `go test ./records-versions` runs the same steps.

## Step 1 — Install the plugin

`versions.New` takes options: `versions.WithKeep(n)` sets how many previous contents each file keeps (default `versions.DefaultKeep`, 10; less than 1 is `CodeInvalidArgument`), and `versions.WithClock` sets the clock of `ReplacedAt` for tests (without it, the Store's clock, `records.WithClock`). Version IDs come from `Store.NewID`, so `records.WithIDs` covers them.

<!-- ds:block id=newdisks-8x2d8493 -->
**newDisks** · [`examples/records-versions/main.go:54-64`](../../examples/records-versions/main.go#L54-L64)

```go
func newDisks() (*filesystem.Manager, error) {
	pub, err := filesystem.NewDisk(publicDisk, memory.New(), filesystem.WithDefaultVisibility(filesystem.VisibilityPublic))
	if err != nil {
		return nil, err
	}
	priv, err := filesystem.NewDisk(privateDisk, memory.New())
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(priv, pub)
}
```
<!-- /ds:block hash=ccee59 -->

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

`Migrate` creates `fs_file_versions`.

## Step 2 — Replace, and the content is kept

Nothing changes in how files are replaced. Inside `Replace`'s transaction the Store calls the plugin's `KeepVersion` (`records.Versioner`) with the file as it was; the plugin records it as the newest version and takes ownership of its object, so the Store does not delete it. When the file has more than the cap, the oldest versions are dropped: their rows in the same transaction, their objects after the commit.

<!-- ds:block id=edit-3jmwen3q -->
**edit** · [`examples/records-versions/main.go:108-110`](../../examples/records-versions/main.go#L108-L110)

```go
func edit(ctx context.Context, s *records.Store, f records.File, body string) (records.File, error) {
	return s.Replace(ctx, tenant, f.ID, strings.NewReader(body), "", textPolicy)
}
```
<!-- /ds:block hash=0c9d31 -->

`Versions` lists a file's versions, newest first. `Number` counts up from 1 per file and is never reused; `ReplacedAt` is when that content stopped being current.

<!-- ds:block id=history-nvyew4ud -->
**history** · [`examples/records-versions/main.go:114-124`](../../examples/records-versions/main.go#L114-L124)

```go
func history(ctx context.Context, s *records.Store, v *versions.Plugin, fileID string) (string, []versions.Version, error) {
	list, err := v.Versions(ctx, s, tenant, fileID)
	if err != nil {
		return "", nil, err
	}
	parts := make([]string, 0, len(list))
	for _, ver := range list {
		parts = append(parts, fmt.Sprintf("v%d:%dB@%s", ver.Number, ver.Size, ver.Disk))
	}
	return strings.Join(parts, " "), list, nil
}
```
<!-- /ds:block hash=59d572 -->

```text
uploaded on private, versions: 0
edit 1 -> 2B, history: v1:1B@private
edit 2 -> 3B, history: v2:2B@private v1:1B@private
edit 3 -> 4B, history: v3:3B@private v2:2B@private
v1 dropped past the cap, object still stored: false
quota counts 4 bytes (current content only)
```

Check: after the third edit only `v3` and `v2` remain, and `v1`'s object is gone from the disk. The quota plugin counts only the current 4 bytes; the versions' bytes are bounded by the cap, not by the quota.

## Step 3 — Restore a version

`Version` returns one version by ID; `Restore` copies its object to the file's disk under `versions/…` and makes the copy the file's content through `ReplaceObject`. The file keeps its ID, attachments, links and tags; only its content, filename, type, size and digest change. The content it replaces becomes the newest version, so the restore can itself be undone.

<!-- ds:block id=restore-v7aa5ygn -->
**restore** · [`examples/records-versions/main.go:130-136`](../../examples/records-versions/main.go#L130-L136)

```go
func restore(ctx context.Context, s *records.Store, v *versions.Plugin, versionID string) (records.File, error) {
	ver, err := v.Version(ctx, s, tenant, versionID)
	if err != nil {
		return records.File{}, err
	}
	return v.Restore(ctx, s, tenant, ver.ID)
}
```
<!-- /ds:block hash=bd45f4 -->

```text
restored v2 -> 2B, history: v4:4B@private v3:3B@private
```

Check: the file is 2 bytes again and the 4-byte content it replaced is `v4`. With a cap of 2, keeping `v4` dropped the oldest version, which was `v2` itself: the restored content lives on as the file's current content, but the version row is gone. With a larger cap the restored version stays in the list.

A file in the trash is refused with `not_found` before the copy is made. If the replacement fails (the [quota](quota.md) refuses the growth) the copy is deleted again, and so it is when an enclosing `Store.Tx` rolls back.

## Step 4 — Delete a version

`Delete` removes one version, its object after the commit. Use it for "delete this version" in the UI, or to drop sensitive content from history.

<!-- ds:block id=forget-jw43mckv -->
**forget** · [`examples/records-versions/main.go:140-142`](../../examples/records-versions/main.go#L140-L142)

```go
func forget(ctx context.Context, s *records.Store, v *versions.Plugin, versionID string) error {
	return v.Delete(ctx, s, tenant, versionID)
}
```
<!-- /ds:block hash=3b8d14 -->

## Step 5 — Versions follow their file's visibility

The plugin is also an `EventHooks` plugin: on `file.visibility_changed`, `file.trashed` and `file.restored` it makes every kept version match the file's object, so an old version never stays public after the file was made private or trashed (a trashed file's versions are made private, or moved to the private disk with it), and never stays behind on a disk the file left. A version whose object was lost is skipped rather than blocking the file's change.

- With `records.WithVisibilityDisks`, `SetVisibility` moves the file to the other disk; the plugin copies each version to that disk, updates its `disk` column, and deletes the old object after the commit.
- On one disk, the plugin calls the disk's `SetVisibility` on each version's object.

Every change is undone if the transaction rolls back.

<!-- ds:block id=publish-3ffb8xnh -->
**publish** · [`examples/records-versions/main.go:148-150`](../../examples/records-versions/main.go#L148-L150)

```go
func publish(ctx context.Context, s *records.Store, f records.File) error {
	return s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic)
}
```
<!-- /ds:block hash=7c9d79 -->

```text
deleted one, then published: history: v4:4B@public
```

Check: after `Delete` and `SetVisibility` the one remaining version is on the `public` disk, and its object on `private` is deleted (`TestPublishMovesVersionObjects`).

## Step 6 — Purge takes the versions along

`BeforePurge` drops every version of a purged file: rows in the purge transaction, objects after the commit.

```text
purged: history "", newest version object still stored: false
```

## Other history models

`WithKeep(n)` gives every file the same count. Two options plug in a different model; both keep the default unchanged when not set.

**A policy per file.** `versions.WithKeepFunc(fn)` asks `fn(ctx, versions.KeepRequest{File})` on every replacement for a `versions.KeepPolicy{Count, MaxAge}`: a count from the scope's plan, and an age past which versions are dropped. An age only drops versions when the file is next replaced, so schedule `Trim(ctx, s, scope, fileID)` to apply the policy to files nobody touches, or `TrimScope(ctx, s, scope)` for every file of a scope at once: it pages through the scope's versions by file ID and trims each file in its own transaction, so a large tenant never holds one long transaction, and a file whose policy fails does not stop the others (the first failure is returned, naming the file); `jobs.VersionsTrim(plugin, store, scopes)` runs it on a schedule for every scope `scopes(ctx)` returns. It fails closed: an error from `fn`, a `Count` below 1 or a negative `MaxAge` refuses the replacement, which rolls back.

```go
v, err := versions.New(versions.WithKeepFunc(func(ctx context.Context, req versions.KeepRequest) (versions.KeepPolicy, error) {
	plan, err := plans.Of(ctx, req.File.Scope) // your billing lookup
	if err != nil {
		return versions.KeepPolicy{}, err // the replacement is refused, not unbounded
	}
	if plan.Pro {
		return versions.KeepPolicy{Count: 100, MaxAge: 30 * 24 * time.Hour}, nil
	}
	return versions.KeepPolicy{Count: 5}, nil
}))
```

**Where restored copies go.** By default a restore copies the version to `versions/` and a random name at the root of the file's disk. When tenants own key prefixes (per-prefix IAM, KMS or lifecycle rules, prefix deletes when a tenant leaves), pass `versions.WithRestoreKey(versions.RestoreBesideFile)`, which puts the copy in a `versions/` directory beside the file's own object, or a `RestoreKeyFunc` of your own. A key that is empty, is the file's or the version's own, or already holds an object is refused before anything is written.

Try it: [`examples/records-versions`](../../examples/records-versions) runs this section in `plans.go` ([`planKeep`](ds:block?id=plankeep-x3n7q2vd)): `go run ./records-versions` from `examples/`.

## The table

`fs_file_versions`, one row per kept version:

| Column | Type | Holds |
|---|---|---|
| `id` | key, primary key | `ver_` plus 128 random bits |
| `scope` | key | the file's scope |
| `file_id` | key | the file |
| `number` | 64-bit integer | 1, 2, 3… per file; unique with `file_id` (`fs_file_versions_number`) |
| `disk` | short | the disk the version's object is on |
| `object_key` | long | the version's object |
| `filename` | text | the file's name when this content was current |
| `content_type` | text | its type |
| `size` | 64-bit integer | its size |
| `sha256` | short | its digest |
| `replaced_at` | timestamp (UTC) | when it stopped being current |

Indexes: `fs_file_versions_number (file_id, number)`, unique, from `versions/0001_file_versions`; `fs_file_versions_scope (scope)` for `HeldBytes`, from `versions/0002_file_versions_scope`.

## How it works with the rest

- **Quota.** Every kept version is reported to `records.Store.Hold` and every dropped, deleted or purged one to `Release`, and `HeldBytes` (`records.HeldCounter`) totals a scope's versions. By default `quota` ignores them and charges only the current content's size difference on each replace and restore; bound history with `WithKeep`. With `quota.WithHeldObjects()` versions count toward the byte limit, and a replacement whose kept version would pass it is refused with `quota_exceeded`. At the cap the oldest version is dropped before the new one is held, so a full file needs room only for the difference.
- **Moving a file.** `Store.MoveFile(ctx, scope, id, to)` takes the file's kept versions with it (the plugin is a `records.FileDiskMover`): inside the move's transaction each version on the file's old disk is copied to `to` under the same key, given the file's visibility, and its row updated; the copies are deleted again if the move rolls back, and the old objects only after it commits. Versions on another disk than the file's, and versions whose object was lost, are left as they are.
- **Renaming a disk.** `Store.RenameDisk(ctx, from, to)` points kept versions at the new name in the same transaction as the files (the plugin is a `records.DiskRenamer`), so they still read and restore afterwards. Like the files, only rows change: the objects must already be reachable on `to` under the same keys.
- **Trash.** A trashed file keeps its versions, hidden like the file's own object (step 5). `Restore` of a version whose file is in the trash fails with `CodeNotFound` before anything is copied.
- **Variants.** A restore is a replace, so the [variants plugin](variants.md) drops the file's variants and makes them again from the restored content.
- **Dimensions.** A restore runs `ReplaceHooks`, so the [dimensions plugin](dimensions.md) measures the restored image.
- **Share links and attachments** point at the file, not a content: after a restore they serve the restored content.
- **Object deletion failures** (a dropped version's object that could not be deleted after the commit) go to `records.WithObjectErrorHandler`, like the Store's own; the row is already gone, so nothing points at the object left behind.

## What can go wrong

- `invalid_argument` from `versions.New` — `WithKeep` below 1.
- `not_found` from `Restore` or `Delete` — no version with that ID in that scope, or (for `Restore`) the file is in the trash.
- `quota_exceeded` from `Restore` — the restored content is larger and the scope is at its byte limit.
- `quota_exceeded` from `Replace` with `quota.WithHeldObjects()` — the new content plus the version it keeps passes the byte limit.
- `unsupported` from `SetVisibility` on one disk — the disk cannot change an object's visibility; the file's own change fails before the plugin runs. Use `WithVisibilityDisks` instead.
- Versions you expected are missing — the cap dropped them; raise `WithKeep`.

## Next

- [Replace and visibility](../records/replace-and-visibility.md): `Replace`, `ReplaceObject`, `SetVisibility` and `WithVisibilityDisks`.
- [Quota](quota.md): what is counted when versions are kept.
- [Events and hooks](../records/events-and-hooks.md): `Versioner` and `EventHooks`, the interfaces this plugin implements.
- The plugin's [README](../../records/versions/README.md) is the reference for its API and table.
