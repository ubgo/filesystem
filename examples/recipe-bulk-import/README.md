# Bulk import: accept a CSV now, import it in a job

The "import your contacts from a CSV" feature of a CRM or any SaaS onboarding flow. `POST /imports` stores the CSV and answers `202` at once, with the import recorded as a file whose status is `processing`; a job, run every two seconds by the `jobs` scheduler, streams each queued CSV, inserts the valid rows into the app's own `contacts` table in the same transaction that marks the import `ready`, and writes the refused rows (with line numbers and reasons) to a report. It uses `upload.Policy`, `records` (`Store.Upload`, `Store.SetStatus`, `Store.Tx`, the typed file columns) and `jobs` (`Func`, `Scheduler`). The guide is [Bulk import](../../docs/recipes/bulk-import.md).

## Run it

```sh
cd examples
go run ./recipe-bulk-import
```

It creates a temporary directory (printed at start-up) holding `app.db` (SQLite: the `fs_` tables and `contacts`) and `files/`, listens on `http://localhost:8122`, and runs the import job in the same process. The tenant is the `X-Tenant` header, a demo shortcut: a real app reads its session. Stop it with Ctrl-C (or SIGTERM): the server and the scheduler stop cleanly and the temporary directory is removed. Run the tests with `go test ./recipe-bulk-import`.

| Route | What it does |
|---|---|
| `POST /imports` | multipart form, a CSV in field `file`; `202` with the import ID |
| `GET /imports/{id}` | `processing`, `ready` with counts, or `failed` with the reason |
| `GET /imports/{id}/report` | the refused rows of a finished import, as CSV |
| `GET /contacts` | the tenant's contacts |

## Walkthrough

1. Upload a CSV with two good rows and two bad ones; it is queued, not parsed:

```sh
printf 'name,email\nAda Lovelace,ada@example.com\nAlan Turing,ALAN@example.com\nNo Address,not-an-email\n,blank@example.com\n' > contacts.csv
curl -s -X POST localhost:8122/imports -H 'X-Tenant: t1' -F file=@contacts.csv -w '\n%{http_code}\n'
```

```text
{"import_id":"fil_su37om36lysl643j4lkwqrhel6","status":"processing","detail":"queued"}

202
```

2. The job runs (the server logs `job app.contact_import: 1 imported, err=<nil>`); the import is ready, the contacts are in, and the report lists the refused rows:

```text
{"import_id":"fil_su37om36lysl643j4lkwqrhel6","status":"ready","detail":"imported 2, refused 2"}
[{"name":"Ada Lovelace","email":"ada@example.com"},{"name":"Alan Turing","email":"alan@example.com"}]
line,reason,name,email
4,email is not an address,No Address,not-an-email
5,name is empty,,blank@example.com
```

3. A file with the columns swapped fails as a whole, with a reason the user can act on; an empty file is refused before it is queued:

```text
{"import_id":"fil_lvuhrqizrrbikfbcvm5p2qasn6","status":"failed","detail":"line 1 must be the header \"name,email\""}
{"error":"the file is 0 bytes, under the minimum of 1","code":"rejected"}
```

4. Another tenant asking for the import gets `404 not_found`.

## The code

Accept and queue, in one transaction:

<!-- ds:block id=app-enqueue-crnrjh47 -->
**App.enqueue** · [`examples/recipe-bulk-import/main.go:150-161`](main.go#L150-L161)

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

The job, and the queue it reads across tenants:

<!-- ds:block id=app-importjob-24hs86qp -->
**App.ImportJob** · [`examples/recipe-bulk-import/main.go:201-217`](main.go#L201-L217)

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

<!-- ds:block id=app-queued-6s5xp9w4 -->
**App.queued** · [`examples/recipe-bulk-import/main.go:223-248`](main.go#L223-L248)

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

One import: the rows and the ready status commit together, so a crash half way leaves nothing and the next run starts over.

<!-- ds:block id=app-importone-dvsv59a6 -->
**App.importOne** · [`examples/recipe-bulk-import/main.go:258-280`](main.go#L258-L280)

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

## Files

| File | What it holds |
|---|---|
| `main.go` | The policy, the accept handler, the job and its queue query, the streaming parser, the report, the status and contacts routes, and the scheduler. |
| `main_test.go` | A full import (status, contacts, report, a second run that finds nothing, tenant isolation) and the bad files (wrong header, empty, not a CSV, no tenant). |

## Next

- The guide: [Bulk import](../../docs/recipes/bulk-import.md).
- [records-status](../records-status): processing status, events and polling on their own.
- [jobs-scheduler](../jobs-scheduler): the scheduler and the built-in maintenance jobs.
- [recipe-exports](../recipe-exports): the other direction, an export built in the background.
