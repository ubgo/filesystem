# Tags

You will have free-form labels on files: set, add and remove a file's tags, list the files carrying a tag (alone or combined with any other condition), show a tag cloud with counts, and rename or delete a tag across a whole scope. It is the labelling of media libraries, document managers and asset managers. Install it when users want to find files across folders and owners by what they are about.

## Before you start

- A Store, as in [Files](../records/files.md). The plugin needs no other plugin.
- The example is [`examples/records-tags`](../../examples/records-tags): `go run ./records-tags` in `examples/` prints every step below; `go test ./records-tags` runs them.
- Tags belong to a scope: the same word in two scopes is two unrelated tags, and every query is limited to one scope. There is no separate tag table: a tag exists while at least one file carries it.

## Step 1 — Install the plugin

Pass `tags.New()` to `records.WithPlugins`. `Migrate` creates `fs_file_tags`, one row per tag of a file.

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

## Step 2 — Tag files

Three methods change a file's tags, each in one transaction that writes only the difference, and each returns the file's tags as stored, sorted:

- `Set(ctx, store, scope, fileID, tags...)` replaces them; no tags clears them.
- `Add(…, tags...)` adds tags; ones the file already has are kept once.
- `Remove(…, tags...)` removes tags; ones it does not have are ignored.

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

`Tags(ctx, store, scope, fileID)` reads them back, sorted. Check: the program prints

```
jan.txt: [2026 invoice]
feb.txt: [invoice paid]
shop.txt: [paid receipt]
```

`"Invoice"`, `" 2026 "` and `"invoice"` became two tags: the default normalization (`tags.DefaultNormalize`) trims spaces and lowercases before storing, and duplicates collapse. `Remove("DRAFT")` removed `draft` for the same reason. All three methods work on trashed files too; an unknown file is `not_found`.

## Step 3 — Choose how tags are normalized

`DefaultNormalize` trims and lowercases, then applies `tags.Check`, which refuses a tag that is empty, longer than `tags.MaxTagChars` (64 characters), not valid UTF-8, or holds control characters. An app that wants case-sensitive tags, slugs or a fixed vocabulary passes its own `tags.Normalize` with `WithNormalize`, and ends it with `tags.Check`:

<!-- ds:block id=slug-rvcmc6am -->
**slug** · [`examples/records-tags/main.go:85-87`](../../examples/records-tags/main.go#L85-L87)

```go
func slug(tag string) (string, error) {
	return tags.Check(strings.Join(strings.Fields(strings.ToLower(tag)), "-"))
}
```
<!-- /ds:block hash=fdb121 -->

<!-- ds:block id=newslugplugin-pqknt7r8 -->
**newSlugPlugin** · [`examples/records-tags/main.go:91`](../../examples/records-tags/main.go#L91-L91)

```go
func newSlugPlugin() *tags.Plugin { return tags.New(tags.WithNormalize(slug)) }
```
<!-- /ds:block hash=9e13b8 -->

Check: with this plugin, `Set(…, "  Annual  Report ")` stores `annual-report`. Every method normalizes its input the same way, including `Filter`, `Files`, `Rename` and `Delete`, so a user can search for `Annual Report` and find `annual-report`. Choose the normalization before users start tagging: changing it later does not rewrite stored tags.

A file carries at most `tags.MaxPerFile` (64) tags; a change that would pass it is `invalid_argument` and changes nothing.

## Step 4 — List files by tag

`Files(ctx, store, fq, tags...)` lists the files of `fq.Scope` that carry every one of the tags, with the order, pagination and trash filter of `records.Store.Files`:

<!-- ds:block id=filestagged-vmpsqbet -->
**filesTagged** · [`examples/records-tags/main.go:118-121`](../../examples/records-tags/main.go#L118-L121)

```go
func filesTagged(ctx context.Context, s *records.Store, tp *tags.Plugin, want ...string) ([]string, error) {
	page, err := tp.Files(ctx, s, records.FileQuery{Scope: tenant}, want...)
	return filenames(page), err
}
```
<!-- /ds:block hash=b98f84 -->

When the tag is one condition among several, take the condition itself with `Filter(store, scope, tags...)`: it returns a `sqlb.Predicate` on the files table that goes into `records.FileQuery.Filters` next to conditions of your own, or into a query you build with sqlb:

<!-- ds:block id=smallinvoices-q3rq7yyy -->
**smallInvoices** · [`examples/records-tags/main.go:126-136`](../../examples/records-tags/main.go#L126-L136)

```go
func smallInvoices(ctx context.Context, s *records.Store, tp *tags.Plugin) ([]string, error) {
	tagged, err := tp.Filter(s, tenant, "invoice")
	if err != nil {
		return nil, err
	}
	page, err := s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{tagged, sqlb.Less(records.FileSize, int64(smallFile))},
	})
	return filenames(page), err
}
```
<!-- /ds:block hash=02275a -->

Check: `invoice+paid: [feb.txt]; invoices under 20 bytes: [feb.txt]` (`jan.txt` is an invoice too, but larger). `Filter` with no tags is `invalid_argument`. A cursor is valid only with the filters it was issued for: keep the same tags and conditions while paging.

## Step 5 — A tag cloud

`Counts(ctx, store, scope)` returns every tag of the scope with the number of live files carrying it, most used first, then alphabetically. Files in the trash do not count.

<!-- ds:block id=cloud-85yyz6vr -->
**cloud** · [`examples/records-tags/main.go:150-160`](../../examples/records-tags/main.go#L150-L160)

```go
func cloud(ctx context.Context, s *records.Store, tp *tags.Plugin) (string, error) {
	counts, err := tp.Counts(ctx, s, tenant)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s(%d)", c.Tag, c.Files))
	}
	return strings.Join(parts, " "), nil
}
```
<!-- /ds:block hash=1a00eb -->

## Step 6 — Rename or delete a tag everywhere

`Rename(ctx, store, scope, from, to)` renames a tag on every file of the scope, in one transaction. A file that already carries the new tag keeps it once, so renaming merges two tags. `Delete(ctx, store, scope, tag)` removes a tag from every file. Both normalize their arguments; renaming a tag to itself does nothing.

<!-- ds:block id=demoqueries-4yrh6m7s -->
**demoQueries** · [`examples/records-tags/main.go:207-236`](../../examples/records-tags/main.go#L207-L236)

```go
func demoQueries(ctx context.Context, w io.Writer, s *records.Store, tp *tags.Plugin, files []records.File) error {
	both, err := filesTagged(ctx, s, tp, "Invoice", "paid")
	if err != nil {
		return err
	}
	small, err := smallInvoices(ctx, s, tp)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "invoice+paid: %v; invoices under %d bytes: %v\n", both, smallFile, small)
	if err := s.Trash(ctx, tenant, files[2].ID); err != nil {
		return err
	}
	c, err := cloud(ctx, s, tp)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "cloud with shop.txt trashed:", c)
	if err := tp.Rename(ctx, s, tenant, "paid", "invoice"); err != nil {
		return err
	}
	if err := tp.Delete(ctx, s, tenant, "2026"); err != nil {
		return err
	}
	if c, err = cloud(ctx, s, tp); err != nil {
		return err
	}
	fmt.Fprintln(w, "cloud after renaming paid to invoice and deleting 2026:", c)
	return nil
}
```
<!-- /ds:block hash=8db2da -->

Check: the program prints

```
cloud with shop.txt trashed: invoice(2) 2026(1) paid(1)
cloud after renaming paid to invoice and deleting 2026: invoice(2)
```

`paid` counted 1, not 2, because `shop.txt` is in the trash. After the rename, `feb.txt` carries `invoice` once, and the trashed `shop.txt` carries `invoice` and `receipt`, neither of which the cloud shows until it is restored.

## The table

`fs_file_tags`, created by the migration `tags/0001_file_tags`:

| Column | Holds |
|---|---|
| `scope` | the file's scope |
| `file_id` | the file |
| `tag` | the normalized tag, up to 64 characters |

Primary key `(file_id, tag)`; index `fs_file_tags_tag (scope, tag, file_id)` for listings by tag and counts. On MySQL the table uses `utf8mb4_bin`, so with a case-sensitive normalization `Tag` and `tag` stay two tags.

## With other plugins and the core

- Purge deletes the file's tags (`BeforePurge`), in the purge transaction. Trash keeps them; they come back with `Restore`, and `Counts` ignores them meanwhile.
- `Replace`, `SetVisibility` and versions keep the file's ID, so its tags stay.
- Every change emits a `records.Event` inside its transaction, through `Store.Emit`: `tags.added` and `tags.removed` once per tag with the file's `FileID`, and the scope-wide `tags.renamed` and `tags.deleted`. `Detail` holds the tag as stored under `tags.DetailTag` (and the old name under `DetailFrom` for a rename); see [Events and hooks](../records/events-and-hooks.md#events-from-plugins). A call that changes nothing emits nothing, and a hook's error rolls the change back. The [audit](audit.md) log writes one entry per event with its kind, file, actor, time, `Subject` and `Detail`.
- With [folders](folders.md): to list a folder's files that carry a tag, put both conditions in one `FileQuery`. `folders.Plugin.Files` adds its own condition to whatever `fq.Filters` you pass, so pass the tag filter to it: `fp.Files(ctx, store, folderID, records.FileQuery{Scope: scope, Filters: []sqlb.Predicate{tagged}})`.
- Call `Set`, `Add` or `Remove` inside a `store.Tx` with an upload to tag a file as it is created: if either fails, neither happens.

## What can go wrong

- `invalid_argument` — a tag that is empty after normalization, longer than 64 characters, invalid UTF-8 or with control characters; more than 64 tags on one file; `Filter` or `Files` with no tags.
- `not_found` — `Tags`, `Set`, `Add` or `Remove` on an unknown file, or one of another scope.
- A search that finds nothing although the tag is visible — your own query compared the raw input; normalize it the same way (call `Filter`, which does).

## Next

- [Folders](folders.md): the other way to organise files, and combining a folder with a tag.
- [Files](../records/files.md): `FileQuery`, filters and cursors.
- [Lifecycle](../records/lifecycle.md): why trashed files drop out of counts and come back on restore.
- The plugin's [README](../../records/tags/README.md) is the reference for its table and Go API.
