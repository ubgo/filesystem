# Files as database rows

The file API of the records layer: every stored object gets a row in `fs_files` with its scope, disk, key, name, type, size and digest, so an app can list, filter and link its files without asking the storage backend. This program wires a `records.Store` on SQLite over an in-memory disk, records an object that is already stored (`CreateFile`), uploads and records in one call (`Upload`), lists a scope page by page and with a filter, and hands out a view link and a download link. A second half covers opaque IDs, the uploader and source, alt text, deduplicated uploads, refusing to delete a used file, and a file embedded on its owner's row. It uses `github.com/ubgo/filesystem/records`, `upload`, `signedurl` and `memory`, with `github.com/ubgo/sqlb` for the SQL. The guide [Files](../../docs/records/files.md) explains each step.

## Run it

```sh
cd examples
go run ./records-files
go test ./records-files
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory, so nothing outlives the run.

## What it does

```sh
go run ./records-files
```

```text
created fil_ "Quarterly report.txt" text/plain; charset=utf-8 19 bytes private
uploaded "todo.txt" key=notes/… by user:alice
refused: rejected
listed 2 files in 2 pages
files over 10 bytes: 1 ("Quarterly report.txt")
view: https://app.example.com/files/media/exports/q3.txt?ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791025039&kid=k1&m=GET&sig=6de63d212bb021c747e3cee1b5a3c9f7f09eb4b06cb9535de61e7520fd1090a3
download: https://app.example.com/files/media/exports/q3.txt?cd=attachment%3B+filename%3D%22Quarterly+report.txt%22%3B+filename%2A%3DUTF-8%27%27Quarterly%2520report.txt&ct=text%2Fplain%3B+charset%3Dutf-8&exp=1791025039&kid=k1&m=GET&sig=11314debb74d22b2a31db31c80c5913a27edc0c2056a7e9998669159ab6506af
id fil_… (36 chars) key exports/q4.txt name "Quarterly report.txt"
uploaded by user:bob via ios app/3.2 from 203.0.113.7, created_at set true
alt "Bar chart of revenue against costs" title "Q3 report" source "export-job"
dedup: two rows, same key true, 2 references
purged one: object kept true
content key under blobs/: true, shared true
purge refused: filesystem: conflict: the file is still used: post 42 as cover (conflict)
detached, then purged
embedded 404 bytes, v1=true, back "Quarterly report.txt"
session actor: uploaded by user:42
no session: uploaded by share:lnk_1
order name:      Agenda.txt(10) budget.txt(3) notes.txt(1)
order name_desc: notes.txt(1) budget.txt(3) Agenda.txt(10)
order largest:   Agenda.txt(10) budget.txt(3) notes.txt(1)
order smallest:  notes.txt(1) budget.txt(3) Agenda.txt(10)
name cursor with the largest order: invalid_argument
```

1. An object written straight to the disk is recorded with `CreateFile`, which checks it exists and fills the size, type and SHA-256 from the disk. Files are private unless told otherwise.
2. `Upload` stores and records in one call under an upload policy; the key is random under `notes/`, and the actor in the context becomes the file's `UploadedBy`.
3. A file named `evil.txt` whose bytes are a PNG is refused with the code `rejected`: the policy accepts plain text only and judges the type from the bytes, not the name, and nothing is recorded.
4. Listing the tenant's files with a page size of one takes two pages, following `NextCursor` until it is empty (keyset pagination, newest first).
5. A filter on the `fs_files` table (larger than 10 bytes) narrows the listing; order and pagination stay the Store's.
6. `URL` gives a private file a signed GET; `DownloadURL` adds a `Content-Disposition` that saves it under its display name. The signatures and expiry change on every run.

The second half (`extras.go`) opens a second Store on the same database with `WithIDs`, `WithOwnerTypes` and `WithSourceIP`:

7. File IDs come from the custom generator: the `fil_` prefix and 32 random hex characters. The original filename is kept apart from the key.
8. The actor and the source set once per request (`WithActor`, `WithSource`) are recorded as `UploadedBy` and `Source`, the IP only because the Store was made `WithSourceIP`.
9. `SetText` sets the alt text, caption and title and keeps the rest of the metadata.
10. Two `Upload` calls with `Dedup` record two rows on one object (two references); purging one keeps the object for the other. Two uploads under `upload.ContentAddressed` land on the same content key.
11. A file attached to a post as its cover is refused by `purgeIfUnused` (`UsagesOf` finds the attachment); once detached it is purged.
12. `Embed` turns the file into versioned JSON for its owner's row, and `Unembed` reads it back.
13. A third Store is made `WithActorFunc`, which reads the app's own session type from its own context key (`session.go`): an upload by the signed-in user 42 is recorded as `user:42` with no `WithActor` call, and a request without a session falls back to the actor `records.WithActor` names (here a share link's).
14. Three files in another tenant are listed in each sort a drive's menu offers (`FileQuery.Order`): by name (in the database's collation, so on SQLite capitals first), by name descending, largest first and smallest first, two per page. A cursor issued by one order is `invalid_argument` with another.

## The code

The disk the Store reaches bytes through: one memory disk that signs its own URLs and builds public URLs under a CDN prefix.

<!-- ds:block id=newdisks-ke5p8pyb -->
**newDisks** · [`examples/records-files/main.go:62-73`](main.go#L62-L73)

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

SQLite opened with one connection (it has one writer) and a busy timeout, then a Store over it. `Migrate` is safe on every start: it applies only what is new.

<!-- ds:block id=opensqlite-cjy2vqp7 -->
**openSQLite** · [`examples/records-files/main.go:78-85`](main.go#L78-L85)

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

<!-- ds:block id=newstore-c3s67q5f -->
**newStore** · [`examples/records-files/main.go:90-99`](main.go#L90-L99)

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

Recording bytes that arrived some other way (a worker's output, an import, a presigned upload):

<!-- ds:block id=recordexisting-x7uxukhd -->
**recordExisting** · [`examples/records-files/main.go:106-118`](main.go#L106-L118)

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

The upload policy and the one-call upload that stores and records under it:

<!-- ds:block id=notespolicy-4kaczpxx -->
**notesPolicy** · [`examples/records-files/main.go:122-126`](main.go#L122-L126)

```go
var notesPolicy = upload.Policy{
	MaxSize: maxUpload,
	Allowed: []string{"text/plain"},
	Key:     upload.RandomKey("notes"),
}
```
<!-- /ds:block hash=d6fc40 -->

<!-- ds:block id=uploadnote-z5zuwjmw -->
**uploadNote** · [`examples/records-files/main.go:131-134`](main.go#L131-L134)

```go
func uploadNote(ctx context.Context, s *records.Store, filename string, body io.Reader) (records.File, error) {
	ctx = records.WithActor(ctx, "user:alice")
	return s.Upload(ctx, tenant, diskName, body, filename, notesPolicy)
}
```
<!-- /ds:block hash=9c04da -->

Walking every page of a scope, and a filtered listing:

<!-- ds:block id=listall-hce32ebc -->
**listAll** · [`examples/records-files/main.go:139-155`](main.go#L139-L155)

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

<!-- ds:block id=listbig-x8dnvzmv -->
**listBig** · [`examples/records-files/main.go:160-165`](main.go#L160-L165)

```go
func listBig(ctx context.Context, s *records.Store) (records.FilePage, error) {
	return s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{sqlb.Greater(records.FileSize, int64(bigFile))},
	})
}
```
<!-- /ds:block hash=d5bb38 -->

The two links a file can have: `URL` (public URL for a public file, otherwise a signed GET) and `DownloadURL` (always signed, saved under the file's name).

<!-- ds:block id=links-gn8gsk2p -->
**links** · [`examples/records-files/main.go:171-177`](main.go#L171-L177)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: disks, SQLite, Store, `CreateFile`, `Upload`, listings and links |
| `extras.go` | The second half: opaque IDs, actor and source, `SetText`, `Dedup` and `References`, a content-addressed policy, `UsagesOf` before a purge, `Embed` and `Unembed` |
| `session.go` | The actor from the app's own session type (`WithActorFunc`), and listings sorted by name and size |
| `main_test.go` | Runs the same steps, plus a missing object for `CreateFile`, scope isolation, a public file's public URL, a malformed cursor, the IP dropped without `WithSourceIP`, a title over its limit, an embedded file from a newer version, and a session that names no actor without `WithActorFunc` |

## Next

- The guide: [Files](../../docs/records/files.md), and the [records overview](../../docs/records/README.md).
- Related examples: [records-attachments](../records-attachments) to attach files to your own rows, [records-lifecycle](../records-lifecycle) for trash and purge, [records-http](../records-http) for the same API over HTTP.
