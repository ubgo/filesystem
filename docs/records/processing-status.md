# Processing status

Much of what an app does with an upload happens after the bytes are stored: thumbnails, a virus scan, a transcode, text extraction. While that runs, the file is stored and usable, but what clients want from it may not be there yet. This page records that state on the file itself, reports progress and the outcome from the worker or plugin doing the work, tells your hooks about each change, finds the files that failed, and lets a client poll for the outcome over HTTP.

## Before you start

- The [records overview](README.md) and [Files](files.md) set up a Store.
- The example is [`examples/records-status`](../../examples/records-status): SQLite, an in-memory disk and the `recordshttp` endpoints in process. `go run ./records-status` in `examples/` prints each step.
- The status columns come with core migration `0003_files_status`; `Migrate` adds them. Files recorded before it are `ready`.

## Step 1 — The status of a file

Every file has a `Status` and a `StatusDetail`:

| Status | Meaning |
|---|---|
| `records.StatusReady` (`ready`) | Nothing is pending. Every file starts ready unless told otherwise. |
| `records.StatusProcessing` (`processing`) | Work on the file is under way. |
| `records.StatusFailed` (`failed`) | The work failed; `StatusDetail` says why. |
| `records.StatusQuarantined` (`quarantined`) | The file is unusable until a scan passes: nothing serves, links, shares or publishes it ([Quarantine until scanned](#quarantine-until-scanned)). |

`StatusDetail` is a short note for your app and its users, `""` for none: progress ("thumbnails 1/2") or a reason ("thumbnail: unsupported format"), at most `records.MaxStatusDetailChars` (255) characters of valid UTF-8 without control characters. Never put a stack trace or a backend message there: clients see it.

A file is usable as stored whatever its status, except `quarantined`. The status tells a client whether the derived things it may want (a thumbnail, a scan verdict) are there yet, so it can show a placeholder or an error instead of a broken image.

## Step 2 — Record a file as processing

When the work starts right after the upload, record the file as processing, so a client never sees it as ready in between: `NewFile.Status` and `NewFile.StatusDetail` set both at creation.

<!-- ds:block id=recordforprocessing-p8xeh35t -->
**recordForProcessing** · [`examples/records-status/main.go:89-101`](../../examples/records-status/main.go#L89-L101)

```go
func recordForProcessing(ctx context.Context, s *records.Store, key string) (records.File, error) {
	disk, err := s.Disks().Disk(diskName)
	if err != nil {
		return records.File{}, err
	}
	if _, err := disk.WriteBytes(ctx, key, pngBytes, filesystem.WriteOptions{}); err != nil {
		return records.File{}, err
	}
	return s.CreateFile(ctx, records.NewFile{
		Scope: tenant, Disk: diskName, Key: key,
		Status: records.StatusProcessing, StatusDetail: "queued",
	})
}
```
<!-- /ds:block hash=959f61 -->

## Step 3 — Report progress and the outcome

The worker or plugin doing the work calls `SetStatus(ctx, scope, id, status, detail)`: `StatusProcessing` with a progress note as it goes, then `StatusReady` (with `""`) when it is done, or `StatusFailed` with the reason. It returns the file as updated. It joins the transaction in `ctx` when there is one, so a plugin records its own rows (the thumbnails' rows) and the status in one transaction. Setting the current status and detail again does nothing. An unknown status or a bad detail is `invalid_argument`; an unknown file is `not_found`. It works on a trashed file too.

<!-- ds:block id=process-jpk92ygd -->
**process** · [`examples/records-status/main.go:106-116`](../../examples/records-status/main.go#L106-L116)

```go
func process(ctx context.Context, s *records.Store, f records.File, fail bool) error {
	if _, err := s.SetStatus(ctx, tenant, f.ID, records.StatusProcessing, "thumbnails 1/2"); err != nil {
		return err
	}
	if fail {
		_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusFailed, "thumbnail: unsupported format")
		return err
	}
	_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusReady, "")
	return err
}
```
<!-- /ds:block hash=1158ec -->

## Step 4 — React to each change

Each change emits `file.status_changed`, and its `Event.File` is the file as updated, so a hook has the key, the new status and the detail without a query. Send the webhook or the push notification from `AfterCommit`, so it never announces a change that rolled back ([Events and hooks](events-and-hooks.md)).

<!-- ds:block id=statuslog-5xm5mey6 -->
**statusLog** · [`examples/records-status/main.go:49`](../../examples/records-status/main.go#L49-L49)

```go
type statusLog struct{ lines []string }
```
<!-- /ds:block hash=0cba14 -->

Check: the demo prints `event: photos/good.png is processing (thumbnails 1/2)`, `event: photos/good.png is ready`, and `event: photos/bad.png is failed (thumbnail: unsupported format)`.

Plugins that do the work emit their own events beside it: the variants plugin's `variants.ready` and `variants.failed`.

## Step 5 — Find the files that failed

`records.FileStatus` is a typed column, so a filter lists the files in one status with the usual order and pages: the failed ones for a retry job or an admin page, the processing ones for a dashboard.

<!-- ds:block id=failedfiles-3z346hpy -->
**failedFiles** · [`examples/records-status/main.go:120-126`](../../examples/records-status/main.go#L120-L126)

```go
func failedFiles(ctx context.Context, s *records.Store) ([]records.File, error) {
	page, err := s.Files(ctx, records.FileQuery{
		Scope:   tenant,
		Filters: []sqlb.Predicate{sqlb.Eq(records.FileStatus, records.StatusFailed)},
	})
	return page.Files, err
}
```
<!-- /ds:block hash=0472d7 -->

Check: `failed: photos/bad.png: thumbnail: unsupported format`.

## Step 6 — Poll from a client

The [HTTP endpoints](http-endpoints.md) answer `GET /files/{id}/status` with `{"file_id", "status", "status_detail"}` and `Cache-Control: no-store`, authorized as a read. A client polls it after an upload until `status` is `ready` or `failed`. The list route also returns `status` and `status_detail` on each file.

<!-- ds:block id=poll-ghfthrat -->
**poll** · [`examples/records-status/main.go:131-139`](../../examples/records-status/main.go#L131-L139)

```go
func poll(srv http.Handler, fileID string) (recordshttp.StatusResponse, error) {
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+fileID+"/status", nil))
	var out recordshttp.StatusResponse
	if rec.Code != http.StatusOK {
		return out, fmt.Errorf("status %d: %s", rec.Code, rec.Body.String())
	}
	return out, json.Unmarshal(rec.Body.Bytes(), &out)
}
```
<!-- /ds:block hash=82c8f6 -->

Check: `poll before: processing (queued)` and, after the worker ran, `poll after: ready`.

## Quarantine until scanned

A synchronous scan (`scan.Inspect` in the upload policy, see [Virus scanning](../recipes/virus-scanning.md)) refuses an infected file before the upload succeeds. When the scan runs later instead (a queue, a slow scanner, a scanning service), record the file as `quarantined`: it is stored, its row and attachments are there, a client can poll its status, but its bytes are handed to nobody until the scan passes.

The second half of [`examples/records-status`](../../examples/records-status) (`quarantine.go`) walks it in five steps: `go run ./records-status` prints every check below.

**Step 1 — Choose what a failed scan does.** It is the Store's option `records.WithScanFailure`: `records.ScanFailureKeep` (the default) keeps an infected file quarantined with the verdict as its detail, for an administrator to inspect or purge; `records.ScanFailurePurge` purges it (its rows, then its object once no other file names it). The example's disk signs URLs only so a released file gets a link:

<!-- ds:block id=newquarantinestore-t4k8w2cn -->
**newQuarantineStore** · [`examples/records-status/quarantine.go:36-57`](../../examples/records-status/quarantine.go#L36-L57)

```go
func newQuarantineStore(ctx context.Context, db *sql.DB, failure records.ScanFailure) (*records.Store, error) {
	signer, err := signedurl.New("https://app.example.com/files", []signedurl.Key{{ID: "k1", Secret: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		return nil, err
	}
	disk, err := filesystem.NewDisk(diskName, memory.New(), filesystem.WithURLSigner(signer))
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithScanFailure(failure))
	if err != nil {
		return nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
```
<!-- /ds:block hash=f35874 -->

**Step 2 — Accept the upload at once, quarantined.** The `records.Quarantine()` upload option records the file as `quarantined` (with `CreateFile`, set `NewFile.Status: records.StatusQuarantined`; over HTTP, `recordshttp.Config{Quarantine: true}`):

<!-- ds:block id=acceptnow-h7c3m9wq -->
**acceptNow** · [`examples/records-status/quarantine.go:63-65`](../../examples/records-status/quarantine.go#L63-L65)

```go
func acceptNow(ctx context.Context, s *records.Store, name, body string) (records.File, error) {
	return s.Upload(ctx, tenant, diskName, strings.NewReader(body), name, inbox, records.Quarantine())
}
```
<!-- /ds:block hash=1d3188 -->

Check: `quarantine: notes.txt is quarantined`.

**Step 3 — Nothing hands it out yet.** A link, a view, a share, a stream or public visibility is refused with `records.ErrQuarantined` (`conflict`); the table below lists every path. Check: `quarantine: link before the scan: conflict (true)` and `quarantine: make public before the scan: conflict`.

**Step 4 — Scan from a worker.** Any `scan.Scanner` works: `scan.Clamd`, a cloud API through `scan.ScannerFunc`, or the example's stand-in, which flags a marker string. List what is still quarantined with a `records.FileStatus` filter and call `Store.Scan` on each:

<!-- ds:block id=scanwaiting-p2v6n8kt -->
**scanWaiting** · [`examples/records-status/quarantine.go:85-99`](../../examples/records-status/quarantine.go#L85-L99)

```go
func scanWaiting(ctx context.Context, s *records.Store, sc scan.Scanner) ([]records.File, error) {
	page, err := s.Files(ctx, records.FileQuery{Scope: tenant, Filters: []sqlb.Predicate{sqlb.Eq(records.FileStatus, records.StatusQuarantined)}})
	if err != nil {
		return nil, err
	}
	var out []records.File
	for _, f := range page.Files {
		scanned, err := s.Scan(ctx, f.Scope, f.ID, sc)
		if err != nil {
			return out, err // the file stays quarantined; retry on the next run
		}
		out = append(out, scanned)
	}
	return out, nil
}
```
<!-- /ds:block hash=2a37c7 -->

`Store.Scan` reads the object and records the verdict: clean calls `PassScan`, infected calls `FailScan` with `infected: <signature>` as the detail. A scan that does not finish (the scanner is down, the object unreadable) returns its error and leaves the file quarantined: scanning fails closed (the test `TestScanFailsClosed` pins it). A worker with its own scanner calls `PassScan(ctx, scope, id)` and `FailScan(ctx, scope, id, detail)` directly. Check: `quarantine: scanned invoice.txt: quarantined infected: Eicar-Test-Signature` and `quarantine: scanned notes.txt: ready`.

**Step 5 — The released file works like any other.** Check: `quarantine: link after the scan: ok`. With `ScanFailurePurge`, the infected file is gone instead (`TestScanFailurePurge`).

Every path that hands a file out refuses a quarantined one, each with `records.ErrQuarantined` (`conflict`):

| Path | What happens |
|---|---|
| `Store.URL`, `Store.DownloadURL` | Refused. The check reads the status from the database, so a stale `File` value, or one a plugin builds for a derived object (an image variant's link), cannot get a link out. |
| `Store.Served` (`ServeStream`) | Refused before any metering or access-log plugin hears of the read. An app streaming bytes itself calls it first, as it already must. |
| `Store.Viewed` | `false` with `ErrQuarantined`; a view-once file's one view is not spent. |
| `Store.SetVisibility` to public | Refused, checked again under the row lock so a racing quarantine cannot be lost. A public object is reachable without the library, so an unscanned one is never public: `CreateFile` and `Upload` refuse a quarantined file with public visibility, and `SetStatus` refuses to quarantine a public file (`records.ErrPublicQuarantine`, `invalid_argument`). Make it private first. |
| `SetStatus` | Cannot move a quarantined file to any other status: every processing plugin calls `SetStatus`, and none may release a file nobody scanned. Only `PassScan` releases it. |
| `Upload` with `Dedup` | Never reuses a quarantined file's object for a new file. |
| `Replace` | A quarantined file stays quarantined whatever replaces it. `records.QuarantineContent()` quarantines a ready file together with its new content, since replaced bytes are unscanned again; every other change refuses that option. |
| [HTTP endpoints](http-endpoints.md) | `GET /files/{id}` answers 409; the list route shows the file without a `url`; the status route reports `quarantined`. `Config.Quarantine` quarantines everything the upload and replace routes store. |
| [Share links](../plugins/share.md) | `Create` refuses a quarantined file; a link whose file was quarantined after it was made answers the uniform 404 and spends nothing; folder links leave quarantined files out of their listing. `share.WithQuarantinedDeposits()` quarantines uploads through request links. |
| [Image variants](../plugins/variants.md) | Nothing is generated from a quarantined file, so `URL`, `Get`, `Srcset` and the variants handler are refused. |

`PassScan` runs every `records.ReleaseHooks` plugin on the file inside its transaction (where a describer reads what it skipped), then emits `file.status_changed` and `file.released`. The bundled plugins never read a quarantined file's bytes and catch up on release:

| Plugin | While quarantined | On release |
|---|---|---|
| [dimensions](../plugins/dimensions.md), [audio](../plugins/audio.md), [video](../plugins/video.md), [pdf](../plugins/pdf.md) describers | Not read (their keys a caller set are still cleared) | Described in `BeforeRelease` |
| [variants](../plugins/variants.md) | No eager variants; `Get`, `URL`, `Generate`, `Srcset` and the handler refuse with `ErrQuarantined`; `GenerateMissing` skips the file | Eager variants made after the commit |
| [ocr](../plugins/ocr.md), [pdf](../plugins/pdf.md) processing | Not marked processing; `Process` refuses with `ErrQuarantined` | Marked processing and processed after the commit |
| [vision](../plugins/vision.md) | Not sent to the labeler; `Label` refuses | Labelled after the commit |
| videoplatform | `Publish` refuses | Publishes |

A plugin of your own that reads bytes should do the same: skip `File.Quarantined()` files, implement `ReleaseHooks` for work inside the row, or react to `file.released` for work after the commit.

## What can go wrong

- `invalid_argument: records: unknown status`: a value outside `ready`, `processing`, `failed`, `quarantined`.
- `conflict: records: the file is quarantined until its scan passes`: a link, view, share or public visibility asked for a quarantined file, or `SetStatus` asked to move one out of quarantine. Wait for the scan (`PassScan`).
- `invalid_argument: records: a public file cannot be quarantined`: make the file private first, and public after the scan.
- `conflict: records: the file is not quarantined`: `FailScan` for a file another worker already released; nothing changed.
- `invalid_argument` about the status detail: longer than 255 characters, invalid UTF-8, or with a line break or another control character.
- A file stays `processing` forever: the worker died between its calls. List the processing files older than your longest job (`FileStatus` and `FileCreatedAt` filters) and retry or fail them.
- A client polls a trashed file and gets `404`: trashed files are not found on the read routes.

## Next

- [Events and hooks](events-and-hooks.md): every event and what `Event.File` holds.
- [Image variants](../plugins/variants.md): thumbnails made after upload.
- [HTTP endpoints](http-endpoints.md): the polling route among the others.
