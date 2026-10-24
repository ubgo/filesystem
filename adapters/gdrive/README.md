# adapters/gdrive

`github.com/ubgo/filesystem/adapters/gdrive` is the `filesystem.Driver` for Google Drive (My Drive and shared drives), over the Drive API v3 as plain JSON on `net/http`, with no dependency beyond the library (`google.golang.org/api/drive/v3` brings the whole Google API client stack for nine requests). It is its own module.

The guide is [docs/adapters/gdrive.md](../../docs/adapters/gdrive.md).

```sh
go get github.com/ubgo/filesystem/adapters/gdrive
```

## Config

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Token` | `TokenSource` | required | Supplies the OAuth access token, once per request; `TokenFunc` adapts a function |
| `FolderID` | `string` | `root` (My Drive) | ID of the folder holding the disk; a shared drive's ID works |
| `ChunkSize` | `int` | `DefaultChunkSize` (8 MiB) | Bytes per upload request, a multiple of `ChunkUnit` (256 KiB); two chunks are held in memory per write (the one being sent and the next, read ahead to tell whether it is the last) |
| `Client` | `*http.Client` | a client with `DefaultTimeout` (5 min) | Transport, proxy, TLS; the driver wraps its transport to add the token |
| `APIURL`, `UploadURL` | `string` | `DefaultAPIURL`, `DefaultUploadURL` | Endpoints, for a proxy or a test server; https, or http on loopback only |
| `Trash` | `bool` | false | Every removal (`Delete`, `DeletePrefix`, the older files a `Write`, `Copy` or `Move` replaces) moves the item to Drive's trash instead of deleting it permanently; needed on shared drives where the account is a Content manager |

`New(cfg)` makes no request. `Config` prints and logs with the token source redacted.

## Capabilities

`Copy` (`files.copy`), `Move` (one update of parent and name), `DeletePrefix`, `ContentType` (Drive stores the MIME type). Never: conditional writes (Drive v3 has no `If-Match`), presigning (Drive has no signed URLs), visibility, metadata, headers, multipart. Names are case-sensitive.

## Behaviour

- Keys are paths of names under `FolderID`, resolved one segment at a time (O(depth) requests, nothing cached). Folders of a repeated name resolve through the oldest; a key naming two or more files is `CodeConflict` for `Stat`, `Read` and `Write`, and `Delete` removes them all. Folder creation is serialised within a process.
- Google Docs, Sheets and other native files have no bytes: listings skip them and they are `CodeNotFound`.
- Writes are resumable uploads in `ChunkSize` pieces (`Content-Range` total from `SizeHint`, else `*` until the final chunk); the file appears only when the last chunk arrives, and a failing body cancels the session. Replacing a key uploads a new revision of the same file ID.
- `Copy` and `Move` delete the files `dst` named before only after the new one is in place, so `dst` is never missing.
- `Delete` and `DeletePrefix` delete permanently, not to the trash, unless `Trash` is set (a trash update, `files.update` with `trashed: true`).
- Errors: 404 `CodeNotFound`, 403 `storageQuotaExceeded` `CodeQuotaExceeded`, 403 rate limits, 429 and 5xx `CodeUnavailable`, other 401/403 or a failing token source `CodePermission`.
- Transport errors never carry a URL's query, where an upload session's ID is.
- `Native()` returns the `*http.Client` (with the token transport).

## Tests

`go test ./...` runs the conformance suite twice (default and 256 KiB chunks) against an in-process fake of the Drive API that allows repeated names, plus resumable ranges, cancellation, repeated names, native documents, query escaping, revisions, redirects, redaction and status mapping. Not verified without a real account: Drive's acceptance of `*` as the total of a stream of unknown size (pass `SizeHint` when known), and shared-drive permission errors.
