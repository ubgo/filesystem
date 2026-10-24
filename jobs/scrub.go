package jobs

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/records"
)

// NameScrub is Scrub's default job name.
const NameScrub = "filesystem.scrub"

// ScrubOptions configure Scrub.
type ScrubOptions struct {
	// Disk is the name of the disk whose files are checked. Required.
	Disk string
	// Cursor keeps the last file checked, so each run continues the
	// pass where the previous one stopped and a pass over a large disk
	// spreads over many runs. Required.
	Cursor Cursor
	// Batch is how many files one run checks; 0 means 500.
	Batch int
	// Interval is the pause after each object read, which bounds the read
	// rate (and the egress bill) of a scrub: 100ms caps it at ten objects
	// a second. 0 reads as fast as the disk answers.
	Interval time.Duration
	// OnMissing receives each file whose object the disk does not have.
	OnMissing func(records.File)
	// OnMismatch receives each file whose object's content no longer
	// matches the row: size is the stored object's, sum its SHA-256.
	OnMismatch func(f records.File, size int64, sum string)
	// OnError receives each file whose object could not be checked: a
	// Stat or read failure other than not found, such as an object in
	// an archive tier. The run reports it, moves past it and goes on.
	OnError func(f records.File, err error)
}

// Scrub returns a job that re-reads the objects of the files recorded on
// one disk, a batch per run, re-hashes each and compares it with the
// SHA-256 and size the row recorded, reporting objects that are missing
// (OnMissing) or whose content changed (OnMismatch): silent corruption, a
// hand-edited bucket, a restore that brought back the wrong version. It
// only reports; it never deletes or rewrites anything, because restoring
// from a backup, re-uploading or purging is the application's decision.
//
// Files are walked in ID order across every scope, trashed ones
// included (their objects are still kept). An object that cannot be read
// goes to OnError and the run goes on; only a database failure or an
// ended ctx stops it. The cursor is saved at the end
// of every run, also when it fails, so a rerun repeats at most the file
// that failed; when a run reaches the last file it saves an empty cursor
// and the next run starts a new pass. A file without a recorded SHA-256
// is checked for size only. Objects that plugins keep (versions,
// variants) are not checked. Result.Count is the files checked.
func Scrub(store *records.Store, opts ScrubOptions) Job {
	return Job{Name: NameScrub, Run: func(ctx context.Context, _ time.Time) (Result, error) {
		if opts.Disk == "" || opts.Cursor == nil {
			return Result{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s needs a disk and a cursor", NameScrub)
		}
		disk, err := store.Disks().Disk(opts.Disk)
		if err != nil {
			return Result{}, err
		}
		after, err := opts.Cursor.Load(ctx)
		if err != nil {
			return Result{}, err
		}
		limit := batchSize(opts.Batch)
		refs, err := filesOn(ctx, store, opts.Disk, after, limit)
		if err != nil {
			return Result{}, err
		}
		var res Result
		checked := after
		for _, ref := range refs {
			if err := scrubOne(ctx, store, disk, ref, opts); err != nil {
				return res, saveOnError(ctx, opts.Cursor, checked, err)
			}
			checked = ref.id
			res.Count++
			if err := pause(ctx, opts.Interval); err != nil {
				return res, saveOnError(ctx, opts.Cursor, checked, err)
			}
		}
		if len(refs) < limit {
			checked = "" // the pass is complete: the next run starts over
		}
		return res, opts.Cursor.Save(ctx, checked)
	}}
}

// scrubOne checks one file's object against its row.
func scrubOne(ctx context.Context, store *records.Store, disk *filesystem.Disk, ref fileRef, opts ScrubOptions) error {
	f, err := store.File(ctx, ref.scope, ref.id)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		return nil // purged since it was listed
	}
	if err != nil {
		return err
	}
	if f.Disk != opts.Disk {
		return nil // moved since it was listed
	}
	e, err := disk.Stat(ctx, f.Key)
	if filesystem.CodeOf(err) == filesystem.CodeNotFound {
		if opts.OnMissing != nil {
			opts.OnMissing(f)
		}
		return nil
	}
	if err != nil {
		return objectFailed(ctx, f, err, opts)
	}
	sum := ""
	if f.SHA256 != "" {
		if sum, err = filesystem.ObjectSHA256(ctx, disk, f.Key); err != nil {
			return objectFailed(ctx, f, err, opts)
		}
	}
	if (e.Size != f.Size || sum != f.SHA256) && opts.OnMismatch != nil {
		opts.OnMismatch(f, e.Size, sum)
	}
	return nil
}

// objectFailed reports a file whose object could not be checked (an
// archived object, a permission error) to OnError and lets the run go
// on, so one unreadable object never holds the cursor back. Only an ended
// ctx stops the run.
func objectFailed(ctx context.Context, f records.File, err error, opts ScrubOptions) error {
	if ctx.Err() != nil {
		return err
	}
	if opts.OnError != nil {
		opts.OnError(f, err)
	}
	return nil
}

// pause waits d, or less when ctx ends first.
func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
