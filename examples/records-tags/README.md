# Tagging files

The tags plugin of the records layer: free-form labels on files, normalized so `Invoice`, ` invoice ` and `INVOICE` are one tag, with listings by tag, a tag cloud, and scope-wide renames and deletes. This is the labelling a document manager, a media library or an invoice archive offers. It uses `github.com/ubgo/filesystem/records` and `records/tags` on SQLite over an in-memory disk. The guide [Tags](../../docs/plugins/tags.md) explains each step.

## Run it

```sh
cd examples
go run ./records-tags
go test ./records-tags
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-tags
```

```text
jan.txt: [2026 invoice]
feb.txt: [invoice paid]
shop.txt: [paid receipt]
invoice+paid: [feb.txt]; invoices under 20 bytes: [feb.txt]
cloud with shop.txt trashed: invoice(2) 2026(1) paid(1)
cloud after renaming paid to invoice and deleting 2026: invoice(2)
```

1. `Set` gives `jan.txt` the tags `Invoice`, ` 2026 ` and `invoice`; the default normalization trims and lowercases, so it stores two. `Add` and then `Remove` (of `DRAFT` and a tag it never had) leave `feb.txt` with `invoice` and `paid`. Each call returns the file's tags as stored, sorted.
2. Files carrying every one of several tags, and the tag filter combined with a filter of the app's own (smaller than 20 bytes) in one query.
3. The tag cloud counts live files only, so trashing `shop.txt` drops its tags from the counts.
4. Renaming `paid` to `invoice` merges them, and deleting `2026` removes it from every file in the scope.

## The code

Installing the plugin; `Migrate` creates `fs_file_tags`:

<!-- ds:block id=newstore-uc78dg7p -->
**newStore** · [`examples/records-tags/main.go:70-79`](main.go#L70-L79)

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

A custom normalization for apps that want URL-safe tags, ending with `tags.Check` to refuse what cannot be stored:

<!-- ds:block id=slug-rvcmc6am -->
**slug** · [`examples/records-tags/main.go:85-87`](main.go#L85-L87)

```go
func slug(tag string) (string, error) {
	return tags.Check(strings.Join(strings.Fields(strings.ToLower(tag)), "-"))
}
```
<!-- /ds:block hash=fdb121 -->

<!-- ds:block id=newslugplugin-pqknt7r8 -->
**newSlugPlugin** · [`examples/records-tags/main.go:91`](main.go#L91-L91)

```go
func newSlugPlugin() *tags.Plugin { return tags.New(tags.WithNormalize(slug)) }
```
<!-- /ds:block hash=9e13b8 -->

Setting, adding and removing tags:

<!-- ds:block id=tagfiles-7nhadn2v -->
**tagFiles** · [`examples/records-tags/main.go:101-113`](main.go#L101-L113)

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

Listing by tag, alone and combined with another filter, with the Store's order and pagination:

<!-- ds:block id=filestagged-vmpsqbet -->
**filesTagged** · [`examples/records-tags/main.go:118-121`](main.go#L118-L121)

```go
func filesTagged(ctx context.Context, s *records.Store, tp *tags.Plugin, want ...string) ([]string, error) {
	page, err := tp.Files(ctx, s, records.FileQuery{Scope: tenant}, want...)
	return filenames(page), err
}
```
<!-- /ds:block hash=b98f84 -->

<!-- ds:block id=smallinvoices-q3rq7yyy -->
**smallInvoices** · [`examples/records-tags/main.go:126-136`](main.go#L126-L136)

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

The cloud, and the scope-wide rename and delete:

<!-- ds:block id=cloud-85yyz6vr -->
**cloud** · [`examples/records-tags/main.go:150-160`](main.go#L150-L160)

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

<!-- ds:block id=demoqueries-4yrh6m7s -->
**demoQueries** · [`examples/records-tags/main.go:207-236`](main.go#L207-L236)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the plugin, the slug normalization, tagging, listings, the cloud, rename and delete |
| `main_test.go` | Runs the same steps, plus the slug normalization, the tag limits, and a purge removing a file's tags |

## Next

- The guide: [Tags](../../docs/plugins/tags.md).
- Related examples: [records-folders](../records-folders) for a folder tree, [records-files](../records-files) for listings and filters.
