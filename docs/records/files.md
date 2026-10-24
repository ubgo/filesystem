# Files

This page wires a `records.Store` and walks the file API: recording an object that is already stored, uploading and recording in one call, reading a file back, listing a scope page by page, narrowing a listing, and handing out links. Then it covers what a production app adds: opaque IDs, who uploaded a file and from where, describing text, storing identical uploads once, refusing to delete a file in use, and a copy of a file on its owner's row. Read it first if you are adding the records layer to an app; every other records page starts from the Store built here.

## Before you start

- The [records overview](README.md) explains what a Store is, the two consistency rules and the engines.
- The example is [`examples/records-files`](../../examples/records-files): SQLite and an in-memory disk, so it runs anywhere. `go run ./records-files` in `examples/` prints what each step returned; `go test ./records-files` runs the same steps.
- You need a `filesystem.Manager` with at least one disk. [Disks and drivers](../concepts/disks-and-drivers.md) covers building one; any driver works.

## Step 1 — Build the disks

The Store reaches bytes through a `filesystem.Manager` and names disks by their names in it. The demo has one memory disk, `media`, that signs its own URLs (so `URL` has something to return without a cloud provider) and builds public URLs under a CDN prefix.

<!-- ds:block id=newdisks-ke5p8pyb -->
**newDisks** · [`examples/records-files/main.go:62-73`](../../examples/records-files/main.go#L62-L73)

```go
func newDisks() (*filesystem.Manager, error) {
	signer, err := signedurl.New(signBase, []signedurl.Key{{ID: "k1", Secret: signingSecret}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(),
		filesystem.WithURLSigner(signer), filesystem.WithPublicURL(cdnBase))
	if err != nil {
		return nil, err
	}
	return filesystem.NewManager(disk)
}
```
<!-- /ds:block hash=8f4830 -->

On S3, GCS or Azure the driver presigns URLs itself and `WithURLSigner` is not needed; set one only to sign downloads at a CDN in front of the bucket (CloudFront, Cloudflare), and it then signs every download while uploads stay with the driver. [Serving files](../concepts/serving-files.md) explains signed and public URLs, and where `fshttp.Signed` is mounted for a disk that needs a signer.

## Step 2 — Open the database

You own the `*sql.DB` and its driver; the module imports none. For SQLite keep one connection: SQLite has one writer, and a second connection would only wait on the lock.

<!-- ds:block id=opensqlite-cjy2vqp7 -->
**openSQLite** · [`examples/records-files/main.go:78-85`](../../examples/records-files/main.go#L78-L85)

```go
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
```
<!-- /ds:block hash=d52e2b -->

Postgres and MySQL DSNs are in the [engines table](README.md#engines).

## Step 3 — Create the Store and its tables

`records.New(db, dialect, disks, options...)` checks its arguments and returns the Store; `Migrate` creates `fs_files`, `fs_attachments` and the `fs_migrations` ledger. Call `Migrate` on every start: it applies only what is new, under a lock, so several instances starting together are safe.

<!-- ds:block id=newstore-c3s67q5f -->
**newStore** · [`examples/records-files/main.go:90-99`](../../examples/records-files/main.go#L90-L99)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks)
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=a22ffb -->

Check: the first `Migrate` returns `[records/0001_files_and_attachments records/0002_files_uploaded_by records/0003_files_status]`, every later one returns an empty list. [Migrations](migrations.md) covers running them from a deploy step instead.

Two options matter mostly in tests: `records.WithClock(now)` replaces `time.Now` for the Store and every plugin that has no clock of its own, and `records.WithIDs(fn)` replaces the ID generator (for `github.com/ubgo/publicid` or UUIDs). The generator receives `records.IDPrefixFile` (`fil_`), `records.IDPrefixAttachment` (`att_`) or a plugin's prefix (plugins call `Store.NewID`), and must return a unique string starting with it, at most 191 bytes. A duplicate ID makes `CreateFile` fail with `exists`.

## Step 4 — Record an object that is already stored

Use `CreateFile` when the bytes arrived some other way: a worker wrote them, an import copied them, the browser uploaded them through a presigned URL. It is the object-first rule in code: the object must already exist on the disk.

<!-- ds:block id=recordexisting-x7uxukhd -->
**recordExisting** · [`examples/records-files/main.go:106-118`](../../examples/records-files/main.go#L106-L118)

```go
func recordExisting(ctx context.Context, s *records.Store, key string, body []byte) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.Write(ctx, key, bytes.NewReader(body), filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{
		Scope: tenant, Disk: diskName, Key: key, Filename: "Quarterly report.txt",
		Metadata: map[string]string{"source": "export-job"},
	})
}
```
<!-- /ds:block hash=cb9e61 -->

What `CreateFile` does with a `records.NewFile`:

| Field | Behaviour |
|---|---|
| `Scope` | Stored as given; every later read filters by it. |
| `Disk`, `Key` | The disk must be in the manager (else `not_found`); the key is cleaned and checked for traversal like every Disk key (else `invalid_key`). The object must exist (else `not_found`). |
| `Filename` | The display name, passed through `upload.SafeFilename` (base name only, no control characters, at most 255 bytes). Empty means the key's base name. Use it for client-supplied names. |
| `ContentType`, `Size`, `SHA256` | Default to what the disk reports for the object. `SHA256` is 64 hex digits in either case and is stored lowercase, so `FileBySHA256` finds it; anything else is `invalid_argument`. A digest the disk reports that is not hex SHA-256 is left empty rather than failing the call. |
| `Visibility` | Defaults to the object's visibility, then to the disk's default. An unknown value is `invalid_argument`. On a disk without per-object visibility, a value other than the disk's default is `unsupported`, so the row never claims a visibility the object does not have. |
| `Metadata` | A `map[string]string`, stored as JSON. |
| `Checksums` | Extra digests beside `SHA256`, by algorithm name (`{"md5": "…", "crc32c": "…"}`), as `upload.Result.Checksums` has them: names of 1 to 32 lowercase letters, digits or dashes, values hex of 1 to `records.MaxChecksumBytes` (64) bytes in either case, stored lowercase, at most `records.MaxChecksums` (16). Anything else is `invalid_argument`. Recorded as given: the Store does not read the object to check them. `File.Checksums` is nil for a file without any. |
| `UploadedBy` | Overrides the actor in the context (`records.WithActor`). Longer than `records.MaxActorBytes` (191) is `invalid_argument`. |

The insert runs in one transaction with the plugins' `BeforeCreate` and `AfterCreate` hooks and the `file.created` event, so a plugin that refuses the file (the [quota plugin](../plugins/quota.md) over its limit) leaves no row. `CreateFile` never deletes the object on failure: it did not write it.

Check: the demo prints `created fil_ "Quarterly report.txt" text/plain; charset=utf-8 19 bytes private`. Type, size and visibility came from the disk; a memory disk's default visibility is private.

## Step 5 — Upload and record in one call

`Upload(ctx, scope, diskName, body, filename, policy)` runs `upload.Receive` (size cap, type detected from the bytes, key chosen by the policy) and then `CreateFile`. The policy is the same `upload.Policy` the rest of the library uses; [Uploads](../concepts/uploads.md) explains every field. Digests `policy.Checksums` asks for (an MD5 for an existing checksum column, a CRC32C to compare with GCS) are computed while the bytes stream and land on `File.Checksums`; `Replace` records the new content's the same way, and a replacement without them leaves the file none, so they never describe bytes it no longer has.

```go
p := notesPolicy
p.Checksums = []filesystem.Checksum{{Name: "md5", New: md5.New}}
f, err := s.Upload(ctx, tenant, "media", body, "notes.txt", p)
// f.Checksums["md5"] is the hex MD5 of what was stored
```

<!-- ds:block id=notespolicy-4kaczpxx -->
**notesPolicy** · [`examples/records-files/main.go:122-126`](../../examples/records-files/main.go#L122-L126)

```go
var notesPolicy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"text/plain"},
	Key:     upload.RandomKey("notes"),
}
```
<!-- /ds:block hash=d6fc40 -->

<!-- ds:block id=uploadnote-z5zuwjmw -->
**uploadNote** · [`examples/records-files/main.go:131-134`](../../examples/records-files/main.go#L131-L134)

```go
func uploadNote(ctx context.Context, s *records.Store, filename string, body io.Reader) (records.File, error) {
	ctx = records.WithActor(ctx, "user:alice")
	return s.Upload(ctx, tenant, diskName, body, filename, notesPolicy)
}
```
<!-- /ds:block hash=9c04da -->

If the row cannot be inserted, `Upload` deletes the object it stored; if an enclosing `Tx` rolls back later, it deletes it then (through `OnRollback`). A failed upload leaves neither a row nor bytes. With `WithVisibilityDisks` an empty `diskName` picks the public or private disk by `policy.Visibility`; without it an empty name is `invalid_argument`.

Check: `uploaded "todo.txt" key=notes/… by user:alice`. The key is random with the detected extension; the client's name is only the display name. Uploading PNG bytes named `evil.txt` under a text-only policy prints `refused: rejected`: the type comes from the bytes, never from the name.

## Step 6 — Read one file

`File(ctx, scope, id)` returns the file, trashed or not (check `DeletedAt`), or `not_found`. The same ID in another scope is `not_found` too: a scope never sees another's files, even by guessing IDs.

`FileBySHA256(ctx, scope, sum)` finds a file by its content: the newest live file of the scope whose `SHA256` is `sum` (64 hex digits, either case), or `not_found`. It is the lookup for deduplication: hash the bytes (`upload.Result.SHA256` already has the digest), and when the scope holds them already, attach that file again instead of recording a copy. It reads the `(scope, sha256)` index. Trashed files are left out, and so is every other scope. A `sum` that is not 64 hex digits is `invalid_argument`, so `""` never matches the files recorded without a digest. To list every file with one digest, pass `sqlb.Eq(records.FileSHA256, sum)` (lowercase) as a filter (Step 8).

## Step 7 — List a scope, page by page

`Files(ctx, records.FileQuery{...})` lists a scope's files newest first, with keyset pagination on `(created_at, id)`: stable while files are added, and as fast on page 1,000 as on page 1, unlike `OFFSET`.

<!-- ds:block id=listall-hce32ebc -->
**listAll** · [`examples/records-files/main.go:139-155`](../../examples/records-files/main.go#L139-L155)

```go
func listAll(ctx context.Context, s *records.Store) ([]records.File, int, error) {
	var all []records.File
	pages := 0
	q := records.FileQuery{Scope: tenant, Limit: pageSize}
	for {
		page, err := s.Files(ctx, q)
		if err != nil {
			return nil, pages, err
		}
		pages++
		all = append(all, page.Files...)
		if page.NextCursor == "" {
			return all, pages, nil
		}
		q.Cursor = page.NextCursor
	}
}
```
<!-- /ds:block hash=2f30a3 -->

| `FileQuery` field | Meaning |
|---|---|
| `Scope` | Required in practice: the scope to list. |
| `Trashed` | `true` lists only trashed files; `false` (the default) only live ones. |
| `Limit` | Page size; 0 means `records.DefaultPageSize` (50). Above `records.MaxPageSize` (1000) or negative is `invalid_argument`. |
| `Cursor` | `FilePage.NextCursor` of the previous page; empty for the first. A malformed cursor is `invalid_argument`. |
| `Filters` | Extra `sqlb.Predicate`s on `fs_files` (`records.TableFiles`). |
| `Order` | `records.OrderNewest` (`""`, the default), `OrderOldest`, `OrderName`, `OrderNameDesc`, `OrderLargest` or `OrderSmallest` (`records.FileOrderValues`), or a name the Store added with `records.WithFileOrder`. Every order is a keyset on (key, id). A cursor works only with the order that issued it; another order's cursor is `invalid_argument`. Names sort in the database's collation. |

`FilePage.NextCursor` is empty on the last page. Cursors are opaque base64url strings; hand them to clients as they are.

Check: with a page size of 1 the demo prints `listed 2 files in 2 pages`.

A drive's sort menu is the same loop with `Order` set; the default order and its cursors are unchanged:

```go
q := records.FileQuery{Scope: tenant, Limit: 50, Order: records.OrderName} // A to Z, then by ID
for {
	page, err := s.Files(ctx, q)
	if err != nil {
		return err
	}
	show(page.Files)
	if page.NextCursor == "" {
		return nil
	}
	q.Cursor = page.NextCursor
}
```

The newest and oldest orders read the `(scope, created_at, id)` index; the name and size orders narrow by scope and let the database sort, which is fine for a tenant's thousands of files and worth an index of your own (on `(scope, filename, id)` or `(scope, size, id)`) for millions.

Names sort in the database's collation: byte order on SQLite, case-sensitive under most Postgres collations. For another sort, add an order of your own to the Store with `records.WithFileOrder(name, records.KeysetOrder(key, dir, parse, format))`: `key` is any expression over `fs_files` (`sqlb.Lower(records.FileFilename)`, a natural-sort column of your own, a `COLLATE` expression), and the cursor holds the last row's key as the database computed it, written with `format` and read back with `parse`. A case-insensitive name sort:

```go
same := func(v string) (string, error) { return v, nil }
s, err := records.New(db, dialect, mgr,
	records.WithFileOrder("name_ci", records.KeysetOrder(sqlb.Lower(records.FileFilename), sqlb.SortAsc, same, func(v string) string { return v })),
)
page, err := s.Files(ctx, records.FileQuery{Scope: tenant, Order: "name_ci"})
```

The name follows `[a-z][a-z0-9_]*` and is neither built in nor added twice, or `New` is `invalid_argument`. Index the expression in a migration of your own when listings are large.

Try it: [`examples/records-files`](../../examples/records-files) runs this section in `session.go` ([`listSorted`](ds:block?id=listsorted-q2v9n6tp)): `go run ./records-files` from `examples/`.

## Step 8 — Narrow a listing with filters

`Filters` add conditions on the files table while keeping the Store's order, pagination and trash filter. The typed columns (`records.FileSize`, `records.FileContentType`, `records.FileUploadedBy`, `records.FileCreatedAt`…) build them with `sqlb`. Plugins use the same hook: the [folders](../plugins/folders.md) and [tags](../plugins/tags.md) plugins list a folder's or a tag's files by adding a filter.

<!-- ds:block id=listbig-x8dnvzmv -->
**listBig** · [`examples/records-files/main.go:160-165`](../../examples/records-files/main.go#L160-L165)

```go
func listBig(ctx context.Context, s *records.Store) (records.FilePage, error) {
	return s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{sqlb.Greater(records.FileSize, int64(bigFile))},
	})
}
```
<!-- /ds:block hash=d5bb38 -->

A cursor is only valid with the filters it was issued for: change the filters and start again from the first page. Check: `files over 10 bytes: 1 ("Quarterly report.txt")`.

`records.EncodeCursor` and `records.DecodeCursor` are exported so a plugin with its own listing (an audit log, share links) paginates the same way.

## An owner's files as a filter

`AttachedTo(scope, owner, role)` returns the condition on `records.TableFiles` that keeps the files attached to an owner, under one role or, with role `""`, under any. Put it in `FileQuery.Filters` to list an owner's files with everything a listing has: newest-first order, keyset pages, the trash filter, and any other condition, your own or a plugin's (a folder, a tag). The owner type must be registered; a bad owner or role is `invalid_argument`. The [records-query example](../../examples/records-query) lists a post's PDFs in any role:

<!-- ds:block id=postpdfs-c8m1qe -->
**postPDFs** · [`examples/records-query/main.go:105-114`](../../examples/records-query/main.go#L105-L114)

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

To walk one role in its own order (positions) rather than newest first, use `AttachmentsPage` ([Attachments](attachments.md)).

## Facet counts

`Facets(ctx, fq, facet)` counts the files a `FileQuery` selects (its `Scope`, `Trashed` and `Filters`; `Limit` and `Cursor` must be unset, since a count has no pages) by one facet, with their bytes, most files first: the sidebar of a media library. The facets are `records.FacetContentType` and `records.FacetVisibility` (`records.FacetValues` lists them). Content types are counted as stored, parameters included (`text/plain; charset=utf-8`). A count by folder is the [folders plugin's](../plugins/folders.md) `FolderFacet(ctx, store, fq)`, with the same shape (`[]records.FacetCount`, `Value` the folder ID, `""` for the root), since folders are its rows. An unknown facet, or a query with `Limit` or `Cursor`, is `invalid_argument`.

<!-- ds:block id=sidebar-n4r8tk -->
**sidebar** · [`examples/records-query/main.go:143-163`](../../examples/records-query/main.go#L143-L163)

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

Check: `content_type: image/png 4 (84 B), application/pdf 2 (29 B)` and `folder: (root) 4 (71 B), Trip 2 (42 B)`.

## Alt text, caption and title

A file can carry describing text: `Alt` for screen readers and images that fail to load, `Caption` shown under it, `Title` as its display heading. `f.Text()` returns them as a `records.Text`, and `SetText(ctx, scope, id, records.Text{Alt, Caption, Title})` replaces all three and returns the file as stored: `""` removes a field, and the rest of the file's `Metadata` is kept. It works on a trashed file too and emits no event.

<!-- ds:block id=describe-f4nq8w2z -->
**describe** · [`examples/records-files/extras.go:75-81`](../../examples/records-files/extras.go#L75-L81)

```go
func describe(ctx context.Context, s *records.Store, f records.File) (records.File, error) {
	return s.SetText(ctx, f.Scope, f.ID, records.Text{
		Alt:     "Bar chart of revenue against costs",
		Caption: "Q3 revenue.\nSource: finance export.",
		Title:   "Q3 report",
	})
}
```
<!-- /ds:block hash=9edf26 -->

They live in `Metadata` under the reserved keys `records.MetaAlt` (`"alt"`), `MetaCaption` (`"caption"`) and `MetaTitle` (`"title"`), not in columns of their own: nothing filters or sorts by them, they are read with the file. So you can also set them at creation in `NewFile.Metadata`, or from a `BeforeCreate` or `BeforeReplace` hook. Every path, a replacement's included, is held to the same limits, in characters: `MaxAltChars` (1000), `MaxCaptionChars` (2000) and `MaxTitleChars` (255), valid UTF-8, no control characters, line breaks allowed in a caption only; anything else is `invalid_argument`. Do not use those three keys for anything else.

`SetText` writes the row only if its metadata is still what it read, so it never overwrites a concurrent metadata writer (the [dimensions](../plugins/dimensions.md) backfill, another `SetText`): that race is `unavailable`, to retry. Over HTTP it is `PUT /files/{id}/text` ([HTTP endpoints](http-endpoints.md)).

Check: the demo prints `alt "Bar chart of revenue against costs" title "Q3 report" source "export-job"`: the text is set and the `source` metadata the file was created with is still there.

## Step 9 — Hand out links

<!-- ds:block id=links-gn8gsk2p -->
**links** · [`examples/records-files/main.go:171-177`](../../examples/records-files/main.go#L171-L177)

```go
func links(ctx context.Context, s *records.Store, f records.File) (view, download string, err error) {
	if view, err = s.URL(ctx, f, linkTTL); err != nil {
		return "", "", err
	}
	download, err = s.DownloadURL(ctx, f, linkTTL)
	return view, download, err
}
```
<!-- /ds:block hash=d80758 -->

- `URL(ctx, f, ttl)` returns the public URL when the file is public and the disk can build one (a CDN prefix, a public bucket), otherwise a signed GET valid for `ttl`.
- `DownloadURL(ctx, f, ttl)` always signs, with a `Content-Disposition` that makes the browser save the file under its original name (`records.AttachmentDisposition`: an ASCII fallback plus the exact UTF-8 name, RFC 6266/5987).

A disk that can neither presign nor sign (no `Presign` capability and no `WithURLSigner`) makes both return `unsupported`. Both take the `File` value you pass: they do not re-read the row, check the scope or the trash. Load the file with `File(ctx, scope, id)` for the requesting scope first, and refuse a trashed one, as the [HTTP endpoints](http-endpoints.md) do.

Check: the demo prints a `view:` URL with `m=GET&sig=…` and a `download:` URL whose `cd=` parameter carries `attachment; filename="Quarterly report.txt"; filename*=UTF-8''Quarterly%20report.txt`.

## The second half of the example: Store options for production

Everything from here on runs in the second half of `examples/records-files`, on a Store made with three more options. They are options of `records.New`, so they apply to every call on that Store:

<!-- ds:block id=newfullstore-p3v7hx2c -->
**newFullStore** · [`examples/records-files/extras.go:48-61`](../../examples/records-files/extras.go#L48-L61)

```go
func newFullStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, error) {
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks,
		records.WithIDs(opaqueID),
		records.WithOwnerTypes(ownerPost),
		records.WithSourceIP(),
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
<!-- /ds:block hash=b3e78b -->

`Migrate` on the same database is a no-op the second time, so the two Stores of the example share one set of tables.

## Opaque public file IDs

A file's ID is what your API and your URLs show (`/files/fil_…`), so it must not leak anything: not a row count, not a creation order, not another tenant's existence. The default generator already gives that: the prefix (`records.IDPrefixFile`, `fil_`, or `records.IDPrefixAttachment`, `att_`) plus 128 random bits in lowercase base32. Keep it unless your app already has an ID scheme.

`records.WithIDs(fn)` replaces it, for files, attachments and the rows plugins create (grants, folders, versions, share links, webhook endpoints and deliveries, quota reservations), which take their IDs from `Store.NewID`. The function receives the prefix and must return a unique string that starts with it, at most 191 bytes (the indexed width on MySQL). The example plugs in its own random hex generator; `github.com/ubgo/publicid` or UUIDs work the same way:

<!-- ds:block id=opaqueid-r8kz2m4q -->
**opaqueID** · [`examples/records-files/extras.go:37-41`](../../examples/records-files/extras.go#L37-L41)

```go
func opaqueID(prefix string) string {
	b := make([]byte, opaqueIDBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on supported platforms
	return prefix + hex.EncodeToString(b)
}
```
<!-- /ds:block hash=0398f9 -->

- IDs are never sequential, so never sort by them; sort by `FileCreatedAt` (the listing does).
- A generator that repeats an ID makes `CreateFile` fail with `exists` instead of overwriting a row.
- Single-role attachment IDs ([attachments](attachments.md)) are derived from the owner and role and ignore the generator.
- Every read is scoped, so a valid ID of another tenant is `not_found`, exactly like an ID that does not exist: an ID alone grants nothing.

Check: the demo prints `id fil_… (36 chars)`: the four-byte prefix and 32 hex characters.

## Who uploaded it, when, and the original filename

Three facts are recorded with every file without extra work, so an admin screen can say "uploaded by Bob from the iOS app on 3 October as Quarterly report.txt":

| Field | Set from | Notes |
|---|---|---|
| `File.Filename` | `NewFile.Filename`, or the `filename` argument of `Upload` | The original name, cleaned by `upload.SafeFilename`, kept apart from `File.Key`. The key is chosen by your policy (random, content-addressed, by tenant) and never has to hold a client's name; the name is shown to users and used in `DownloadURL`'s `Content-Disposition`. Renaming the file changes the row, not the object. |
| `File.UploadedBy` | `NewFile.UploadedBy`, else `Store.Actor(ctx)`: the actor of `records.WithActor(ctx, actor)`, or of `records.WithActorFunc`'s lookup, else `""` | At most `records.MaxActorBytes` (191). A replacement keeps it. The [audit plugin](../plugins/audit.md) reads the same actor. |
| `File.CreatedAt` | The Store's clock (`records.WithClock` in tests) | UTC at microsecond precision. The listing sorts by it. |
| `File.Source` | `NewFile.Source`, else `records.WithSource(ctx, src)` | `Channel` (up to 64 bytes), `Client` (255) and `IP` (64). Values are the app's; the library never interprets them. |

Set the actor and the source once per request, in the middleware that knows who is signed in and which client is calling:

<!-- ds:block id=requestcontext-b6wq9n3t -->
**requestContext** · [`examples/records-files/extras.go:67-70`](../../examples/records-files/extras.go#L67-L70)

```go
func requestContext(ctx context.Context, user, ip string) context.Context {
	ctx = records.WithActor(ctx, user)
	return records.WithSource(ctx, records.Source{Channel: "ios", Client: "app/3.2", IP: ip})
}
```
<!-- /ds:block hash=9a6f4f -->

An application that already keeps its user under its own context key does not need `WithActor` at all: `records.WithActorFunc` gives the Store its own lookup, and every place that records an actor (this column, the share plugin's viewers and depositors) reads `Store.Actor(ctx)`:

```go
store, err := records.New(db, dialect, disks,
	records.WithActorFunc(func(ctx context.Context) string {
		if u, ok := auth.UserFrom(ctx); ok { // your own session type
			return "user:" + u.ID
		}
		return "" // falls back to records.WithActor, which the library itself sets for share-link uploads
	}),
)
```

The IP is personal data, so it is dropped before the row is written unless the Store was made `WithSourceIP()`, whatever the context or `NewFile` carries. A source wider than its columns is `invalid_argument` rather than cut short. All four are columns (`records.FileUploadedBy`, `FileCreatedAt`, `FileSourceChannel`, `FileSourceClient`, `FileSourceIP`), so they work as listing filters: "everything uploaded by this user", "everything imported last week".

Check: the demo prints `uploaded by user:bob via ios app/3.2 from 203.0.113.7`. The test `TestSourceIPDroppedByDefault` shows the same context on a Store without `WithSourceIP` keeps the channel and drops the IP.

Try it: [`examples/records-files`](../../examples/records-files) runs this section in `session.go` ([`newSessionStore`](ds:block?id=newsessionstore-m8x3k7wd)): `go run ./records-files` from `examples/`.

## Deduplicated uploads and shared objects

Two ways exist to store identical bytes once. Pick by what you want to share:

| Way | What it does | When to use it |
|---|---|---|
| `records.Dedup()` on `Upload` | After the upload is stored and hashed, a live file of the same scope, disk and visibility with the same SHA-256 lends the new row its object; the copy just uploaded is deleted after the commit. | Any key scheme. Users re-uploading the same attachment, chat images forwarded around a workspace. |
| `upload.ContentAddressed` as the policy's `Key` and `Settle` | The key is the content's hash: `<dir>/<owner digest>/<sha256>.<ext>`. Identical uploads of one owner land on the same key whether or not records is used. | You want keys that prove their content, as Git and container registries do, or the disk deduplicated without a database. |

`Dedup` is one option on the call:

<!-- ds:block id=uploaddedup-x2m9c7kd -->
**uploadDedup** · [`examples/records-files/extras.go:88-90`](../../examples/records-files/extras.go#L88-L90)

```go
func uploadDedup(ctx context.Context, s *records.Store, filename, body string) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader(body), filename, notesPolicy, records.Dedup())
}
```
<!-- /ds:block hash=a5f1b3 -->

The content-addressed policy sets the policy's `Key` and `Settle` together. `Owner` decides whose uploads may share: here the tenant, from the request's context. `Shared: true` shares across owners, with the consequences listed on `upload.ContentAddressed`:

<!-- ds:block id=blobpolicy-k7t3w9qe -->
**blobPolicy** · [`examples/records-files/extras.go:100-104`](../../examples/records-files/extras.go#L100-L104)

```go
func blobPolicy() upload.Policy {
	p := upload.Policy{MaxSize: maxUpload, Allowed: []string{"text/plain"}}
	p.Key, p.Settle = upload.ContentAddressed(blobDir, upload.ContentAddressing{Owner: tenantFromContext})
	return p
}
```
<!-- /ds:block hash=b46183 -->

Either way several rows may name one object (`CreateFile` with an existing key does the same). Each file keeps its own row, name, metadata and attachments. The object is reference-counted by its rows, not by a counter that could drift: before any deletion (purge, replace, move, a plugin's `DeleteObjectAfterCommit`), the transaction asks whether another row or an `ObjectOwner` plugin still names the object, and keeps it if so; the last file to go deletes it. `References(ctx, disk, key)` returns the count. A failed or rolled-back upload never deletes an object whose key a Settler chose: identical uploads still in flight share that key. It is left to `SweepOrphans`, whose `olderThan` grace covers them.

Check: the demo prints `dedup: two rows, same key true, 2 references`, then `purged one: object kept true` (the other row still names it), then `content key under blobs/: true, shared true`.

Rules worth knowing:

- Dedup saves storage, not bandwidth: the body is received and hashed in full first.
- It never reuses an object across scopes, and never reuses a [quarantined](processing-status.md#quarantine-until-scanned) file's object.
- Purges of files sharing an object lock every row naming it in one order, so they run one after the other and the last deletes it. A replace, move or visibility change of one sharing file racing a purge of another can fail with the database's deadlock error; retry it. No object a row still names is ever deleted.
- Trashing one of two public files that share an object leaves the object public for the live one, since the same bytes answer at its URL.
- An in-place `SetVisibility` on a shared object is `conflict`, because it would reach the other files. With `WithVisibilityDisks` the change copies the object, so it works.
- Content-addressed uploads wait under `<dir>/.incoming/` until hashed. A crash can leave one there; point `upload.Sweep` at that directory.

## Where a file came from

See [who uploaded it, when, and the original filename](#who-uploaded-it-when-and-the-original-filename): `records.WithSource(ctx, records.Source{Channel, Client, IP})`, set once per request beside `WithActor`, is recorded as `File.Source`, and the IP only with `WithSourceIP()`.

## Where a file is used

`UsagesOf(ctx, records.UsageQuery{Scope, FileID, Limit, Cursor})` lists every attachment of a file (owner and role), newest first, a page at a time: the "used in 3 posts" panel, or the check before deleting a shared asset. `Purge` itself does not refuse a file that is attached; it deletes the attachments with the file, in the same transaction. When a used file must not disappear from under its owners (a logo used by ten pages, an image a published post shows), ask first and refuse:

<!-- ds:block id=purgeifunused-h9c4r2vn -->
**purgeIfUnused** · [`examples/records-files/extras.go:114-124`](../../examples/records-files/extras.go#L114-L124)

```go
func purgeIfUnused(ctx context.Context, s *records.Store, f records.File) error {
	used, err := s.UsagesOf(ctx, records.UsageQuery{Scope: f.Scope, FileID: f.ID, Limit: 1})
	if err != nil {
		return err
	}
	if len(used.Usages) > 0 {
		a := used.Usages[0]
		return fmt.Errorf("%w: %s %s as %s", errInUse, a.Owner.Type, a.Owner.ID, a.Role)
	}
	return s.Purge(ctx, f.Scope, f.ID)
}
```
<!-- /ds:block hash=bf712f -->

A page of one is enough to know whether a file is used, and the query reads the `fs_attachments_file` index, so it costs one indexed lookup however widely the file is used. The check and the purge are two statements: an `Attach` landing between them still loses its attachment to the purge. When that matters, run both in one `store.Tx`, which locks the file's row for `Purge`. A file that does not exist, or is attached nowhere, has an empty page.

Check: the demo attaches the report to post 42 as its cover and prints `purge refused: … the file is still used: post 42 as cover (conflict)`, then detaches it and prints `detached, then purged`.

## A file on its owner's row

`records.Embed(f)` returns the file as versioned JSON (`{"v":1,"id":…,"key":…}`) to store on the row that owns it, so a page renders a user's avatar or a post's cover without reading `fs_files`; `records.Unembed(raw)` reads it back:

<!-- ds:block id=embedonpost-q5j8m2xw -->
**embedOnPost** · [`examples/records-files/extras.go:129-136`](../../examples/records-files/extras.go#L129-L136)

```go
func embedOnPost(f records.File) (records.File, json.RawMessage, error) {
	raw, err := records.Embed(f)
	if err != nil {
		return records.File{}, nil, err
	}
	back, err := records.Unembed(raw)
	return back, raw, err
}
```
<!-- /ds:block hash=80dd04 -->

It keeps what a reader needs: ID, scope, disk, key, name, type, size, digest, visibility, metadata and creation time. Pass the unembedded `File` to `Store.URL` to build its link. `Unembed` refuses JSON that is not an embedded file with `invalid_argument`, and a newer version (written by a newer library) with `unsupported`, so a row is never misread after a downgrade.

The copy goes stale when the file changes. Keep it in sync with an `EventHooks` plugin that re-embeds on `file.replaced`, `file.visibility_changed`, `file.moved` and `file.released` (whose release hooks may set metadata), and clears it on `file.trashed` and `file.purged`; `fs_files` stays the source of truth. [Events and hooks](events-and-hooks.md) shows how to write one.

Check: the demo prints `embedded 404 bytes, v1=true, back "Quarterly report.txt"`.

## What can go wrong

- `invalid_argument: records: db, dialect and disks are required` from `records.New`: one of the three is nil.
- `unsupported: records: engine "…" is not supported`: a dialect other than SQLite, Postgres or MySQL.
- `not_found` from `CreateFile`: the object is not on the disk (write it first), or the disk name is not in the manager.
- `rejected` or `too_large` from `Upload`: the policy refused the bytes. The object was not kept.
- `invalid_argument: records: limit … is not in [0, 1000]`: a client passed its own page size; clamp it before calling `Files`.
- A listing that skips or repeats files: the cursor was reused with different `Filters` or another scope.
- `invalid_argument` naming `alt`, `caption` or `title`: the text is over its limit or holds control characters, from `SetText` or from `Metadata` given to `CreateFile`.
- `conflict` from `SetVisibility` on a file whose object other files share: give the Store `WithVisibilityDisks`, or keep shared objects at one visibility.
- An object still on the disk after its last file was purged: a rolled-back content-addressed upload leaves its object for `SweepOrphans`, by design.
- `unsupported` from `Unembed`: the row was written by a newer library version; upgrade before reading it.
- MySQL: `unsupported Scan, storing driver.Value type []uint8 into type *time.Time`: the DSN lacks `parseTime=true`.

## Next

- [Attachments](attachments.md): link these files to your rows.
- [Lifecycle](lifecycle.md): trash, restore and purge.
- [HTTP endpoints](http-endpoints.md): the same operations over HTTP.
