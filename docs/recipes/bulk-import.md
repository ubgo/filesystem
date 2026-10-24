# Bulk import

"Import your contacts from a CSV", "upload your product catalogue", "bring your data from the old system": a user sends one file with thousands of rows, and the app must not parse it inside the request. A 10 MB CSV takes seconds to validate and insert, a proxy times the request out at 30 or 60 seconds, and a client that retries then imports everything twice. This recipe accepts the file in milliseconds, processes it in a background job that is safe to run again after a crash, and reports back which rows were refused and why.

The program is [examples/recipe-bulk-import](../../examples/recipe-bulk-import). Every code block below is copied from it, and every transcript is from running it.

## What you get

- `POST /imports` stores the CSV and answers `202 Accepted` with an import ID; nothing is parsed yet.
- A job running every two seconds picks up each queued import, streams it row by row, and inserts the valid rows into the app's own `contacts` table.
- The rows and the "done" status are committed in one transaction: a crash half way leaves nothing behind, and the next run starts that import over.
- `GET /imports/{id}` answers `processing`, `ready` with a count (`imported 2, refused 2`) or `failed` with the reason the user must fix (`line 1 must be the header "name,email"`).
- `GET /imports/{id}/report` downloads the refused rows with their line numbers and reasons, ready to fix and upload again.
- Each tenant sees only its own imports and contacts.

## Before you start

You need the examples module, or your own module requiring `github.com/ubgo/filesystem`, `github.com/ubgo/filesystem/records` and `github.com/ubgo/filesystem/jobs`, a SQL database and a disk. The demo uses SQLite and a local disk; a production app keeps the imports on a private bucket.

Which intake to use:

| Situation | Intake |
|---|---|
| A CSV or JSON file of rows, up to tens of MB | This recipe: a multipart `POST`, a status on the file row, a job. |
| A file too large for one request, or a flaky network | Receive it with tus ([resumable uploads](resumable-uploads.md)), then queue it the same way from `OnStored`. |
| Files straight to a bucket | A presigned upload and `upload.Confirm` ([browser direct uploads](browser-direct-uploads.md)), then queue it in the confirm handler. |
| Many processes, or jobs that must survive a deploy mid-run | The same job run by River or Asynq instead of the in-process scheduler ([maintenance jobs](../concepts/maintenance-jobs.md)). |

## The design

| Piece | Why |
|---|---|
| `upload.Policy` with `Allowed: text/csv` and `Extensions: .csv` | Only CSV is accepted, detected from the bytes and the extension; a renamed spreadsheet or HTML file is refused. |
| `Store.Upload` and `Store.SetStatus(processing, "queued")` in one `Store.Tx` | The import exists as a file row whose status is the queue. No client sees it as ready before the job ran. |
| `jobs.Func` run by `jobs.Scheduler` | The background half, with the job contract: safe to run again. |
| The rows and `SetStatus(ready)` in one `Store.Tx` | All or nothing per import, which makes rerunning after a crash correct. |
| A report object keyed by the import ID | The refused rows, written before the commit; a repeated run overwrites it. |

## Step 1: The policy

<!-- ds:block id=importpolicy-fn7wrfhw -->
**importPolicy** · [`examples/recipe-bulk-import/main.go:82-88`](../../examples/recipe-bulk-import/main.go#L82-L88)

```go
var importPolicy = upload.Policy{
	MaxSize:    maxImportBytes,
	MinSize:    1,
	Allowed:    []string{"text/csv"},
	Extensions: []string{".csv"},
	Key:        upload.RandomKey("incoming"),
}
```
<!-- /ds:block hash=a23eb7 -->

`MinSize: 1` refuses an empty file with `rejected` instead of queueing an import of nothing. The detected type of plain text with a `.csv` name is `text/csv; charset=utf-8`; `Allowed` matches on the type without its parameters. `upload.RandomKey("incoming")` names the object; the user's file name is kept on the row (`File.Filename`) but never reaches the key.

## Step 2: Accept and queue

<!-- ds:block id=app-enqueue-crnrjh47 -->
**App.enqueue** · [`examples/recipe-bulk-import/main.go:150-161`](../../examples/recipe-bulk-import/main.go#L150-L161)

```go
func (a *App) enqueue(ctx context.Context, tenant string, body io.Reader, filename string) (records.File, error) {
	var f records.File
	err := a.Store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		stored, err := a.Store.Upload(ctx, tenant, diskName, body, filename, importPolicy)
		if err != nil {
			return err
		}
		f, err = a.Store.SetStatus(ctx, tenant, stored.ID, records.StatusProcessing, detailQueued)
		return err
	})
	return f, err
}
```
<!-- /ds:block hash=40eade -->

`Store.Upload` registers a rollback hook that deletes the stored object, so if `SetStatus` fails the CSV does not stay on the disk without a row. The status is the queue: the job looks for files whose status is `processing`, which needs no separate jobs table and survives a restart.

<!-- ds:block id=app-accept-q7zkg5cx -->
**App.accept** · [`examples/recipe-bulk-import/main.go:173-193`](../../examples/recipe-bulk-import/main.go#L173-L193)

```go
func (a *App) accept(w http.ResponseWriter, r *http.Request) {
	tenant := r.Header.Get(headerTenant)
	if tenant == "" {
		writeError(w, filesystem.Errorf(filesystem.CodePermission, "sign in first"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes+formOverhead)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, filesystem.Errorf(filesystem.CodeInvalidArgument, "send the CSV in the form field \"file\""))
		return
	}
	defer func() { _ = file.Close() }()
	f, err := a.enqueue(r.Context(), tenant, file, hdr.Filename)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Location", "/imports/"+f.ID)
	writeJSON(w, http.StatusAccepted, importResponse{ImportID: f.ID, Status: f.Status, Detail: f.StatusDetail})
}
```
<!-- /ds:block hash=d00789 -->

`http.MaxBytesReader` bounds the request before the form is read; the policy's `MaxSize` bounds the stored file. The answer carries `Location: /imports/{id}`, which the client polls.

## Step 3: The job

<!-- ds:block id=app-importjob-24hs86qp -->
**App.ImportJob** · [`examples/recipe-bulk-import/main.go:201-217`](../../examples/recipe-bulk-import/main.go#L201-L217)

```go
func (a *App) ImportJob() jobs.Job {
	return jobs.Func(jobName, func(ctx context.Context, _ time.Time) (jobs.Result, error) {
		var n int64
		for {
			queued, err := a.queued(ctx)
			if err != nil || len(queued) == 0 {
				return jobs.Result{Count: n}, err
			}
			for _, f := range queued {
				if err := a.importOne(ctx, f); err != nil {
					return jobs.Result{Count: n}, err
				}
				n++
			}
		}
	})
}
```
<!-- /ds:block hash=1d2709 -->

`Store.Files` lists one scope, and the job serves every tenant, so it reads the queue straight from the files table with the typed columns `records` exports (`FileStatus`, `FileDisk`, `FileDeletedAt`):

<!-- ds:block id=app-queued-6s5xp9w4 -->
**App.queued** · [`examples/recipe-bulk-import/main.go:223-248`](../../examples/recipe-bulk-import/main.go#L223-L248)

```go
func (a *App) queued(ctx context.Context) ([]records.File, error) {
	var scope, id string
	q := sqlb.NewSelect(a.Store.Dialect()).
		Columns(sqlb.Bind(records.FileScope, &scope), sqlb.Bind(records.FileID, &id)).
		From(records.TableFiles).
		Where(sqlb.Eq(records.FileStatus, records.StatusProcessing), sqlb.Eq(records.FileDisk, diskName), sqlb.IsNull(records.FileDeletedAt)).
		Order(sqlb.OrderBy(records.FileCreatedAt, sqlb.SortAsc)).Limit(queueBatch)
	var keys [][2]string
	if err := a.Store.Each(ctx, q, func() error {
		keys = append(keys, [2]string{scope, id})
		return nil
	}); err != nil {
		return nil, err
	}
	// Read the rows after the listing is closed: the query holds its
	// connection until then, and SQLite here has only one.
	out := make([]records.File, 0, len(keys))
	for _, k := range keys {
		f, err := a.Store.File(ctx, k[0], k[1])
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
```
<!-- /ds:block hash=f582a6 -->

The loop ends when nothing is queued, so one run drains the queue, and each import leaves `processing` whatever happens to it (ready, or failed with a reason), so the loop cannot spin on one file.

## Step 4: Parse, then commit rows and status together

<!-- ds:block id=app-parse-j59h3hyv -->
**App.parse** · [`examples/recipe-bulk-import/main.go:286-315`](../../examples/recipe-bulk-import/main.go#L286-L315)

```go
func (a *App) parse(ctx context.Context, f records.File) (rows, refused [][]string, err error) {
	rc, err := a.Disk.Read(ctx, f.Key)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rc.Close() }()
	cr := csv.NewReader(rc)
	cr.FieldsPerRecord = -1 // a short row is refused with a reason, not fatal
	first, err := cr.Read()
	if err != nil || len(first) != len(header) || !strings.EqualFold(strings.TrimSpace(first[0]), header[0]) || !strings.EqualFold(strings.TrimSpace(first[1]), header[1]) {
		return nil, nil, fmt.Errorf("%w: line 1 must be the header %q", errBadFile, strings.Join(header, ","))
	}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return rows, refused, nil
		}
		if line-1 > maxRows {
			return nil, nil, fmt.Errorf("%w: more than %d rows", errBadFile, maxRows)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: line %d: not valid CSV", errBadFile, line)
		}
		if reason := checkRow(rec); reason != "" {
			refused = append(refused, append([]string{fmt.Sprint(line), reason}, rec...))
			continue
		}
		rows = append(rows, []string{strings.TrimSpace(rec[0]), strings.ToLower(strings.TrimSpace(rec[1]))})
	}
}
```
<!-- /ds:block hash=27ec74 -->

The CSV is streamed from the disk, never loaded whole. Two kinds of problem are kept apart. A problem with the whole file (the wrong header, more than 100,000 rows, broken quoting) is `errBadFile`: the user must fix it, so the import is marked `failed` with the reason. A problem with one row (an empty name, an address that does not parse) refuses that row only, and it goes to the report.

<!-- ds:block id=app-importone-dvsv59a6 -->
**App.importOne** · [`examples/recipe-bulk-import/main.go:258-280`](../../examples/recipe-bulk-import/main.go#L258-L280)

```go
func (a *App) importOne(ctx context.Context, f records.File) error {
	rows, refused, err := a.parse(ctx, f)
	if errors.Is(err, errBadFile) {
		_, err = a.Store.SetStatus(ctx, f.Scope, f.ID, records.StatusFailed, strings.TrimPrefix(err.Error(), errBadFile.Error()+": "))
		return err
	}
	if err != nil {
		return err
	}
	if err := a.writeReport(ctx, f.ID, refused); err != nil {
		return err
	}
	return a.Store.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, c := range rows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO contacts (scope, email, name, import_id) VALUES (?, ?, ?, ?)
				ON CONFLICT (scope, email) DO UPDATE SET name = excluded.name, import_id = excluded.import_id`, f.Scope, c[1], c[0], f.ID); err != nil {
				return err
			}
		}
		_, err := a.Store.SetStatus(ctx, f.Scope, f.ID, records.StatusReady, fmt.Sprintf("imported %d, refused %d", len(rows), len(refused)))
		return err
	})
}
```
<!-- /ds:block hash=0da5e7 -->

`Store.Tx` hands the callback the `*sql.Tx`, and `SetStatus` joins the transaction in the context, so the inserts and the status change commit together. That is what makes the job safe to repeat: until the commit, the import is still `processing` and its rows do not exist; after it, the import is `ready` and the next run does not see it. The `ON CONFLICT` upsert makes a second import of the same address update the contact rather than fail, which is what users expect when they re-upload a corrected file.

## Step 5: Run the job

<!-- ds:block id=runjobs-sptdz9dw -->
**runJobs** · [`examples/recipe-bulk-import/main.go:454-468`](../../examples/recipe-bulk-import/main.go#L454-L468)

```go
func runJobs(ctx context.Context, app *App) <-chan struct{} {
	sched := jobs.NewScheduler(jobs.SchedulerOptions{OnRun: func(r jobs.Report) {
		if r.Err != nil || r.Result.Count > 0 {
			fmt.Printf("job %s: %d imported, err=%v\n", r.Job, r.Result.Count, r.Err)
		}
	}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sched.Run(ctx, jobs.Schedule{Job: app.ImportJob(), Every: importEvery, Jitter: importJitter, RunAtStart: true}); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()
	return done
}
```
<!-- /ds:block hash=2f54af -->

`RunAtStart` drains whatever was queued while the process was down. The scheduler runs one copy of a job at a time in this process (a run still going when the next is due is skipped with `jobs.ErrRunning`); see the pitfalls for several processes.

## Try it

```sh
cd examples
go run ./recipe-bulk-import
```

```text
listening on http://localhost:8122 data in /var/folders/sk/.../T/recipe-bulk-import-187311588
```

`contacts.csv` has a header, two good rows, a row whose email is not an address, and a row without a name:

```sh
printf 'name,email\nAda Lovelace,ada@example.com\nAlan Turing,ALAN@example.com\nNo Address,not-an-email\n,blank@example.com\n' > contacts.csv
curl -s -X POST localhost:8122/imports -H 'X-Tenant: t1' -F file=@contacts.csv -w '\n%{http_code}\n'
```

```text
{"import_id":"fil_su37om36lysl643j4lkwqrhel6","status":"processing","detail":"queued"}

202
```

Two seconds later the server logs `job app.contact_import: 1 imported, err=<nil>`, and:

```sh
curl -s localhost:8122/imports/fil_su37om36lysl643j4lkwqrhel6 -H 'X-Tenant: t1'
curl -s localhost:8122/contacts -H 'X-Tenant: t1'
curl -s localhost:8122/imports/fil_su37om36lysl643j4lkwqrhel6/report -H 'X-Tenant: t1'
```

```text
{"import_id":"fil_su37om36lysl643j4lkwqrhel6","status":"ready","detail":"imported 2, refused 2"}
[{"name":"Ada Lovelace","email":"ada@example.com"},{"name":"Alan Turing","email":"alan@example.com"}]
line,reason,name,email
4,email is not an address,No Address,not-an-email
5,name is empty,,blank@example.com
```

Check: the addresses are lower-cased, and the report's line numbers match the file the user has open in a spreadsheet (the header is line 1).

A file with the columns swapped fails as a whole, and an empty file is refused at once:

```sh
printf 'email,name\nada@example.com,Ada\n' > swapped.csv
curl -s -X POST localhost:8122/imports -H 'X-Tenant: t1' -F file=@swapped.csv
# … after the next job run:
curl -s localhost:8122/imports/fil_lvuhrqizrrbikfbcvm5p2qasn6 -H 'X-Tenant: t1'
: > empty.csv
curl -s -X POST localhost:8122/imports -H 'X-Tenant: t1' -F file=@empty.csv -w '\n%{http_code}\n'
```

```text
{"import_id":"fil_lvuhrqizrrbikfbcvm5p2qasn6","status":"failed","detail":"line 1 must be the header \"name,email\""}
{"error":"the file is 0 bytes, under the minimum of 1","code":"rejected"}

415
```

Another tenant asking for the first import gets `404 not_found`. `go test ./recipe-bulk-import` checks all of this, plus that a second job run finds nothing to do.

## Pitfalls

- **Parsing in the request.** It works in development with ten rows and times out in production with ten thousand. Accept, answer `202`, and parse in a job.
- **Committing rows and status separately.** If the rows commit and the process dies before the status does, the next run imports them again. One transaction for both is what makes the job repeatable; with a database the rows cannot share a transaction with (a search index, another service), make each row write idempotent instead (an upsert keyed by the row's natural key).
- **Several processes running the scheduler.** `jobs.Scheduler` is single-flight per process only. Two instances can pick up the same queued import; the upsert keeps the data right, but the work is done twice. Run the job on one instance, or with River or Asynq, which run a job once across processes.
- **Status detail is user-facing.** `StatusDetail` is at most 255 characters and is shown to clients. Put "line 1 must be the header" there, never a driver error or a stack trace; this example only writes messages it composed.
- **A huge file with one bad row in the middle.** This example keeps the valid rows and reports the rest, which suits contacts. For data where a partial import is worse than none (accounting entries), fail the whole import when anything is refused, before the transaction.
- **Old imports pile up.** The CSV and its report stay on the disk. Trash imports older than a month with `Store.Trash` and let `jobs.SweepTrash` purge them, or upload with `records.Expires` and run `jobs.SweepExpired`.

## For production

- Authenticate with your session instead of `X-Tenant`, and use the tenant as the records scope, as here, so one tenant can never read another's imports.
- Store imports on a private bucket; they often hold personal data. Serve the report through your handler (as here) or a short-lived signed URL.
- Push the outcome instead of making clients poll: `SetStatus` emits `file.status_changed`, and a hook in `AfterCommit` can send a webhook or an email ([processing status](../records/processing-status.md)).
- Bound the work per run (here, ten imports per query, and a row limit per file) so one huge import cannot starve the others.

## Related

- [Processing status](../records/processing-status.md): statuses, details, events and polling.
- [Maintenance jobs](../concepts/maintenance-jobs.md): the `jobs` package, the scheduler, River and Asynq.
- [Uploads](../concepts/uploads.md): policies, types detected from the bytes, size limits.
- [Exports](exports.md): the other direction, a large download built in the background.
- [Resumable uploads](resumable-uploads.md): tus, for files too large for one request.
