package remote

import (
	"context"
	"slices"
	"time"

	"github.com/ubgo/filesystem"
)

// SweepOptions bounds the cache. At least one bound is required.
type SweepOptions struct {
	// MaxBytes, when positive, evicts the least recently used entries
	// until the cache holds at most this many bytes.
	MaxBytes int64
	// MaxAge, when positive, evicts entries not used for longer.
	MaxAge time.Duration
}

// SweepResult is what a sweep did.
type SweepResult struct {
	// Deleted and Freed count the evicted entries and their bytes.
	Deleted int
	Freed   int64
	// Kept and Bytes count what is left.
	Kept  int
	Bytes int64
}

// Sweep evicts entries, media and link previews alike, by modification
// time: the last fetch, or the last hit on a disk that can touch (see
// Options.TouchInterval). First every entry older than MaxAge goes, then
// the oldest until the rest fits in MaxBytes. It deletes only under the
// cache's own directories, and is safe to run again after a failure and
// alongside readers: an evicted entry is fetched again on its next use.
// Run it on a schedule (jobs.Func). CodeInvalidArgument without a bound.
func (c *Cache) Sweep(ctx context.Context, o SweepOptions) (SweepResult, error) {
	if o.MaxBytes <= 0 && o.MaxAge <= 0 {
		return SweepResult{}, filesystem.Errorf(filesystem.CodeInvalidArgument, "remote: a sweep needs MaxBytes or MaxAge")
	}
	var entries []filesystem.Entry
	var res SweepResult
	for _, kind := range []string{mediaDir, previewDir} {
		for e, err := range c.disk.List(ctx, filesystem.JoinKey(c.opts.Dir, kind), filesystem.ListOptions{Recursive: true}) {
			if err != nil {
				return res, err
			}
			if !e.IsDir {
				entries = append(entries, e)
				res.Bytes += e.Size
			}
		}
	}
	slices.SortFunc(entries, func(a, b filesystem.Entry) int { return a.ModTime.Compare(b.ModTime) })
	cutoff := c.opts.Now().Add(-o.MaxAge)
	var evict []string
	for _, e := range entries {
		aged := o.MaxAge > 0 && e.ModTime.Before(cutoff)
		over := o.MaxBytes > 0 && res.Bytes > o.MaxBytes
		if !aged && !over {
			break // sorted oldest first: nothing after is aged either
		}
		evict = append(evict, e.Key)
		res.Deleted++
		res.Freed += e.Size
		res.Bytes -= e.Size
	}
	res.Kept = len(entries) - res.Deleted
	if len(evict) == 0 {
		return res, nil
	}
	if err := c.disk.DeleteKeys(ctx, evict); err != nil {
		// Some keys may be gone already; the next sweep finishes the rest.
		return SweepResult{}, err
	}
	return res, nil
}
