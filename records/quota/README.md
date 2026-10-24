# github.com/ubgo/filesystem/records/quota

A records plugin that limits how much each scope (a tenant, a workspace, a user) may store, in bytes and in files, and keeps the usage that billing pages and admin screens show. The guide is [docs/plugins/quota.md](../../docs/plugins/quota.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is part of the records module, which requires only `github.com/ubgo/sqlb` and the core module.

## Wire it

<!-- ds:block id=newstore-cnhfwgs6 -->
**newStore** · [`examples/records-quota/main.go:101-118`](../../examples/records-quota/main.go#L101-L118)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *quota.Plugin, error) {
	q, err := quota.New(limits)
	if err != nil {
		return nil, nil, err
	}
	v, err := versions.New()
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q, v))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}
```
<!-- /ds:block hash=9bad72 -->

A runnable version is [`examples/records-quota`](../../examples/records-quota).

## Behaviour

- **Charging.** `BeforeCreate` (every `CreateFile` and `Upload`) adds the file's size and one file to its scope with one conditional `UPDATE` inside the file's transaction, so concurrent uploads cannot both pass a limit and a rollback un-charges. Past a limit the call fails with `CodeQuotaExceeded` and stores nothing.
- **First upload of a scope.** It creates the scope's usage row. Two first uploads racing make one insert fail; that call returns `CodeUnavailable` and should be retried.
- **Replacing.** `BeforeReplace` (`records.ReplaceHooks`) charges the size difference; growth past the byte limit is `CodeQuotaExceeded`, a shrink is credited. Versions kept by the versions plugin are not charged unless `WithHeldObjects` is on.
- **Per uploader.** With `WithUploaderLimits`, `BeforeCreate` charges the file's uploader (`File.UploadedBy`) after its scope, in the same transaction; `BeforeReplace` charges or credits the file's uploader (not the replacer), and `BeforePurge` releases from it. Held objects never count toward an uploader. `Recount` rebuilds the scope's uploader rows too, and `Store.PurgeScope` deletes them.
- **Held objects.** With `WithHeldObjects`, `OnHold` and `OnRelease` (`records.UsageHooks`) charge and release the objects other plugins report with `records.Store.Hold`/`Release` (kept versions, image variants), as bytes only, with the same conditional `UPDATE` as uploads. A hold past the byte limit is `CodeQuotaExceeded` and aborts the holder's change. Without the option both do nothing.
- **Trash and purge.** A trashed file still counts. `BeforePurge` releases the file's size and count, never below zero.
- **Offboarding.** `records.Store.PurgeScope` deletes the scope's usage row once every file is purged (`records.ScopePurger`); a later upload starts a fresh one.
- **Refusals.** `CodeQuotaExceeded` names the limit the upload passes: `limit of N bytes`, `limit of N files`, or both when it passes both.
- **Limits.** `LimitFunc` is called on every create and every growing replace; a zero field of `Limit` is unlimited; its error is returned unchanged.

## Table

`fs_quota_reservations` (`TableNameReservations`), migration `quota/0003_reservations`: `id` (primary key, `qrs_`), `scope`, `bytes`, `expires_at`, `created_at`; the same migration adds `reserved` (`ColReserved`, the bytes open reservations hold, counted against the limit) to `fs_quota_usage`. `Usage.Reserved` reads it.

`fs_quota_usage` (constant `TableNameUsage`), migration `quota/0001_usage`:

| Column | Constant | Type | Notes |
|---|---|---|---|
| `scope` | `ColScope` | key | primary key |
| `bytes` | `ColBytes` | 64-bit integer | trashed files included |
| `files` | `ColFiles` | 64-bit integer | trashed files included |
| `updated_at` | `ColUpdatedAt` | timestamp | UTC |

Typed columns for queries of your own: `quota.Table`, `quota.Scope`, `quota.Bytes`, `quota.Files`, `quota.UpdatedAt`.

`fs_quota_uploader_usage` (constant `TableNameUploaderUsage`), migration `quota/0002_uploader_usage`, filled only with `WithUploaderLimits`: the same columns plus `uploader` (`ColUploader`, key, `""` for files recorded without an actor), primary key `(scope, uploader)`. Typed columns: `quota.UploaderTable`, `quota.UploaderScope`, `quota.Uploader`, `quota.UploaderBytes`, `quota.UploaderFiles`, `quota.UploaderUpdatedAt`.

## Go API

| Symbol | What it does |
|---|---|
| `New(fn LimitFunc, opts ...Option) (*Plugin, error)` | A plugin taking limits from `fn`; a nil `fn` is `CodeInvalidArgument` |
| `WithUploaderLimits(fn UploaderLimitFunc)` | Keeps usage per uploader (`File.UploadedBy`) within each scope as well, and refuses a file past its uploader's limit with `CodeQuotaExceeded`; a replacement is charged to the file's uploader. A nil `fn` is `CodeInvalidArgument` from `New`. Off by default; `Recount` every scope when turning it on |
| `UploaderLimitFunc` | `func(ctx, scope, uploader) (Limit, error)`; `uploader` is `""` for files recorded without an actor |
| `(*Plugin).UploaderUsage(ctx, s, scope, uploader)` | One uploader's usage in a scope (files only, never held objects); zero when it has none or the option is off |
| `(*Plugin).RecountUploader(ctx, s, scope, uploader)` | Rebuilds one uploader's row from `fs_files` and returns it |
| `(*Plugin).Reserve(ctx, s, scope, n, ttl)` | Holds `n` bytes of the scope's byte limit for an upload in flight (presigned PUT, tus), with one conditional `UPDATE`; `CodeQuotaExceeded` past the limit. Returns a `Reservation{ID, Scope, Bytes, ExpiresAt}` |
| `WithReservation(ctx, id)` | The file recorded with this context uses the reservation up in its own transaction: its real size is charged and the reservation let go in one `UPDATE` (the commit) |
| `(*Plugin).Release(ctx, s, scope, id)` | Ends an open reservation without a file; reports whether it was open |
| `(*Plugin).Sweep(ctx, s, now)` | Releases the reservations expired at `now`; schedule `jobs.QuotaSweepReservations` |
| `WithUsageEvents()` | Every change to a scope's usage emits `quota.usage_changed` (`EventUsageChanged`, `Detail` keys `bytes`, `files`, `delta_bytes`, `delta_files`) inside the change's transaction, with the Store's event ID. Off by default |
| `WithClock(now)` | The time usage rows are stamped with (`updated_at`), for tests; without it the Store's clock (`records.WithClock`). Reservations always expire by the Store's clock |
| `WithDimensions(fn DimensionFunc)` | Charges every file to the `Dimension{Key, Limit}`s `fn(ctx, file)` returns as well as to its scope, in the same transaction, refusing it past any of their limits with `CodeQuotaExceeded`: an organisation's cap across workspaces, an owner type's, a content class's. One key over several scopes is one counter (`fs_quota_dimension_usage`, migration `0004_dimension_usage`). Fails closed: an error, an empty, too long (`MaxDimensionBytes`, 191) or repeated key refuses the change. Off by default; `RecountDimensions` when turning it on |
| `(*Plugin).DimensionUsage(ctx, s, key)`, `(*Plugin).RecountDimensions(ctx, s)` | One dimension's usage; rebuilding every dimension row from every file |
| `(*Plugin).RecountDimensionsFor(ctx, s, RecountFilter{Keys, Scopes})` | Rebuilds only the rows `Keys` from the files of `Scopes` (every scope when empty), in a transaction holding only those rows: the repair after one organisation changes plan or a workspace moves (list the old key and the new). Every file charged to a listed key must be in a listed scope. `Scopes` without `Keys` is `invalid_argument`; the zero filter is `RecountDimensions` |
| `WithHeldObjects()` | Counts objects other plugins hold (kept versions, image variants) toward the scope's bytes, refusing a hold past the byte limit with `CodeQuotaExceeded`. Off by default; `Recount` every scope when turning it on |
| `Limit{Bytes, Files}` | What a scope may store; 0 is unlimited |
| `LimitFunc` | `func(ctx, scope) (Limit, error)` |
| `Usage{Bytes, Files}` | What a scope stores |
| `(*Plugin).Usage(ctx, s, scope)` | The scope's usage; a scope that never uploaded has a zero `Usage` and no error |
| `(*Plugin).Recount(ctx, s, scope)` | Rebuilds the scope's row from `fs_files` (trashed files included), plus `Store.HeldBytes` with `WithHeldObjects`, and returns it, reading the sums in the transaction that writes the row, after locking it, so a concurrent upload's charge is never lost; run it after a manual repair or after installing the plugin on existing files |
| `PluginName` | `"quota"`, its name in the migration ledger |

The plugin implements `records.Plugin`, `records.Migrator`, `records.FileHooks`, `records.ReplaceHooks` and `records.UsageHooks`.

## Limits

- Files recorded before the plugin was installed are not in any usage row; purging or shrinking one releases what was never charged, and usage stops at zero rather than going negative, but it stays below the truth until `Recount`. Run `Recount` for every scope after installing.
- Bytes of kept versions and image variants are counted only with `WithHeldObjects`, and only from plugins that report them through `records.Store.Hold`/`Release`. Turning it on for existing data needs a `Recount` of every scope.
