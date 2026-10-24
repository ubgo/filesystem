// Package jobs defines the maintenance work of a filesystem deployment as
// plain functions, and a small in-process scheduler to run them.
//
// The library starts no background work on its own: trash retention, the
// sweep of orphaned objects and abandoned uploads, audit and share-link
// retention and quota recounts all wait for something to call them. This
// package names each one as a Job, so the same list runs under the
// in-process Scheduler here, under River (jobs/river) or Asynq
// (jobs/asynq), or under any runner an application already has: a Job is
// a name and a function of a context and the current time.
package jobs

import (
	"context"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
	"github.com/ubgo/filesystem/records"
	"github.com/ubgo/filesystem/records/audit"
	"github.com/ubgo/filesystem/records/locks"
	"github.com/ubgo/filesystem/records/quota"
	"github.com/ubgo/filesystem/records/share"
	"github.com/ubgo/filesystem/records/versions"
)

// Job is one unit of maintenance work; see diskjobs.Job. It is an alias,
// so a Job built here runs under a runner that imports only diskjobs.
type Job = diskjobs.Job

// Result is what one run did; see diskjobs.Result.
type Result = diskjobs.Result

// Func makes a Job of any function; see diskjobs.Func.
func Func(name string, run func(ctx context.Context, now time.Time) (Result, error)) Job {
	return diskjobs.Func(name, run)
}

// Default job names. NameUploadSweep and NameSweepAbandoned are
// diskjobs' own.
const (
	NameSweepTrash     = "filesystem.sweep_trash"
	NameSweepOrphans   = "filesystem.sweep_orphans"
	NameUploadSweep    = diskjobs.NameUploadSweep
	NameSweepAbandoned = diskjobs.NameSweepAbandoned
	NameAuditPrune     = "filesystem.audit_prune"
	NameSharePrune     = "filesystem.share_prune"
	NameSweepLocks     = "filesystem.sweep_locks"
	NameQuotaRecount   = "filesystem.quota_recount"
	NameVersionsTrim   = "filesystem.versions_trim"
)

// retentionJob and previewable are diskjobs' RetentionJob and
// WithDryRun, under the names the jobs here were written with.
var (
	retentionJob = diskjobs.RetentionJob
	previewable  = diskjobs.WithDryRun
)

// SweepTrash purges files trashed more than retention ago
// (records.Store.SweepTrash), giving the trash its retention period. Its
// dry run lists the IDs of the files it would purge (FindTrash).
func SweepTrash(store *records.Store, retention time.Duration) Job {
	return previewable(retentionJob(NameSweepTrash, retention, func(ctx context.Context, cutoff time.Time) (int64, error) {
		n, err := store.SweepTrash(ctx, cutoff)
		return int64(n), err
	}), retention, func(ctx context.Context, cutoff time.Time, found func(string)) error {
		_, err := store.FindTrash(ctx, cutoff, func(f records.File) { found(f.ID) })
		return err
	})
}

// SweepOrphans deletes objects under prefix on the named disk that no file
// or plugin owns and that are older than minAge
// (records.Store.SweepOrphans). Keep minAge well above the longest time an
// upload may wait before it is recorded. Its dry run lists the keys it
// would delete (FindOrphans).
func SweepOrphans(store *records.Store, diskName, prefix string, minAge time.Duration) Job {
	return previewable(retentionJob(NameSweepOrphans, minAge, func(ctx context.Context, cutoff time.Time) (int64, error) {
		n, err := store.SweepOrphans(ctx, diskName, prefix, cutoff)
		return int64(n), err
	}), minAge, func(ctx context.Context, cutoff time.Time, found func(string)) error {
		_, err := store.FindOrphans(ctx, diskName, prefix, cutoff, found)
		return err
	})
}

// UploadSweep deletes uploads under dir older than minAge that were never
// confirmed or attached; see diskjobs.UploadSweep.
func UploadSweep(disk *filesystem.Disk, dir string, minAge time.Duration) Job {
	return diskjobs.UploadSweep(disk, dir, minAge)
}

// SweepAbandoned removes multipart uploads and staging files idle for
// minAge; see diskjobs.SweepAbandoned.
func SweepAbandoned(disk *filesystem.Disk, minAge time.Duration) Job {
	return diskjobs.SweepAbandoned(disk, minAge)
}

// AbandonedItem is how SweepAbandoned's dry run names one leftover; see
// diskjobs.AbandonedItem.
func AbandonedItem(a filesystem.Abandoned) string { return diskjobs.AbandonedItem(a) }

// AuditPrune deletes audit entries older than retention (audit.Plugin.Prune).
func AuditPrune(plugin *audit.Plugin, store *records.Store, retention time.Duration) Job {
	return retentionJob(NameAuditPrune, retention, func(ctx context.Context, cutoff time.Time) (int64, error) {
		return plugin.Prune(ctx, store, cutoff)
	})
}

// SharePrune deletes share links expired or revoked more than retention
// ago (share.Plugin.Prune).
func SharePrune(plugin *share.Plugin, store *records.Store, retention time.Duration) Job {
	return retentionJob(NameSharePrune, retention, func(ctx context.Context, cutoff time.Time) (int64, error) {
		return plugin.Prune(ctx, store, cutoff)
	})
}

// SweepLocks deletes the check-out locks expired at the run's time
// (locks.Plugin.Sweep). An expired lock already allows every change, so
// the job only tidies the table; it needs no retention of its own.
func SweepLocks(plugin *locks.Plugin, store *records.Store) Job {
	return Job{Name: NameSweepLocks, Run: func(ctx context.Context, now time.Time) (Result, error) {
		n, err := plugin.Sweep(ctx, store, now)
		return Result{Count: n}, err
	}}
}

// QuotaRecount rebuilds the usage of every scope scopes returns
// (quota.Plugin.Recount), repairing drift after manual database changes.
// A failing scope does not stop the others; the error names the first one
// that failed and the count says how many succeeded.
func QuotaRecount(plugin *quota.Plugin, store *records.Store, scopes func(ctx context.Context) ([]string, error)) Job {
	return Job{Name: NameQuotaRecount, Run: func(ctx context.Context, _ time.Time) (Result, error) {
		list, err := scopes(ctx)
		if err != nil {
			return Result{}, err
		}
		var res Result
		var first error
		for _, scope := range list {
			if _, err := plugin.Recount(ctx, store, scope); err != nil {
				if first == nil {
					first = filesystem.Errorf(filesystem.CodeOf(err), "recount scope %q: %w", scope, err)
				}
				continue
			}
			res.Count++
		}
		return res, first
	}}
}

// VersionsTrim applies each file's history policy (versions.WithKeep or
// WithKeepFunc, count and MaxAge) in every scope scopes returns
// (versions.Plugin.TrimScope), so "30 days of versions" holds for files
// nobody replaces. The count is the versions dropped. A failing scope does
// not stop the others; the error names the first one that failed.
func VersionsTrim(plugin *versions.Plugin, store *records.Store, scopes func(ctx context.Context) ([]string, error)) Job {
	return Job{Name: NameVersionsTrim, Run: func(ctx context.Context, _ time.Time) (Result, error) {
		list, err := scopes(ctx)
		if err != nil {
			return Result{}, err
		}
		var res Result
		var first error
		for _, scope := range list {
			n, err := plugin.TrimScope(ctx, store, scope)
			res.Count += int64(n)
			if err != nil && first == nil {
				first = filesystem.Errorf(filesystem.CodeOf(err), "trim versions of scope %q: %w", scope, err)
			}
		}
		return res, first
	}}
}
