package jobs

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// NameMigrateDisk is MigrateDisk's default job name.
const NameMigrateDisk = "filesystem.migrate_disk"

// MigrateOptions configure MigrateDisk.
type MigrateOptions struct {
	// Cursor keeps the last file handled, so a run continues the pass
	// where the previous one stopped and skips files it could not move
	// until the next pass. Required.
	Cursor Cursor
	// Batch is how many files one run handles; 0 means 500.
	Batch int
	// OnMoved receives each file moved, as it is on the new disk.
	OnMoved func(records.File)
	// OnFailed receives each file that could not be moved, with the
	// reason; it stays whole on the old disk and is retried next pass.
	OnFailed func(f records.File, err error)
}

// MigrateDisk returns a job that moves every file recorded on the disk
// named from to the disk named to, a batch per run, for a change of
// backend ("local disk to S3", "one bucket to another") done in the
// background while the application keeps serving.
//
// Each file moves with records.Store.MoveFile, which is the cut-over and
// its safety: the object is copied to to and checked against the row
// (size, and SHA-256 when known) before the row changes, the row changes
// in one transaction with the plugins' own objects, and the old object is
// deleted only after the commit. A crash at any point therefore leaves
// every file readable: before the commit the row still names the old
// disk, whose object is untouched (at most an extra copy waits on to, and
// the next attempt overwrites it); after it the row names the new disk,
// whose copy was verified. Rerunning the job finishes what was left.
//
// Trashed files are not moved (MoveFile refuses them) and stay on the old
// disk until purged; a file that fails goes to OnFailed and the run goes
// on. The cursor is saved at the end of every run, also when it fails
// part-way; when a pass reaches the last file it saves an empty cursor,
// and the next run starts a new pass, which retries the failures. The
// migration is complete when a pass moves nothing and fails nothing.
// Result.Count is the files moved.
func MigrateDisk(store *records.Store, from, to string, opts MigrateOptions) Job {
	return Job{Name: NameMigrateDisk, Run: func(ctx context.Context, _ time.Time) (Result, error) {
		if from == "" || to == "" || from == to || opts.Cursor == nil {
			return Result{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s needs two different disks and a cursor", NameMigrateDisk)
		}
		if _, err := store.Disks().Disk(to); err != nil {
			return Result{}, err
		}
		after, err := opts.Cursor.Load(ctx)
		if err != nil {
			return Result{}, err
		}
		limit := batchSize(opts.Batch)
		refs, err := filesOn(ctx, store, from, after, limit)
		if err != nil {
			return Result{}, err
		}
		var res Result
		handled := after
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return res, saveOnError(ctx, opts.Cursor, handled, err)
			}
			moved, err := migrateOne(ctx, store, ref, to, opts)
			if err != nil {
				return res, saveOnError(ctx, opts.Cursor, handled, err)
			}
			if moved {
				res.Count++
			}
			handled = ref.id
		}
		if len(refs) < limit {
			handled = ""
		}
		return res, opts.Cursor.Save(ctx, handled)
	}}
}

// migrateOne moves one file and reports whether it moved. A failure of
// the file itself goes to OnFailed; only a failure that stops the run (the
// database unreachable, ctx ended) is returned.
func migrateOne(ctx context.Context, store *records.Store, ref fileRef, to string, opts MigrateOptions) (bool, error) {
	f, err := store.File(ctx, ref.scope, ref.id)
	switch {
	case filesystem.CodeOf(err) == filesystem.CodeNotFound:
		return false, nil // purged since it was listed
	case err != nil:
		return false, err
	case f.DeletedAt != nil:
		return false, nil
	}
	moved, err := store.MoveFile(ctx, f.Scope, f.ID, to)
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		if opts.OnFailed != nil {
			opts.OnFailed(f, err)
		}
		return false, nil
	}
	if opts.OnMoved != nil {
		opts.OnMoved(moved)
	}
	return true, nil
}
