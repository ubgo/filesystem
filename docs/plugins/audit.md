# Audit log

You will have an append-only log of every change the records Store makes to files and attachments: who uploaded, replaced, made public, trashed, restored or purged a file, who attached, detached or reordered it, and when; and, from the bundled plugins, folder changes, tags and share links. It is the activity feed of a drive and the trail a compliance review asks for. Install it when people need to answer "who did this?" about a file, or when a customer asks for an activity log of their workspace.

## Before you start

- A Store, as in [Files](../records/files.md). The plugin needs no other plugin.
- The example is [`examples/records-audit`](../../examples/records-audit): `go run ./records-audit` in `examples/` prints every step below; `go test ./records-audit` runs them.
- What is logged: one entry per [event](../records/events-and-hooks.md): the Store's twelve kinds, and every kind a plugin emits with `Store.Emit` (the [folders](folders.md), [tags](tags.md) and [share](share.md) plugins' `folders.*`, `tags.*` and `share.*`, listed under [Events from plugins](../records/events-and-hooks.md#events-from-plugins)). What is not logged: reads (`File`, `Files`, `URL`), plugin changes that emit no event (quota recounts, variant generation), and anything the app writes to the `fs_` tables behind the Store's back.

## Step 1 — Install the plugin

Pass `audit.New()` to `records.WithPlugins`. `Migrate` creates `fs_audit` next to the core tables.

<!-- ds:block id=newstore-4az2gwj7 -->
**newStore** · [`examples/records-audit/main.go:97-107`](../../examples/records-audit/main.go#L97-L107)

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

Check: `Migrate` returns `audit/0001_audit` and `audit/0002_audit_subject_detail` among the migrations it applied on the first run, and nothing on the next.

## Step 2 — Name the actor once per request

An entry's actor is whatever the context carries. Set it once, in the middleware that authenticates the request, with `audit.WithActor(ctx, "user:alice")` (the same function as `records.WithActor`, so the Store also records it as the file's `UploadedBy`). Background jobs set their own: `"system:cron"`, `"api-key:…"`. A change made with no actor is logged with `actor = ""`.

If your app already keeps the signed-in user under its own context key, read it from there instead with `WithActorFunc`:

<!-- ds:block id=newpluginwithownkey-ss59fp5h -->
**newPluginWithOwnKey** · [`examples/records-audit/main.go:116-123`](../../examples/records-audit/main.go#L116-L123)

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

With `WithActorFunc`, the plugin ignores `records.WithActor`, but the Store still uses `records.WithActor` for `File.UploadedBy`. Set both, or derive one from the other in the middleware, if you want them to agree.

An actor longer than `audit.MaxActorBytes` (191 bytes, the indexed width of the column) fails the change with `invalid_argument` instead of being cut short: a trail with truncated actors could not be trusted.

<!-- ds:block id=toolongactor-vjp5uu4w -->
**tooLongActor** · [`examples/records-audit/main.go:212-216`](../../examples/records-audit/main.go#L212-L216)

```go
func tooLongActor(ctx context.Context, s *records.Store) error {
	ctx = audit.WithActor(ctx, "user:"+strings.Repeat("x", audit.MaxActorBytes))
	_, err := s.Upload(ctx, tenant, diskName, bytes.NewReader([]byte("hi")), "hi.txt", textPolicy)
	return err
}
```
<!-- /ds:block hash=fb7627 -->

Check: the program prints `long actor: invalid_argument`, and no file was recorded. (For `Upload`, the core's own check on `UploadedBy` refuses it first, with the same code; for changes that record no actor in the core, such as `Trash`, the plugin is what refuses it.)

## Step 3 — Every change is an entry

This function makes one change of each kind; the comments name the event each call emits.

<!-- ds:block id=everychange-aq6m25az -->
**everyChange** · [`examples/records-audit/main.go:128-167`](../../examples/records-audit/main.go#L128-L167)

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

Each entry is a `records.Event` plus the entry ID and the actor. Which fields are filled depends on the kind; the ones a kind does not set are stored as `""`:

| Kind | `FileID` | `AttachmentID`, `Owner`, `Role` |
|---|---|---|
| `file.created`, `file.trashed`, `file.restored`, `file.replaced`, `file.visibility_changed`, `file.purged` | set | empty |
| `attachment.created`, `attachment.deleted` | set | set |
| `attachments.reordered` | empty | `Owner` and `Role` set, `AttachmentID` empty |

A plugin's event is logged the same way, with two more fields kept: `Subject`, the ID of what it is about when that is not a file (a folder, a share link), and `Detail`, its particulars (a folder's old and new name, the tag added). An entry for `folders.renamed` has `Subject` set to the folder and `Detail` `{"name": "new", "from": "old"}`; an entry for `tags.added` has `FileID` and `Detail` `{"tag": "…"}`, so it is in that file's history. The Store's own kinds have neither.

A purge logs one `file.purged`; the attachments that go with the file get no entries of their own. A change that changes nothing (trashing a trashed file, restoring a live one, detaching an unknown attachment, `SetVisibility` to the current visibility) logs nothing.

## Step 4 — A file's history

`Entries` with `FileID` set is the history of one file, newest first:

<!-- ds:block id=history-23qsz95p -->
**history** · [`examples/records-audit/main.go:171-174`](../../examples/records-audit/main.go#L171-L174)

```go
func history(ctx context.Context, s *records.Store, au *audit.Plugin, fileID string) ([]audit.Entry, error) {
	page, err := au.Entries(ctx, s, audit.Query{Scope: tenant, FileID: fileID})
	return page.Entries, err
}
```
<!-- /ds:block hash=78dc27 -->

Check: the program prints the twelve entries of the purged file; `file.moved` carries the disks it went from and to in its detail:

```
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
```

Alice's thirteenth entry, `attachments.reordered`, is not in a file's history because it carries no `FileID`. Entries outlive their file: the history is still readable after `file.purged`, which is what an investigation needs.

## Step 5 — An actor's activity, page by page

`Entries` with `Actor` set is everything one actor did in the scope. Pages use the same keyset cursor as `records.Store.Files`: pass `Page.NextCursor` back as `Query.Cursor` with the same filters until it is empty. Entries are ordered by time, then by ID, and IDs carry the plugin's write sequence, so entries stamped with the same microsecond (several changes in one transaction, or a burst of requests) still list in the order they were written, on every page. Same-microsecond entries written by two different processes have no defined order between them. `Limit` defaults to 50 (`audit.DefaultPageSize`); more than 1000 (`audit.MaxPageSize`), a negative limit or a malformed cursor is `invalid_argument`.

<!-- ds:block id=activity-n7j5jaye -->
**activity** · [`examples/records-audit/main.go:179-193`](../../examples/records-audit/main.go#L179-L193)

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

Check: `activity of user:alice: 13 entries in 5 pages` with a page size of 3. The background upload made as `system:cron` is not among them. `FileID` and `Actor` can be combined ("what did Alice do to this file"), and with neither set `Entries` returns the whole scope's feed. Every query is limited to one scope; there is no cross-scope listing.

## Step 6 — Rolled-back changes leave no entry

Entries are written by `OnEvent` inside the change's own transaction. If the transaction rolls back, the entry goes with it; if it commits, the entry is there. The log can never show a change that did not happen or miss one that did.

<!-- ds:block id=rolledback-df6ptbdc -->
**rolledBack** · [`examples/records-audit/main.go:199-207`](../../examples/records-audit/main.go#L199-L207)

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

Check: `rollback: demo: request failed after the upload`, then `activity after the rollback: 13 entries`: the count did not change. The uploaded object was deleted too (see [Lifecycle](../records/lifecycle.md)).

The reverse holds as well: if the audit insert fails (a database error), the change fails and rolls back. An app that cannot record a change does not make it.

## Step 7 — Keep it for as long as you need

Nothing deletes entries except `Prune`, which deletes the entries of every scope older than a cutoff and returns how many it deleted, and `PruneScope`, which does the same for one scope. The library starts no background work, so schedule it yourself (a cron job, a ticker in a worker):

<!-- ds:block id=prune-vhnhy5ef -->
**prune** · [`examples/records-audit/main.go:221-223`](../../examples/records-audit/main.go#L221-L223)

```go
func prune(ctx context.Context, s *records.Store, au *audit.Plugin, now time.Time) (int64, error) {
	return au.Prune(ctx, s, now.Add(-retention))
}
```
<!-- /ds:block hash=c6ea22 -->

Check: run with a clock past the retention, the program prints `pruned 14 entries`; the test that prunes with today's clock deletes none.

`PruneScope(ctx, store, scope, cutoff)` applies a retention period to one scope only, for a tenant whose contract keeps its log for a different period. Passed a cutoff after the scope's last entry, it deletes the whole log of a tenant being offboarded; other scopes are never touched. Both run inside the caller's transaction when the context carries one (`store.Tx`), so the log can be deleted together with the tenant's other rows.

## Reads, tamper evidence and a change feed

Three options, all off by default, turn the log into an access log, a tamper-evident trail and a change feed:

```go
au := audit.New(
	audit.WithAccessLog(audit.SampleRate(0.01)), // one read in a hundred
	audit.WithHashChain(chainKey),               // key kept outside the database
	audit.WithRetentionFloor(7*365*24*time.Hour), // Prune never deletes the last seven years
)
```

- **Access log.** With `WithAccessLog(sample)` the plugin is also a `records.ServeHooks` and logs the reads handlers report with `Store.Served`: links handed out by recordshttp, share links followed, variants served. Their kind is the `records.ServeKind` (`access.link`, `access.share`, `access.variant`, `access.stream`) and `Detail["bytes"]` the bytes handed out. Reads outnumber changes, so pass a sampler: `audit.SampleRate(r)` keeps a fraction, or a func of your own keeps what matters (every read of a confidential file, no thumbnails).
- **Hash chain.** With `WithHashChain(key)` each entry gets `Seq`, its place in its scope's chain, and an HMAC-SHA256 under `key` over its fields and the previous entry's hash. `Verify(ctx, store, scope)` walks the chain and returns `conflict` wrapping `audit.ErrChainBroken` at the first entry edited, deleted, reordered or inserted behind the plugin's back, and when the newest entries were cut off. The chain's row in `fs_audit_chain` (its head and prune anchor) carries a MAC under the same key, so cutting off the newest entries or faking a prune by rewriting that row is caught too, and a write that finds the row altered is refused rather than sealing the forgery. Keep the key outside the database: without it (`nil`), the chain catches edits that did not recompute it, but someone who can rewrite every row could rebuild it. The scope's lock is taken mid-transaction, when the change emits its event; on Postgres and MySQL that can deadlock with another transaction's row locks, and the aborted one returns `unavailable` (`records.IsLockConflict`): run the whole transaction again.
- **Change feed.** `Changes(ctx, store, audit.ChangeQuery{Scope: s, Cursor: c})` returns the scope's changes (reads left out) after a cursor, oldest first, and a `NextCursor` to continue from. The cursor is the entry's sequence number, stored in the table, so it means the same after a restart or on another machine. It never skips a commit that lands out of order: a scope's next number is taken by an `UPDATE` of its `fs_audit_chain` row, and the row stays locked until the change's transaction commits, so change N+1 cannot be numbered before change N has committed, and a rolled-back change gives its number back, leaving no gap. The price is that a scope's writes are serialized, which is why the chain is opt-in.
- **Retention floor.** `WithRetentionFloor(d)` makes `Prune` and `PruneScope` refuse, with `invalid_argument` and deleting nothing, any cutoff later than the Store's clock minus `d`. `Verify` refuses a chain whose prune anchor is younger than the floor. With the chain, pruning deletes a scope's oldest entries in sequence order and records the last deleted entry's hash and time as the chain's anchor, so `Verify` still checks what remains, and `Changes` refuses (`not_found`) a cursor older than the anchor rather than silently skipping what was pruned.

### Step 8 — Turn on the chain and the access log

The second half of [`examples/records-audit`](../../examples/records-audit) (`chain.go`) builds the plugin with both:

<!-- ds:block id=newchainedplugin-t6w2k8qm -->
**newChainedPlugin** · [`examples/records-audit/chain.go:30-35`](../../examples/records-audit/chain.go#L30-L35)

```go
func newChainedPlugin() *audit.Plugin {
	return audit.New(
		audit.WithHashChain(chainKey),
		audit.WithAccessLog(audit.SampleRate(1)),
	)
}
```
<!-- /ds:block hash=221dea -->

and installs it on a Store as in step 1. Its user uploads, replaces, streams (reported with `Store.Served`) and trashes a file.

### Step 9 — Follow the change feed

A consumer (a search indexer, a sync to another system) reads the feed a page at a time and stores the cursor between polls:

<!-- ds:block id=follow-c9n4v7hx -->
**follow** · [`examples/records-audit/chain.go:41-56`](../../examples/records-audit/chain.go#L41-L56)

```go
func follow(ctx context.Context, s *records.Store, au *audit.Plugin, scope, cursor string) ([]string, string, error) {
	var kinds []string
	for {
		page, err := au.Changes(ctx, s, audit.ChangeQuery{Scope: scope, Cursor: cursor, Limit: 2})
		if err != nil {
			return nil, cursor, err
		}
		for _, e := range page.Entries {
			kinds = append(kinds, fmt.Sprintf("%d:%s", e.Seq, e.Kind))
		}
		if len(page.Entries) == 0 {
			return kinds, page.NextCursor, nil
		}
		cursor = page.NextCursor
	}
}
```
<!-- /ds:block hash=d1ce52 -->

Check: the example prints `change feed: 1:file.created, 2:file.replaced, 4:file.trashed (reads left out)`: sequence 3 is the stream, in the log but not in the feed. After a restore it polls again from the stored cursor and prints `next poll from the stored cursor: 5:file.restored`. An empty page returns the query's own cursor, so polling with it again is always safe.

### Step 10 — Verify the chain

<!-- ds:block id=verify-k3p8w5rc -->
**verify** · [`examples/records-audit/chain.go:62-64`](../../examples/records-audit/chain.go#L62-L64)

```go
func verify(ctx context.Context, s *records.Store, au *audit.Plugin, scope string) (int64, error) {
	return au.Verify(ctx, s, scope)
}
```
<!-- /ds:block hash=d4afa9 -->

Check: `verify: 5 entries, err=<nil>`. The example then rewrites the first entry's actor with plain SQL, as someone with database access but not the key would, and prints `after an edit with SQL: conflict, chain broken true`. Run `Verify` on a schedule and alert on it; it reads the whole chain of a scope.

The records module's own tests go further: `go test ./audit/ -run 'Verify|Changes|AccessLog|RetentionFloor'` in `records/` edits, deletes and reorders entries with SQL and sees `Verify` fail each time.

### Rotating the chain key, and logging where changes came from

`WithHashChain(key)` uses one key forever. `audit.WithHashChainKeyring(ring)` takes a `audit.Keyring` instead: `Current` names the ID and key new entries are hashed with, and `Key(id)` returns any key by ID, so `Verify` checks each entry under the key it recorded (`Entry.KeyID`). Entries written with `WithHashChain` carry the ID `""`, so to move an existing log to key IDs, keep the old key under `""`:

```go
ring := audit.StaticKeyring{
	CurrentID: "2026-10",
	Keys: map[string][]byte{
		"":        oldKey, // entries chained with WithHashChain(oldKey)
		"2026-10": newKey,
	},
}
p := audit.New(audit.WithHashChainKeyring(ring))
```

Back `Keyring` with your KMS to rotate without a deploy. A ring that fails fails the change (it rolls back) or the `Verify`; it never skips the chain. A rotation takes effect on each scope's next entry.

A `Keyring` hands its keys to the process. When the key must never leave an HSM or a KMS, use `audit.WithHashChainMAC(signer)` with an `audit.Signer` instead: `CurrentID(ctx)` names the key new entries are signed with, and `MAC(ctx, id, msg)` returns the tag, which the KMS computes (AWS KMS `GenerateMac`, Azure Key Vault, Vault transit `hmac`). A signer that computes HMAC-SHA256 under the same key yields the same hashes as `WithHashChain` and `WithHashChainKeyring` (`audit.KeyringMAC(ring)` is that signer in process), so an existing log moves to it without breaking `Verify`. A tag must be `audit.MinMACBytes` (16) to `audit.MaxMACBytes` (64) bytes, so HMAC-SHA512 fits and a truncated tag is refused; a failing signer, or a tag outside those bounds, fails the change or the `Verify`, as a failing ring does:

```go
// kmsSigner asks Vault transit for every MAC; the key stays in Vault.
type kmsSigner struct{ vault *vault.Client }

func (k kmsSigner) CurrentID(ctx context.Context) (string, error) { return "audit-2026", nil }

func (k kmsSigner) MAC(ctx context.Context, id string, msg []byte) ([]byte, error) {
	return k.vault.TransitHMAC(ctx, id, "sha2-256", msg) // your client's call
}

p := audit.New(audit.WithHashChainMAC(kmsSigner{vault: client}))
```

`audit.WithSource()` logs, with every entry, the channel and client the application put on the request with `records.WithSource`; `audit.WithSourceIP()` logs the IP address too, which is personal data and therefore a separate choice. With the chain on, the source is part of the entry's hash, so it cannot be edited unnoticed. Entries with neither a key ID nor a source hash exactly as before, so existing chains keep verifying.

`audit.WithEntryDetail(fn)` adds fields the fixed source columns do not hold, such as a request or trace ID, a user agent, or the admin acting on behalf of a user. `fn(ctx, event)` runs inside the change and returns a `map[string]string` that is stored with the event's own `Detail`, so with the chain on it is hashed too. A key the event already carries keeps the event's value, so the hook can add context but cannot rewrite what the change reported; a nil map adds nothing:

```go
p := audit.New(audit.WithEntryDetail(func(ctx context.Context, e records.Event) map[string]string {
	return map[string]string{"request_id": middleware.RequestID(ctx)}
}))
```

Try it: [`examples/records-audit`](../../examples/records-audit) runs this section in `rotate.go` ([`rotatedRing`](ds:block?id=rotatedring-m5c9x2hq)): `go run ./records-audit` from `examples/`.

## The table

`fs_audit`, created by the migration `audit/0001_audit`; `audit/0002_audit_subject_detail` adds `subject` and `detail` (entries written before it read back with neither):

| Column | Type (Postgres / MySQL / SQLite) | Holds |
|---|---|---|
| `id` | `TEXT` / `VARCHAR(191)` / `TEXT`, primary key | `aud_`, the plugin's write sequence in 16 hex digits (the tie-breaker for entries with the same `at`), then 26 random base32 characters so IDs from different processes never collide. Entries written before the sequence was added have only the random part |
| `scope` | key | the event's scope |
| `at` | `TIMESTAMPTZ` / `DATETIME(6)` / `TIMESTAMP` | when the Store made the change, UTC, microseconds |
| `actor` | key | the actor, or `""` |
| `kind` | short | the `records.EventKind` |
| `file_id`, `attachment_id`, `owner_id` | key | `""` when the kind does not set them |
| `owner_type`, `role` | short | `""` when the kind does not set them |
| `subject` | key | a plugin event's `Subject`, `""` otherwise |
| `detail` | `TEXT` / `MEDIUMTEXT` / `TEXT` | a plugin event's `Detail` as a JSON object, `""` when it has none |

Migration `audit/0003_audit_chain` adds `seq`, `prev_hash` and `hash` (`0`, `""`, `""` for entries written without the chain), the index `(scope, seq)`, and the table `fs_audit_chain` (one row per chained scope: the head, the prune anchor and its time, when the chain began, and a MAC over all of it). Migration `audit/0004_audit_keys_source` adds `key_id`, `source_channel`, `source_client` and `source_ip` to `fs_audit` and `key_id` to `fs_audit_chain`, all `""` without `WithHashChainKeyring` and `WithSource`.

Indexes: `(scope, at, id)` for the scope feed, `(scope, file_id, at, id)` for a file's history, `(scope, actor, at, id)` for an actor's activity. The table has no foreign key to `fs_files`, on purpose: entries must outlive purged files. Apps may read and join it freely (it holds no secrets), but write it only through the plugin.

## With other plugins and the core

- Folders, tags and share links are logged through their plugins' events, with the actor of the context they ran with: a share link downloaded by an anonymous visitor is logged with `actor = ""` unless your handler sets one. Changes that emit no event (quota recounts, variant generation) are invisible to the log; a plugin of your own reports its changes with `Store.Emit` (see [Events and hooks](../records/events-and-hooks.md#events-from-plugins)). `Entries` filters by file and actor, not by `Subject`: a folder's own history is the scope feed filtered in your code, or a query of your own on `audit.Subject`.
- `SweepTrash` purges without a request, so its `file.purged` entries carry whatever actor the job's context has; give the job one (`audit.WithActor(ctx, "system:sweep")`).
- `Replace` and `versions.Restore` both log `file.replaced`; `SetVisibility` logs `file.visibility_changed` once, even when the [versions](versions.md) and [variants](variants.md) plugins move their own objects in the same transaction.
- Entries hold actors (user IDs) and owner IDs. Treat the table as personal data in your privacy notice, `Prune` on a schedule, and `PruneScope` when a tenant leaves.

## What can go wrong

- `invalid_argument` on a change that worked before — the actor in the context is longer than 191 bytes.
- `invalid_argument` from `Entries` — `Limit` outside `[0, 1000]`, or a cursor that was not issued by `Entries` (cursors from `Store.Files` decode, but only mean something for the query that issued them).
- Entries with an empty actor — the code path that made the change did not set one: a job, a CLI, or middleware that runs after the handler.
- An entry you expected is missing — the call changed nothing (re-trash, restore of a live file, detach of an unknown attachment), or it was a plugin operation that emits no event (see `EventKindValues` in each plugin).

## Next

- [Events and hooks](../records/events-and-hooks.md): the event stream this plugin is built on, for webhooks and search indexes of your own.
- [Lifecycle](../records/lifecycle.md): trash, restore, purge and the transaction rules the log follows.
- The plugin's [README](../../records/audit/README.md) is the reference for its options, table and Go API.
