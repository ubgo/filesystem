package jobs

import (
	"context"
	"time"

	"github.com/ubgo/filesystem/records"
)

// Job names of the records lifecycle sweeps.
const (
	NameSweepExpired   = "filesystem.sweep_expired"
	NameSweepRetention = "filesystem.sweep_retention"
)

// SweepExpired purges every file whose ExpiresAt has passed at the run's
// time (records.Store.SweepExpired): temporary and viewed view-once
// files. It needs no retention of its own; each file carries its expiry.
func SweepExpired(store *records.Store) Job {
	return Job{Name: NameSweepExpired, Run: func(ctx context.Context, now time.Time) (Result, error) {
		n, err := store.SweepExpired(ctx, now)
		return Result{Count: int64(n)}, err
	}}
}

// SweepRetention applies the Store's retention rules (records.WithRetention)
// at the run's time (records.Store.SweepRetention, never a dry run): it
// detaches attachments past their rule's age and purges files left with
// none. Count is the attachments detached. Its dry run
// (Store.SweepRetention's) lists the attachment IDs it would detach.
func SweepRetention(store *records.Store) Job {
	return Job{Name: NameSweepRetention, Run: func(ctx context.Context, now time.Time) (Result, error) {
		rep, err := store.SweepRetention(ctx, now, false)
		return Result{Count: int64(len(rep.Detached))}, err
	}, DryRun: func(ctx context.Context, now time.Time) (Result, error) {
		rep, err := store.SweepRetention(ctx, now, true)
		res := Result{Count: int64(len(rep.Detached))}
		for _, a := range rep.Detached {
			res.Planned = append(res.Planned, a.ID)
		}
		return res, err
	}}
}
