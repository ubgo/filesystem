package filesystem

import (
	"context"
	"slices"
	"strings"
	"time"
)

// SweepAbandoned removes what unfinished work left in the backend before
// before: multipart uploads never completed or aborted, and staging files
// of interrupted writes (see AbandonedSweeper for exactly what counts). It
// returns how many it removed. Stored objects are never touched; to delete
// uploaded objects nobody confirmed, use upload.Sweep on their prefix.
//
// Choose before well beyond the longest upload the application allows (a
// day ago is usual): a recent before aborts uploads still in progress. The
// library starts no background goroutine, so schedule it like upload.Sweep.
//
// Needs the SweepAbandoned capability; without it the call fails with
// CodeUnsupported. A scoped or mirrored disk never has it: sweep the disk
// it was derived from.
func (d *Disk) SweepAbandoned(ctx context.Context, before time.Time) (int, error) {
	if !d.caps.SweepAbandoned {
		return 0, d.unsupported(OpSweepAbandoned, "")
	}
	sweeper := d.driver.(AbandonedSweeper)
	// A retried attempt continues where the failed one stopped, so the
	// counts of every attempt add up to what was removed.
	var n int
	op := &Op{Kind: OpSweepAbandoned, Mutating: true, Replayable: true}
	err := d.do(ctx, op, func(ctx context.Context) error {
		removed, err := sweeper.SweepAbandoned(ctx, before)
		n += removed
		return err
	})
	return n, err
}

// FindAbandoned is the dry run of SweepAbandoned: it returns what
// SweepAbandoned(ctx, before) would remove now, under the same rules, and
// removes nothing, so a maintenance job can report before it acts. The
// result is ordered by LastActivity, oldest first, then ID. Work that
// starts, completes or is aborted between the preview and the sweep makes
// them differ (see AbandonedFinder).
//
// Needs the FindAbandoned capability; without it the call fails with
// CodeUnsupported. Like SweepAbandoned, a scoped or mirrored disk never
// has it: ask the disk it was derived from.
func (d *Disk) FindAbandoned(ctx context.Context, before time.Time) ([]Abandoned, error) {
	if !d.caps.FindAbandoned {
		return nil, d.unsupported(OpFindAbandoned, "")
	}
	finder := d.driver.(AbandonedFinder)
	var found []Abandoned
	op := &Op{Kind: OpFindAbandoned, Replayable: true}
	err := d.do(ctx, op, func(ctx context.Context) error {
		var err error
		found, err = finder.FindAbandoned(ctx, before)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(found, func(a, b Abandoned) int {
		if c := a.LastActivity.Compare(b.LastActivity); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return found, nil
}
