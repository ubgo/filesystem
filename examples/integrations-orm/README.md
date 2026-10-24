# Reading the records tables with sqlc, GORM and ent

This program reads the records tables (`fs_files`, `fs_attachments`) with the data layer an application already generates, instead of calling the Store row by row. A `records.Store` writes every row: three files uploaded and attached to a post's gallery, the middle one then trashed. Three readers then read the same rows over the same database and answer what the Store answers: the package sqlc generates from `github.com/ubgo/filesystem/contrib/sqlc`, the GORM models of `github.com/ubgo/filesystem/contrib/gorm`, and a client ent generates from the mixins of `github.com/ubgo/filesystem/contrib/ent`. The guides are [sqlc](../../docs/integrations/sqlc.md), [GORM](../../docs/integrations/gorm.md) and [ent](../../docs/integrations/ent.md).

## Run it

```sh
cd examples
go run ./integrations-orm
go test ./integrations-orm
```

Everything runs in-process: one memory disk and an in-memory SQLite database (`modernc.org/sqlite`, pure Go), which GORM reaches through the pure-Go `gormlite` dialector over the same `*sql.DB`. No Docker, no network, nothing to install: `fsdb/` and `ent/` are committed as sqlc and ent generated them.

## What it does

```text
sqlc gallery: 1 cover.txt 9 bytes
sqlc gallery: 3 map.txt 7 bytes
sqlc live files, 1 per page: map.txt, cover.txt
sqlc totals: 3 files, 25 bytes (trash included)
gorm gallery: 1 cover.txt 9 bytes
gorm gallery: 3 map.txt 7 bytes
gorm live file: map.txt, 1 attachment(s), records.File status ready
gorm live file: cover.txt, 1 attachment(s), records.File status ready
ent gallery: 1 cover.txt 9 bytes
ent gallery: 3 map.txt 7 bytes
ent delete refused: true
ent rows after the refused delete: 3
prefix: the Store writes app_fs_files
prefix: gorm reads 1 live file(s) from app_fs_files, the default namer 2 from fs_files
prefix: sqlc queries name app_fs_files: true, any fs_ name left: false
prefix: ent schema table app_fs_files
prefix: a mapping giving two names one table: invalid_argument
```

1. The Store migrates the tables, then uploads `cover.txt`, `draft.txt` and `map.txt` into scope `acme`, attaches each to post `p1` under the role `gallery` (positions 1, 2, 3) and trashes `draft.txt`.
2. `ListOwnerAttachments` returns the gallery in position order with each file's columns, leaving the trashed file out, as `Store.Attachments` does.
3. `ListLiveFiles` and `ListLiveFilesAfter` walk the live files one per page, newest first, with a `(created_at, id)` cursor, as `Store.Files` orders them.
4. `ScopeTotals` counts every file of the scope, the trashed one included: 9 + 9 + 7 bytes.
5. GORM lists the gallery with the `InScope`, `WithLiveFile` and `ByPosition` scopes, then the live files with `Live` and `NewestFirst` and their attachments preloaded, and converts each row back to a `records.File` with `Record`.
6. ent lists the gallery with `HasFileWith(file.DeletedAtIsNil())` and `WithFile`, then tries a generated delete: the mixins' hook refuses it with `fsent.ErrReadOnly`, and all three rows remain.
7. `prefix.go` opens a second Store with `records.WithTablePrefix("app_")` in the same database, so its tables (`app_fs_files`, its own ledger `app_fs_migrations`) sit beside the default ones; `Store.TableName` names them for SQL of your own. It uploads one file. GORM opened with `fsgorm.NewNamer(nil, names)` reads that file from `app_fs_files`, while the default namer still reads the two live files of `fs_files`. `fssqlc.QueriesWith` renders the queries to generate from with every name mapped, and `fsent.FileMixin{Names: names}` gives the ent schema the table `app_fs_files`. A mapping that gives every name one table is refused by `Migrate` with `invalid_argument`.

## The code

The Store writes; no generated query ever writes an `fs_` row.

<!-- ds:block id=seed-8tv7pj5w -->
**seed** · [`examples/integrations-orm/main.go:117-138`](main.go#L117-L138)

```go
func seed(ctx context.Context, store *records.Store) error {
	policy := upload.Policy{MaxSize: 1 << 10, Allowed: []string{"text/plain"}}
	post := records.Owner{Type: ownerPost, ID: postID}
	for _, name := range galleryFiles {
		f, err := store.Upload(ctx, tenant, diskName, strings.NewReader(name), name, policy)
		if err != nil {
			return err
		}
		if _, err := store.Attach(ctx, tenant, f.ID, post, roleGallery); err != nil {
			return err
		}
		if name == trashedName {
			if err := store.Trash(ctx, tenant, f.ID); err != nil {
				return err
			}
		}
		// SQLite stores timestamps to the microsecond; a pause keeps the
		// newest-first order of the listings deterministic.
		time.Sleep(time.Millisecond)
	}
	return nil
}
```
<!-- /ds:block hash=0534f3 -->

The gallery, one generated call filtered by scope first:

<!-- ds:block id=sqlcgallery-udcqpkzw -->
**sqlcGallery** · [`examples/integrations-orm/main.go:169-176`](main.go#L169-L176)

```go
func sqlcGallery(ctx context.Context, q *fsdb.Queries, scope, owner string) ([]fsdb.ListOwnerAttachmentsRow, error) {
	return q.ListOwnerAttachments(ctx, fsdb.ListOwnerAttachmentsParams{
		Scope:     scope,
		OwnerType: ownerPost,
		OwnerID:   owner,
		Role:      roleGallery,
	})
}
```
<!-- /ds:block hash=d082fe -->

Keyset pagination over the live files:

<!-- ds:block id=sqlcalllivefiles-tgujm898 -->
**sqlcAllLiveFiles** · [`examples/integrations-orm/main.go:183-199`](main.go#L183-L199)

```go
func sqlcAllLiveFiles(ctx context.Context, q *fsdb.Queries, scope string) ([]string, error) {
	page, err := q.ListLiveFiles(ctx, fsdb.ListLiveFilesParams{Scope: scope, PageSize: pageSize})
	var names []string
	for err == nil {
		for _, f := range page {
			names = append(names, f.Filename)
		}
		if len(page) < pageSize {
			return names, nil
		}
		last := page[len(page)-1]
		page, err = q.ListLiveFilesAfter(ctx, fsdb.ListLiveFilesAfterParams{
			Scope: scope, AfterCreatedAt: last.CreatedAt, AfterID: last.ID, PageSize: pageSize,
		})
	}
	return nil, err
}
```
<!-- /ds:block hash=d7ff3d -->

GORM, opened on the Store's `*sql.DB`, and its gallery:

<!-- ds:block id=opengorm-me6qbh5f -->
**openGorm** · [`examples/integrations-orm/gorm.go:21-23`](gorm.go#L21-L23)

```go
func openGorm(db *sql.DB) (*gorm.DB, error) {
	return gorm.Open(gormlite.OpenDB(db), &gorm.Config{Logger: logger.Discard})
}
```
<!-- /ds:block hash=847b46 -->

<!-- ds:block id=gormgallery-27wg6mmp -->
**gormGallery** · [`examples/integrations-orm/gorm.go:30-36`](gorm.go#L30-L36)

```go
func gormGallery(db *gorm.DB, scope, owner string) ([]fsgorm.Attachment, error) {
	var out []fsgorm.Attachment
	err := db.Scopes(fsgorm.InScope(scope), fsgorm.WithLiveFile, fsgorm.ByPosition).
		Where(&fsgorm.Attachment{OwnerType: ownerPost, OwnerID: owner, Role: roleGallery}).
		Preload("File").Find(&out).Error
	return out, err
}
```
<!-- /ds:block hash=045bec -->

The ent schemas, the two of the [ent guide](../../docs/integrations/ent.md), from which `ent/` was generated:

<!-- ds:block id=schemas-fjjwqp9j -->
**File** · [`examples/integrations-orm/ent/schema/schema.go:16-31`](ent/schema/schema.go#L16-L31)

```go
type File struct{ ent.Schema }

// Mixin implements ent.Interface.
func (File) Mixin() []ent.Mixin { return []ent.Mixin{fsent.FileMixin{}} }

// Edges implements ent.Interface.
func (File) Edges() []ent.Edge { return []ent.Edge{fsent.FileAttachments(Attachment.Type)} }

// Attachment reads fs_attachments, with its fields from
// fsent.AttachmentMixin for the same reason as File.
type Attachment struct{ ent.Schema }

// Mixin implements ent.Interface.
func (Attachment) Mixin() []ent.Mixin { return []ent.Mixin{fsent.AttachmentMixin{}} }

// Edges implements ent.Interface.
```
<!-- /ds:block hash=12d5d6 -->

And the ent gallery and the refused delete:

<!-- ds:block id=entgallery-2fun6t4a -->
**entGallery** · [`examples/integrations-orm/ent.go:31-41`](ent.go#L31-L41)

```go
func entGallery(ctx context.Context, client *ent.Client, scope, owner string) ([]*ent.Attachment, error) {
	return client.Attachment.Query().
		Where(
			attachment.Scope(scope),
			attachment.OwnerType(ownerPost), attachment.OwnerID(owner), attachment.Role(roleGallery),
			attachment.HasFileWith(file.DeletedAtIsNil()),
		).
		Order(ent.Asc(attachment.FieldPosition), ent.Asc(attachment.FieldID)).
		WithFile().
		All(ctx)
}
```
<!-- /ds:block hash=819074 -->

<!-- ds:block id=readwithent-qkkbw8hc -->
**readWithEnt** · [`examples/integrations-orm/ent.go:46-63`](ent.go#L46-L63)

```go
func readWithEnt(ctx context.Context, out io.Writer, db *sql.DB) error {
	client := openEnt(db)
	gallery, err := entGallery(ctx, client, tenant, postID)
	if err != nil {
		return err
	}
	for _, a := range gallery {
		fmt.Fprintf(out, "ent gallery: %d %s %d bytes\n", a.Position, a.Edges.File.Filename, a.Edges.File.Size)
	}
	_, err = client.File.Delete().Where(file.Scope(tenant)).Exec(ctx)
	fmt.Fprintf(out, "ent delete refused: %t\n", errors.Is(err, fsent.ErrReadOnly))
	n, err := client.File.Query().Where(file.Scope(tenant)).Count(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "ent rows after the refused delete: %d\n", n)
	return nil
}
```
<!-- /ds:block hash=4ffc69 -->

## Regenerating fsdb and ent

`fsdb/` is what sqlc v1.31.1 generates from `contrib/sqlc`'s `sqlite` directory (`sqlite/schema.sql`, `queries.sql`, `sqlite/pages.sql`, `sqlite/sqlc.yaml`). To regenerate it after the module's queries change, copy `contrib/sqlc/queries.sql` and the `contrib/sqlc/sqlite` directory side by side into a scratch directory, run `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate` inside its `sqlite` directory, and copy the resulting `sqlite/fsdb/*.go` here. `TestGeneratedCoversEveryQuery` fails while a query the module ships has no generated method.

`ent/` (everything but `ent/schema`) is what ent v0.14.6 generates from `ent/schema/schema.go`. ent's generator is pinned in `contrib/ent/go.mod` (a `tool` directive, with a `golang.org/x/tools` new enough for Go 1.26), not in this module, so regenerate from a scratch module that copies `contrib/ent/go.mod` and `go.sum` with the module path renamed to `github.com/ubgo/filesystem/examples` and absolute `replace` paths, holding `integrations-orm/ent/schema`; run `go tool entgo.io/ent/cmd/ent generate ./integrations-orm/ent/schema` there and copy the generated `integrations-orm/ent` back. This is what `contrib/ent`'s `TestGenerateBuildAndRead` does on every run.

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the Store and its rows, then the sqlc reads |
| `gorm.go` | The GORM reads: `openGorm`, `gormGallery`, `gormLiveFiles`, `readWithGorm` |
| `ent.go` | The ent reads: `openEnt`, `entGallery`, `readWithEnt` (the refused delete) |
| `prefix.go` | A Store with `WithTablePrefix` beside the default one, and the same names given to GORM, sqlc and ent; code: [`openPrefixedStore`](ds:block?id=openprefixedstore-g4t8w2nc), [`prefixedReaders`](ds:block?id=prefixedreaders-u6m3x9kd) |
| `ent/schema/schema.go` | The two ent schemas that mix in `contrib/ent` |
| `ent/` | The client ent generated (do not edit by hand) |
| `main_test.go` | Runs the program and checks every line; checks that `fsdb` covers every query of `contrib/sqlc` |
| `fsdb/` | The Go sqlc generated (do not edit by hand) |

## Next

- The guides: [sqlc](../../docs/integrations/sqlc.md), [GORM](../../docs/integrations/gorm.md), [ent](../../docs/integrations/ent.md), and the module reference [contrib/sqlc/README.md](../../contrib/sqlc/README.md).
- The module references: [contrib/gorm/README.md](../../contrib/gorm/README.md) and [contrib/ent/README.md](../../contrib/ent/README.md).
- [Attachments](../../docs/records/attachments.md): the Store side of what these queries read.
