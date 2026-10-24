# Check-out locks

The `locks` plugin of the records layer: a user checks a document out, and until they check it in or the lock expires nobody else can replace, trash or purge it. It runs on SQLite over an in-memory disk, with a clock the demo moves past a lock's expiry. The guide [Check-out locks](../../docs/plugins/locks.md) explains each step.

## Run it

```sh
cd examples
go run ./records-locks
go test ./records-locks
```

It needs no network or environment variables.

## What it does

```text
alice checked out until 09:30
bob check out: locked (conflict)
alice saves: ok
bob saves: locked (conflict)
bob trashes: locked (conflict)
purge with no actor: locked (conflict)
alice asks for 3h: invalid_argument
held true by user:alice
31 minutes later, bob saves: ok
alice checks in bob's lock: ok, still held true
admin breaks it: held false
sweep removed 1 expired lock(s)
token lock: alice's laptop got a token starting lkt_
token lock: laptop saves with its token: ok
token lock: tablet saves as alice without it: locked (conflict)
token lock: extending keeps the token: true
shared lock: alice and bob hold design.txt, own tokens: true
shared lock: alice saves with her token: ok
shared lock: bob saves with his token: ok
shared lock: bob saves with alice's token: locked (conflict)
shared lock: carol saves: locked (conflict)
shared lock: carol checks out alone: locked (conflict)
shared lock: after alice unlocks, held by user:bob
shared lock: without WithSharedLocks: unsupported
```

1. Alice checks `plan.txt` out for 30 minutes; Bob cannot take the lock.
2. Alice's replacement goes through. Bob's replacement, his trash and a purge with no actor (as `SweepTrash` makes) are refused with `locks.ErrLocked`, code `conflict`.
3. A check-out longer than the plugin's `WithMaxTTL` (two hours) is refused rather than shortened. `LockOf` shows who holds the file.
4. Once the lock expires, Bob saves without anyone unlocking it, and takes the lock.
5. Alice cannot release Bob's lock (`Unlock` of a lock you do not hold changes nothing); an administrator's `Break` can.
6. `Sweep` deletes expired locks to keep the table small.
7. `tokens.go` opens a second Store with `locks.WithHolderFunc(locks.TokenHolder)`, WebDAV's rule. Alice checks `spec.txt` out from her laptop, which gets the lock's token (`lkt_` and 128 random bits). The laptop's save puts the token on the request with `locks.WithLockToken` and goes through; her tablet, signed in as Alice but without the token, is refused with `locks.ErrLocked`. Checking out again extends the lock and keeps its token. The test `TestTokenHolderIsWhatRefuses` shows the same token-less save passes under the default holder, and that a wrong token is refused like a missing one.
8. `shared.go` opens a third Store with `locks.WithSharedLocks()` and `locks.TokenHolder`, for co-editing. Alice and Bob each take a shared lock on `design.txt` with `LockShared` and get a token of their own; each saves with their own token, while Bob with Alice's token, Carol saving, and Carol asking for an exclusive check-out are all refused with `locks.ErrLocked`. After Alice's `UnlockShared`, `SharedLocksOf` shows only Bob. A plugin without the option answers `LockShared` with `unsupported`. The test `TestSharedLockIsWhatRefuses` shows Carol's same save passes before anyone locks and after every holder unlocks.

## Files

| File | What it holds |
|---|---|
| `main.go` | The Store with the plugin, the clock and the steps |
| `tokens.go` | Lock tokens with `locks.TokenHolder` and `locks.WithLockToken`; code: [`newTokenStore`](ds:block?id=newtokenstore-t4k9w2mx) |
| `shared.go` | Shared locks with `locks.WithSharedLocks`, several holders each with a token; code: [`newSharedStore`](ds:block?id=newsharedstore-m5r8k2wd), [`coEdit`](ds:block?id=coedit-x3q7n9vb) |
| `main_test.go` | Runs the steps, and pins that the token rule is what refuses a token-less save and that the shared locks are what refuse Carol |

## Next

- The guide: [Check-out locks](../../docs/plugins/locks.md); the plugin's [README](../../records/locks/README.md).
- Related examples: [records-replace](../records-replace) for `IfUnchanged`, saving without locks.
