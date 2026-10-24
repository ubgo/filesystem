# Storage quotas per plan

The quota plugin of the records layer: a byte and file limit per scope, taken from the scope's plan, charged in the database inside the same transaction as each upload. Uploads past a limit are refused, trashed files still count until they are purged, a replacement is charged by its size difference, a rolled-back upload un-charges itself, and `Recount` rebuilds the usage after a manual repair. This is the "you have used 18 of 25 MB" line of a SaaS billing page. It uses `github.com/ubgo/filesystem/records`, `records/quota` and `records/versions` on SQLite over an in-memory disk. The guide [Quota](../../docs/plugins/quota.md) explains each step.

## Run it

```sh
cd examples
go run ./records-quota
go test ./records-quota
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory. The limits are tiny on purpose: the free plan of tenant `acme` allows 25 bytes and 3 files.

## What it does

```sh
go run ./records-quota
```

```text
two uploads:           acme: 18 of 25 bytes, 2 of 3 files
over the byte limit:   quota_exceeded
after a rollback:      acme: 18 of 25 bytes, 2 of 3 files
replaced 10 -> 4:      acme: 12 of 25 bytes, 2 of 3 files
growth past the limit: quota_exceeded
trashed:               acme: 12 of 25 bytes, 2 of 3 files
purged:                acme: 8 of 25 bytes, 1 of 3 files
recount:               8 bytes, 1 files
second ticket while the first holds the room: quota_exceeded
reserved 15 of 25 bytes
confirmed: 5 bytes used, 0 reserved
abandoned ticket released: true
usage events: 5 bytes (5)
org over its plan:     quota_exceeded
  org:initech: 18 bytes, 2 files
  type:initech-north:text: 12 bytes, 1 files
rows deleted, then RecountDimensions:
  org:initech: 18 bytes, 2 files
  type:initech-north:text: 12 bytes, 1 files
```

1. Two uploads of 10 and 8 bytes are charged: 18 of 25 bytes, 2 of 3 files. They go through a retry helper, because two first uploads of a scope can race to create its usage row and the loser fails with `unavailable`.
2. A 19-byte upload would pass 25 bytes and is refused with `quota_exceeded`.
3. An upload inside a transaction that then fails takes its charge with it: the usage is unchanged.
4. Replacing the 10-byte file with 4 bytes credits 6. The replaced content is kept by the versions plugin and is not charged.
5. Growing the same file to 20 bytes would pass the limit and is refused.
6. Trashing a file keeps it charged (it can be restored); purging releases it.
7. `Recount` rebuilds the usage from `fs_files` and agrees: 8 bytes, 1 file.

The second half (`inflight.go`) opens a second Store on the same database with `quota.WithUsageEvents()`, on a free-plan scope of its own:

8. A reservation of 15 bytes for a presigned upload holds the room, so a second ticket of 20 bytes is refused before any byte arrives.
9. The 5-byte object that actually arrived is recorded under the reservation: 5 bytes are charged and the reservation is used up.
10. An abandoned ticket is released at once.
11. The only `quota.usage_changed` event is the recorded file's: reservations do not bill.

The third part (`dimensions.go`) charges files to counters of the app's own with `quota.WithDimensions`, on a Store and database of its own. Two workspaces, `initech-north` and `initech-south`, belong to the organisation `initech`, whose plan is 20 bytes over both; the workspaces themselves are unlimited:

12. North stores 12 bytes and south 6: the organisation's counter holds 18. A 5-byte upload to south passes south's own counter but would take the organisation past 20, so it is refused with `quota_exceeded`.
13. `DimensionUsage` reads each counter: the organisation's, and a content-class counter (`type:<workspace>:text`) that is counted but has no limit.
14. After the counter rows are deleted (a manual repair gone wrong), `RecountDimensions` rebuilds them from the files and gives the same totals. The test `TestDimensionsFailClosed` shows a workspace with no organisation is refused rather than stored uncounted.

## The code

The app's own plan table, read by the plugin through a `LimitFunc`; an unknown scope is an error:

<!-- ds:block id=limits-du27j4uv -->
**limits** · [`examples/records-quota/main.go:66-72`](main.go#L66-L72)

```go
func limits(_ context.Context, scope string) (quota.Limit, error) {
	l, ok := planOf[scope]
	if !ok {
		return quota.Limit{}, filesystem.Errorf(filesystem.CodePermission, "no plan for scope %q", scope)
	}
	return l, nil
}
```
<!-- /ds:block hash=db64e2 -->

Installing quota, and versions to show that kept versions are not charged:

<!-- ds:block id=newstore-cnhfwgs6 -->
**newStore** · [`examples/records-quota/main.go:101-118`](main.go#L101-L118)

```go
func newStore(ctx context.Context, db *sql.DB, disks *filesystem.Manager) (*records.Store, *quota.Plugin, error) {
	q, err := quota.New(limits)
	if err != nil {
		return nil, nil, err
	}
	v, err := versions.New()
	if err != nil {
		return nil, nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithPlugins(q, v))
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Migrate(ctx); err != nil {
		return nil, nil, err
	}
	return s, q, nil
}
```
<!-- /ds:block hash=9bad72 -->

An upload the plugin charges or refuses, and the retry for the first-upload race:

<!-- ds:block id=uploadtext-8afxkytn -->
**uploadText** · [`examples/records-quota/main.go:123-125`](main.go#L123-L125)

```go
func uploadText(ctx context.Context, s *records.Store, scope, body string) (records.File, error) {
	return s.Upload(ctx, scope, diskName, strings.NewReader(body), "note.txt", textPolicy)
}
```
<!-- /ds:block hash=20a8bc -->

<!-- ds:block id=uploadwithretry-g656j6mk -->
**uploadWithRetry** · [`examples/records-quota/main.go:131-140`](main.go#L131-L140)

```go
func uploadWithRetry(ctx context.Context, s *records.Store, scope, body string) (records.File, error) {
	var err error
	for range maxRetries {
		var f records.File
		if f, err = uploadText(ctx, s, scope, body); filesystem.CodeOf(err) != filesystem.CodeUnavailable {
			return f, err
		}
	}
	return records.File{}, err
}
```
<!-- /ds:block hash=3cd41f -->

The billing line:

<!-- ds:block id=usageline-m7guc4ar -->
**usageLine** · [`examples/records-quota/main.go:144-154`](main.go#L144-L154)

```go
func usageLine(ctx context.Context, s *records.Store, q *quota.Plugin, scope string) (string, error) {
	u, err := q.Usage(ctx, s, scope)
	if err != nil {
		return "", err
	}
	l, err := limits(ctx, scope)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %d of %d bytes, %d of %d files", scope, u.Bytes, l.Bytes, u.Files, l.Files), nil
}
```
<!-- /ds:block hash=c0e890 -->

A rolled-back upload, a replacement charged by its difference, and the repair:

<!-- ds:block id=uploadthenfail-9vecxbsh -->
**uploadThenFail** · [`examples/records-quota/main.go:162-169`](main.go#L162-L169)

```go
func uploadThenFail(ctx context.Context, s *records.Store, scope string) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := uploadText(ctx, s, scope, "draft"); err != nil {
			return err
		}
		return errAbort
	})
}
```
<!-- /ds:block hash=3d3a5e -->

<!-- ds:block id=replacecontent-v8pf85nf -->
**replaceContent** · [`examples/records-quota/main.go:176-178`](main.go#L176-L178)

```go
func replaceContent(ctx context.Context, s *records.Store, f records.File, body string) (records.File, error) {
	return s.Replace(ctx, f.Scope, f.ID, strings.NewReader(body), "", textPolicy)
}
```
<!-- /ds:block hash=f9992a -->

<!-- ds:block id=repair-828jum3m -->
**repair** · [`examples/records-quota/main.go:183-185`](main.go#L183-L185)

```go
func repair(ctx context.Context, s *records.Store, q *quota.Plugin, scope string) (quota.Usage, error) {
	return q.Recount(ctx, s, scope)
}
```
<!-- /ds:block hash=7de84c -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: plans, the plugin, uploads, rollback, replace, trash, purge and recount |
| `inflight.go` | The second half: reservations for uploads in flight, confirming under a reservation, releasing one, and `quota.usage_changed` events |
| `dimensions.go` | The third part: an organisation-wide cap and a content-class counter through `quota.WithDimensions`, read with `DimensionUsage` and rebuilt with `RecountDimensions`; code: [`orgDimensions`](ds:block?id=orgdimensions-q7m2x9ka), [`newOrgStore`](ds:block?id=neworgstore-p4v8c2nd), [`dimensionLine`](ds:block?id=dimensionline-h3w6t9rb) |
| `main_test.go` | Runs the same steps, plus the file limit, an unknown scope, the zero usage of a new scope, `Recount` rebuilding a deleted row, and a workspace without an organisation refused |

## Next

- The guide: [Quota](../../docs/plugins/quota.md).
- Related examples: [recipe-saas-tenants](../recipe-saas-tenants) for quotas in a multi-tenant app, [records-versions](../records-versions) for the kept versions, [start-production](../start-production) for limits per plan in a deployed app.
