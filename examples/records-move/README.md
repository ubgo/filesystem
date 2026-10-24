# Moving files between disks

"Accept fast, store later": uploads land on a fast disk so the request returns at once, and a background job promotes them to the disk they live on, one file at a time, with `Store.MoveFile`. Each file keeps its ID, key and attachments; only where its bytes live changes. It uses `github.com/ubgo/filesystem/records` on SQLite over two in-memory disks. The guide [Lifecycle](../../docs/records/lifecycle.md#move-a-file-to-another-disk) explains the move.

## Run it

```sh
cd examples
go run ./records-move
go test ./records-move
```

It needs no network or environment variables. The SQLite database lives in a temporary directory that is deleted when the program exits, and the bytes live in memory.

## What it does

```text
uploaded 3 notes to "incoming"
promoted 3
first note: disk=kept, still on incoming=false, reads "first note"
promote again: 0, <nil>
```

1. Three notes are uploaded to the `incoming` disk.
2. The promote job lists the files still on `incoming` and moves each to `kept`: the copy is checked, the row changes, and the old object is deleted after the commit.
3. The first note's row now names `kept`, its object is gone from `incoming`, and it reads back from `kept`.
4. Running the job again finds nothing left to move: it is safe to schedule and to rerun after a crash.

## The code

<!-- ds:block id=promote-4a2sna36 -->
**promote** · [`examples/records-move/main.go:70-90`](main.go#L70-L90)

```go
func promote(ctx context.Context, s *records.Store) (int, error) {
	moved := 0
	for {
		page, err := s.Files(ctx, records.FileQuery{
			Scope:   tenant,
			Filters: []sqlb.Predicate{sqlb.Eq(records.FileDisk, incoming)},
		})
		if err != nil {
			return moved, err
		}
		if len(page.Files) == 0 {
			return moved, nil
		}
		for _, f := range page.Files {
			if _, err := s.MoveFile(ctx, tenant, f.ID, kept); err != nil {
				return moved, fmt.Errorf("promote %s: %w", f.ID, err)
			}
			moved++
		}
	}
}
```
<!-- /ds:block hash=bf5901 -->

## Files

- `main.go`: the demo.
- `main_test.go`: runs it and checks the output line by line.

## Next

- [Lifecycle](../../docs/records/lifecycle.md#move-a-file-to-another-disk): what `MoveFile` checks and refuses.
- [Replace and visibility](../../docs/records/replace-and-visibility.md): `RenameDisk` for every file at once, and visibility disks.
