# github.com/ubgo/filesystem/records/audit

An append-only log of every change the records Store makes to files and attachments: who did what to which file, and when. The activity feed of a drive, and the trail a compliance review asks for. The guide is [docs/plugins/audit.md](../../docs/plugins/audit.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is in the records module, which requires only `github.com/ubgo/sqlb` and the core module. It brings no dependency of its own.

## Wire it

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

`Migrate` creates `fs_audit`. Set the actor once per request, in the middleware that authenticates: `ctx = audit.WithActor(ctx, "user:"+userID)` (the same as `records.WithActor`).

The plugin depends on no other plugin and serves no route. Read the log from Go with `au.Entries`; expose it over HTTP from your own handler, behind your own authorization.

## How it records

The plugin implements `records.EventHooks`. `OnEvent` inserts one row per event inside the change's transaction, so a rolled-back change leaves no entry and a committed one always has its entry; a failed insert fails the change. The actor comes from the context (`records.ActorFrom`, or the function given to `WithActorFunc`); `""` when none was set. An actor longer than `MaxActorBytes` fails the change with `invalid_argument` rather than being truncated.

Entries outlive their files: there is no foreign key, and nothing deletes them except `Prune` and `PruneScope`. `records.Store.PurgeScope` leaves the log on purpose (the plugin is not a `ScopePurger`): call `PruneScope` with a cutoff after the last entry when an offboarded tenant's history must go too.

Entries list by `(at, id)`. An ID is `aud_`, the plugin's write sequence in 16 hex digits, then 26 random base32 characters, so entries stamped with the same microsecond list in the order this process wrote them; between two processes writing in the same microsecond the order is not defined.

Every event kind is recorded: the Store's (`file.created`, `file.trashed`, `file.restored`, `file.replaced`, `file.visibility_changed`, `file.status_changed`, `file.released`, `file.moved`, `file.purged`, `attachment.created`, `attachment.deleted`, `attachments.reordered`) and every kind a plugin emits with `Store.Emit` (`folders.*`, `tags.*`, `share.*`), with the event's `Subject` and `Detail`. Plugin operations that emit no event are not.

## Options

| Option | What it does | Default |
|---|---|---|
| `audit.WithAccessLog(sample)` | Also logs reads reported with `records.Store.Served` (kind `access.*`, `Detail["bytes"]`) that `sample` keeps; `audit.SampleRate(r)` keeps a fraction, `nil` keeps all | off |
| `audit.WithHashChain(key)` | Numbers each scope's entries in commit order (`Entry.Seq`) and chains them with HMAC-SHA256 under `key`; enables `Verify` and `Changes`; serializes a scope's writes | off |
| `audit.WithHashChainKeyring(ring)` | `WithHashChain` with keys by ID from a `Keyring` (`Current`, `Key(id)`), so the chain key rotates while `Verify` checks each entry under the key it recorded (`Entry.KeyID`); entries chained with `WithHashChain` carry ID `""`, so the ring must return that key for `""`. `audit.StaticKeyring{CurrentID, Keys}` is an in-memory ring. A failing ring fails the change or the `Verify` | off |
| `audit.WithHashChainMAC(signer)` | `WithHashChain` with every MAC computed by an `audit.Signer` (`CurrentID`, `MAC(ctx, id, msg)`), so the key can stay in an HSM or a KMS; an HMAC-SHA256 signer under the same key gives the same hashes (`audit.KeyringMAC(ring)` is that signer in process). A tag outside `MinMACBytes` (16) to `MaxMACBytes` (64), or a failing signer, fails the change or the `Verify` | off |
| `audit.WithSource()`, `audit.WithSourceIP()` | Logs `records.SourceFrom(ctx)` with each entry (`Entry.Source`): channel and client, and the IP address only with `WithSourceIP`. With the chain on the source is hashed | off |
| `audit.WithEntryDetail(fn)` | Adds `fn(ctx, event)`'s fields to each entry's `Detail` (a request ID, a user agent, "on behalf of"); the event's own keys win a collision, and with the chain on the fields are hashed | nothing added |
| `audit.WithRetentionFloor(d)` | `Prune` and `PruneScope` refuse (`invalid_argument`) a cutoff later than the Store's clock minus `d` | none |
| `audit.WithActorFunc(fn)` | Reads the actor with `fn(ctx)` for this plugin alone, for apps that keep the user under their own context key | the Store's actor, `Store.Actor(ctx)` (`records.WithActorFunc`, else `records.ActorFrom`) |

## Table

`fs_audit` (migrations `audit/0001_audit` and `audit/0002_audit_subject_detail`; the second adds `subject` and `detail` with `records.DDL`'s `AddColumn`, so it can rerun after a crash, and on MySQL adds `detail` nullable, back-fills `""` and then makes it `NOT NULL`, since MySQL gives a TEXT column no literal default):

| Column | Notes |
|---|---|
| `id` | primary key, `aud_` + the write sequence (16 hex digits) + 26 random base32 characters; entries written before the sequence existed have only the random part |
| `scope` | the event's scope |
| `at` | when the change was made, UTC, microsecond precision |
| `actor` | up to 191 bytes, `""` when unknown |
| `kind` | a `records.EventKind` |
| `file_id`, `attachment_id`, `owner_type`, `owner_id`, `role` | `""` when the kind does not set them |
| `subject` | a plugin event's `Subject` (a folder, a share link), `""` otherwise |
| `detail` | a plugin event's `Detail` as a JSON object, `""` when none; reads back as `nil` |
| `seq`, `prev_hash`, `hash` | the entry's place in its scope's chain and its HMAC chain links (`audit/0003_audit_chain`); `0`, `""`, `""` without `WithHashChain` |
| `key_id`, `source_channel`, `source_client`, `source_ip` | the chain key's ID and the change's source (`audit/0004_audit_keys_source`, which also adds `key_id` to `fs_audit_chain`); `""` without `WithHashChainKeyring` or `WithSource` |

`fs_audit_chain` (`audit/0003_audit_chain`): one row per chained scope, `scope` (primary key), `seq` and `head` (the last entry), `anchor_seq`, `anchor_hash` and `anchor_at` (the last entry `Prune` deleted), `since` (when the chain began), `mac` (HMAC of the row under the chain key).

Indexes: `fs_audit_scope (scope, at, id)`, `fs_audit_file (scope, file_id, at, id)`, `fs_audit_actor (scope, actor, at, id)`. On MySQL they are declared inside `CREATE TABLE`. Typed columns for queries of your own: `audit.Table`, `audit.ID`, `audit.Scope`, `audit.At`, `audit.Actor`, `audit.Kind`, `audit.FileID`, `audit.AttachmentID`, `audit.OwnerType`, `audit.OwnerID`, `audit.Role`, `audit.Subject`, `audit.Detail`.

## Go API

| Function or method | What it does |
|---|---|
| `audit.New(opts...)` | Returns the plugin |
| `audit.WithActor(ctx, actor)` | `records.WithActor`: the actor of the changes made with `ctx`, also stored as `File.UploadedBy` |
| `audit.ActorFrom(ctx)` | `records.ActorFrom`: the actor set with `WithActor`, or `""` |
| [`(*Plugin).Entries(ctx, store, audit.Query{Scope, FileID, Actor, Limit, Cursor})`](ds:block?id=history-23qsz95p) | One page of a scope's entries, newest first, keyset-paginated on `(at, id)`. `FileID` keeps one file's history, `Actor` one actor's activity; both may be combined. `Limit` 0 means `DefaultPageSize` (50); above `MaxPageSize` (1000), negative, or a bad cursor is `invalid_argument` |
| [`(*Plugin).Prune(ctx, store, cutoff)`](ds:block?id=prune-vhnhy5ef) | Deletes the entries of every scope older than `cutoff` and returns how many. Schedule it; the library starts no background work |
| `(*Plugin).PruneScope(ctx, store, scope, cutoff)` | Deletes the entries of one scope older than `cutoff` and returns how many: retention per tenant, or with a cutoff after the last entry, the whole log of an offboarded tenant. Joins the caller's transaction when `ctx` carries one |
| `(*Plugin).Verify(ctx, store, scope)` | Checks the scope's chain from its anchor to its head and returns how many entries it checked; an edited, deleted, reordered or inserted entry, a rewritten chain row (MAC), or with `WithRetentionFloor` a prune inside the floor, is `conflict` wrapping `audit.ErrChainBroken` |
| `(*Plugin).Changes(ctx, store, ChangeQuery{Scope, Cursor, Limit})` | The change feed: entries after `Cursor` in commit order, reads left out; `ChangePage.NextCursor` is stable across restarts; a cursor before the pruned anchor is `not_found`; needs `WithHashChain` |
| `(*Plugin).OnServed` | The `records.ServeHooks` implementation behind `WithAccessLog` |
| `(*Plugin).OnEvent`, `Migrations`, `Name` | The `records.EventHooks`, `records.Migrator` and `records.Plugin` implementations the Store calls |

| Type | Fields |
|---|---|
| `audit.Entry` | `ID`, `Actor`, `Seq`, `KeyID`, `Source`, and the embedded `records.Event` (`Kind`, `Scope`, `At`, `FileID`, `AttachmentID`, `Owner`, `Role`, `Subject`, `Detail`) |
| `audit.Page` | `Entries`, `NextCursor` (`""` on the last page) |

| Constant | Value |
|---|---|
| `PluginName` | `"audit"` |
| `TableNameAudit` | `"fs_audit"` |
| `IDPrefix` | `"aud_"` |
| `MaxActorBytes` | 191 |
| `DefaultPageSize`, `MaxPageSize` | 50, 1000 |
