package upload

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
)

// Sweep deletes every object under dir last modified before cutoff and
// returns how many it deleted. Point it at the prefix where direct uploads
// land before Confirm (and where server uploads wait to be attached), and
// schedule it: uploads that are never confirmed would otherwise cost storage
// forever. The library starts no background goroutine; the application
// decides when sweeping runs.
//
// It deletes as it lists, so a failure part-way returns the count so far
// with the error, and a rerun continues where it stopped.
//
// dir must not be the root: sweeping a whole disk by age is never what an
// upload area wants, so it is refused with CodeInvalidArgument. An object
// whose backend reports no modification time is kept: its age is unknown,
// and a zero time would otherwise count as older than any cutoff.
func Sweep(ctx context.Context, disk *filesystem.Disk, dir string, cutoff time.Time) (int, error) {
	return stale(ctx, disk, dir, cutoff, func(e filesystem.Entry) error { return disk.Delete(ctx, e.Key) })
}

// FindStale is Sweep's dry run: it calls found with every object Sweep
// would delete for the same dir and cutoff, in listing order, and returns
// how many it found, deleting nothing. It refuses the root as Sweep does.
func FindStale(ctx context.Context, disk *filesystem.Disk, dir string, cutoff time.Time, found func(filesystem.Entry)) (int, error) {
	return stale(ctx, disk, dir, cutoff, func(e filesystem.Entry) error { found(e); return nil })
}

// stale calls act with every object under dir (never the root) last
// modified before cutoff, stopping at the first error, and returns how
// many act accepted: the selection Sweep and FindStale share, so the dry
// run reports exactly what the sweep deletes.
func stale(ctx context.Context, disk *filesystem.Disk, dir string, cutoff time.Time, act func(filesystem.Entry) error) (int, error) {
	clean, err := filesystem.CleanDir(dir)
	if err != nil {
		return 0, err
	}
	if clean == "" {
		return 0, filesystem.Errorf(filesystem.CodeInvalidArgument, "refusing to sweep the root of disk %s", disk.Name())
	}
	n := 0
	for e, err := range disk.List(ctx, clean, filesystem.ListOptions{Recursive: true}) {
		if err != nil {
			return n, err
		}
		if e.ModTime.IsZero() || !e.ModTime.Before(cutoff) {
			continue
		}
		if err := act(e); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
