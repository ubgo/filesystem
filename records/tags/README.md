# github.com/ubgo/filesystem/records/tags

Free-form tags on the files of a records Store: per scope, normalized before they are stored, with listings by tag, a filter that combines with any other condition, and a tag cloud with counts. The guide is [docs/plugins/tags.md](../../docs/plugins/tags.md).

## Install

```sh
go get github.com/ubgo/filesystem/records
```

The package is in the records module, which requires only `github.com/ubgo/sqlb` and the core module.

## Wire it

<!-- ds:block id=newstore-uc78dg7p -->
**newStore** · [`examples/records-tags/main.go:70-79`](../../examples/records-tags/main.go#L70-L79)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager, tp *tags.Plugin) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(tp))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=07a2f2 -->

<!-- ds:block id=tagfiles-7nhadn2v -->
**tagFiles** · [`examples/records-tags/main.go:101-113`](../../examples/records-tags/main.go#L101-L113)

```go
func tagFiles(ctx context.Context, s *records.Store, tp *tags.Plugin, a, b, c records.File) error {
	if _, err := tp.Set(ctx, s, tenant, a.ID, "Invoice", " 2026 ", "invoice"); err != nil {
		return err
	}
	if _, err := tp.Add(ctx, s, tenant, b.ID, "invoice", "Paid", "draft"); err != nil {
		return err
	}
	if _, err := tp.Remove(ctx, s, tenant, b.ID, "DRAFT", "never-there"); err != nil {
		return err
	}
	_, err := tp.Set(ctx, s, tenant, c.ID, "paid", "receipt")
	return err
}
```
<!-- /ds:block hash=7b9e87 -->

<!-- ds:block id=filestagged-vmpsqbet -->
**filesTagged** · [`examples/records-tags/main.go:118-121`](../../examples/records-tags/main.go#L118-L121)

```go
func filesTagged(ctx context.Context, s *records.Store, tp *tags.Plugin, want ...string) ([]string, error) {
	page, err := tp.Files(ctx, s, records.FileQuery{Scope: tenant}, want...)
	return filenames(page), err
}
```
<!-- /ds:block hash=b98f84 -->

`Migrate` creates `fs_file_tags`.

The plugin depends on no other plugin and serves no route. Every method takes the Store explicitly and joins the transaction in `ctx` when there is one.

## Rules

- Every tag passes through the plugin's `Normalize` before it is stored or compared. `DefaultNormalize` trims spaces and lowercases, then applies `Check`: 1 to `MaxTagChars` (64) characters of valid UTF-8 without control characters.
- A file carries at most `MaxPerFile` (64) tags.
- `Set`, `Add` and `Remove` write only the difference, in one transaction, and return the file's tags as stored, sorted.
- `Counts` counts live files only; trashed files keep their tags but do not count until restored.
- A purged file's tags go with it (`BeforePurge`).
- `records.Store.PurgeScope` deletes any tag row of the scope left over (`ScopePurger`).

## Events

Every change emits a `records.Event` through `Store.Emit` inside its transaction, with tags as stored (a call that changes nothing emits nothing; a hook's error rolls the change back). `EventKindValues` lists them.

| Kind | Method | Fields |
|---|---|---|
| `EventAdded` (`tags.added`), `EventRemoved` (`tags.removed`) | `Set`, `Add`, `Remove` | `FileID`; `Detail` `DetailTag`; one event per tag |
| `EventRenamed` (`tags.renamed`) | `Rename` | `Detail` `DetailTag` (new), `DetailFrom` (old) |
| `EventDeleted` (`tags.deleted`) | `Delete` | `Detail` `DetailTag` |

## Options

| Option | What it does | Default |
|---|---|---|
| `tags.WithNormalize(fn)` | Replaces the normalization: case-sensitive tags, slugs, a fixed vocabulary. End `fn` with `tags.Check` | `tags.DefaultNormalize` |

## Table

`fs_file_tags` (migration `tags/0001_file_tags`): columns `scope`, `file_id`, `tag`; primary key `(file_id, tag)`; index `fs_file_tags_tag (scope, tag, file_id)`. Typed columns for queries of your own: `tags.Table`, `tags.Scope`, `tags.FileID`, `tags.Tag`.

## Go API

| Function or method | What it does | Errors |
|---|---|---|
| `tags.New(opts...)` | Returns the plugin | |
| `tags.DefaultNormalize(tag)` | Trims, lowercases, then `Check` | `invalid_argument` |
| [`tags.Check(tag)`](ds:block?id=slug-rvcmc6am) | Refuses a tag that cannot be stored; returns it unchanged otherwise | `invalid_argument` |
| `(*Plugin).Tags(ctx, store, scope, fileID)` | The file's tags, sorted | `not_found` |
| [`(*Plugin).Set(ctx, store, scope, fileID, tags...)`](ds:block?id=tagfiles-7nhadn2v) | Replaces the file's tags; none clears them | `not_found`, `invalid_argument` |
| `(*Plugin).Add(ctx, store, scope, fileID, tags...)` | Adds tags; ones already there are kept once | `not_found`, `invalid_argument` |
| `(*Plugin).Remove(ctx, store, scope, fileID, tags...)` | Removes tags; unknown ones are ignored | `not_found`, `invalid_argument` |
| [`(*Plugin).Filter(store, scope, tags...)`](ds:block?id=smallinvoices-q3rq7yyy) | A `sqlb.Predicate` on `records.TableFiles` keeping the files that carry every one of `tags`, for `records.FileQuery.Filters` or a query of your own | `invalid_argument` (no tags, bad tag) |
| [`(*Plugin).Files(ctx, store, fq, tags...)`](ds:block?id=filestagged-vmpsqbet) | The files of `fq.Scope` carrying every one of `tags`, with the order, pagination and trash filter of `records.Store.Files` | `invalid_argument` |
| [`(*Plugin).Counts(ctx, store, scope)`](ds:block?id=cloud-85yyz6vr) | Every tag of the scope with its number of live files, most used first, then by tag | |
| `(*Plugin).Rename(ctx, store, scope, from, to)` | Renames a tag on every file of the scope; a file with both keeps one | `invalid_argument` |
| `(*Plugin).Delete(ctx, store, scope, tag)` | Removes a tag from every file of the scope; a tag no file carries does nothing | `invalid_argument` |
| `BeforeCreate`, `AfterCreate`, `BeforePurge`, `Migrations`, `Name` | The `records.FileHooks`, `records.Migrator` and `records.Plugin` implementations the Store calls | |

| Type | Meaning |
|---|---|
| `tags.Normalize` | `func(string) (string, error)`: a tag as typed to the tag as stored, or a refusal |
| `tags.Count` | `Tag` and `Files`, the number of live files carrying it |

| Constant | Value |
|---|---|
| `PluginName` | `"tags"` |
| `TableNameTags` | `"fs_file_tags"` |
| `MaxTagChars` | 64 |
| `MaxPerFile` | 64 |
