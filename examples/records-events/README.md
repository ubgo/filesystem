# Your own plugins: events and hooks

How an application takes part in what the records Store does, by installing its own plugins. One plugin is told of every change (`EventHooks`), keeps a changelog table of its own (`Migrator`) and queues a notification that waits for the commit (`AfterCommit`); one stamps a classification on every new file and refuses to purge a file under legal hold (`FileHooks`); one refuses a replacement that changes a file's type (`ReplaceHooks`). This is how an app adds search indexing, webhooks, compliance rules or cache invalidation without forking the library. It uses `github.com/ubgo/filesystem/records` on SQLite over an in-memory disk, with `github.com/ubgo/sqlb` for the changelog table. The guide [Events and hooks](../../docs/records/events-and-hooks.md) explains each step.

## Run it

```sh
cd examples
go run ./records-events
go test ./records-events
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```sh
go run ./records-events
```

```text
changelog: file.created attachment.created attachments.reordered file.trashed file.restored file.replaced file.status_changed file.status_changed file.released file.visibility_changed file.moved attachment.deleted file.purged <nil>
sent: 13
rolled back: handler failed; rows 13, sent 13
purge under hold: permission; metadata map[classification:internal legal_hold:true]
replace text with PNG: rejected
```

1. One file goes through every kind of change. The changelog plugin writes one row per event inside the change's transaction, so its table lists all twelve kinds in thirteen rows (the file is quarantined for a rescan and released, two status changes followed by `file.released`), and thirteen notifications are sent after their commits.
2. An upload inside a transaction that then fails rolls the changelog row back with the file, and its notification is never sent: still thirteen rows, thirteen sent.
3. The guard stamped `classification=internal` on the file when it was created; a file marked `legal_hold=true` cannot be purged (`permission`).
4. Replacing a text file with a PNG is refused by the type lock (`rejected`).

## The code

The changelog plugin: a table of its own, created by `Migrate` with the core tables, and a row per event that joins the change's transaction:

<!-- ds:block id=changelog-95pvcy28 -->
**Changelog** · [`examples/records-events/main.go:79-82`](main.go#L79-L82)

```go
type Changelog struct {
	// Sent collects notifications, standing in for webhook calls.
	Sent []string
}
```
<!-- /ds:block hash=0e4a82 -->

<!-- ds:block id=changelog-migrations-b8j9my5y -->
**Changelog.Migrations** · [`examples/records-events/main.go:91-101`](main.go#L91-L101)

```go
func (c *Changelog) Migrations(engine string) ([]records.Migration, error) {
	t := records.TypesFor(engine)
	st, err := records.NewDDL(engine).CreateTable(records.Table{
		Table: changes,
		Columns: []ddl.ColumnSpec{
			records.Column(changeScope, t.Key), records.Column(changeKind, t.Short),
			records.Column(changeFileID, t.Key), records.Column(changeAt, t.Timestamp),
		},
	}).Statements()
	return []records.Migration{{Owner: c.Name(), Name: "0001_changes", Statements: st}}, err
}
```
<!-- /ds:block hash=d0d169 -->

<!-- ds:block id=changelog-onevent-smrprhjr -->
**Changelog.OnEvent** · [`examples/records-events/main.go:107-116`](main.go#L107-L116)

```go
func (c *Changelog) OnEvent(ctx context.Context, s *records.Store, e records.Event) error {
	ins := sqlb.NewInsert(s.Dialect()).Into(changes).Row(
		sqlb.Set(changeScope, e.Scope), sqlb.Set(changeKind, string(e.Kind)), sqlb.Set(changeFileID, e.FileID), sqlb.Set(changeAt, e.At),
	)
	if _, err := s.Exec(ctx, ins); err != nil {
		return err
	}
	s.AfterCommit(ctx, func(context.Context) { c.Sent = append(c.Sent, string(e.Kind)) })
	return nil
}
```
<!-- /ds:block hash=c38355 -->

File hooks: `BeforeCreate` may change metadata and visibility (never the ID, disk or key), and an error from `BeforePurge` aborts the purge:

<!-- ds:block id=guard-beforecreate-abrznwmq -->
**Guard.BeforeCreate** · [`examples/records-events/main.go:138-147`](main.go#L138-L147)

```go
func (Guard) BeforeCreate(_ context.Context, _ *records.Store, f *records.File) error {
	if f.Metadata[metaClass] == "" {
		meta := map[string]string{metaClass: defaultClass}
		for k, v := range f.Metadata {
			meta[k] = v
		}
		f.Metadata = meta
	}
	return nil
}
```
<!-- /ds:block hash=f01504 -->

<!-- ds:block id=guard-beforepurge-nq6eteax -->
**Guard.BeforePurge** · [`examples/records-events/main.go:154-159`](main.go#L154-L159)

```go
func (Guard) BeforePurge(_ context.Context, _ *records.Store, f records.File) error {
	if f.Metadata[metaHold] == "true" {
		return filesystem.Errorf(filesystem.CodePermission, "file %s is under legal hold", f.ID)
	}
	return nil
}
```
<!-- /ds:block hash=3c84aa -->

A replace hook that vets new content:

<!-- ds:block id=typelock-beforereplace-947bgqwy -->
**TypeLock.BeforeReplace** · [`examples/records-events/main.go:170-175`](main.go#L170-L175)

```go
func (TypeLock) BeforeReplace(_ context.Context, _ *records.Store, cur records.File, next *records.File) error {
	if cur.ContentType != next.ContentType {
		return filesystem.Errorf(filesystem.CodeRejected, "content type %s cannot become %s", cur.ContentType, next.ContentType)
	}
	return nil
}
```
<!-- /ds:block hash=2cee6f -->

Installing the three plugins, one change of each kind, and the rollback:

<!-- ds:block id=newstore-9yrcnb6u -->
**newStore** · [`examples/records-events/main.go:191-211`](main.go#L191-L211)

```go
func newStore(ctx context.Context, db *sql.DB, log *Changelog) (*records.Store, error) {
	disk, err := filesystem.NewDisk(diskName, memory.New())
	if err != nil {
		return nil, err
	}
	archive, err := filesystem.NewDisk(archiveDisk, memory.New())
	if err != nil {
		return nil, err
	}
	disks, err := filesystem.NewManager(disk, archive)
	if err != nil {
		return nil, err
	}
	s, err := records.New(db, sqlb.SQLiteDialect{}, disks, records.WithOwnerTypes(ownerPost),
		records.WithPlugins(log, Guard{}, TypeLock{}))
	if err != nil {
		return nil, err
	}
	_, err = s.Migrate(ctx)
	return s, err
}
```
<!-- /ds:block hash=e67b61 -->

<!-- ds:block id=everykind-nd3v32m9 -->
**everyKind** · [`examples/records-events/main.go:215-250`](main.go#L215-L250)

```go
func everyKind(ctx context.Context, s *records.Store) (records.File, error) {
	post := records.Owner{Type: ownerPost, ID: "post_1"}
	f, err := s.Upload(ctx, tenant, diskName, strings.NewReader("hello"), "a.txt", textPolicy)
	if err != nil {
		return f, err
	}
	a, err := s.Attach(ctx, tenant, f.ID, post, roleGallery)
	if err != nil {
		return f, err
	}
	steps := []func() error{
		func() error { return s.Reorder(ctx, tenant, post, roleGallery, []string{a.ID}) },
		func() error { return s.Trash(ctx, tenant, f.ID) },
		func() error { return s.Restore(ctx, tenant, f.ID) },
		func() error {
			_, err := s.Replace(ctx, tenant, f.ID, strings.NewReader("hello again"), "", textPolicy)
			return err
		},
		func() error {
			_, err := s.SetStatus(ctx, tenant, f.ID, records.StatusQuarantined, "rescan")
			return err
		},
		// The scan passed: file.status_changed (ready), then file.released.
		func() error { _, err := s.PassScan(ctx, tenant, f.ID); return err },
		func() error { return s.SetVisibility(ctx, tenant, f.ID, filesystem.VisibilityPublic) },
		func() error { _, err := s.MoveFile(ctx, tenant, f.ID, archiveDisk); return err },
		func() error { return s.Detach(ctx, tenant, a.ID) },
		func() error { return s.Purge(ctx, tenant, f.ID) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return f, err
		}
	}
	return f, nil
}
```
<!-- /ds:block hash=22e10c -->

<!-- ds:block id=rolledback-94kuxr9u -->
**rolledBack** · [`examples/records-events/main.go:255-262`](main.go#L255-L262)

```go
func rolledBack(ctx context.Context, s *records.Store) error {
	return s.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := s.Upload(ctx, tenant, diskName, strings.NewReader("x"), "x.txt", textPolicy); err != nil {
			return err
		}
		return errAbort
	})
}
```
<!-- /ds:block hash=458206 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | The program: the changelog, guard and type-lock plugins, and the run that exercises them |
| `main_test.go` | Runs the same steps and checks the run covers every event kind the library defines |

## Next

- The guide: [Events and hooks](../../docs/records/events-and-hooks.md), and [Write a records plugin](../../docs/extending/write-a-records-plugin.md) for the full plugin contract.
- Related examples: [records-audit](../records-audit), a built-in plugin on the same events, and [extending-records-plugin](../extending-records-plugin).
