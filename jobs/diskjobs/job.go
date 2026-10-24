// Package diskjobs is the half of jobs that needs no database: the Job
// and Result types, the in-process Scheduler, and the jobs that act on
// disks alone (UploadSweep, SweepAbandoned, Promote).
//
// Why it exists: package jobs also holds the records-backed jobs (trash,
// orphans, audit, share, quota and the rest), so importing it compiles in
// records and sqlb. An application that keeps no records imports this
// package instead and builds none of that. Package jobs re-exports
// everything here under the same names (its Job is this Job, by alias),
// so both import paths name the same types and mix freely.
//
// Invariant: this package imports the standard library and the core
// module only, never records; deps_test.go in jobs enforces it.
package diskjobs

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/upload"
)

// Job is one unit of maintenance work. Run receives the time the runner
// considers "now", from which a job computes its cutoffs, so tests and
// runners with their own clock stay deterministic. A job must be safe to
// run again after a failure or a crash: every job here is (each finishes
// what the previous run left).
type Job struct {
	// Name identifies the job to schedulers, logs and runners; it must be
	// unique among the jobs scheduled together. The constructors give a
	// default; Named changes it, for two jobs of one kind (two disks).
	Name string
	// Run does the work once.
	Run func(ctx context.Context, now time.Time) (Result, error)
	// DryRun, when set, reports what Run would do at now without changing
	// anything: Result.Count is how many items Run would act on and
	// Result.Planned names them. A schedule with DryRun set runs it in
	// place of Run (Schedule.DryRun). Nil means the job cannot preview
	// itself, and scheduling it as a dry run is refused.
	DryRun func(ctx context.Context, now time.Time) (Result, error)
}

// Result is what one run did.
type Result struct {
	// Count is how many items the run processed: files purged, objects
	// deleted, rows pruned, scopes recounted.
	Count int64
	// Planned is set by a dry run only: what a real run would act on, as
	// the job names its items (object keys for disk sweeps, file IDs for
	// file sweeps), in the order it found them.
	Planned []string
}

// Named returns a copy of j under another name.
func (j Job) Named(name string) Job {
	j.Name = name
	return j
}

// Func makes a Job of any function: the hook point for work defined
// elsewhere, such as generating image variants eagerly
// (contrib/imaging's variants plugin) or an application's own cleanup.
func Func(name string, run func(ctx context.Context, now time.Time) (Result, error)) Job {
	return Job{Name: name, Run: run}
}

// Default names of the jobs in this package.
const (
	NameUploadSweep    = "filesystem.upload_sweep"
	NameSweepAbandoned = "filesystem.sweep_abandoned"
)

// errRetention refuses a non-positive retention: "keep nothing" would
// purge or prune everything the moment the job first runs, which is never
// what a misconfigured zero means.
func errRetention(job string, d time.Duration) error {
	return filesystem.Errorf(filesystem.CodeInvalidArgument, "job %s: retention %s is not positive", job, d)
}

// RetentionJob builds a job named name that acts on everything older
// than retention: Run computes the cutoff now minus retention and passes
// it to run, whose count becomes Result.Count. It is the shape of every
// sweep and prune here, exported so an application's own retention work
// gets the same guard.
//
// Invariant: a retention that is not positive fails every run with
// CodeInvalidArgument instead of acting on everything, because a
// misconfigured zero never means "keep nothing".
func RetentionJob(name string, retention time.Duration, run func(ctx context.Context, cutoff time.Time) (int64, error)) Job {
	return Job{Name: name, Run: func(ctx context.Context, now time.Time) (Result, error) {
		if retention <= 0 {
			return Result{}, errRetention(name, retention)
		}
		n, err := run(ctx, now.Add(-retention))
		return Result{Count: n}, err
	}}
}

// WithDryRun gives a job built by RetentionJob its dry run: find calls
// found with each item the job would act on for the same cutoff, and the
// items become Result.Planned. The retention guard applies to the dry
// run as to Run.
func WithDryRun(j Job, retention time.Duration, find func(ctx context.Context, cutoff time.Time, found func(string)) error) Job {
	j.DryRun = func(ctx context.Context, now time.Time) (Result, error) {
		if retention <= 0 {
			return Result{}, errRetention(j.Name, retention)
		}
		var res Result
		err := find(ctx, now.Add(-retention), func(item string) { res.Planned = append(res.Planned, item) })
		res.Count = int64(len(res.Planned))
		return res, err
	}
	return j
}

// UploadSweep deletes uploads under dir older than minAge that were never
// confirmed or attached (upload.Sweep). Its dry run lists the keys it
// would delete (upload.FindStale).
func UploadSweep(disk *filesystem.Disk, dir string, minAge time.Duration) Job {
	return WithDryRun(RetentionJob(NameUploadSweep, minAge, func(ctx context.Context, cutoff time.Time) (int64, error) {
		n, err := upload.Sweep(ctx, disk, dir, cutoff)
		return int64(n), err
	}), minAge, func(ctx context.Context, cutoff time.Time, found func(string)) error {
		_, err := upload.FindStale(ctx, disk, dir, cutoff, func(e filesystem.Entry) { found(e.Key) })
		return err
	})
}

// SweepAbandoned removes multipart uploads and staging files idle for
// minAge (filesystem.Disk.SweepAbandoned). Keep minAge well above the
// longest upload allowed. On a disk with the FindAbandoned capability its
// dry run lists what it would remove (filesystem.Disk.FindAbandoned), each
// as AbandonedItem names it; on any other disk it has none, so a dry-run
// schedule of it is refused with CodeUnsupported when it is registered.
func SweepAbandoned(disk *filesystem.Disk, minAge time.Duration) Job {
	j := RetentionJob(NameSweepAbandoned, minAge, func(ctx context.Context, cutoff time.Time) (int64, error) {
		n, err := disk.SweepAbandoned(ctx, cutoff)
		return int64(n), err
	})
	if !disk.Capabilities().FindAbandoned {
		return j
	}
	return WithDryRun(j, minAge, func(ctx context.Context, cutoff time.Time, found func(string)) error {
		items, err := disk.FindAbandoned(ctx, cutoff)
		for _, a := range items {
			found(AbandonedItem(a))
		}
		return err
	})
}

// AbandonedItem is how SweepAbandoned's dry run names one leftover in
// Result.Planned: "<kind>:<id>", followed by a space and the key for a
// multipart upload whose key is known ("multipart:7 videos/big.mp4",
// "staging:ABC…"). The kind and ID never hold a space, so the first space
// separates the key, which may.
func AbandonedItem(a filesystem.Abandoned) string {
	item := string(a.Kind) + ":" + a.ID
	if a.Key != "" {
		item += " " + a.Key
	}
	return item
}
