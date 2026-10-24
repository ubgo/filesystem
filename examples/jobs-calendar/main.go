// Command jobs-calendar runs a deployment's maintenance with no database:
// the jobs of github.com/ubgo/filesystem/jobs/diskjobs, a module that
// requires the core module only, on a calendar ("every day at 03:00")
// rather than an interval, plus a job of the application's own that
// touches nothing but a disk.
//
//	go run ./jobs-calendar          (from the examples directory)
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ubgo/filesystem"
	"github.com/ubgo/filesystem/jobs/diskjobs"
	"github.com/ubgo/filesystem/memory"
)

// Settings of this deployment.
const (
	// pendingDir is where uploads wait for the application to confirm
	// them; the sweep removes those nobody confirmed.
	pendingDir = "pending"
	// uploadMaxAge leaves younger uploads alone: one still being written
	// is never swept.
	uploadMaxAge = 24 * time.Hour
	// sweepHour and sweepMinute are the wall time of the nightly sweep.
	sweepHour, sweepMinute = 3, 0
	// cacheBudget is how many bytes the thumbnail cache may hold.
	cacheBudget = 10
	// nameTrimCache names the application's own job in reports.
	nameTrimCache = "app.trim_thumb_cache"
)

// data is the bytes every demo object holds.
const data = "12345"

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// schedules is this deployment's maintenance: the upload sweep every day
// at 03:00 UTC, a preview of the abandoned-upload sweep on the same
// calendar (it reports what it would remove and changes nothing, until
// the schedule's DryRun is dropped), and the application's own cache
// trim every hour.
// ds:def id=calendarschedules-k7d2qm41
func schedules(uploads, thumbs *filesystem.Disk) []diskjobs.Schedule {
	nightly := diskjobs.Daily(sweepHour, sweepMinute, time.UTC)
	trim := diskjobs.Func(nameTrimCache, func(ctx context.Context, _ time.Time) (diskjobs.Result, error) {
		n, err := filesystem.TrimCache(ctx, thumbs, cacheBudget)
		return diskjobs.Result{Count: int64(n)}, err
	})
	return []diskjobs.Schedule{
		{Job: diskjobs.UploadSweep(uploads, pendingDir, uploadMaxAge), Next: nightly, RunAtStart: true},
		{Job: diskjobs.SweepAbandoned(uploads, uploadMaxAge), Next: nightly, RunAtStart: true, DryRun: true},
		{Job: trim, Every: time.Hour, RunAtStart: true},
	}
}

func run(ctx context.Context, out io.Writer) error {
	// The objects are written on day 0 at 09:00; the scheduler starts two
	// days later at 14:30, past every minimum age.
	day0 := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	start := day0.Add(2*24*time.Hour + 5*time.Hour + 30*time.Minute)
	uploads, thumbs, err := seed(ctx, day0)
	if err != nil {
		return err
	}

	all := schedules(uploads, thumbs)
	for _, sc := range all {
		if sc.Next != nil {
			fmt.Fprintf(out, "%s: next run %s\n", sc.Job.Name, sc.Next(start).Format(time.RFC3339))
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	first := map[string]string{}
	s := diskjobs.NewScheduler(diskjobs.SchedulerOptions{
		Now: func() time.Time { return start },
		OnRun: func(r diskjobs.Report) {
			mu.Lock()
			defer mu.Unlock()
			if _, seen := first[r.Job]; seen {
				return
			}
			line := fmt.Sprintf("%s: %d, err %v", r.Job, r.Result.Count, r.Err)
			if r.DryRun {
				line += fmt.Sprintf(", dry run: %s", strings.Join(r.Result.Planned, " "))
			}
			first[r.Job] = line
			if len(first) == len(all) {
				cancel() // every job ran once at start; stop the demo
			}
		},
	})
	if err := s.Run(ctx, all...); err != nil {
		return err
	}
	lines := make([]string, 0, len(first))
	for _, l := range first {
		lines = append(lines, l)
	}
	slices.Sort(lines)
	fmt.Fprintln(out, strings.Join(lines, "\n"))
	return nil
}

// seed returns the uploads disk, holding an upload nobody confirmed and an
// abandoned multipart upload, and the thumbnail cache, holding three
// thumbnails over its budget.
func seed(ctx context.Context, at time.Time) (uploads, thumbs *filesystem.Disk, err error) {
	clock := func() time.Time { return at }
	if uploads, err = filesystem.NewDisk("uploads", memory.New(memory.WithClock(clock))); err != nil {
		return nil, nil, err
	}
	if thumbs, err = filesystem.NewDisk("thumbs", memory.New(memory.WithClock(clock))); err != nil {
		return nil, nil, err
	}
	if _, err = uploads.WriteBytes(ctx, pendingDir+"/never-confirmed.png", []byte(data), filesystem.WriteOptions{}); err != nil {
		return nil, nil, err
	}
	if _, err = uploads.CreateMultipart(ctx, pendingDir+"/big.mov", filesystem.WriteOptions{}); err != nil {
		return nil, nil, err
	}
	for _, k := range []string{"a.webp", "b.webp", "c.webp"} {
		if _, err = thumbs.WriteBytes(ctx, k, []byte(data), filesystem.WriteOptions{}); err != nil {
			return nil, nil, err
		}
	}
	return uploads, thumbs, nil
}
