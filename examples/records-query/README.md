# Querying and bulk changes

The query and bulk side of the records layer, as a media library or a file manager uses it: a post's gallery capped at three photos, a post's PDFs listed like any other listing, the gallery a page at a time, the counts of a sidebar (by type, by visibility, by folder), a selection trashed with one result per file, and the two storage audits that look for objects without rows and rows without objects. It uses `github.com/ubgo/filesystem/records` and the `records/folders` plugin on SQLite over an in-memory disk. The guides [Files](../../docs/records/files.md), [Attachments](../../docs/records/attachments.md) and [Lifecycle](../../docs/records/lifecycle.md) explain each call.

## Run it

```sh
cd examples
go run ./records-query
go test ./records-query
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```text
attach 4 photos to a gallery of 3: quota_exceeded
the post's PDFs: 1 (docs/report.pdf)
gallery pages: [[photos/1.png photos/2.png] [photos/3.png]]
content_type: image/png 4 (84 B), application/pdf 2 (29 B)
visibility: private 5 (92 B), public 1 (21 B)
folder: (root) 4 (71 B), Trip 2 (42 B)
trash 3 selected: ok, not_found, ok
orphans: [tmp/left-behind.bin]; rows without an object: [docs/other.pdf]
```

1. The gallery role holds at most three files per post (`WithRoleLimit`), so the fourth photo is refused with `quota_exceeded`.
2. `AttachedTo` is a filter for `FileQuery`: the post's files in any role, narrowed to PDFs, newest first. The other PDF is not attached to the post and is left out.
3. `AttachmentsPage` walks the gallery two at a time in position order, following `NextCursor`.
4. `Facets` counts the tenant's live files by content type and by visibility, with their bytes; the folders plugin's `FolderFacet` counts them by folder.
5. `TrashMany` trashes a selection of three IDs, each in its own transaction: the unknown one fails alone with `not_found`.
6. `FindOrphans` lists an object no row names, deleting nothing; `MissingObjects` lists a row whose object is gone, changing nothing.

## The code

A Store with a capped gallery role and the folders plugin:

<!-- ds:block id=newquerystore-a3w0yj -->
**newStore** · [`examples/records-query/main.go:55-76`](main.go#L55-L76)

```go
func newStore(ctx context.Context, db *sql.DB, fp *folders.Plugin) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithOwnerTypes(ownerPost),
		records.WithRoleLimit(roleGallery, galleryMax),
		records.WithPlugins(fp),
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=2595bd -->

Attaching to a capped role; the first refusal is `quota_exceeded`:

<!-- ds:block id=attachphotos-y5k2hb -->
**attachPhotos** · [`examples/records-query/main.go:93-100`](main.go#L93-L100)

```go
func attachPhotos(ctx context.Context, s *records.Store, post records.Owner, photos []records.File) error {
	for _, f := range photos {
		if _, err := s.Attach(ctx, tenant, f.ID, post, roleGallery); err != nil {
			return fmt.Errorf("photo %s: %w", f.Key, err)
		}
	}
	return nil
}
```
<!-- /ds:block hash=c1c9fa -->

An owner's files as a filter, combined with a condition of the app's own:

<!-- ds:block id=postpdfs-c8m1qe -->
**postPDFs** · [`examples/records-query/main.go:105-114`](main.go#L105-L114)

```go
func postPDFs(ctx context.Context, s *records.Store, post records.Owner) (records.FilePage, error) {
	attached, err := s.AttachedTo(tenant, post, "")
	if err != nil {
		return records.FilePage{}, err
	}
	return s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{attached, sqlb.Eq(records.FileContentType, "application/pdf")},
	})
}
```
<!-- /ds:block hash=f052c0 -->

An owner's attachments a page at a time:

<!-- ds:block id=gallerypages-q7d5vz -->
**galleryPages** · [`examples/records-query/main.go:118-138`](main.go#L118-L138)

```go
func galleryPages(ctx context.Context, s *records.Store, post records.Owner) ([][]string, error) {
	var pages [][]string
	cursor := ""
	for {
		page, err := s.AttachmentsPage(ctx, records.AttachmentQuery{
			Scope: tenant, Owner: post, Role: roleGallery, Limit: pageSize, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, af := range page.Files {
			keys = append(keys, af.File.Key)
		}
		pages = append(pages, keys)
		if page.NextCursor == "" {
			return pages, nil
		}
		cursor = page.NextCursor
	}
}
```
<!-- /ds:block hash=d131f2 -->

The sidebar counts:

<!-- ds:block id=sidebar-n4r8tk -->
**sidebar** · [`examples/records-query/main.go:143-163`](main.go#L143-L163)

```go
func sidebar(ctx context.Context, s *records.Store, fp *folders.Plugin) (string, error) {
	fq := records.FileQuery{Scope: tenant}
	var lines []string
	for _, facet := range records.FacetValues {
		counts, err := s.Facets(ctx, fq, facet)
		if err != nil {
			return "", err
		}
		lines = append(lines, string(facet)+": "+render(counts, nil))
	}
	byFolder, err := fp.FolderFacet(ctx, s, fq)
	if err != nil {
		return "", err
	}
	names, err := folderNames(ctx, s, fp)
	if err != nil {
		return "", err
	}
	lines = append(lines, "folder: "+render(byFolder, names))
	return strings.Join(lines, "\n"), nil
}
```
<!-- /ds:block hash=c237ab -->

A bulk change with per-file results:

<!-- ds:block id=trashselection-h2p6xw -->
**trashSelection** · [`examples/records-query/main.go:194-208`](main.go#L194-L208)

```go
func trashSelection(ctx context.Context, s *records.Store, ids []string) (string, error) {
	results, err := s.TrashMany(ctx, tenant, ids)
	if err != nil {
		return "", err
	}
	var out []string
	for _, r := range results {
		status := "ok"
		if r.Err != nil {
			status = string(filesystem.CodeOf(r.Err))
		}
		out = append(out, status)
	}
	return strings.Join(out, ", "), nil
}
```
<!-- /ds:block hash=43c400 -->

The read-only storage audits:

<!-- ds:block id=storageaudit-u9f3ld -->
**storageAudit** · [`examples/records-query/main.go:213-224`](main.go#L213-L224)

```go
func storageAudit(ctx context.Context, s *records.Store) (orphans, missing []string, err error) {
	_, err = s.FindOrphans(ctx, diskName, "", time.Now().Add(time.Minute), func(key string) {
		orphans = append(orphans, key)
	})
	if err != nil {
		return nil, nil, err
	}
	_, err = s.MissingObjects(ctx, diskName, func(f records.File) {
		missing = append(missing, f.Key)
	})
	return orphans, missing, err
}
```
<!-- /ds:block hash=8fa5b5 -->

## Files

- `main.go`: the demo.
- `main_test.go`: runs it and checks each step's output.

## Next

- [Files](../../docs/records/files.md): `AttachedTo`, `Facets`.
- [Attachments](../../docs/records/attachments.md): `AttachmentsPage`, `WithRoleLimit`.
- [Lifecycle](../../docs/records/lifecycle.md): bulk changes, `FindOrphans`, `MissingObjects`.
