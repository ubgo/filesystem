# Check-out locks

You will have check-out: a user locks a document while editing it, and until they unlock it or the lock expires nobody else can replace its content. Install the `locks` plugin when two people saving the same file must not overwrite each other, and reserving the file is the workflow you want. If you only need saving to fail when the file changed since it was read, `records.IfUnchanged` already does that without locks.

Which one to use:

| You want | Use |
|---|---|
| Two people saving the same file must not overwrite each other, and the second should learn it at save time | `records.IfUnchanged(sha256)` on `Replace` ([save only what you read](../records/replace-and-visibility.md#save-only-what-you-read)). No table, no expiry, no admin override needed. |
| An editor reserves a file before a long edit (a desktop app's check-out, a legal review), and others see who holds it | This plugin. |
| Both | Both: the lock keeps others out, `IfUnchanged` catches the editor's own stale tab. |

## Before you start

- A Store, as in [Files](../records/files.md). Every request that changes files must name its actor with `records.WithActor` (or the context key you pass to `locks.WithOwnerFunc`): the lock owner is compared with it.
- The example is [`examples/records-locks`](../../examples/records-locks): `go run ./records-locks` in `examples/` prints every step below; `go test ./records-locks` runs them.

## Step 1 — Install it

`locks.New` takes the longest check-out the app allows (`WithMaxTTL`, default `locks.DefaultMaxTTL`, 8 hours), a clock for tests (`WithClock`; without it the plugin reads the Store's clock, so `records.WithClock` alone ages a lock), and where the owner comes from (`WithOwnerFunc`):

<!-- ds:block id=newlocksstore-p8w3k6nc -->
**newStore** · [`examples/records-locks/main.go:63-81`](../../examples/records-locks/main.go#L63-L81)

```go
func newStore(ctx context.Context, db *sql.DB, now func() time.Time) (*records.Store, *locks.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	lp := locks.New(locks.WithMaxTTL(maxLock), locks.WithClock(now))
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(lp))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, lp, nil
}
```
<!-- /ds:block hash=c59769 -->

`Migrate` creates `fs_locks` (and `locks/0002_lock_tokens` adds its `token` column) and `fs_shared_locks` (`locks/0003_shared_locks`, used only with `WithSharedLocks`). Who a request acts for is `records.WithActor`'s actor; pass `locks.WithOwnerFunc` if your app keeps the user under its own context key.

## Step 2 — Check out, edit, check in

Check out with `Lock(ctx, store, scope, fileID, owner, ttl)`:

<!-- ds:block id=checkout-v2n7q4hx -->
**checkOut** · [`examples/records-locks/main.go:86-88`](../../examples/records-locks/main.go#L86-L88)

```go
func checkOut(ctx context.Context, s *records.Store, lp *locks.Plugin, fileID, user string) (locks.Lock, error) {
	return lp.Lock(ctx, s, tenant, fileID, user, editWindow)
}
```
<!-- /ds:block hash=26f4a7 -->

Save as usual. The plugin's `BeforeReplace` hook compares the actor with the lock's owner inside the replacement's transaction:

<!-- ds:block id=save-k3m8t5wz -->
**save** · [`examples/records-locks/main.go:93-97`](../../examples/records-locks/main.go#L93-L97)

```go
func save(ctx context.Context, s *records.Store, fileID, user, body string) error {
	ctx = records.WithActor(ctx, user)
	_, err := s.Replace(ctx, tenant, fileID, strings.NewReader(body), "", docs)
	return err
}
```
<!-- /ds:block hash=0db07f -->

Check in with `Unlock`:

<!-- ds:block id=checkin-r6c9w2tm -->
**checkIn** · [`examples/records-locks/main.go:102-104`](../../examples/records-locks/main.go#L102-L104)

```go
func checkIn(ctx context.Context, s *records.Store, lp *locks.Plugin, fileID, user string) error {
	return lp.Unlock(ctx, s, tenant, fileID, user)
}
```
<!-- /ds:block hash=b53547 -->

Check: the example prints `alice checked out until 09:30`, `bob check out: locked (conflict)`, `alice saves: ok` and `bob saves: locked (conflict)`.

Locking a file again extends your own lock. A lock lasts at most `locks.DefaultMaxTTL` (8 hours) unless you install the plugin with `locks.WithMaxTTL(d)`; a longer ttl is refused with `invalid_argument` rather than silently shortened, so a forgotten check-out frees itself instead of blocking trash and purges until someone breaks it. Another owner's lock is `ErrLocked` until it expires. After that, anyone may replace the file or take the lock, at once and without a sweep. `Break` removes anyone's lock (an administrator's override), and `LockOf` shows who holds it and until when.

A lock also guards the file's existence: `Trash` and `Purge` by anyone but the owner return `ErrLocked`, so a checked-out file cannot vanish under its editor; `Break` the lock first. That includes purges with no actor, such as `SweepTrash` and `SweepExpired`, which report the file in their error and purge it on a later run once the lock expired. Changing the visibility, status or metadata stays allowed. Check: the example prints `bob trashes: locked (conflict)` and `purge with no actor: locked (conflict)`, then `alice asks for 3h: invalid_argument` (over its two-hour `WithMaxTTL`), `held true by user:alice` from `LockOf`, and, its clock moved 31 minutes on, `31 minutes later, bob saves: ok` with nobody having unlocked anything. `Unlock` of someone else's lock changes nothing (`alice checks in bob's lock: ok, still held true`); `Break` removes it (`admin breaks it: held false`). `Store.PurgeScope` is the exception: offboarding a tenant beats check-out, so it purges locked files and their locks (hooks see it through `records.PurgingScope(ctx)`). Every guard locks the file's row as `Lock` does, so a lock that commits while a replacement's or a purge's transaction is open is seen, not missed, on Postgres and MySQL as well as SQLite.

## Step 3 — Tidy expired locks

`Sweep(ctx, store, now)` deletes expired locks in every scope. They already allow everything, so this only keeps the table small. Schedule it with your other jobs. Check: the example prints `sweep removed 1 expired lock(s)`.

## Who holds a lock: tokens and your own rule

Every lock carries a random token, `Lock.Token` (`lkt_` and 128 random bits), made when the lock is taken and kept while its owner extends it; it is never put in an event. By default only the owner matters (`locks.DefaultHolder`). `locks.WithHolderFunc(fn)` decides instead, from `locks.HolderRequest{Lock, Owner, Action, File}`, where `Action` is the change asked (`locks.ActionReplace`, `ActionTrash` or `ActionPurge`) and `File` the file:

- `locks.TokenHolder` asks for the owner and the token, as WebDAV does with its `If` header, so a user's second client or tab cannot overwrite a check-out made by the first. Hand `Lock.Token` to the client that locked, and put what it presents on the request with `locks.WithLockToken(ctx, token)`.
- A rule of your own, for example letting a team lead pass a member's lock. An error from it refuses the change with that error; `false` refuses it with `ErrLocked`.

```go
p := locks.New(locks.WithHolderFunc(locks.TokenHolder))

// In the WebDAV handler:
ctx = records.WithActor(ctx, user.ID)
ctx = locks.WithLockToken(ctx, tokenFromIfHeader(r))
```

A rule can tell the changes apart. An admin who may purge a locked file but not overwrite or trash it:

```go
p := locks.New(locks.WithHolderFunc(func(ctx context.Context, req locks.HolderRequest) (bool, error) {
	if isAdmin(req.Owner) {
		return req.Action == locks.ActionPurge, nil
	}
	return req.Owner == req.Lock.Owner, nil
}))
```

Tokens are `lkt_` and random text by default. A front end that needs another shape, such as WebDAV's `opaquelocktoken:` URIs, gives `locks.WithTokenFunc(fn)`: the plugin still draws the randomness, `locks.TokenRandomBytes` (16) bytes, and `fn` only renders them. `New` checks that every one of the 128 bits changes the token, so the floor cannot drop: a function that keeps only part of the bytes, or a UUID version 4 whose version bits overwrite six of them, makes every lock that needs a new token `invalid_argument`. Write all 128 bits as hex in a UUID's layout instead:

```go
p := locks.New(locks.WithTokenFunc(func(r [locks.TokenRandomBytes]byte) string {
	h := hex.EncodeToString(r[:])
	return "opaquelocktoken:" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}))
```

Locks per device need no option: `WithOwnerFunc` may return `"user:alice/device:ipad"`.

Try it: [`examples/records-locks`](../../examples/records-locks) runs this section in `tokens.go` ([`saveWithToken`](ds:block?id=savewithtoken-h7n3c8qv)): `go run ./records-locks` from `examples/`.

## Shared locks: several holders

A lock is exclusive by default. `locks.WithSharedLocks()` adds WebDAV's shared write lock: several owners hold a file at once, for co-editing, and anyone else is kept out.

```go
p := locks.New(locks.WithSharedLocks(), locks.WithHolderFunc(locks.TokenHolder))

ann, err := p.LockShared(ctx, store, scope, fileID, "user:ann", time.Hour) // ann.Token is hers alone
ben, err := p.LockShared(ctx, store, scope, fileID, "user:ben", time.Hour) // a second holder, own token
held, err := p.SharedLocksOf(ctx, store, scope, fileID)                    // who has it open: ann, ben
err = p.UnlockShared(ctx, store, scope, fileID, "user:ann")                 // ben keeps his
```

| | Exclusive (`Lock`) | Shared (`LockShared`) |
|---|---|---|
| Holders | one | any number, one row and one token each |
| Who may change the file | the holder (`HolderFunc`) | any holder (`HolderFunc` asked for each shared lock, so `TokenHolder` wants that holder's own token) |
| Taken while the other kind is held | `ErrLocked` if another owner holds a live shared lock; the owner's own shared lock is given up for it | `ErrLocked` while anyone holds a live exclusive lock, the asking owner included |
| Extended | `Lock` again keeps the token | `LockShared` again keeps the token while live |
| Ended | `Unlock`, expiry, `Break` | `UnlockShared`, expiry, `Break` (which removes every shared lock too) |

`Sweep` deletes expired locks of both kinds; a purge removes both with the file. Their `locks.locked` and `locks.unlocked` events carry `DetailMode: "shared"`. Without the option `LockShared` is `unsupported`, and shared locks a plugin with it stored still guard their files, so turning it off never unlocks one.

Try it: [`examples/records-locks`](../../examples/records-locks) runs this section in `shared.go` ([`coEdit`](ds:block?id=coedit-x3q7n9vb)): `go run ./records-locks` from `examples/`.

## Try it

```sh
cd examples
go run ./records-locks
go test ./records-locks
```

## Pitfalls

- **No actor, no owner.** A replacement made without `records.WithActor` has owner `""`, which matches no lock: it is refused while anyone holds one. Set the actor in the middleware of every route that changes files.
- **Lock is not a permission.** Anyone who may replace a file may also check it out. Decide who may lock in your own handler (or with [permissions](acl.md)) before calling `Lock`.
- **Long edits need renewal.** A lock is not extended by saving. An editor that keeps a document open renews by calling `Lock` again before `ExpiresAt`, which extends its own lock.
- **Trash and purge wait for the lock.** `SweepTrash` and `SweepExpired` report a locked file in their error and purge it on a later run once the lock expired, so a daily sweep can report the same file until then.
- **`PurgeScope` ignores locks**, by design: offboarding a tenant cannot be blocked by a check-out.

## Next

- [Replace and visibility](../records/replace-and-visibility.md): `IfUnchanged`, the lock-free alternative.
- The plugin's [README](../../records/locks/README.md).
