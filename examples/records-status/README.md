# Processing status

The processing status of recorded files: a file recorded as processing, a worker reporting progress and then the outcome, a plugin told of every change with the file in the event, the failed files listed with a status filter, a client polling the status route of the HTTP endpoints, and files quarantined until a scan passes. It uses `github.com/ubgo/filesystem/records` and `records/recordshttp` on SQLite over an in-memory disk. The guide [Processing status](../../docs/records/processing-status.md) explains each step.

## Run it

```sh
cd examples
go run ./records-status
go test ./records-status
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the HTTP endpoints are called in process.

## What it does

```text
poll before: processing (queued)
event: photos/good.png is processing (thumbnails 1/2)
event: photos/good.png is ready
event: photos/bad.png is processing (thumbnails 1/2)
event: photos/bad.png is failed (thumbnail: unsupported format)
poll after: ready
failed: photos/bad.png: thumbnail: unsupported format
quarantine: notes.txt is quarantined
quarantine: link before the scan: conflict (true)
quarantine: make public before the scan: conflict
quarantine: scanned invoice.txt: quarantined infected: Eicar-Test-Signature
quarantine: scanned notes.txt: ready 
quarantine: link after the scan: ok
```

1. Two photos are recorded as `processing` with the note `queued`; polling `GET /files/{id}/status` says so.
2. The worker reports progress, then `ready` for one photo and `failed` with a reason for the other. Each change reaches the plugin as `file.status_changed`, with the file as updated in `Event.File`.
3. Polling again answers `ready`.
4. A filter on `records.FileStatus` lists the failed photo with its reason.
5. `quarantine.go` accepts two uploads at once with `records.Quarantine()`. Until a scan passes, a link and public visibility are refused with `records.ErrQuarantined`.
6. A worker lists the quarantined files and runs `Store.Scan` with a stand-in scanner: the clean note is released (`ready`), the infected invoice stays quarantined with the verdict (`WithScanFailure` keeps it by default). The released note gets a link.

## The code

A plugin told of each status change:

<!-- ds:block id=statuslog-5xm5mey6 -->
**statusLog** · [`examples/records-status/main.go:49`](main.go#L49-L49)

```go
type statusLog struct{ lines []string }
```
<!-- /ds:block hash=0cba14 -->

Recording a file as processing:

<!-- ds:block id=recordforprocessing-p8xeh35t -->
**recordForProcessing** · [`examples/records-status/main.go:89-101`](main.go#L89-L101)

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

The worker's reports:

<!-- ds:block id=process-jpk92ygd -->
**process** · [`examples/records-status/main.go:106-116`](main.go#L106-L116)

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

The failed files:

<!-- ds:block id=failedfiles-3z346hpy -->
**failedFiles** · [`examples/records-status/main.go:120-126`](main.go#L120-L126)

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

Polling over HTTP:

<!-- ds:block id=poll-ghfthrat -->
**poll** · [`examples/records-status/main.go:131-139`](main.go#L131-L139)

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

## Files

- `main.go`: the demo.
- `quarantine.go`: quarantine until scanned, a stand-in scanner and the worker.
- `main_test.go`: runs it and checks the output line by line, plus a purging scan failure and a scanner that is down.

## Next

- [Processing status](../../docs/records/processing-status.md): the guide.
- [Events and hooks](../../docs/records/events-and-hooks.md): what `Event.File` holds for every event.
