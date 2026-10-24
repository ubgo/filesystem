# Consistency: bytes and rows without a shared transaction

An object store and a database cannot commit together, so every app that keeps file rows next to stored bytes needs ordering rules. This program shows the ones the library follows: object first and row second on the way in, row first and object last on the way out, an upload inside a rolled-back transaction deleting its object, a row refused for bytes that were never stored, finding orphan objects without deleting an upload still in flight, conditional writes, and a mirrored disk converging after the mirror was down. It uses the core `filesystem`, `memory`, `layer` and `upload` packages with `records` on an in-memory SQLite database through `ubgo/sqlb`. The guide [Consistency](../../docs/concepts/consistency.md) explains each step.

## Run it

```sh
cd examples
go run ./concepts-consistency
go test ./concepts-consistency
```

It needs no network, files or environment variables: the database is a private in-memory SQLite (`modernc.org/sqlite`, pure Go) and the disks are memory disks. The mirror's outage is simulated with a `layer.Guard` interceptor, and an old orphan is made by moving the memory driver's clock back. It prints eight lines and exits.

## What it does

```sh
go run ./concepts-consistency
```

```text
rolled back upload: true; objects 0, rows 0
record a missing object: not_found
purge rolled back: true; objects 1, rows 1
purge committed: objects 0, rows 0
orphans deleted [uploads/crashed.pdf]; objects 2, rows 1
second IfNotExists write: exists
mirror down: unavailable; primary true, replica false
written again: replica true
```

1. An upload inside `store.Tx` whose later statement fails leaves neither a row nor an object: the Store deletes the object it had written.
2. `CreateFile` for a key that was never written is `not_found`: a row never points at missing bytes.
3. A purge inside a transaction that rolls back keeps both row and object; a committed purge deletes the row and then the object.
4. The orphan finder deletes only objects older than the grace period that no row points at: the old crashed upload goes, the fresh in-flight one and the recorded file stay.
5. A second `IfNotExists` write of a lock key is `exists` rather than a silent overwrite.
6. A write through a mirrored disk while the mirror is down lands on the primary and reports `unavailable`; writing again once it is back brings the replica into line.

## The code

A rolled-back upload leaves nothing behind.

<!-- ds:block id=rolledbackupload-v4hfbev6 -->
**rolledBackUpload** · [`examples/concepts-consistency/steps.go:19-32`](steps.go#L19-L32)

```go
func rolledBackUpload(ctx context.Context, d *demo, out io.Writer) error {
	err := d.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if _, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "invoice.pdf", policy); err != nil {
			return err
		}
		return errAppFailed
	})
	objects, rows, serr := d.state(ctx)
	if serr != nil {
		return serr
	}
	fmt.Fprintf(out, "rolled back upload: %v; objects %d, rows %d\n", errors.Is(err, errAppFailed), objects, rows)
	return nil
}
```
<!-- /ds:block hash=d239a4 -->

No row for missing bytes.

<!-- ds:block id=objectfirst-e5ygz2n4 -->
**objectFirst** · [`examples/concepts-consistency/steps.go:37-41`](steps.go#L37-L41)

```go
func objectFirst(ctx context.Context, d *demo, out io.Writer) error {
	_, err := d.store.CreateFile(ctx, records.NewFile{Scope: scope, Disk: diskName, Key: "uploads/never-written.pdf", Filename: "x.pdf"})
	fmt.Fprintf(out, "record a missing object: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=bcc3ff -->

Purge deletes the object only after the row's transaction commits.

<!-- ds:block id=purgeaftercommit-cjatkcy8 -->
**purgeAfterCommit** · [`examples/concepts-consistency/steps.go:47-72`](steps.go#L47-L72)

```go
func purgeAfterCommit(ctx context.Context, d *demo, out io.Writer) error {
	f, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "keep.pdf", policy)
	if err != nil {
		return err
	}
	err = d.store.Tx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		if err := d.store.Purge(ctx, scope, f.ID); err != nil {
			return err
		}
		return errAppFailed
	})
	objects, rows, serr := d.state(ctx)
	if serr != nil {
		return serr
	}
	fmt.Fprintf(out, "purge rolled back: %v; objects %d, rows %d\n", errors.Is(err, errAppFailed), objects, rows)
	if err := d.store.Purge(ctx, scope, f.ID); err != nil {
		return err
	}
	objects, rows, err = d.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "purge committed: objects %d, rows %d\n", objects, rows)
	return nil
}
```
<!-- /ds:block hash=9729db -->

Orphans: unrecorded and older than the grace period. `upload.Sweep` deletes by age alone, so it is only for a prefix nothing records.

<!-- ds:block id=findorphans-4kdhftcx -->
**findOrphans** · [`examples/concepts-consistency/orphans.go:30-69`](orphans.go#L30-L69)

```go
func findOrphans(ctx context.Context, d *demo, out io.Writer) error {
	cutoff := time.Now().Add(-orphanGrace)
	if _, err := d.store.Upload(ctx, scope, diskName, strings.NewReader(pdf), "kept.pdf", policy); err != nil {
		return err
	}
	if _, err := d.disk.WriteBytes(ctx, uploadDir+"/in-flight.pdf", []byte(pdf), filesystem.WriteOptions{}); err != nil {
		return err
	}
	d.age = 2 * orphanGrace
	_, err := d.disk.WriteBytes(ctx, uploadDir+"/crashed.pdf", []byte(pdf), filesystem.WriteOptions{})
	d.age = 0
	if err != nil {
		return err
	}
	var orphans []string
	for e, err := range d.disk.List(ctx, uploadDir, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return err
		}
		if !e.ModTime.Before(cutoff) {
			continue // possibly an upload still in flight
		}
		var n int
		if err := d.db.QueryRowContext(ctx, recordedQuery, d.disk.Name(), e.Key).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			orphans = append(orphans, e.Key)
		}
	}
	if err := d.disk.DeleteKeys(ctx, orphans); err != nil {
		return err
	}
	objects, rows, err := d.state(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "orphans deleted %v; objects %d, rows %d\n", orphans, objects, rows)
	return nil
}
```
<!-- /ds:block hash=1e8f5b -->

One object: a conditional write instead of last-writer-wins.

<!-- ds:block id=conditionalwrite-d37uctzq -->
**conditionalWrite** · [`examples/concepts-consistency/mirror.go:20-32`](mirror.go#L20-L32)

```go
func conditionalWrite(ctx context.Context, out io.Writer) error {
	disk, err := filesystem.NewDisk("locks", memory.New())
	if err != nil {
		return err
	}
	opts := filesystem.WriteOptions{IfNotExists: true}
	if _, err := disk.WriteBytes(ctx, "jobs/nightly.lock", []byte("worker-1"), opts); err != nil {
		return err
	}
	_, err = disk.WriteBytes(ctx, "jobs/nightly.lock", []byte("worker-2"), opts)
	fmt.Fprintf(out, "second IfNotExists write: %s\n", filesystem.CodeOf(err))
	return nil
}
```
<!-- /ds:block hash=fb4b27 -->

A mirror that failed converges when the write is repeated.

<!-- ds:block id=mirrorconverges-zsn6p4n9 -->
**mirrorConverges** · [`examples/concepts-consistency/mirror.go:38-70`](mirror.go#L38-L70)

```go
func mirrorConverges(ctx context.Context, out io.Writer) error {
	down := true
	outage := layer.Guard(func(context.Context, *filesystem.Op) error {
		if down {
			return filesystem.NewError(filesystem.CodeUnavailable, errMirrorDown)
		}
		return nil
	})
	primary, err := filesystem.NewDisk("s3-old", memory.New())
	if err != nil {
		return err
	}
	replica, err := filesystem.NewDisk("s3-new", memory.New(), filesystem.WithInterceptors(outage))
	if err != nil {
		return err
	}
	both, err := filesystem.Mirror("media", primary, replica)
	if err != nil {
		return err
	}
	body := []byte("%PDF-1.7 mirrored")
	_, err = both.WriteBytes(ctx, "a.pdf", body, filesystem.WriteOptions{})
	onPrimary, _ := primary.Exists(ctx, "a.pdf")
	onReplica, _ := replica.Exists(ctx, "a.pdf")
	fmt.Fprintf(out, "mirror down: %s; primary %t, replica %t\n", filesystem.CodeOf(err), onPrimary, onReplica)
	down = false
	if _, err := both.WriteBytes(ctx, "a.pdf", body, filesystem.WriteOptions{}); err != nil {
		return err
	}
	onReplica, _ = replica.Exists(ctx, "a.pdf")
	fmt.Fprintf(out, "written again: replica %t\n", onReplica)
	return nil
}
```
<!-- /ds:block hash=18ad75 -->

## Files

| File | What it holds |
|---|---|
| `main.go` | Wiring: SQLite, a memory disk with a movable clock, the records Store, and the step runner |
| `steps.go` | Rolled-back upload, object-first, purge after commit |
| `orphans.go` | Finding and deleting orphan objects safely |
| `mirror.go` | Conditional write and the converging mirror |
| `main_test.go` | Runs the program and checks each ordering rule |

## Next

- The guide: [Consistency](../../docs/concepts/consistency.md).
- [Files as records](../../docs/records/README.md) and [Lifecycle](../../docs/records/lifecycle.md); examples [records-files](../records-files) and [records-lifecycle](../records-lifecycle).
- [Interceptors](../../docs/concepts/interceptors.md) for what Retry repeats; example [concepts-interceptors](../concepts-interceptors).
- [Backups and mirrors](../recipe-backups-and-mirrors).
