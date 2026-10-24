# github.com/ubgo/filesystem/records/locks

Check-out locks for the files of a records Store: a subject locks a file for a while, and until it unlocks it or the lock expires nobody else may replace the file's content. The guide is [docs/plugins/locks.md](../../docs/plugins/locks.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

Part of the records module; no dependency of its own.

## Go API

| Symbol | What it does |
|---|---|
| `New(opts...)`, `WithOwnerFunc(fn)`, `WithClock(now)` | A plugin; who a request acts for is the Store's actor (`Store.Actor(ctx)`: `records.WithActorFunc`, else `records.ActorFrom`), or `fn`, which wins; `WithClock` decides lock expiry, without it the plugin reads the Store's clock (`records.WithClock`), so one clock rules the Store and its plugins |
| `(*Plugin).Lock(ctx, s, scope, fileID, owner, ttl)` | Checks a live file out to `owner`; a free, expired or own lock is taken (an own one extended), another owner's is `ErrLocked` (`conflict`) |
| `(*Plugin).Unlock(ctx, s, scope, fileID, owner)` | Checks it back in; only the owner's lock is removed |
| `(*Plugin).Break(ctx, s, scope, fileID)` | Removes anyone's lock, and every shared lock on the file: the administrator's override |
| `(*Plugin).LockOf(ctx, s, scope, fileID)` | The lock and whether it holds now |
| `(*Plugin).Sweep(ctx, s, now)` | Deletes expired locks, exclusive and shared, in every scope; schedule it |
| `WithSharedLocks()`, `(*Plugin).LockShared(ctx, s, scope, fileID, owner, ttl)`, `UnlockShared(ctx, s, scope, fileID, owner)`, `SharedLocksOf(ctx, s, scope, fileID)`, `Mode` (`Exclusive`, `Shared`) | WebDAV's shared write lock: several owners hold a file's lock at once, each with its own token, and a change passes when the `HolderFunc` says the request holds any one of them (so `TokenHolder` asks for that holder's own token). Exclusive and shared exclude each other: `Lock` is `ErrLocked` while another owner holds a live shared lock (an owner's own is given up for the exclusive one), `LockShared` while anyone holds a live exclusive lock. Without the option `LockShared` is `unsupported`; stored shared locks still guard their files. Their events carry `DetailMode: "shared"` |
| `WithHolderFunc(fn HolderFunc)` | Decides whether a request holds a file's live lock (and may replace, trash or purge it) with `fn(ctx, HolderRequest{Lock, Owner, Action, File}) (bool, error)`, where `Action` is `ActionReplace`, `ActionTrash` or `ActionPurge`, so "an admin may purge a locked file but not overwrite it" needs no `Break`; an error refuses the change with it. Default `DefaultHolder` (the request's owner is the lock's) |
| `TokenHolder`, `WithLockToken(ctx, token)`, `LockTokenFrom(ctx)` | A `HolderFunc` that asks for the owner and the lock's token (WebDAV's `If` header), compared in constant time, so one user's two clients do not overwrite each other |
| `Lock.Token`, `TokenPrefix` | Each lock's random token (`lkt_` and 128 random bits), kept while its owner extends it, never put in an event; `""` for locks taken before migration `0002_lock_tokens` |
| `WithTokenFunc(fn TokenFunc)`, `TokenRandomBytes`, `MaxTokenBytes` | Renders each new token from `TokenRandomBytes` (16) random bytes the plugin draws, in the shape a front end needs (WebDAV's `opaquelocktoken:` or `urn:uuid:`). `New` checks that every one of the 128 bits changes the output and that it fits `MaxTokenBytes` (191); a function that drops a bit makes every lock that needs a new token `invalid_argument`. Default `lkt_` tokens |
| Per-device locks | Already a seam: `WithOwnerFunc` may name a device as well as a user (`"user:alice/device:ipad"`) |

`Store.Replace` and `ReplaceObject` refuse with `ErrLocked` while another owner holds the lock (`records.ReplaceHooks`); the owner, and anyone once it expired, may replace. `Store.Trash` and `Purge` by anyone but the owner (a sweep with no actor included) are refused with `ErrLocked` too; `Break` first. `SetVisibility` and other changes stay allowed. The guards lock the file's row (`FOR UPDATE`, as `Lock` does), so a lock committed while a change's transaction was open is still seen on Postgres and MySQL. A purge by the owner removes the lock with the file, and `Store.PurgeScope` (tenant offboarding) is never refused: it purges locked files with their locks. A lock lasts at most `DefaultMaxTTL` (8 hours) or `WithMaxTTL(d)`; a longer ttl is `invalid_argument`, not shortened, and an editor extends its own lock by locking again. Locking and unlocking emit `locks.locked` and `locks.unlocked`.

## Table

`fs_locks` (migration `locks/0001_locks`): `scope`, `file_id` (primary key together), `owner`, `expires_at` (indexed, for `Sweep`), `locked_at`, `token` (from `locks/0002_lock_tokens`).

`fs_shared_locks` (migration `locks/0003_shared_locks`): the same columns, one row per holder, primary key `(scope, file_id, owner)`, `expires_at` indexed.
