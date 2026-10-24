package diskjobs

import (
	"context"
	"errors"
	"time"

	"github.com/ubgo/filesystem"
)

// NamePromote is the default name of Promote's job.
const NamePromote = "filesystem.promote"

// Promote is the background half of "accept fast, store later" (UP-23):
// uploads are stored on a fast disk under dir (a local disk next to the
// application, Receive with RandomKey(dir)), the request answers at once,
// and this job moves each of them to the slow, durable store (S3) under
// the same key without the dir prefix: "pending/a.png" on staging becomes
// "a.png" on store. Run it on a short interval, and RunNow right after an
// upload for the shortest delay; it is single-flight under the Scheduler.
//
// An object is moved only once it is at least minAge old (0 moves
// everything), which leaves an upload still being written alone. Each move
// is a copy, held to the source's size, then the staging object's
// deletion (filesystem.MoveAcross), so a crash between the two leaves the
// object on both disks and the next run finishes it. A failing object does
// not stop the others: the error joins one error per object left, and the
// count is the objects moved. dir must not be the root (CodeInvalidArgument).
//
// Until an object is moved, readers must look on staging: Stat the store
// first and fall back to staging under dir.
func Promote(staging *filesystem.Disk, dir string, store *filesystem.Disk, minAge time.Duration) Job {
	return Job{Name: NamePromote, Run: func(ctx context.Context, now time.Time) (Result, error) {
		clean, err := filesystem.CleanDir(dir)
		if err != nil {
			return Result{}, err
		}
		if clean == "" || minAge < 0 {
			return Result{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: the staging directory must not be the root and minAge %s not negative", NamePromote, minAge)
		}
		cutoff := now.Add(-minAge)
		var res Result
		var errs []error
		for e, err := range staging.List(ctx, clean, filesystem.ListOptions{Recursive: true}) {
			if err != nil {
				return res, errors.Join(append(errs, err)...)
			}
			if minAge > 0 && (e.ModTime.IsZero() || e.ModTime.After(cutoff)) {
				continue
			}
			if _, err := filesystem.MoveAcross(ctx, staging, e.Key, store, filesystem.RelativeKey(e.Key, clean)); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Count++
		}
		return res, errors.Join(errs...)
	}}
}
