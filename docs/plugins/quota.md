# Quota

The quota plugin limits how much each scope may store, in bytes and in files, and keeps the usage a billing page or an admin screen shows. Use it when scopes are customers on plans ("free: 1 GB, pro: 100 GB"), when one tenant must not be able to fill the bucket for everyone, or when you only want a reliable "used 3.2 GB" number without summing `fs_files` on every page view.

The limit is enforced inside the transaction that records the file, with one conditional `UPDATE` ("add the size only if the total stays within the limit"), so two concurrent uploads can never both pass a limit, and an upload whose transaction rolls back takes its charge with it.

## Before you start

- A Store, as in [Files](../records/files.md). The plugin works on every engine the Store supports.
- The example is [`examples/records-quota`](../../examples/records-quota): a free plan of 25 bytes and 3 files (tiny, so every limit shows), the versions plugin installed beside it, and a walk through upload, refusal, rollback, replace, trash, purge and recount. `go run ./records-quota` in `examples/` prints the usage after every step; `go test ./records-quota` runs the same steps.
- Where the limits come from is yours: the plugin asks a function you write, once per upload, so a plan change takes effect on the next upload without a restart.

## Step 1 — Return a scope's limit

`quota.New` takes a `quota.LimitFunc`: given a scope, return its `quota.Limit{Bytes, Files}`. A zero field means unlimited in that dimension, so `quota.Limit{}` only counts. Return an error to refuse the upload outright (an unknown scope, a suspended account); the error reaches the caller unchanged.

<!-- ds:block id=limits-du27j4uv -->
**limits** · [`examples/records-quota/main.go:66-72`](../../examples/records-quota/main.go#L66-L72)

```go
func limits(_ context.Context, scope string) (quota.Limit, error) {
	l, ok := planOf[scope]
	if !ok {
		return quota.Limit{}, filesystem.Errorf(filesystem.CodePermission, "no plan for scope %q", scope)
	}
	return l, nil
}
```
<!-- /ds:block hash=db64e2 -->

The demo keeps plans in a map; a real app reads the scope's plan from its billing table here. The function runs inside the upload's transaction, so it may read through `Store.Conn(ctx)` if the plan lives in the same database.

## Step 2 — Install the plugin

Install it with `records.WithPlugins` and run `Migrate`, which creates `fs_quota_usage`. The demo installs the [versions plugin](versions.md) too, to show how the two interact in step 5.

The plugin stamps usage rows with the Store's clock (`records.WithClock`); `quota.WithClock(now)` gives it a clock of its own for a test. Reservation IDs come from the Store's generator (`records.WithIDs`, through `Store.NewID`).

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

Check: `Migrate` returns `quota/0001_usage` among the migrations it applied.

## Step 3 — Upload; past the limit is refused

Nothing changes in how files are recorded: `Upload` and `CreateFile` run the plugin's `BeforeCreate` hook, which charges the size and one file to the scope or fails the call with `CodeQuotaExceeded`. A refused upload stores nothing: `Upload` deletes the object it had written.

<!-- ds:block id=uploadtext-8afxkytn -->
**uploadText** · [`examples/records-quota/main.go:123-125`](../../examples/records-quota/main.go#L123-L125)

```go
func uploadText(ctx context.Context, s *records.Store, scope, body string) (records.File, error) {
	return s.Upload(ctx, scope, diskName, strings.NewReader(body), "note.txt", textPolicy)
}
```
<!-- /ds:block hash=20a8bc -->

A scope's usage row is created by its first upload. Two first uploads of the same scope at the same moment race to create it; the loser fails with `CodeUnavailable` (on Postgres a failed statement aborts the transaction, so the plugin cannot retry in place). Retry that one code:

<!-- ds:block id=uploadwithretry-g656j6mk -->
**uploadWithRetry** · [`examples/records-quota/main.go:131-140`](../../examples/records-quota/main.go#L131-L140)

```go
func uploadWithRetry(ctx context.Context, s *records.Store, scope, body string) (records.File, error) {
	var err error
	for range maxRetries {
		var f records.File
		if f, err = uploadText(ctx, s, scope, body); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
			return f, err
		}
	}
	return records.File{}, err
}
```
<!-- /ds:block hash=3cd41f -->

Show usage with `Usage`; a scope that never uploaded uses nothing (a zero `quota.Usage`, no error):

<!-- ds:block id=usageline-m7guc4ar -->
**usageLine** · [`examples/records-quota/main.go:144-154`](../../examples/records-quota/main.go#L144-L154)

```go
func usageLine(ctx context.Context, s *records.Store, q *quota.Plugin, scope string) (string, error) {
	u, err := q.Usage(ctx, s, scope)
	if err != nil {
		return "", err
	}
	l, err := limits(ctx, scope)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %d of %d bytes, %d of %d files", scope, u.Bytes, l.Bytes, u.Files, l.Files), nil
}
```
<!-- /ds:block hash=c0e890 -->

```text
two uploads:           acme: 18 of 25 bytes, 2 of 3 files
over the byte limit:   quota_exceeded
```

Check: the 19-byte third upload is refused with `quota_exceeded` and the usage stays at 18 bytes. The file limit works the same way: on the free plan the fourth file of any size is refused (`TestFileLimit`).

## Step 4 — A rolled-back upload un-charges itself

The charge is a row update in the upload's transaction, so it commits or rolls back with the file row. When a request uploads inside `Store.Tx` and then fails, the row, the charge and the object all go:

<!-- ds:block id=uploadthenfail-9vecxbsh -->
**uploadThenFail** · [`examples/records-quota/main.go:162-169`](../../examples/records-quota/main.go#L162-L169)

```go
func uploadThenFail(ctx context.Context, s *records.Store, scope string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := uploadText(ctx, s, scope, "draft"); err != nil {
			return err
		}
		return errAbort
	})
}
```
<!-- /ds:block hash=3d3a5e -->

```text
after a rollback:      acme: 18 of 25 bytes, 2 of 3 files
```

## Step 5 — Replace charges the difference

`Replace` and `ReplaceObject` run the plugin's `BeforeReplace` hook (`records.ReplaceHooks`): the new content's size minus the old one's is charged. Growth that would pass the byte limit is refused with `CodeQuotaExceeded` and the file keeps its content; a shrink is credited. The file count does not change, because it is the same file.

<!-- ds:block id=replacecontent-v8pf85nf -->
**replaceContent** · [`examples/records-quota/main.go:176-178`](../../examples/records-quota/main.go#L176-L178)

```go
func replaceContent(ctx context.Context, s *records.Store, f records.File, body string) (records.File, error) {
	return s.Replace(ctx, f.Scope, f.ID, strings.NewReader(body), "", textPolicy)
}
```
<!-- /ds:block hash=f9992a -->

```text
replaced 10 -> 4:      acme: 12 of 25 bytes, 2 of 3 files
growth past the limit: quota_exceeded
```

Check: the usage drops by 6 bytes even though the [versions plugin](versions.md) still stores the 10-byte original as a version. By default kept versions are not charged; bound them with `versions.WithKeep`, which caps how many each file keeps. If your plans must count them, turn on `WithHeldObjects` (next section).

### Counting versions and variants: `WithHeldObjects`

`quota.New(limits, quota.WithHeldObjects())` charges the objects other plugins hold beside a file's content, kept [versions](versions.md) and [image variants](variants.md), to the scope's bytes, so the byte limit bounds everything the scope stores. Holders report them through `records.Store.Hold` and `Release` inside their own transaction (the core calls every installed `records.UsageHooks` plugin, quota among them), so the same conditional update keeps the guarantee that two writes never both pass a limit. Held objects count as bytes, never as files.

With it on, a replacement is charged its new content plus the version it keeps (a file at its version cap first releases the version it drops, so it needs room only for the difference), and one that would pass the byte limit is refused with `quota_exceeded` and changes nothing. Deleting a version, a version dropped past the cap, and purging a file release what they held; `Recount` adds every holder's `HeldBytes` to the files' total.

It is off by default, so usage rows built before it keep their meaning. Turning it on for a Store that already has versions or variants needs a `Recount` of every scope; until then releasing those older objects (never charged) leaves usage below the truth, at zero at worst.

## Step 6 — Trash still counts, purge releases

A trashed file's bytes are still stored, so it still counts. `Purge` (and `SweepTrash`, `PurgeOwner`, `SetAttachment` with `PreviousPurge`, which all purge) runs `BeforePurge`, which subtracts the file's size and one file in the purge transaction.

```text
trashed:               acme: 12 of 25 bytes, 2 of 3 files
purged:                acme: 8 of 25 bytes, 1 of 3 files
```

If your product promises "deleted files do not count", purge on delete, or schedule `SweepTrash` with a short retention: see [Lifecycle](../records/lifecycle.md).

## Step 7 — Recount after a repair

`Recount` rebuilds a scope's usage from `fs_files` (trashed files included), plus, with `WithHeldObjects`, the bytes plugins hold in the scope, and returns it. Run it after editing `fs_files` by hand, after installing the plugin on a Store that already has files (their scopes have no usage row, or one that misses the older files), or from an admin "recalculate" button.

<!-- ds:block id=repair-828jum3m -->
**repair** · [`examples/records-quota/main.go:183-185`](../../examples/records-quota/main.go#L183-L185)

```go
func repair(ctx context.Context, s *records.Store, q *quota.Plugin, scope string) (quota.Usage, error) {
	return q.Recount(ctx, s, scope)
}
```
<!-- /ds:block hash=7de84c -->

```text
recount:               8 bytes, 1 files
```

## Per-uploader limits: seats in a workspace

A scope limit stops one tenant from filling the bucket; it does not stop one member of a team from filling the team's plan. `quota.WithUploaderLimits(fn)` keeps a second counter per uploader within each scope (the file's `UploadedBy`, which the Store takes from `records.WithActor`) and limits it with a function you write, `func(ctx, scope, uploader) (quota.Limit, error)`. Every file is then charged to its scope and to its uploader in the same transaction, with the same conditional update, and is refused with `quota_exceeded` when either counter would pass its limit; the message names which one (`uploader "user:alice" in scope "team-acme"`). Return `quota.Limit{}` to count an uploader without limiting it, as the example does for uploads made without an actor (they are uploader `""`).

The example is [`examples/records-quota-seats`](../../examples/records-quota-seats): a 100-byte team plan with 40-byte seats.

<!-- ds:block id=seatlimit-r3vq8w2m -->
**seatLimit** · [`examples/records-quota-seats/main.go:61-66`](../../examples/records-quota-seats/main.go#L61-L66)

```go
func seatLimit(_ context.Context, _, uploader string) (quota.Limit, error) {
	if uploader == "" {
		return quota.Limit{}, nil
	}
	return seatPlan, nil
}
```
<!-- /ds:block hash=2b0657 -->

<!-- ds:block id=newstore-seats-6m2kq9xa -->
**newStore** · [`examples/records-quota-seats/main.go:73-94`](../../examples/records-quota-seats/main.go#L73-L94)

```go
func newStore(ctx context.Context, db *sql.DB) (*records.Store, *quota.Plugin, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, nil, err
	}
	q, err := quota.New(workspaceLimit, quota.WithUploaderLimits(seatLimit))
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}
```
<!-- /ds:block hash=cfe521 -->

`UploaderUsage(ctx, s, scope, uploader)` reads one member's counter, for a members page:

<!-- ds:block id=seatusage-p4n7c2tz -->
**seatUsage** · [`examples/records-quota-seats/main.go:105-119`](../../examples/records-quota-seats/main.go#L105-L119)

```go
func seatUsage(ctx context.Context, s *records.Store, q *quota.Plugin) (string, error) {
	var parts []string
	for _, m := range []string{alice, bob} {
		u, err := q.UploaderUsage(ctx, s, team, m)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("%s %d/%d", strings.TrimPrefix(m, "user:"), u.Bytes, seatPlan.Bytes))
	}
	u, err := q.Usage(ctx, s, team)
	if err != nil {
		return "", err
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(", team %d/%d bytes", u.Bytes, teamPlan.Bytes), nil
}
```
<!-- /ds:block hash=86d8a2 -->

```text
two uploads:             alice 30/40, bob 25/40, team 55/100 bytes
alice past her seat:     filesystem: quota_exceeded: quota: uploader "user:alice" in scope "team-acme" would exceed its limit of 40 bytes
bob grew alice's file:   alice 38/40, bob 25/40, team 63/100 bytes
recount:                 team 63 bytes, 2 files
after the recount:       alice 38/40, bob 25/40, team 63/100 bytes
```

Rules worth knowing:

- **The uploader owns the bytes.** A replacement is charged to (or credits) the file's uploader, whoever makes it: Bob editing Alice's document grows Alice's usage, as the third line shows. Purging releases from the uploader too.
- **Files only.** Held objects (`WithHeldObjects`: kept versions, variants) count toward the scope, never toward an uploader, because a held object names no uploader.
- **Recount.** `Recount(ctx, s, scope)` rebuilds every uploader row of the scope from `fs_files` along with the scope's; `RecountUploader(ctx, s, scope, uploader)` rebuilds one. Turning the option on for a Store that already has files needs a `Recount` of every scope, since until then the existing files are in no uploader row.
- **Offboarding.** `Store.PurgeScope` removes the scope's uploader rows with its total.
- **Off by default.** Without the option no uploader row is kept and `UploaderUsage` reads zero. The table exists either way (migration `quota/0002_uploader_usage`), so turning it on later needs no migration.

## Other counters: organisations, owner types, content classes

A scope and, with `WithUploaderLimits`, its uploaders are the counters the plugin keeps by default. `quota.WithDimensions(fn)` charges every file to more counters, named by `fn(ctx, file)` as `quota.Dimension{Key, Limit}`, in the same transaction and with the same conditional `UPDATE`, so a file past any of their limits is refused with `quota_exceeded`. A key is shared across scopes, so one key returned for every workspace of an organisation is one cap over all of them:

```go
q, err := quota.New(scopeLimits, quota.WithDimensions(func(ctx context.Context, f records.File) ([]quota.Dimension, error) {
	org, err := orgs.OfWorkspace(ctx, f.Scope) // your directory
	if err != nil {
		return nil, err // the upload is refused, not left uncounted
	}
	return []quota.Dimension{
		{Key: "org:" + org.ID, Limit: quota.Limit{Bytes: org.PlanBytes}},
		{Key: "type:" + f.Scope + ":" + strings.SplitN(f.ContentType, "/", 2)[0]}, // a content class, counted only
	}, nil
}))
```

The function is asked on upload, on both sides of a replacement (a file whose class changes moves between counters) and on purge, so it must answer the same for the same file. Read a counter with `DimensionUsage`; rebuild them all from the files with `RecountDimensions` after turning the option on or changing the function. On a large Store, repair only what changed with `RecountDimensionsFor(ctx, s, quota.RecountFilter{Keys, Scopes})`: it rebuilds the listed keys from the files of the listed scopes, in a transaction that holds only those rows and reads only those scopes. Every file charged to a listed key must be in a listed scope, or the row is rebuilt short; `Scopes` without `Keys` is refused for that reason. When workspace `w2` moves from organisation `acme` to `beta`:

```go
err := q.RecountDimensionsFor(ctx, store, quota.RecountFilter{
	Keys:   []string{"org:acme", "org:beta"},
	Scopes: append(workspacesOf("acme"), workspacesOf("beta")...), // w2 included
})
```
 Held objects (`WithHeldObjects`) count toward the scope only. Trashed files still count everywhere; a "trash is free" model is not offered yet.

Try it: [`examples/records-quota`](../../examples/records-quota) runs this section in `dimensions.go` (`orgCounters`): `go run ./records-quota` from `examples/`.

## Reserve room for uploads in flight

A presigned PUT or a tus upload reaches the backend before any file is recorded, so the charge at `CreateFile` comes too late to stop several uploads that together pass the limit. Reserve the bytes when you issue the ticket, and let the recorded file use the reservation up. The second half of the example (`inflight.go`) walks it on a free-plan scope (25 bytes).

**1. Reserve when you issue the ticket.** `Reserve(ctx, store, scope, n, ttl)` holds `n` bytes; past the limit it is `quota_exceeded`, before the client sends a byte. Hand `r.ID` to the client with the presigned URL or tus session:

<!-- ds:block id=issueticket-w5t9m3hr -->
**issueTicket** · [`examples/records-quota/inflight.go:58-60`](../../examples/records-quota/inflight.go#L58-L60)

```go
func issueTicket(ctx context.Context, s *records.Store, q *quota.Plugin, scope string, size int64) (quota.Reservation, error) {
	return q.Reserve(ctx, s, scope, size, ticketTTL)
}
```
<!-- /ds:block hash=6e34a5 -->

**2. Confirm under the reservation.** Record the object with a context from `quota.WithReservation(ctx, r.ID)`: the file's real size is charged and the reservation let go in one `UPDATE`, in the file's transaction:

<!-- ds:block id=confirm-h2r7c9vk -->
**confirm** · [`examples/records-quota/inflight.go:65-67`](../../examples/records-quota/inflight.go#L65-L67)

```go
func confirm(ctx context.Context, s *records.Store, r quota.Reservation, key string) (records.File, error) {
	return s.CreateFile(quota.WithReservation(ctx, r.ID), records.NewFile{Scope: r.Scope, Disk: diskName, Key: key})
}
```
<!-- /ds:block hash=3418ed -->

**3. Release what never arrives.** `Release(ctx, store, scope, r.ID)` gives an abandoned upload's room back at once; `Sweep(ctx, store, now)` (scheduled as `jobs.QuotaSweepReservations`) releases the ones nobody released once their `ttl` passed.

Check: the example reserves 15 bytes and prints `second ticket while the first holds the room: quota_exceeded` for a 20-byte ticket, `reserved 15 of 25 bytes`, then, once a 5-byte object is confirmed, `confirmed: 5 bytes used, 0 reserved`, and `abandoned ticket released: true`.

Reserved bytes count against the byte limit for every other upload and reservation (the check and the hold are one conditional `UPDATE`, so concurrent reservations never promise more than the limit). A file larger than its reservation passes only if the scope has room for the difference; a smaller one frees the rest. Reservations hold bytes only; files are counted when recorded. `Usage.Reserved` shows what is held, and `Recount` keeps it. `n` and `ttl` must be positive (`invalid_argument`).

## Usage events for billing

With `quota.WithUsageEvents()` every change to a scope's usage (an upload, a growing or shrinking replacement, a purge, a held object) emits `quota.usage_changed` through `Store.Emit`, inside the change's transaction, so a rolled-back upload never bills. Off by default: it adds one read and one event to every change.

<!-- ds:block id=newbillingstore-c8n4w2kq -->
**newBillingStore** · [`examples/records-quota/inflight.go:38-51`](../../examples/records-quota/inflight.go#L38-L51)

```go
func newBillingStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, consumer records.Plugin) (*records.Store, *quota.Plugin, error) {
	q, err := quota.New(limits, quota.WithUsageEvents())
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q, consumer))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}
```
<!-- /ds:block hash=86c4df -->

| Detail key | Holds |
|---|---|
| `bytes`, `files` (`quota.DetailBytes`, `DetailFiles`) | The scope's usage after the change |
| `delta_bytes`, `delta_files` (`DetailDeltaBytes`, `DetailDeltaFiles`) | What the change added; negative for a shrink or a purge |

The event's `ID` is the Store's, kept by every redelivery, so a billing consumer fed by [webhooks](webhooks.md) at least once can drop repeats. Inside the process an `EventHooks` plugin sees the same events; the example's consumer prints `usage events: 5 bytes (5)`: one event, for the recorded file. Reserving and releasing emit nothing, because they do not change what is stored.

When to use which: bill on `bytes` (the total after the change) when you sample usage, or sum `delta_bytes` when you meter changes; never both. For bandwidth rather than storage, use [egress](egress.md).

## The table

`fs_quota_usage`, one row per scope that ever uploaded:

| Column | Type | Holds |
|---|---|---|
| `scope` | key, primary key | the scope |
| `bytes` | 64-bit integer | bytes stored by the scope's files, trashed ones included, plus held objects with `WithHeldObjects` |
| `files` | 64-bit integer | number of the scope's files, trashed ones included |
| `updated_at` | timestamp (UTC) | the last change |

`fs_quota_uploader_usage`, one row per scope and uploader, filled only with `WithUploaderLimits` (migration `quota/0002_uploader_usage`): the same `bytes`, `files` and `updated_at`, plus `uploader` (key, `""` for files recorded without an actor), with the primary key `(scope, uploader)`.

`fs_quota_dimension_usage`, one row per dimension key, filled only with `WithDimensions` (migration `quota/0004_dimension_usage`): `dimension` (key, primary key), `bytes`, `files` and `updated_at`, across every scope.

Applications may read them (a usage column on an admin list, a join for a report). Change them only through the plugin, or rebuild them with `Recount` after you did.

## How it works with the rest

- **Transactions.** Every charge and release runs on `Store.Conn(ctx)` inside the operation's transaction, so it joins an outer `Store.Tx` too.
- **Versions.** Kept versions are not charged (step 5) unless `WithHeldObjects` is on. `versions.Restore` replaces the file's content through `ReplaceObject`, so restoring a larger version is charged its difference and can be refused.
- **Visibility.** `SetVisibility` moves bytes between disks but does not change sizes; usage is unchanged.
- **Image variants and other plugin objects.** Variants are not files, so they are never charged as files; with `WithHeldObjects` their bytes are charged once the variants plugin reports them through `Store.Hold`. A plugin of your own that stores objects can do the same: call `Hold` and `Release` in its transactions and implement `records.HeldCounter` so `Recount` includes it.
- **Files recorded before the plugin was installed.** Purging or shrinking one releases space a usage row never counted, which can drive usage below the truth; it stops at zero, never negative. Run `Recount` for every existing scope when you install the plugin.

## What can go wrong

- `quota_exceeded` from `Upload`, `CreateFile`, `Replace` or `versions.Restore` — the scope is at its limit. Over HTTP, [recordshttp](../records/http-endpoints.md) answers `507 Insufficient Storage` with the plugin's message, which names the limit the upload passes (bytes, files, or both).
- `unavailable` on a scope's very first upload — a concurrent first upload created the usage row; retry (step 3). Over HTTP it is `503`.
- The `LimitFunc`'s own error — returned as is; `recordshttp` maps it by its code like any other error.
- Usage drifted from the files — you changed `fs_files` outside the Store, or installed the plugin after the files; run `Recount`.

## Next

- [Versions](versions.md): the history this plugin counts only with `WithHeldObjects`.
- [Lifecycle](../records/lifecycle.md): trash, purge and the sweeps that release space.
- [Events and hooks](../records/events-and-hooks.md): `FileHooks` and `ReplaceHooks`, the interfaces this plugin implements.
- The plugin's [README](../../records/quota/README.md) is the reference for its API and table.
